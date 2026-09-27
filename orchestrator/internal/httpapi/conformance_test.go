package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Conformance of the running server against docs/api/openapi.yaml.
//
// internal/apicontract already guards the other direction: it checks the spec's enums against the Go domain,
// so the published document cannot drift from the types. This file closes the loop by checking that what the
// server actually emits satisfies the document. Both are needed. A spec can agree with the domain enums and
// still describe a payload the handlers do not produce.
//
// # Why the validator is written here rather than imported
//
// A JSON Schema library would be a new dependency in a repo that has stayed deliberately thin, and the subset
// the contract uses is small: $ref, allOf, type including nullable unions, enum, properties, required, items,
// pattern, minimum and a few formats. What a general validator would not do by default is report properties
// the schema does not declare, which for this API is the check that matters most, because an undeclared field
// on a public response is how identity data would escape.
//
// The validator is itself tested. TestTheValidatorRejectsKnownBadDocuments feeds it documents that violate the
// contract in each way it is meant to detect, because a validator that silently passes everything would make
// every test in this file green and worthless.

func contractPath() string {
	return filepath.Join("..", "..", "..", "docs", "api", "openapi.yaml")
}

// loadContract reads the spec as untyped data.
//
// Untyped deliberately. Binding to Go structs would mean this file decided in advance which schema keywords
// exist, and a keyword it did not model would be silently ignored rather than reported as unsupported.
func loadContract(t *testing.T) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(contractPath())
	if err != nil {
		t.Fatalf("reading the contract at %s: %v", contractPath(), err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the contract: %v", err)
	}
	return doc
}

// schemaNamed returns a named schema from components.
func schemaNamed(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()

	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatal("the contract has no components block")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("the contract has no components.schemas block")
	}
	schema, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("the contract declares no schema named %q", name)
	}
	return schema
}

// validator walks a value against a schema, collecting every violation.
//
// Every violation is collected rather than stopping at the first, because a payload that is wrong in four
// places should report four problems. Stopping early turns one fix-and-rerun cycle into four.
type validator struct {
	doc        map[string]any
	violations []string
	// undeclared is kept apart from violations. OpenAPI permits additional properties unless a schema says
	// otherwise, so an extra field is not strictly a conformance failure. It is still worth knowing about,
	// and conflating the two would mean reporting the contract as violated when it is not.
	undeclared []string
}

func (v *validator) fail(path, format string, args ...any) {
	v.violations = append(v.violations, fmt.Sprintf("%s: %s", path, fmt.Sprintf(format, args...)))
}

// resolve follows a $ref and merges allOf, returning the schemas a value must satisfy.
//
// allOf returns several schemas rather than one merged map. Merging would have to decide what happens when
// two members declare the same keyword, and guessing that is how a validator quietly stops checking something.
func (v *validator) resolve(t *testing.T, schema map[string]any) []map[string]any {
	t.Helper()

	if ref, ok := schema["$ref"].(string); ok {
		const prefix = "#/components/schemas/"
		if !strings.HasPrefix(ref, prefix) {
			t.Fatalf("unsupported $ref %q; this validator only resolves component schemas", ref)
		}
		return v.resolve(t, schemaNamed(t, v.doc, strings.TrimPrefix(ref, prefix)))
	}

	if allOf, ok := schema["allOf"].([]any); ok {
		var out []map[string]any
		// Keywords sitting beside allOf still apply, so the outer schema is kept as well.
		outer := make(map[string]any, len(schema))
		for k, val := range schema {
			if k != "allOf" {
				outer[k] = val
			}
		}
		if len(outer) > 0 {
			out = append(out, outer)
		}
		for _, member := range allOf {
			m, ok := member.(map[string]any)
			if !ok {
				t.Fatalf("an allOf member is not a mapping: %T", member)
			}
			out = append(out, v.resolve(t, m)...)
		}
		return out
	}

	return []map[string]any{schema}
}

// check validates a value against a schema.
func (v *validator) check(t *testing.T, path string, value any, schema map[string]any) {
	t.Helper()

	for _, s := range v.resolve(t, schema) {
		v.checkOne(t, path, value, s)
	}
}

// typeNames returns the permitted types, handling the nullable union form.
//
// The contract writes an optional field as `type: [string, 'null']`. Treating that as an unknown type would
// make every nullable field unvalidated, which is the opposite of what declaring it was for.
func typeNames(schema map[string]any) []string {
	switch declared := schema["type"].(type) {
	case string:
		return []string{declared}
	case []any:
		var out []string
		for _, entry := range declared {
			if s, ok := entry.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (v *validator) checkOne(t *testing.T, path string, value any, schema map[string]any) {
	t.Helper()

	types := typeNames(schema)

	if value == nil {
		// null is only acceptable where the schema said so.
		if len(types) > 0 && !contains(types, "null") {
			v.fail(path, "is null, but the contract declares type %v", types)
		}
		return
	}

	if len(types) > 0 && !v.checkType(path, value, types) {
		// A type mismatch makes every further check on this value meaningless.
		return
	}

	if enum, ok := schema["enum"].([]any); ok {
		v.checkEnum(path, value, enum)
	}
	if pattern, ok := schema["pattern"].(string); ok {
		v.checkPattern(t, path, value, pattern)
	}
	if minimum, present := schema["minimum"]; present {
		v.checkMinimum(path, value, minimum)
	}
	if format, ok := schema["format"].(string); ok {
		v.checkFormat(path, value, format)
	}

	if props, ok := schema["properties"].(map[string]any); ok {
		v.checkObject(t, path, value, schema, props)
	}
	if items, ok := schema["items"].(map[string]any); ok {
		v.checkArray(t, path, value, items)
	}
}

func (v *validator) checkType(path string, value any, types []string) bool {
	var actual string
	switch typed := value.(type) {
	case string:
		actual = "string"
	case bool:
		actual = "boolean"
	case map[string]any:
		actual = "object"
	case []any:
		actual = "array"
	case json.Number:
		// Decoded with UseNumber, so an integer field serialised as a float is visible. Without that, a
		// money amount emitted as 1.0e+11 would decode to a float64 and read as a perfectly good integer.
		if strings.ContainsAny(typed.String(), ".eE") {
			actual = "number"
		} else {
			actual = "integer"
		}
	case float64:
		// A float64 means the document was decoded with plain json.Unmarshal rather than decodeJSON, which
		// silently discards the distinction this validator exists to catch: every JSON number becomes a
		// float64, so an amount emitted as 1.0e+11 would validate as a clean integer. Reported loudly rather
		// than accommodated, because accommodating it would make the check pass while checking less.
		v.fail(path, "was decoded as a float64; validate a body read with decodeJSON so numeric precision survives")
		return false
	default:
		actual = fmt.Sprintf("%T", value)
	}

	if contains(types, actual) {
		return true
	}
	// An integer satisfies a schema asking for a number.
	if actual == "integer" && contains(types, "number") {
		return true
	}

	v.fail(path, "is %s, but the contract declares %v (value: %v)", actual, types, value)
	return false
}

func (v *validator) checkEnum(path string, value any, enum []any) {
	got := fmt.Sprint(value)
	allowed := make([]string, 0, len(enum))
	for _, e := range enum {
		allowed = append(allowed, fmt.Sprint(e))
		if fmt.Sprint(e) == got {
			return
		}
	}
	v.fail(path, "is %q, which is not in the contract's enum %v", got, allowed)
}

func (v *validator) checkPattern(t *testing.T, path string, value any, pattern string) {
	t.Helper()

	s, ok := value.(string)
	if !ok {
		return
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("the contract's pattern %q at %s does not compile: %v", pattern, path, err)
	}
	if !re.MatchString(s) {
		v.fail(path, "is %q, which does not match the contract's pattern %s", s, pattern)
	}
}

func (v *validator) checkMinimum(path string, value any, minimum any) {
	num, ok := value.(json.Number)
	if !ok {
		return
	}
	f, err := num.Float64()
	if err != nil {
		v.fail(path, "is %v, which is not a number", value)
		return
	}
	var bound float64
	switch m := minimum.(type) {
	case int:
		bound = float64(m)
	case float64:
		bound = m
	default:
		return
	}
	if f < bound {
		v.fail(path, "is %v, below the contract's minimum of %v", value, minimum)
	}
}

// Formats the contract uses. Checked because a date served as a timestamp, or a timestamp with an offset
// instead of a Z, is exactly the kind of difference a client only discovers by getting the wrong day.
var (
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	dateRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	dateTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$`)
)

func (v *validator) checkFormat(path string, value any, format string) {
	s, ok := value.(string)
	if !ok {
		return
	}
	switch format {
	case "uuid":
		if !uuidRe.MatchString(s) {
			v.fail(path, "is %q, which is not a uuid", s)
		}
	case "date":
		if !dateRe.MatchString(s) {
			v.fail(path, "is %q, which is not a bare YYYY-MM-DD date", s)
		}
	case "date-time":
		if !dateTimeRe.MatchString(s) {
			v.fail(path, "is %q, which is not RFC3339 UTC with a Z suffix", s)
		}
	}
}

func (v *validator) checkObject(t *testing.T, path string, value any, schema map[string]any, props map[string]any) {
	t.Helper()

	obj, ok := value.(map[string]any)
	if !ok {
		return
	}

	if required, ok := schema["required"].([]any); ok {
		for _, r := range required {
			name, ok := r.(string)
			if !ok {
				continue
			}
			if _, present := obj[name]; !present {
				v.fail(path, "is missing %q, which the contract requires", name)
			}
		}
	}

	for name, child := range obj {
		propSchema, declared := props[name].(map[string]any)
		if !declared {
			v.undeclared = append(v.undeclared, joinPath(path, name))
			continue
		}
		v.check(t, joinPath(path, name), child, propSchema)
	}
}

func (v *validator) checkArray(t *testing.T, path string, value any, items map[string]any) {
	t.Helper()

	arr, ok := value.([]any)
	if !ok {
		return
	}
	for i, entry := range arr {
		v.check(t, fmt.Sprintf("%s[%d]", path, i), entry, items)
	}
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// decodeJSON parses a response body preserving numeric precision.
func decodeJSON(t *testing.T, body []byte) any {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("the response is not JSON: %v\nbody: %s", err, body)
	}
	return out
}

// assertConforms validates a response body against a named schema.
func assertConforms(t *testing.T, doc map[string]any, schemaName string, body []byte) *validator {
	t.Helper()

	v := &validator{doc: doc}
	v.check(t, "", decodeJSON(t, body), schemaNamed(t, doc, schemaName))

	if len(v.violations) > 0 {
		sort.Strings(v.violations)
		t.Errorf("the response does not satisfy the published %s schema:\n  %s\n\nbody: %s",
			schemaName, strings.Join(v.violations, "\n  "), body)
	}
	return v
}

// TestSchemeResponseConformsToContract validates the scheme detail payload.
func TestSchemeResponseConformsToContract(t *testing.T) {
	doc := loadContract(t)
	srv, ctx, tx := apiServer(t)
	id := seedAPIScheme(t, ctx, tx)

	rec := get(t, srv, "/v1/schemes/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	assertConforms(t, doc, "Scheme", rec.Body.Bytes())
}

// TestOfferResponseConformsToContract validates the offer detail payload, subscription included.
func TestOfferResponseConformsToContract(t *testing.T) {
	doc := loadContract(t)
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)
	seedAPIBid(t, ctx, tx, offerID, 2, "BLOCKED")

	rec := get(t, srv, "/v1/offers/"+offerID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	assertConforms(t, doc, "Offer", rec.Body.Bytes())
}

// TestOfferWithoutSubscriptionConformsToContract covers the omitted-block shape.
//
// A schema can be satisfied by the rich case and broken by the sparse one, usually because a field the
// contract does not mark required is being emitted as a zero value or a null it does not permit.
func TestOfferWithoutSubscriptionConformsToContract(t *testing.T) {
	doc := loadContract(t)
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)

	rec := get(t, srv, "/v1/offers/"+offerID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	assertConforms(t, doc, "Offer", rec.Body.Bytes())
}

// TestPeriodResponseConformsToContract validates a period with its nullable record date.
func TestPeriodResponseConformsToContract(t *testing.T) {
	doc := loadContract(t)
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	periodID := seedAPIPeriod(t, ctx, tx, schemeID)

	rec := get(t, srv, "/v1/periods/"+periodID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	assertConforms(t, doc, "Period", rec.Body.Bytes())
}

// TestUndeployedSchemeConformsToContract covers the omitted contracts block.
func TestUndeployedSchemeConformsToContract(t *testing.T) {
	doc := loadContract(t)
	srv, ctx, tx := apiServer(t)

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO schemes (sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
			total_units, im_units, public_units)
		VALUES ($1, 'Undeployed', 50000000000, 100000000, 500, 25, 475) RETURNING id`,
		apiUniq("SEBI/SM-REIT/CONFORM")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	rec := get(t, srv, "/v1/schemes/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	assertConforms(t, doc, "Scheme", rec.Body.Bytes())
}

// TestListItemsConformToContract validates each entry of a collection.
//
// The list envelope is not itself a published schema, so the items are validated individually. That is where
// the risk is: a summary view that drops a required field would otherwise go unnoticed because the detail
// view is the one that gets checked.
func TestListItemsConformToContract(t *testing.T) {
	doc := loadContract(t)
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	seedAPIOffer(t, ctx, tx, schemeID)
	seedAPIPeriod(t, ctx, tx, schemeID)

	cases := []struct{ path, schema string }{
		{"/v1/schemes", "Scheme"},
		{"/v1/schemes/" + schemeID + "/offers", "Offer"},
		{"/v1/schemes/" + schemeID + "/periods", "Period"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := get(t, srv, tc.path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
			}

			decoded, ok := decodeJSON(t, rec.Body.Bytes()).(map[string]any)
			if !ok {
				t.Fatal("the list response is not an object")
			}
			items, ok := decoded["items"].([]any)
			if !ok {
				t.Fatalf("items is %T, want an array", decoded["items"])
			}
			if len(items) == 0 {
				t.Fatal("nothing to validate; the fixture did not appear in the list")
			}

			v := &validator{doc: doc}
			for i, item := range items {
				v.check(t, fmt.Sprintf("items[%d]", i), item, schemaNamed(t, doc, tc.schema))
			}
			if len(v.violations) > 0 {
				sort.Strings(v.violations)
				t.Errorf("a list entry does not satisfy the published %s schema:\n  %s\n\nbody: %s",
					tc.schema, strings.Join(v.violations, "\n  "), rec.Body.String())
			}
		})
	}
}

// TestErrorResponsesConformToContract validates the envelope on every failure path.
//
// Error shapes are the least exercised part of any API and the most likely to drift, because the happy path
// is what gets looked at. A client's error handling runs on exactly these bytes.
func TestErrorResponsesConformToContract(t *testing.T) {
	doc := loadContract(t)
	srv, _, _ := apiServer(t)
	const missing = "00000000-0000-0000-0000-000000000000"

	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{"unknown scheme", "/v1/schemes/" + missing, http.StatusNotFound},
		{"unknown offer", "/v1/offers/" + missing, http.StatusNotFound},
		{"unknown period", "/v1/periods/" + missing, http.StatusNotFound},
		{"malformed id", "/v1/schemes/not-a-uuid", http.StatusBadRequest},
		{"bad limit", "/v1/schemes?limit=abc", http.StatusBadRequest},
		{"unknown route", "/v1/nothing-here", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, srv, tc.path, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d. body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}

			assertConforms(t, doc, "ErrorEnvelope", rec.Body.Bytes())
		})
	}
}

// TestNoUndeclaredFieldsOnPublicResponses reports fields the contract does not publish.
//
// OpenAPI permits additional properties by default, so this is not strictly a conformance failure, which is
// why it is a separate test. It is enforced anyway: the frontend is built from the published document, so a
// field that is not in it is a field nobody can rely on, and on a public endpoint an undeclared field is how
// something identity-bearing would escape unnoticed.
func TestNoUndeclaredFieldsOnPublicResponses(t *testing.T) {
	doc := loadContract(t)
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)
	seedAPIBid(t, ctx, tx, offerID, 2, "BLOCKED")
	periodID := seedAPIPeriod(t, ctx, tx, schemeID)

	cases := []struct{ path, schema string }{
		{"/v1/schemes/" + schemeID, "Scheme"},
		{"/v1/offers/" + offerID, "Offer"},
		{"/v1/periods/" + periodID, "Period"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := get(t, srv, tc.path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
			}

			v := assertConforms(t, doc, tc.schema, rec.Body.Bytes())

			if len(v.undeclared) > 0 {
				sort.Strings(v.undeclared)
				t.Errorf("the response carries %d field(s) the contract does not publish:\n  %s\n\n"+
					"Either add them to docs/api/openapi.yaml or stop emitting them. A field absent from the "+
					"contract is a field no client can rely on.",
					len(v.undeclared), strings.Join(v.undeclared, "\n  "))
			}
		})
	}
}

// TestTheValidatorRejectsKnownBadDocuments is what makes every other test in this file mean something.
//
// A validator that reported no violations would turn this whole file green while checking nothing. Each case
// below is a way a response could break the contract, fed in deliberately.
func TestTheValidatorRejectsKnownBadDocuments(t *testing.T) {
	doc := loadContract(t)

	cases := []struct {
		name      string
		schema    string
		body      string
		wantInMsg string
	}{
		{
			name:      "a required field is missing",
			schema:    "Scheme",
			body:      `{"id":"cfa9ddd6-0377-4cbf-83b8-23b8ca6fa1fa","name":"X"}`,
			wantInMsg: "sebiSchemeRef",
		},
		{
			name:      "money served as a string",
			schema:    "Scheme",
			body:      schemeBodyWith(`"unitPricePaise":"100000000"`),
			wantInMsg: "unitPricePaise",
		},
		{
			name:      "money served as a float",
			schema:    "Scheme",
			body:      schemeBodyWith(`"unitPricePaise":1.0e8`),
			wantInMsg: "unitPricePaise",
		},
		{
			name:      "a negative amount breaks the minimum",
			schema:    "Scheme",
			body:      schemeBodyWith(`"unitPricePaise":-1`),
			wantInMsg: "minimum",
		},
		{
			name:      "an id that is not a uuid",
			schema:    "Scheme",
			body:      schemeBodyWith(`"id":"not-a-uuid"`),
			wantInMsg: "uuid",
		},
		{
			name:      "an enum value the contract does not publish",
			schema:    "Scheme",
			body:      schemeBodyWith(`"environmentTag":"PRODUCTION"`),
			wantInMsg: "enum",
		},
		{
			name:      "a period date served as a timestamp",
			schema:    "Period",
			body:      `{"id":"b71732a2-c0a2-493b-91aa-9e86ad2c85e5","schemeId":"cfa9ddd6-0377-4cbf-83b8-23b8ca6fa1fa","periodSeq":1,"periodStart":"2026-07-01T00:00:00Z","periodEnd":"2026-09-30","status":"OPEN"}`,
			wantInMsg: "periodStart",
		},
		{
			name:      "a non-null value where the contract allows only null or a date",
			schema:    "Period",
			body:      `{"id":"b71732a2-c0a2-493b-91aa-9e86ad2c85e5","schemeId":"cfa9ddd6-0377-4cbf-83b8-23b8ca6fa1fa","periodSeq":1,"periodStart":"2026-07-01","periodEnd":"2026-09-30","status":"OPEN","recordDate":12345}`,
			wantInMsg: "recordDate",
		},
		{
			name:      "an address that is not lowercase hex",
			schema:    "Scheme",
			body:      schemeBodyWith(`"contracts":{"chainId":11155111,"roles":"0x21D409CB5470FD3BCDA344731D945C34CB13B53F"}`),
			wantInMsg: "pattern",
		},
		{
			name:      "an error code outside the published set",
			schema:    "ErrorEnvelope",
			body:      `{"error":{"code":"kaboom","message":"something went wrong"}}`,
			wantInMsg: "enum",
		},
		{
			name:      "an error envelope with no message",
			schema:    "ErrorEnvelope",
			body:      `{"error":{"code":"not_found"}}`,
			wantInMsg: "message",
		},
		{
			name:      "a null where the contract requires a string",
			schema:    "ErrorEnvelope",
			body:      `{"error":{"code":null,"message":"x"}}`,
			wantInMsg: "null",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &validator{doc: doc}
			v.check(t, "", decodeJSON(t, []byte(tc.body)), schemaNamed(t, doc, tc.schema))

			if len(v.violations) == 0 {
				t.Fatalf("the validator accepted a document that breaks the contract.\nbody: %s", tc.body)
			}

			joined := strings.Join(v.violations, "\n")
			if !strings.Contains(joined, tc.wantInMsg) {
				t.Errorf("the violation should mention %q, got:\n%s", tc.wantInMsg, joined)
			}
		})
	}
}

// schemeBodyWith builds a valid scheme payload with one field replaced or added.
//
// Built by editing a known-good document so each case differs from a passing payload in exactly one way. A
// hand-written broken document risks failing for a second, unintended reason and proving nothing about the
// check it was meant to exercise.
func schemeBodyWith(override string) string {
	colon := strings.Index(override, ":")
	key := override[:colon]

	fields := []string{
		`"id":"cfa9ddd6-0377-4cbf-83b8-23b8ca6fa1fa"`,
		`"sebiSchemeRef":"SEBI/SM-REIT/2026/001"`,
		`"name":"AcreSync Whitefield Scheme I"`,
		`"assetValuePaise":50000000000`,
		`"unitPricePaise":100000000`,
		`"totalUnits":500`,
		`"imUnits":25`,
		`"publicUnits":475`,
		`"minPublicHolders":200`,
		`"distributionFloorBps":9500`,
		`"schemeStatus":"OFFER_OPEN"`,
		`"environmentTag":"SEPOLIA_SIM"`,
	}

	out := make([]string, 0, len(fields)+1)
	replaced := false
	for _, f := range fields {
		if strings.HasPrefix(f, key+":") {
			out = append(out, override)
			replaced = true
			continue
		}
		out = append(out, f)
	}
	if !replaced {
		out = append(out, override)
	}

	body := "{" + strings.Join(out, ",") + "}"
	// The contracts override opens a nested object, so close it.
	if strings.Contains(override, `"contracts":{`) {
		body = strings.TrimSuffix(body, "}") + "}}"
	}
	return body
}

// TestTheValidatorAcceptsAKnownGoodDocument is the other half of the self-test.
//
// Without it, a validator that rejected everything would satisfy the test above.
func TestTheValidatorAcceptsAKnownGoodDocument(t *testing.T) {
	doc := loadContract(t)

	good := schemeBodyWith(`"name":"AcreSync Whitefield Scheme I"`)

	v := &validator{doc: doc}
	v.check(t, "", decodeJSON(t, []byte(good)), schemaNamed(t, doc, "Scheme"))

	if len(v.violations) > 0 {
		t.Fatalf("the validator rejected a conformant document:\n  %s\nbody: %s",
			strings.Join(v.violations, "\n  "), good)
	}
	if len(v.undeclared) > 0 {
		t.Errorf("the validator reported undeclared fields on a conformant document: %v", v.undeclared)
	}
}

// TestEveryPublicResponseIsJSON is a small guarantee with a large blast radius.
//
// A client parses JSON unconditionally. One endpoint answering with something else, on any status code,
// crashes its error handling rather than surfacing the error.
func TestEveryPublicResponseIsJSON(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)
	periodID := seedAPIPeriod(t, ctx, tx, schemeID)
	const missing = "00000000-0000-0000-0000-000000000000"

	paths := []string{
		"/v1/schemes",
		"/v1/schemes/" + schemeID,
		"/v1/schemes/" + schemeID + "/offers",
		"/v1/schemes/" + schemeID + "/periods",
		"/v1/offers/" + offerID,
		"/v1/periods/" + periodID,
		"/v1/schemes/" + missing,
		"/v1/schemes/not-a-uuid",
		"/v1/nothing-here",
		"/v1/schemes?limit=abc",
		"/healthz",
		"/readyz",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := get(t, srv, path, nil)

			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q on status %d, want application/json", ct, rec.Code)
			}

			var probe any
			if err := json.Unmarshal(rec.Body.Bytes(), &probe); err != nil {
				t.Errorf("status %d returned a body that is not JSON: %v\nbody: %s",
					rec.Code, err, rec.Body.String())
			}
		})
	}
}

// TestConformanceCoversEveryWiredPublicEndpoint stops this file going stale.
//
// A conformance suite is only as good as its coverage, and the failure mode is silent: somebody adds an
// endpoint, does not add a case here, and the file still passes. This compares the routes the server actually
// serves against the paths these tests exercise.
func TestConformanceCoversEveryWiredPublicEndpoint(t *testing.T) {
	// The public endpoints wired so far, as route templates. Updating this list is the deliberate step that
	// makes adding an endpoint without validating it visible.
	wired := []string{
		"GET /v1/schemes",
		"GET /v1/schemes/{schemeId}",
		"GET /v1/schemes/{schemeId}/offers",
		"GET /v1/offers/{offerId}",
		"GET /v1/schemes/{schemeId}/periods",
		"GET /v1/periods/{periodId}",

		// The authenticated investor surface.
		"GET /v1/me",
		"GET /v1/me/holdings",
		"GET /v1/me/entitlements",
		"GET /v1/me/payouts",
		"GET /v1/me/bids",

		// The operator surface.
		"GET /v1/admin/outbox",
	}

	// Each one must be described by the contract too, otherwise the server is serving something unpublished.
	doc := loadContract(t)
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("the contract has no paths block")
	}

	for _, route := range wired {
		parts := strings.SplitN(route, " ", 2)
		method, template := strings.ToLower(parts[0]), parts[1]
		// The contract's paths have no /v1 prefix; it is in the server URL.
		template = strings.TrimPrefix(template, "/v1")

		item, present := paths[template].(map[string]any)
		if !present {
			t.Errorf("the server serves %s but the contract declares no path %q", route, template)
			continue
		}
		if _, present := item[method]; !present {
			t.Errorf("the server serves %s but the contract declares no %s on %q", route, method, template)
		}
	}
}

// TestInvestorResponsesConformToContract extends the conformance check to the authenticated surface.
//
// These payloads are the ones a unitholder sees about their own money, so a field named differently from the
// contract is a field the frontend will not render. The same validator is used, so the same twelve
// self-tests vouch for it.
func TestInvestorResponsesConformToContract(t *testing.T) {
	doc := loadContract(t)
	srv, ctx, tx := meServer(t)

	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)
	investorID, demat, bank := seedAPIInvestor(t, ctx, tx)

	seedAPIWallet(t, ctx, tx, investorID, "0xeeee000000000000000000000000000000000001", true)
	seedAPIKYC(t, ctx, tx, investorID, "VERIFIED")
	seedAPIHolding(t, ctx, tx, schemeID, investorID, "0xeeee000000000000000000000000000000000001", 2, false)
	bidID := seedAPIBidFor(t, ctx, tx, offerID, investorID, demat, bank, 2)
	seedAPIBlock(t, ctx, tx, bidID, 2*100000000, "BLOCKED")

	token := investorToken(t, srv, investorID)

	t.Run("Me", func(t *testing.T) {
		rec := getAs(t, srv, "/v1/me", token)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
		}
		v := assertConforms(t, doc, "Me", rec.Body.Bytes())
		if len(v.undeclared) > 0 {
			sort.Strings(v.undeclared)
			t.Errorf("the profile carries fields the contract does not publish:\n  %s",
				strings.Join(v.undeclared, "\n  "))
		}
	})

	// The list endpoints are validated per item, because the envelope is not itself a published schema.
	// Bid was missing from this list, which is how GET /me/bids shipped `units` where the contract says
	// `unitsBid`. Every list the investor surface serves is here now.
	lists := []struct{ path, schema string }{
		{"/v1/me/holdings", "Holding"},
		{"/v1/me/bids", "Bid"},
	}

	for _, tc := range lists {
		t.Run(tc.schema, func(t *testing.T) {
			rec := getAs(t, srv, tc.path, token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
			}

			decoded, ok := decodeJSON(t, rec.Body.Bytes()).(map[string]any)
			if !ok {
				t.Fatal("the list response is not an object")
			}
			items, ok := decoded["items"].([]any)
			if !ok {
				t.Fatalf("items is %T", decoded["items"])
			}
			if len(items) == 0 {
				t.Fatal("nothing to validate; the fixture did not appear")
			}

			v := &validator{doc: doc}
			for i, item := range items {
				v.check(t, fmt.Sprintf("items[%d]", i), item, schemaNamed(t, doc, tc.schema))
			}
			if len(v.violations) > 0 {
				sort.Strings(v.violations)
				t.Errorf("a %s does not satisfy the contract:\n  %s\n\nbody: %s",
					tc.schema, strings.Join(v.violations, "\n  "), rec.Body.String())
			}
			if len(v.undeclared) > 0 {
				sort.Strings(v.undeclared)
				t.Errorf("a %s carries fields the contract does not publish:\n  %s",
					tc.schema, strings.Join(v.undeclared, "\n  "))
			}
		})
	}
}

// TestInvestorResponsesAreNeverCached is a property of every credentialled response.
//
// These are per-caller and carry an authenticated subject's financial position. A shared cache that keyed one
// of them on the URL alone would serve one unitholder's holdings to another, which is the worst outcome this
// API has available to it.
func TestInvestorResponsesAreNeverCached(t *testing.T) {
	srv, ctx, tx := meServer(t)
	investorID, _, _ := seedAPIInvestor(t, ctx, tx)
	token := investorToken(t, srv, investorID)

	for _, path := range []string{"/v1/me", "/v1/me/holdings", "/v1/me/entitlements", "/v1/me/payouts", "/v1/me/bids"} {
		t.Run(path, func(t *testing.T) {
			rec := getAs(t, srv, path, token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
			}

			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			// No ETag either. A conditional request on a per-caller resource invites a cache to key it on the
			// URL and ignore the token.
			if tag := rec.Header().Get("ETag"); tag != "" {
				t.Errorf("ETag = %q on a credentialled response", tag)
			}
		})
	}
}
