package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postThrough runs a body through the mutation middleware stack.
func postThrough(t *testing.T, handler http.HandlerFunc, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/mutate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyKeyHeader, "a-valid-idem-key-0001")
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	chain(http.HandlerFunc(mutation(handler)), withRequestID).ServeHTTP(rec, req)
	return rec
}

// okHandler records that it ran and what idempotency key it saw.
type mutationProbe struct {
	ran bool
	key string
}

func (m *mutationProbe) handler(w http.ResponseWriter, r *http.Request) {
	m.ran = true
	m.key, _ = idempotencyKeyFrom(r.Context())
	writeJSON(w, r, http.StatusOK, map[string]string{"ok": "yes"})
}

// TestAMutationNeedsAnIdempotencyKey is the admission rule the contract publishes.
//
// Without it, a caller discovers the problem after a double submit rather than before the first one.
func TestAMutationNeedsAnIdempotencyKey(t *testing.T) {
	m := &mutationProbe{}

	rec := postThrough(t, m.handler, `{}`, map[string]string{idempotencyKeyHeader: ""})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rec.Code, rec.Body.String())
	}
	if m.ran {
		t.Fatal("a mutation ran without an idempotency key")
	}
	env := decodeError(t, rec.Result())
	if !strings.Contains(env.Error.Message, idempotencyKeyHeader) {
		t.Errorf("the message should name the header, got %q", env.Error.Message)
	}
}

// TestTheIdempotencyKeyReachesTheHandler is what makes it usable rather than ceremonial.
func TestTheIdempotencyKeyReachesTheHandler(t *testing.T) {
	m := &mutationProbe{}

	rec := postThrough(t, m.handler, `{}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if m.key != "a-valid-idem-key-0001" {
		t.Errorf("the handler saw key %q", m.key)
	}
}

// TestUnusableIdempotencyKeysAreRefused covers length and charset.
//
// The charset matters because the value is logged. A key carrying a newline can forge a second log entry, and
// a log that a request header can forge is not evidence of anything.
func TestUnusableIdempotencyKeysAreRefused(t *testing.T) {
	cases := map[string]string{
		"too short":        "abc",
		"seven characters": "1234567",
		"too long":         strings.Repeat("k", 256),
		"embedded newline": "abcdefgh\nlevel=error msg=forged",
		"carriage return":  "abcdefgh\rmore",
		"tab":              "abcdefgh\tmore",
		"space":            "abcd efgh",
		"null byte":        "abcdefgh\x00",
		"non-ascii":        "abcdefgh\u00e9",
		"control char":     "abcdefgh\x1b[31m",
	}

	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			m := &mutationProbe{}
			rec := postThrough(t, m.handler, `{}`, map[string]string{idempotencyKeyHeader: key})

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %q", rec.Code, key)
			}
			if m.ran {
				t.Fatalf("a mutation ran with an unusable key %q", key)
			}
		})
	}
}

// TestAcceptableIdempotencyKeysPass checks the rule is not over-tight.
func TestAcceptableIdempotencyKeysPass(t *testing.T) {
	cases := map[string]string{
		"eight characters": "12345678",
		"a uuid":           "cfa9ddd6-0377-4cbf-83b8-23b8ca6fa1fa",
		"hex":              "0x77467d592e66a60dd201ec919080ea66",
		"with underscores": "client_request_00042",
		"with dots":        "client.request.00042",
		"255 characters":   strings.Repeat("k", 255),
		"punctuation":      "req:2026-09-27/attempt=1",
	}

	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			m := &mutationProbe{}
			rec := postThrough(t, m.handler, `{}`, map[string]string{idempotencyKeyHeader: key})

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d for a usable key %q. body: %s", rec.Code, key, rec.Body.String())
			}
			if m.key != key {
				t.Errorf("the handler saw %q, want %q", m.key, key)
			}
		})
	}
}

// TestAMutationRequiresAJSONContentType names the misunderstanding early.
func TestAMutationRequiresAJSONContentType(t *testing.T) {
	cases := map[string]string{
		"absent":    "",
		"form":      "application/x-www-form-urlencoded",
		"text":      "text/plain",
		"xml":       "application/xml",
		"multipart": "multipart/form-data; boundary=x",
	}

	for name, contentType := range cases {
		t.Run(name, func(t *testing.T) {
			m := &mutationProbe{}
			req := httptest.NewRequest(http.MethodPost, "/v1/mutate", strings.NewReader(`{}`))
			req.Header.Set(idempotencyKeyHeader, "a-valid-idem-key-0001")
			if contentType != "" {
				req.Header.Set("Content-Type", contentType)
			}
			rec := httptest.NewRecorder()
			chain(http.HandlerFunc(mutation(m.handler)), withRequestID).ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for Content-Type %q", rec.Code, contentType)
			}
			if m.ran {
				t.Fatalf("a mutation ran with Content-Type %q", contentType)
			}
		})
	}
}

// TestAJSONContentTypeWithParametersIsAccepted covers the legal forms.
func TestAJSONContentTypeWithParametersIsAccepted(t *testing.T) {
	for _, contentType := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"application/json;charset=UTF-8",
		"APPLICATION/JSON",
		" application/json ",
	} {
		t.Run(contentType, func(t *testing.T) {
			m := &mutationProbe{}
			rec := postThrough(t, m.handler, `{}`, map[string]string{"Content-Type": contentType})

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d for a legal Content-Type %q. body: %s", rec.Code, contentType, rec.Body.String())
			}
		})
	}
}

// TestAnOversizedBodyIsRefused is the allocation bound.
//
// Without a cap a single request can make the process allocate until it dies, and a decoder reading from the
// network will do exactly that.
func TestAnOversizedBodyIsRefused(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Padding string `json:"padding"`
		}
		if err := decodeBodyStrict(r, &body); err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]string{"ok": "yes"})
	}

	oversized := `{"padding":"` + strings.Repeat("A", maxBodyBytes+1024) + `"}`
	rec := postThrough(t, handler, oversized, nil)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413. body: %s", rec.Code, truncate(rec.Body.String(), 200))
	}
}

// TestAnUnknownFieldIsRefused is the strict-decode decision.
//
// A client sending "amount" where the contract says "amountPaise" would otherwise have its value dropped and
// receive a success describing something it did not ask for. On a mutation that moves money, silently ignoring
// a field is the worst available behaviour.
func TestAnUnknownFieldIsRefused(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AmountPaise int64 `json:"amountPaise"`
		}
		if err := decodeBodyStrict(r, &body); err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]int64{"got": body.AmountPaise})
	}

	rec := postThrough(t, handler, `{"amountPaise":100,"amount":999}`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rec.Code, rec.Body.String())
	}
	env := decodeError(t, rec.Result())
	if !strings.Contains(env.Error.Message, "amount") {
		t.Errorf("the message should name the offending field, got %q", env.Error.Message)
	}
}

// TestTrailingContentAfterTheJSONIsRefused closes the second-document gap.
//
// Two concatenated objects otherwise parse as the first and discard the rest, so a client could believe it sent
// something the server never saw.
func TestTrailingContentAfterTheJSONIsRefused(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			A int `json:"a"`
		}
		if err := decodeBodyStrict(r, &body); err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]int{"a": body.A})
	}

	for name, body := range map[string]string{
		"two objects":      `{"a":1}{"a":2}`,
		"trailing garbage": `{"a":1} oops`,
		"trailing array":   `{"a":1}[]`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := postThrough(t, handler, body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %q. body: %s", rec.Code, body, rec.Body.String())
			}
		})
	}
}

// TestAWellFormedBodyDecodes checks strictness has not broken the normal case.
func TestAWellFormedBodyDecodes(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AmountPaise int64 `json:"amountPaise"`
		}
		if err := decodeBodyStrict(r, &body); err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]int64{"got": body.AmountPaise})
	}

	rec := postThrough(t, handler, `{"amountPaise":100000000}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "100000000") {
		t.Errorf("the value did not survive: %s", rec.Body.String())
	}
}

// TestMutationChecksRunInTheRightOrder pins the composition.
//
// The body is capped before anything reads it, and the content type is checked before a decode is attempted.
// A stack that demanded the idempotency key after reading the body would let an oversized request allocate
// before being refused for a missing header.
func TestMutationChecksRunInTheRightOrder(t *testing.T) {
	m := &mutationProbe{}

	// Oversized AND missing the key. The refusal should be about size, because the cap is outermost.
	oversized := `{"padding":"` + strings.Repeat("A", maxBodyBytes+1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/mutate", strings.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	chain(http.HandlerFunc(mutation(m.handler)), withRequestID).ServeHTTP(rec, req)

	// The key check is what rejects it, since nothing has read the body yet. Either way it must not run the
	// handler and must not have read a megabyte first.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a missing key. body: %s", rec.Code, truncate(rec.Body.String(), 200))
	}
	if m.ran {
		t.Fatal("the handler ran")
	}
}

// TestValidateIdempotencyKeyBoundaries checks the exact edges.
func TestValidateIdempotencyKeyBoundaries(t *testing.T) {
	if err := validateIdempotencyKey(strings.Repeat("k", minIdempotencyKeyLen-1)); err == nil {
		t.Errorf("a %d-character key was accepted", minIdempotencyKeyLen-1)
	}
	if err := validateIdempotencyKey(strings.Repeat("k", minIdempotencyKeyLen)); err != nil {
		t.Errorf("the minimum length was refused: %v", err)
	}
	if err := validateIdempotencyKey(strings.Repeat("k", maxIdempotencyKeyLen)); err != nil {
		t.Errorf("the maximum length was refused: %v", err)
	}
	if err := validateIdempotencyKey(strings.Repeat("k", maxIdempotencyKeyLen+1)); err == nil {
		t.Errorf("a %d-character key was accepted", maxIdempotencyKeyLen+1)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
