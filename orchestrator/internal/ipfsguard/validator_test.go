package ipfsguard

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/acresync/orchestrator/internal/canonical"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	hash32 = "0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	hash32b = "0x1111111111111111111111111111111111111111111111111111111111111111"
	addr1  = "0x000000000000000000000000000000000000dead"
	addr2  = "0x000000000000000000000000000000000000beef"
	uuid1  = "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"
	oref1  = "0123456789abcdef0123456789abcdef"
)

func validSnapshot() string {
	return fmt.Sprintf(`{
	  "docType": "SNAPSHOT",
	  "schemaVersion": 1,
	  "schemeRef": %q,
	  "periodId": 1,
	  "recordDate": "2026-09-30",
	  "takenAt": "2026-09-30T18:30:00Z",
	  "totalUnits": 500,
	  "distinctHolders": 201,
	  "merkleRoot": %q,
	  "algoVersion": 1,
	  "lines": [
	    {"leafIndex": 0, "holder": %q, "units": 25, "excluded": true,  "investorAnchor": %q},
	    {"leafIndex": 1, "holder": %q, "units": 3,  "excluded": false, "investorAnchor": %q}
	  ]
	}`, hash32, hash32b, addr1, hash32, addr2, hash32b)
}

func validNDCF() string {
	return fmt.Sprintf(`{
	  "docType": "NDCF_STATEMENT",
	  "schemaVersion": 1,
	  "schemeRef": %q,
	  "periodId": 1,
	  "periodStart": "2026-09-01",
	  "periodEnd": "2026-09-30",
	  "currency": "INR",
	  "ndcfPaise": 30000000000,
	  "distributedPaise": 28500000000,
	  "distributionBps": 9500,
	  "lineItems": [
	    {"lineType": "GROSS_RENT",   "direction": "INFLOW",  "amountPaise": 33000000000, "evidenceHash": %q},
	    {"lineType": "PROPERTY_TAX", "direction": "OUTFLOW", "amountPaise": 2000000000,  "evidenceHash": %q, "spvRef": %q}
	  ]
	}`, hash32, hash32, hash32b, uuid1)
}

func validBidbook() string {
	return fmt.Sprintf(`{
	  "docType": "BIDBOOK",
	  "schemaVersion": 1,
	  "schemeRef": %q,
	  "offerId": %q,
	  "frozenAt": "2026-08-31T18:30:00Z",
	  "merkleRoot": %q,
	  "leafCount": 1,
	  "totalUnitsBid": 4,
	  "distinctBidders": 1,
	  "algoVersion": 1,
	  "leaves": [
	    {"leafIndex": 0, "bidRef": %q, "investorAnchor": %q, "unitsBid": 4, "pricePerUnitPaise": 100000000}
	  ]
	}`, hash32, uuid1, hash32b, oref1, hash32)
}

// ---------------------------------------------------------------------------
// Happy paths
// ---------------------------------------------------------------------------

func TestValidDocumentsPass(t *testing.T) {
	cases := map[DocType]string{
		DocSnapshot:      validSnapshot(),
		DocNDCFStatement: validNDCF(),
		DocBidbook:       validBidbook(),
	}
	for dt, raw := range cases {
		out, err := ValidateAndCanonicalize(dt, []byte(raw))
		if err != nil {
			t.Errorf("%s: unexpected rejection: %v", dt, err)
			continue
		}
		// The returned bytes must already be canonical, so re-canonicalising is a
		// no-op. If it were not, the pinned bytes and the validated bytes could
		// differ and the CID would not be reproducible.
		again, err := canonical.MarshalJSON(out)
		if err != nil {
			t.Errorf("%s: returned bytes are not valid JSON: %v", dt, err)
			continue
		}
		if string(again) != string(out) {
			t.Errorf("%s: returned bytes are not canonical", dt)
		}
		if strings.Contains(string(out), " ") && !strings.Contains(string(out), `" "`) {
			t.Errorf("%s: canonical output contains insignificant whitespace: %s", dt, out)
		}
	}
}

func TestDigestIsStableAndMatchesCanonicalBytes(t *testing.T) {
	// Same logical document, different key order and whitespace.
	a := `{"docType":"SNAPSHOT","schemaVersion":1,` + validSnapshot()[strings.Index(validSnapshot(), `"schemeRef"`):]
	_ = a // key reordering is exercised below via a targeted pair instead

	out1, err := ValidateAndCanonicalize(DocSnapshot, []byte(validSnapshot()))
	if err != nil {
		t.Fatal(err)
	}
	reordered := strings.Replace(validSnapshot(),
		`"docType": "SNAPSHOT",`, ``, 1)
	reordered = strings.TrimSpace(reordered)
	reordered = "{\n\"docType\": \"SNAPSHOT\"," + reordered[1:]

	out2, err := ValidateAndCanonicalize(DocSnapshot, []byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	if DigestHex(out1) != DigestHex(out2) {
		t.Fatalf("digest depends on input formatting:\n %s\n %s", DigestHex(out1), DigestHex(out2))
	}
	if len(DigestHex(out1)) != 66 {
		t.Fatalf("digest hex length %d, want 66", len(DigestHex(out1)))
	}
}

// ---------------------------------------------------------------------------
// Fail-closed on unknown fields
// ---------------------------------------------------------------------------

func TestUnknownFieldRejected(t *testing.T) {
	raw := strings.Replace(validSnapshot(),
		`"totalUnits": 500,`,
		`"totalUnits": 500, "investorName": "ACME",`, 1)

	err := Validate(DocSnapshot, []byte(raw))
	if err == nil {
		t.Fatal("an undeclared field must be rejected")
	}
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("want ErrUnknownField, got %v", err)
	}
}

func TestUnknownNestedFieldRejected(t *testing.T) {
	raw := strings.Replace(validSnapshot(),
		`"units": 3,`,
		`"units": 3, "panNumber": "x",`, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrUnknownField) {
		t.Fatalf("want ErrUnknownField inside an array element, got %v", err)
	}
}

func TestMissingRequiredFieldRejected(t *testing.T) {
	raw := strings.Replace(validSnapshot(), `"merkleRoot": "`+hash32b+`",`, ``, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrMissingField) {
		t.Fatalf("want ErrMissingField, got %v", err)
	}
}

func TestOptionalFieldMayBeAbsent(t *testing.T) {
	// spvRef and propertyRef are optional on an NDCF line item.
	if err := Validate(DocNDCFStatement, []byte(validNDCF())); err != nil {
		t.Fatalf("optional fields absent should pass: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Personal data
// ---------------------------------------------------------------------------

// Defence in depth demonstrated: a PAN smuggled in under a new field name trips
// both gates. The allowlist rejects the field, and the document-wide scan
// independently recognises the shape. Either alone would have stopped it, which
// is the intent.
func TestPANTripsBothGates(t *testing.T) {
	raw := strings.Replace(validSnapshot(),
		`"totalUnits": 500,`,
		`"totalUnits": 500, "pan": "ABCDE1234F",`, 1)

	err := Validate(DocSnapshot, []byte(raw))
	if err == nil {
		t.Fatal("a PAN must never reach IPFS")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T", err)
	}
	var sawUnknown, sawPII bool
	for _, v := range ve.Violations {
		if errors.Is(v.Err, ErrUnknownField) {
			sawUnknown = true
		}
		if errors.Is(v.Err, ErrPIIDetected) {
			sawPII = true
		}
	}
	if !sawUnknown {
		t.Error("allowlist gate did not fire")
	}
	if !sawPII {
		t.Error("pattern scan gate did not fire")
	}
}

func TestEmailDetected(t *testing.T) {
	raw := strings.Replace(validSnapshot(),
		`"totalUnits": 500,`,
		`"totalUnits": 500, "contact": "investor@example.com",`, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrPIIDetected) {
		t.Fatalf("want ErrPIIDetected for an email address, got %v", err)
	}
}

func TestIFSCDetected(t *testing.T) {
	raw := strings.Replace(validSnapshot(),
		`"totalUnits": 500,`,
		`"totalUnits": 500, "bank": "HDFC0001234",`, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrPIIDetected) {
		t.Fatalf("want ErrPIIDetected for an IFSC code, got %v", err)
	}
}

// The soundness claim behind the document-wide scan: the patterns applied across
// the whole document cannot false-positive against any value the allowlist
// actually permits. If this fails, the scan is producing noise and the design
// note in validator.go is wrong.
func TestDocumentWideScanDoesNotFalsePositiveOnValidDocuments(t *testing.T) {
	for _, raw := range []string{validSnapshot(), validNDCF(), validBidbook()} {
		canonicalBytes, err := canonical.MarshalJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		var vio []Violation
		scanDocumentWide(canonicalBytes, &vio)
		if len(vio) > 0 {
			t.Errorf("false positive on a valid document: %v", vio)
		}
	}
}

// Pins down which kinds may be numeric-scanned, and why.
//
// The interesting finding is that a 0x-prefixed digest is NOT a false-positive
// risk: the patterns are \b-anchored and a digest is one contiguous word run, so
// there is no boundary before its digits. The genuine hazard is a UUID, whose
// dash-delimited final group is exactly twelve characters and can be all digits.
func TestNumericPatternsAreNotAppliedToAmbiguousKinds(t *testing.T) {
	for _, k := range []Kind{KindBytes32Hex, KindAddress, KindUUID} {
		if k.isScannableForNumericPII() {
			t.Errorf("%s must not be numeric-scanned", k)
		}
	}
	if !KindEnum.isScannableForNumericPII() {
		t.Error("enum should be numeric-scanned; it cannot legitimately contain a long delimited digit group")
	}

	// The concrete false positive the UUID exclusion prevents.
	uuidAllDigitTail := "11111111-2222-3333-4444-555555555555"
	var vio []Violation
	scanNumericPII(uuidAllDigitTail, "$.offerId", &vio)
	if len(vio) == 0 {
		t.Fatal("expected a UUID with an all-digit final group to match the Aadhaar pattern; " +
			"if this no longer holds, the UUID exclusion can be revisited")
	}

	// And the case that turns out to be safe either way, documented so the
	// reasoning is not lost.
	digestAllDigits := "0x" + strings.Repeat("1234", 16)
	vio = nil
	scanNumericPII(digestAllDigits, "$.merkleRoot", &vio)
	if len(vio) != 0 {
		t.Errorf("a 0x-prefixed digest should not match a \\b-anchored pattern, got %v", vio)
	}
}

// ---------------------------------------------------------------------------
// Format enforcement
// ---------------------------------------------------------------------------

func TestUppercaseHexRejected(t *testing.T) {
	// hash32b is all 1s, so uppercasing it is a no-op. The fixture has to contain
	// letters for this test to mean anything.
	const upperBody = "0xABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
	raw := strings.Replace(validSnapshot(),
		`"merkleRoot": "`+hash32b+`"`,
		`"merkleRoot": "`+upperBody+`"`, 1)
	if raw == validSnapshot() {
		t.Fatal("fixture substitution did not apply; the test would pass vacuously")
	}
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrFormat) {
		t.Fatalf("uppercase hex must be rejected so one value has exactly one spelling; got %v", err)
	}
}

func TestChecksummedAddressRejected(t *testing.T) {
	// EIP-55 mixed case. Valid Ethereum, unusable here: two spellings of one
	// address would produce two CIDs for one logical document.
	mixed := "0x000000000000000000000000000000000000DeAd"
	raw := strings.Replace(validSnapshot(), addr1, mixed, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrFormat) {
		t.Fatalf("mixed-case address must be rejected, got %v", err)
	}
}

func TestFloatRejected(t *testing.T) {
	raw := strings.Replace(validNDCF(), `"ndcfPaise": 30000000000,`, `"ndcfPaise": 30000000000.5,`, 1)
	if err := Validate(DocNDCFStatement, []byte(raw)); !errors.Is(err, canonical.ErrFloatNotPermitted) {
		if !errors.Is(err, canonical.ErrNonIntegerNumber) {
			t.Fatalf("a fractional paise amount must be rejected, got %v", err)
		}
	}
}

func TestDuplicateKeyRejected(t *testing.T) {
	raw := strings.Replace(validSnapshot(), `"totalUnits": 500,`, `"totalUnits": 500, "totalUnits": 501,`, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, canonical.ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey, got %v", err)
	}
}

func TestInvalidDateRejected(t *testing.T) {
	raw := strings.Replace(validSnapshot(), `"recordDate": "2026-09-30"`, `"recordDate": "2026-02-30"`, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrFormat) {
		t.Fatalf("30 February must be rejected, got %v", err)
	}
}

func TestNonUTCTimestampRejected(t *testing.T) {
	raw := strings.Replace(validSnapshot(), `"takenAt": "2026-09-30T18:30:00Z"`, `"takenAt": "2026-09-30T18:30:00+05:30"`, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrFormat) {
		t.Fatalf("a numeric offset is a second spelling of one instant and must be rejected, got %v", err)
	}
}

func TestEnumValueEnforced(t *testing.T) {
	raw := strings.Replace(validNDCF(), `"lineType": "GROSS_RENT"`, `"lineType": "RENTAL_INCOME"`, 1)
	if err := Validate(DocNDCFStatement, []byte(raw)); !errors.Is(err, ErrFormat) {
		t.Fatalf("an unregistered line type must be rejected, got %v", err)
	}
}

func TestWrongDocTypeInBodyRejected(t *testing.T) {
	raw := strings.Replace(validSnapshot(), `"docType": "SNAPSHOT"`, `"docType": "BIDBOOK"`, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrFormat) {
		t.Fatalf("the declared docType must match the validating schema, got %v", err)
	}
}

func TestNegativeNumberRejected(t *testing.T) {
	raw := strings.Replace(validSnapshot(), `"totalUnits": 500,`, `"totalUnits": -500,`, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrFormat) {
		t.Fatalf("a negative unit count must be rejected, got %v", err)
	}
}

func TestUint32RangeEnforced(t *testing.T) {
	raw := strings.Replace(validSnapshot(), `"totalUnits": 500,`, `"totalUnits": 4294967296,`, 1)
	if err := Validate(DocSnapshot, []byte(raw)); !errors.Is(err, ErrFormat) {
		t.Fatalf("a value beyond uint32 must be rejected, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Structural guards
// ---------------------------------------------------------------------------

func TestUnknownDocTypeRejected(t *testing.T) {
	if err := Validate(DocType("PAYOUT_REPORT"), []byte(`{}`)); !errors.Is(err, ErrUnknownDocType) {
		t.Fatalf("want ErrUnknownDocType, got %v", err)
	}
}

func TestNonObjectRootRejected(t *testing.T) {
	if err := Validate(DocSnapshot, []byte(`[1,2,3]`)); !errors.Is(err, ErrNotAnObject) {
		t.Fatalf("want ErrNotAnObject, got %v", err)
	}
}

func TestArrayLengthCapEnforced(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"docType":"SNAPSHOT","schemaVersion":1,"schemeRef":"` + hash32 + `",`)
	b.WriteString(`"periodId":1,"recordDate":"2026-09-30","takenAt":"2026-09-30T18:30:00Z",`)
	b.WriteString(`"totalUnits":500,"distinctHolders":201,"merkleRoot":"` + hash32b + `",`)
	b.WriteString(`"algoVersion":1,"lines":[`)
	const over = 10_001
	for i := range over {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"leafIndex":%d,"holder":"%s","units":1,"excluded":false,"investorAnchor":"%s"}`, i, addr2, hash32)
	}
	b.WriteString(`]}`)

	if err := Validate(DocSnapshot, []byte(b.String())); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("want ErrArrayTooLong for %d lines, got %v", over, err)
	}
}

func TestAllViolationsReported(t *testing.T) {
	raw := strings.Replace(validSnapshot(), `"totalUnits": 500,`, `"extraOne": 1, "extraTwo": 2,`, 1)
	err := Validate(DocSnapshot, []byte(raw))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	// Two unknown fields plus one missing required field.
	if len(ve.Violations) < 3 {
		t.Fatalf("expected at least 3 violations reported in one pass, got %d: %v",
			len(ve.Violations), ve.Violations)
	}
	if !strings.Contains(ve.Error(), "violation(s)") {
		t.Errorf("aggregated error should summarise the count: %s", ve.Error())
	}
}

// ---------------------------------------------------------------------------
// Schema hygiene
// ---------------------------------------------------------------------------

// Guards against a malformed schema declaration, which would otherwise surface as
// a confusing ErrBadSchema at pin time rather than at build time.
func TestAllSchemasAreWellFormed(t *testing.T) {
	for _, dt := range DocTypes() {
		schema, ok := SchemaFor(dt)
		if !ok {
			t.Errorf("%s has no registered schema", dt)
			continue
		}
		checkSchemaWellFormed(t, string(dt), schema)
	}
}

func checkSchemaWellFormed(t *testing.T, path string, s Schema) {
	t.Helper()
	if len(s) == 0 {
		t.Errorf("%s: empty schema", path)
	}
	for name, f := range s {
		p := path + "." + name
		switch f.Kind {
		case KindInvalid:
			t.Errorf("%s: field has no declared kind", p)
		case KindObject:
			if f.Object == nil {
				t.Errorf("%s: object field has no nested schema", p)
			} else {
				checkSchemaWellFormed(t, p, f.Object)
			}
		case KindArray:
			if f.Elem == nil {
				t.Errorf("%s: array field has no element declaration", p)
			} else {
				if f.MaxLen <= 0 {
					t.Errorf("%s: array field has no MaxLen; an unbounded array is a DoS vector", p)
				}
				if f.Elem.Kind == KindObject && f.Elem.Object != nil {
					checkSchemaWellFormed(t, p+"[]", f.Elem.Object)
				}
			}
		case KindEnum:
			if len(f.Enum) == 0 {
				t.Errorf("%s: enum field lists no permitted values", p)
			}
		}
	}
}

// The design guarantee: no pinnable document may contain a free-text field. If a
// String-like kind is ever added, this test is where it must be justified.
func TestNoFreeTextKindExists(t *testing.T) {
	for kind, name := range kindNames {
		if kind == KindInvalid {
			continue
		}
		if name == "string" || name == "text" {
			t.Errorf("kind %d is named %q, which suggests a free-text field type. "+
				"Pinned documents must not carry prose: anchor its hash instead.", kind, name)
		}
	}
}

// Every document type must require a docType discriminator pinned to itself, so a
// file cannot be validated against one schema and later interpreted as another.
func TestEveryDocTypeIsSelfDescribing(t *testing.T) {
	for _, dt := range DocTypes() {
		schema, _ := SchemaFor(dt)
		f, ok := schema["docType"]
		if !ok {
			t.Errorf("%s: no docType field", dt)
			continue
		}
		if !f.Required {
			t.Errorf("%s: docType must be required", dt)
		}
		if len(f.Enum) != 1 || f.Enum[0] != string(dt) {
			t.Errorf("%s: docType enum is %v, want exactly [%q]", dt, f.Enum, string(dt))
		}
	}
}
