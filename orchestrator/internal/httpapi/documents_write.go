package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Presigned document uploads.
//
// POST /documents/presign records an upload intent and returns a URL. The URL is the capability: it names one
// document, expires, and is signed, so the PUT that follows needs no bearer token, exactly as an object-store
// presigned URL would not. Bytes are hashed on arrival; a declared hash that disagrees rejects the upload and
// nothing is stored.
//
// Investor purposes only (KYC, OTHER). See migration 0015 for why the operator document types are refused
// rather than accepted into storage with no lifecycle.

// UploadStore keeps uploaded bytes.
type UploadStore interface {
	Put(ctx context.Context, id string, content []byte) error
}

const (
	// uploadTTL is how long a presigned URL stays valid. Long enough to pick a file and upload it on a slow
	// connection; short enough that a leaked URL is soon useless.
	uploadTTL = 15 * time.Minute

	// maxUploadBytes bounds an upload with no declared size. It matches doc_uploads_declared_size.
	maxUploadBytes = 25 << 20
)

// investorPurposes are what an investor may upload. The contract's enum is wider; see the package comment.
var investorPurposes = map[string]bool{"KYC": true, "OTHER": true}

var allPurposes = map[string]bool{
	"KYC": true, "VALUATION_REPORT": true, "OFFER_DOCUMENT": true, "TRUSTEE_ESCROW": true,
	"RENT_EVIDENCE": true, "OTHER": true,
}

var uploadMimeTypes = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true}

var bytes32Re = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

// uploadSigner signs upload URLs under a key derived from the session secret, so an upload signature can never
// be replayed as, or confused with, a session token signature.
func (s *Server) uploadMAC(id string, expires int64) []byte {
	k := hmac.New(sha256.New, s.deps.SessionSecret)
	k.Write([]byte("acresync/upload-url/v1"))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte(id + "\n" + strconv.FormatInt(expires, 10)))
	return m.Sum(nil)
}

type wirePresigned struct {
	DocumentID string    `json:"documentId"`
	UploadURL  string    `json:"uploadUrl"`
	ExpiresAt  timestamp `json:"expiresAt"`
	PublicHash *string   `json:"publicHash"`
}

// handlePresignDocument serves POST /v1/documents/presign.
func (s *Server) handlePresignDocument(c *writeCtx) (int, any, error) {
	var body struct {
		Filename   string  `json:"filename"`
		MimeType   string  `json:"mimeType"`
		Purpose    string  `json:"purpose"`
		ByteSize   *int64  `json:"byteSize"`
		PublicHash *string `json:"publicHash"`
	}
	if err := c.decode(&body); err != nil {
		return 0, nil, err
	}

	switch {
	case body.Filename == "" || len(body.Filename) > 255 || strings.ContainsAny(body.Filename, `/\`) ||
		strings.IndexFunc(body.Filename, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return 0, nil, validation("filename must be 1 to 255 characters with no path separators or control characters")
	case !uploadMimeTypes[body.MimeType]:
		return 0, nil, validation("mimeType must be application/pdf, image/png or image/jpeg")
	case !allPurposes[body.Purpose]:
		return 0, nil, validation(fmt.Sprintf("purpose %q is not a document purpose", body.Purpose))
	case !investorPurposes[body.Purpose]:
		return 0, nil, validation(fmt.Sprintf("purpose %s is an operator document; an investor may upload KYC or OTHER",
			body.Purpose))
	case body.ByteSize != nil && (*body.ByteSize < 1 || *body.ByteSize > maxUploadBytes):
		return 0, nil, validation(fmt.Sprintf("byteSize must be between 1 and %d", maxUploadBytes))
	case body.PublicHash != nil && !bytes32Re.MatchString(*body.PublicHash):
		return 0, nil, validation("publicHash must be 0x followed by 64 lowercase hex characters")
	}

	var declared []byte
	if body.PublicHash != nil {
		declared, _ = hex.DecodeString((*body.PublicHash)[2:])
	}

	now, err := s.deps.Wall.Now(c.ctx())
	if err != nil {
		return 0, nil, err
	}
	expires := now.Add(uploadTTL).UTC().Truncate(time.Second)

	var id string
	if err := c.tx.QueryRow(c.ctx(), `
		INSERT INTO document_uploads (investor_id, purpose, filename, mime_type, declared_size, declared_sha256, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		c.principal.InvestorID, body.Purpose, body.Filename, body.MimeType, body.ByteSize, declared, expires).Scan(&id); err != nil {
		return 0, nil, err
	}

	q := url.Values{}
	q.Set("expires", strconv.FormatInt(expires.Unix(), 10))
	q.Set("sig", hex.EncodeToString(s.uploadMAC(id, expires.Unix())))
	return http.StatusCreated, wirePresigned{
		DocumentID: id,
		UploadURL:  strings.TrimRight(s.deps.UploadBaseURL, "/") + "/v1/documents/" + id + "/content?" + q.Encode(),
		ExpiresAt:  expires,
		PublicHash: body.PublicHash,
	}, nil
}

type wireReceived struct {
	DocumentID string `json:"documentId"`
	SHA256     string `json:"sha256"`
	ByteSize   int    `json:"byteSize"`
}

// handleUploadContent serves PUT /v1/documents/{documentId}/content, the URL presign hands out.
//
// Not part of the published contract, deliberately: it is the storage end of a presigned URL, and a client
// should treat uploadUrl as opaque. In production that URL would point at object storage instead.
func (s *Server) handleUploadContent(w http.ResponseWriter, r *http.Request) {
	if err := s.receiveUpload(w, r); err != nil {
		writeError(w, r, err)
	}
}

func (s *Server) receiveUpload(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "documentId")
	if err != nil {
		return err
	}

	// One refusal for every way the URL can be wrong, so it cannot be probed for which part was guessed.
	invalid := forbidden("the upload URL is not valid for this document")
	expires, err := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
	if err != nil {
		return invalid
	}
	sig, err := hex.DecodeString(r.URL.Query().Get("sig"))
	if err != nil || !hmac.Equal(sig, s.uploadMAC(id, expires)) {
		return invalid
	}
	now, err := s.deps.Wall.Now(r.Context())
	if err != nil {
		return err
	}
	if now.Unix() > expires {
		return forbidden("the upload URL has expired; request a new one")
	}

	tx, err := s.deps.DB.Begin(r.Context())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var (
		status, mimeType string
		declaredSize     *int64
		declaredSHA      []byte
	)
	err = tx.QueryRow(r.Context(), `
		SELECT status, mime_type, declared_size, declared_sha256 FROM document_uploads WHERE id = $1 FOR UPDATE`, id).
		Scan(&status, &mimeType, &declaredSize, &declaredSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("document")
	}
	if err != nil {
		return err
	}
	if status != "PENDING" {
		return conflict(CodePreconditionFailed, "this document has already been uploaded")
	}

	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != mimeType {
		return validation(fmt.Sprintf("Content-Type must be %s, as declared when the URL was issued", mimeType))
	}

	limit := int64(maxUploadBytes)
	if declaredSize != nil {
		limit = *declaredSize
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	switch {
	case len(content) == 0:
		return validation("the upload is empty")
	case int64(len(content)) > limit:
		return validation(fmt.Sprintf("the upload exceeds %d bytes", limit))
	case declaredSize != nil && int64(len(content)) != *declaredSize:
		return validation(fmt.Sprintf("received %d bytes, %d were declared", len(content), *declaredSize))
	}

	digest := sha256.Sum256(content)
	if declaredSHA != nil && !hmac.Equal(declaredSHA, digest[:]) {
		return validation(fmt.Sprintf("the bytes hash to %s but publicHash declared %s; nothing was stored",
			hexBytes(digest[:]), hexBytes(declaredSHA)))
	}

	if err := s.deps.Uploads.Put(r.Context(), id, content); err != nil {
		return fmt.Errorf("storing the upload: %w", err)
	}
	if _, err := tx.Exec(r.Context(), `
		UPDATE document_uploads
		   SET status = 'RECEIVED', received_sha256 = $2, received_size = $3, received_at = $4
		 WHERE id = $1 AND status = 'PENDING'`, id, digest[:], len(content), now.UTC()); err != nil {
		return translatePgError(r, err)
	}
	if err := tx.Commit(r.Context()); err != nil {
		return err
	}

	writeJSON(w, r, http.StatusOK, wireReceived{DocumentID: id, SHA256: hexBytes(digest[:]), ByteSize: len(content)})
	return nil
}
