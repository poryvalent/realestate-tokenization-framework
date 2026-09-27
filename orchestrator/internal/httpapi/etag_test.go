package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

type sample struct {
	Status      string `json:"status"`
	UnitsIssued int64  `json:"unitsIssued"`
}

// serveETag runs writeJSONWithETag once and returns the recorder.
func serveETag(t *testing.T, v any, ifNoneMatch string, method string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, "/v1/offers/abc", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	req = req.WithContext(req.Context())

	rec := httptest.NewRecorder()
	writeJSONWithETag(rec, req, http.StatusOK, v)
	return rec
}

// TestETagIsStableForAnUnchangedRepresentation is the property a poller depends on.
//
// The contract tells the frontend to poll every three seconds with If-None-Match. That only saves anything
// if an unchanged resource produces a byte-identical tag every time.
func TestETagIsStableForAnUnchangedRepresentation(t *testing.T) {
	v := sample{Status: "BALLOT_DRAWN", UnitsIssued: 475}

	first := serveETag(t, v, "", http.MethodGet).Header().Get("ETag")
	second := serveETag(t, v, "", http.MethodGet).Header().Get("ETag")

	if first == "" {
		t.Fatal("no ETag was set")
	}
	if first != second {
		t.Fatalf("the same value produced two tags, %q then %q, so every poll would refetch", first, second)
	}
}

// TestETagChangesWhenTheRepresentationChanges is the other half.
//
// A tag that does not move on a real change is worse than no tag: the poller would never see the update.
func TestETagChangesWhenTheRepresentationChanges(t *testing.T) {
	before := serveETag(t, sample{Status: "SEED_REVEALED", UnitsIssued: 0}, "", http.MethodGet).Header().Get("ETag")
	after := serveETag(t, sample{Status: "BALLOT_DRAWN", UnitsIssued: 0}, "", http.MethodGet).Header().Get("ETag")

	if before == after {
		t.Fatalf("a status change did not move the ETag, both were %q", before)
	}
}

// TestMatchingIfNoneMatchGives304WithNoBody covers the saving itself.
func TestMatchingIfNoneMatchGives304WithNoBody(t *testing.T) {
	v := sample{Status: "SETTLED", UnitsIssued: 500}

	tag := serveETag(t, v, "", http.MethodGet).Header().Get("ETag")
	rec := serveETag(t, v, tag, http.MethodGet)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("a 304 must carry no body, got %d bytes: %s", rec.Body.Len(), rec.Body.String())
	}
	if rec.Header().Get("ETag") != tag {
		t.Fatal("a 304 should repeat the ETag so a client that lost it can resynchronise")
	}
}

// TestStaleIfNoneMatchGivesTheBody is the case that must not be a 304.
func TestStaleIfNoneMatchGivesTheBody(t *testing.T) {
	stale := serveETag(t, sample{Status: "OPEN"}, "", http.MethodGet).Header().Get("ETag")

	rec := serveETag(t, sample{Status: "CLOSED"}, stale, http.MethodGet)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 because the representation changed", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("the changed representation was not sent")
	}
}

// TestContentLengthMatchesTheBody catches a truncated write.
func TestContentLengthMatchesTheBody(t *testing.T) {
	rec := serveETag(t, sample{Status: "OPEN", UnitsIssued: 12}, "", http.MethodGet)

	declared := rec.Header().Get("Content-Length")
	if declared == "" {
		t.Fatal("Content-Length was not set")
	}
	n, err := strconv.Atoi(declared)
	if err != nil {
		t.Fatalf("Content-Length %q is not a number", declared)
	}
	if n != rec.Body.Len() {
		t.Fatalf("Content-Length says %d, the body is %d bytes", n, rec.Body.Len())
	}
}

// TestHeadCarriesHeadersWithoutABody keeps HEAD usable for a cheap tag check.
func TestHeadCarriesHeadersWithoutABody(t *testing.T) {
	rec := serveETag(t, sample{Status: "OPEN"}, "", http.MethodHead)

	if rec.Header().Get("ETag") == "" {
		t.Fatal("a HEAD must still carry the ETag; that is the point of using it")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("a HEAD must carry no body, got %d bytes", rec.Body.Len())
	}
}

// TestETagMatches covers the header's legal forms.
func TestETagMatches(t *testing.T) {
	const tag = `"abc123"`

	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"exact", `"abc123"`, true},
		{"wildcard", "*", true},
		{"first in a list", `"abc123", "def456"`, true},
		{"last in a list", `"def456", "abc123"`, true},
		{"middle of a list", `"111", "abc123", "222"`, true},
		{"list without spaces", `"def456","abc123"`, true},
		{"tab separated", "\"def456\",\t\"abc123\"", true},
		{"absent from the list", `"def456", "999"`, false},
		{"empty", "", false},
		{"a prefix is not a match", `"abc"`, false},
		{"a superstring is not a match", `"abc1234"`, false},
		// A weak tag must not satisfy a strong comparison. Every tag here is strong, and accepting a weak
		// match would risk a 304 for a representation that genuinely changed.
		{"weak form of the same tag", `W/"abc123"`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := etagMatches(tc.header, tag); got != tc.want {
				t.Fatalf("etagMatches(%q, %q) = %v, want %v", tc.header, tag, got, tc.want)
			}
		})
	}
}

// TestETagIsQuoted keeps the header syntactically valid.
//
// An unquoted entity-tag is malformed, and a cache or proxy is entitled to ignore it, which would silently
// remove the conditional-request saving without any visible failure.
func TestETagIsQuoted(t *testing.T) {
	tag := etagFor([]byte(`{"status":"OPEN"}`))

	if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		t.Fatalf("the ETag must be quoted, got %s", tag)
	}
}

// TestETagDiffersForDifferentBodies is the hash doing its job.
func TestETagDiffersForDifferentBodies(t *testing.T) {
	if etagFor([]byte("a")) == etagFor([]byte("b")) {
		t.Fatal("two different bodies produced the same tag")
	}
}
