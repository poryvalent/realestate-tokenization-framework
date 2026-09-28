package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Presigned uploads: the intent, the signed URL, and the receipt that hashes what actually arrived.

func presignBody(hash *string) map[string]any {
	b := map[string]any{"filename": "pan-card.pdf", "mimeType": "application/pdf", "purpose": "KYC"}
	if hash != nil {
		b["publicHash"] = *hash
	}
	return b
}

// put sends bytes to an upload URL the way a browser would: no bearer token, the file as the body.
func (h *writeHarness) put(t *testing.T, uploadURL, contentType string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(uploadURL)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, u.RequestURI(), strings.NewReader(string(content)))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	chain(h.srv, withRequestID).ServeHTTP(rec, req)
	return rec
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "0x" + hex.EncodeToString(sum[:])
}

// TestPresignThenUpload is the happy path: the bytes arrive, are hashed, and match what was declared.
func TestPresignThenUpload(t *testing.T) {
	h := newWriteHarness(t)
	investorID, _, _ := seedAPIInvestor(t, h.ctx, h.tx)
	content := []byte("%PDF-1.7 a scan of a PAN card")
	declared := hashOf(content)

	got := expect(t, h.send(t, http.MethodPost, "/v1/documents/presign", h.investor(t, investorID), idem("presign"),
		presignBody(&declared)), http.StatusCreated)

	docID, _ := got["documentId"].(string)
	uploadURL, _ := got["uploadUrl"].(string)
	if !strings.HasPrefix(uploadURL, "http://acresync.test/v1/documents/"+docID+"/content?") {
		t.Fatalf("uploadUrl = %q", uploadURL)
	}
	if got["publicHash"] != declared {
		t.Errorf("publicHash = %v, want the declared %s echoed", got["publicHash"], declared)
	}
	expires, err := time.Parse(time.RFC3339, got["expiresAt"].(string))
	if err != nil || !expires.After(tokenNow) {
		t.Fatalf("expiresAt = %v (%v), want a time after now", got["expiresAt"], err)
	}

	// The wrong content type is refused, and nothing is stored.
	expectCode(t, h.put(t, uploadURL, "image/png", content), http.StatusUnprocessableEntity, CodeValidationFailed)

	rec := h.put(t, uploadURL, "application/pdf", content)
	received := expect(t, rec, http.StatusOK)
	if received["sha256"] != declared {
		t.Fatalf("sha256 = %v, want %s", received["sha256"], declared)
	}
	stored, err := h.uploads.Get(docID)
	if err != nil || string(stored) != string(content) {
		t.Fatalf("stored %q (%v), want the uploaded bytes", stored, err)
	}
	if n := h.count(t, `SELECT count(*) FROM document_uploads WHERE id = $1 AND status = 'RECEIVED' AND investor_id = $2`, docID, investorID); n != 1 {
		t.Fatal("the upload is not recorded as received against the investor")
	}

	// A URL is good for one upload.
	expectCode(t, h.put(t, uploadURL, "application/pdf", content), http.StatusConflict, CodePreconditionFailed)
}

// TestAnUploadThatDoesNotMatchItsDeclaredHashIsRejected is the property the declared hash exists for.
func TestAnUploadThatDoesNotMatchItsDeclaredHashIsRejected(t *testing.T) {
	h := newWriteHarness(t)
	investorID, _, _ := seedAPIInvestor(t, h.ctx, h.tx)
	declared := hashOf([]byte("the file I meant"))

	got := expect(t, h.send(t, http.MethodPost, "/v1/documents/presign", h.investor(t, investorID), idem("presign"),
		presignBody(&declared)), http.StatusCreated)
	uploadURL := got["uploadUrl"].(string)

	msg := expectCode(t, h.put(t, uploadURL, "application/pdf", []byte("a different file")), http.StatusUnprocessableEntity, CodeValidationFailed)
	if !strings.Contains(msg, "nothing was stored") {
		t.Errorf("message = %q", msg)
	}
	if _, err := h.uploads.Get(got["documentId"].(string)); err == nil {
		t.Fatal("mismatched bytes were stored")
	}

	// The right bytes still go through on the same URL.
	expect(t, h.put(t, uploadURL, "application/pdf", []byte("the file I meant")), http.StatusOK)
}

// TestUploadURLsAreCapabilities covers tampering and expiry.
func TestUploadURLsAreCapabilities(t *testing.T) {
	h := newWriteHarness(t)
	investorID, _, _ := seedAPIInvestor(t, h.ctx, h.tx)
	got := expect(t, h.send(t, http.MethodPost, "/v1/documents/presign", h.investor(t, investorID), idem("presign"),
		presignBody(nil)), http.StatusCreated)
	uploadURL := got["uploadUrl"].(string)

	u, _ := url.Parse(uploadURL)
	q := u.Query()

	// Extending the expiry invalidates the signature.
	extended := *u
	q2 := url.Values{"expires": {"9999999999"}, "sig": {q.Get("sig")}}
	extended.RawQuery = q2.Encode()
	expectCode(t, h.put(t, extended.String(), "application/pdf", []byte("x")), http.StatusForbidden, CodeForbidden)

	// Pointing the signature at another document does too.
	other := expect(t, h.send(t, http.MethodPost, "/v1/documents/presign", h.investor(t, investorID), idem("other"),
		presignBody(nil)), http.StatusCreated)
	swapped := strings.Replace(uploadURL, got["documentId"].(string), other["documentId"].(string), 1)
	expectCode(t, h.put(t, swapped, "application/pdf", []byte("x")), http.StatusForbidden, CodeForbidden)

	// A genuine URL past its expiry is refused.
	h.srv.deps.Wall = fixedClock{at: tokenNow.Add(uploadTTL + time.Minute)}
	msg := expectCode(t, h.put(t, uploadURL, "application/pdf", []byte("x")), http.StatusForbidden, CodeForbidden)
	if !strings.Contains(msg, "expired") {
		t.Errorf("message = %q", msg)
	}
}

// TestPresignRefusesWhatItShouldNotStore covers the request validation.
func TestPresignRefusesWhatItShouldNotStore(t *testing.T) {
	h := newWriteHarness(t)
	investorID, _, _ := seedAPIInvestor(t, h.ctx, h.tx)
	token := h.investor(t, investorID)

	cases := map[string]func(map[string]any){
		"a path in the filename":   func(b map[string]any) { b["filename"] = "../../etc/passwd" },
		"an executable":            func(b map[string]any) { b["mimeType"] = "application/x-msdownload" },
		"an operator document":     func(b map[string]any) { b["purpose"] = "VALUATION_REPORT" },
		"an unknown purpose":       func(b map[string]any) { b["purpose"] = "SELFIE" },
		"an uppercase hash":        func(b map[string]any) { b["publicHash"] = "0x" + strings.Repeat("A", 64) },
		"an oversized declaration": func(b map[string]any) { b["byteSize"] = 1 << 30 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := presignBody(nil)
			mutate(b)
			expectCode(t, h.send(t, http.MethodPost, "/v1/documents/presign", token, idem("bad"), b),
				http.StatusUnprocessableEntity, CodeValidationFailed)
		})
	}
	expectCode(t, h.send(t, http.MethodPost, "/v1/documents/presign", h.operator(t, RoleManager), idem("op"), presignBody(nil)),
		http.StatusForbidden, CodeForbidden)
	if n := h.count(t, `SELECT count(*) FROM document_uploads WHERE investor_id = $1`, investorID); n != 0 {
		t.Fatalf("%d upload intents recorded by refused requests", n)
	}
}
