package ipfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/ipfsguard"
)

// Pinata pins through Pinata's IPFS service.
//
// # Status
//
// This path is written but unexercised: it has never run against live credentials. Everything in
// M5 runs on MockProvider. Saying so plainly matters more than the code looking finished, because
// the one property this provider is meant to establish is empirical and still open.
//
// # What it is here to find out
//
// The CID derivation in cid.go assumes a raw, single-block address. Pinata's documented
// pinataOptions only expose cidVersion, which selects CIDv1 over CIDv0 and says nothing about the
// codec. So Pinata may well return a dag-pb CIDv1, whose digest is the hash of a UnixFS node rather
// than of the document. Until a real upload happens, which of the two we get is a guess.
//
// That uncertainty is survivable only because the anchored commitment is the document digest, not
// the CID. If the codecs disagree the pin records CodecVerified false, VerifyRoundTrip still proves
// retrievability against the anchored digest, and nothing on-chain is wrong. If they agree, the
// published derivation additionally resolves on any gateway. Either way the failure mode is a
// weaker claim, not a broken ledger — which is the reason for structuring it this way rather than
// asserting the codec and hoping.
type Pinata struct {
	jwt     config.Secret
	gateway string
	client  *http.Client
}

const (
	pinataPinFileURL = "https://api.pinata.cloud/pinning/pinFileToIPFS"

	// pinataTimeout bounds one attempt. Generous because a multi-megabyte document over a slow
	// link is legitimate, and a pin that gets cancelled after the content was stored leaves an
	// ambiguous result: content addressing makes the retry harmless, but the first answer is lost.
	pinataTimeout = 90 * time.Second

	pinataMaxAttempts = 3
)

func NewPinataProvider(cfg config.IPFSConfig) (*Pinata, error) {
	if cfg.PinataJWT.IsZero() {
		return nil, errors.New("ipfs: Pinata selected but ACRESYNC_PINATA_JWT is unset")
	}
	if cfg.Gateway == "" {
		return nil, errors.New("ipfs: Pinata selected but ACRESYNC_IPFS_GATEWAY is unset")
	}
	return &Pinata{
		jwt:     cfg.PinataJWT,
		gateway: strings.TrimRight(cfg.Gateway, "/"),
		client:  &http.Client{Timeout: pinataTimeout},
	}, nil
}

func (p *Pinata) Name() string { return "PINATA" }

type pinataResponse struct {
	IpfsHash    string `json:"IpfsHash"`
	PinSize     int64  `json:"PinSize"`
	Timestamp   string `json:"Timestamp"`
	IsDuplicate bool   `json:"isDuplicate"`
}

func (p *Pinata) Pin(ctx context.Context, docType ipfsguard.DocType, canonical []byte) (*Pin, error) {
	if len(canonical) == 0 {
		return nil, ErrEmptyDocument
	}

	digest := sha256.Sum256(canonical)
	derived := CIDFromDigest(digest)

	// The filename is the derived CID rather than anything descriptive.
	//
	// Two reasons. It cannot leak: a name like "snapshot-period-3-ramesh.json" would put content
	// into Pinata's metadata that the guard spent its whole existence keeping out of the document
	// body, and pinataMetadata is not covered by the allowlist. And a content-derived name is
	// stable, so re-pinning the same document does not create a second differently-labelled copy.
	filename := derived + ".json"

	var lastErr error
	for attempt := 1; attempt <= pinataMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		resp, err := p.upload(ctx, filename, canonical)
		if err == nil {
			return &Pin{
				DocType:     docType,
				ByteSize:    len(canonical),
				Digest:      digest,
				DerivedCID:  derived,
				ProviderCID: resp.IpfsHash,
				Provider:    p.Name(),
				ProviderRef: resp.IpfsHash,

				// One pinning account is one copy. Reported honestly as 1 rather than inflated,
				// because this number is the basis of any durability claim and a wrong value there
				// is worse than no value.
				PinCount:    1,
				SingleBlock: len(canonical) <= SingleBlockMaxBytes,
			}, nil
		}

		lastErr = err
		if !isRetryablePinataErr(err) {
			break
		}
		// Content addressing makes a retry safe: the same bytes produce the same address, so a
		// duplicate upload converges on the identical CID and Pinata reports isDuplicate rather
		// than creating a second object.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}

	return nil, fmt.Errorf("ipfs: pinata pin failed after %d attempts: %w", pinataMaxAttempts, lastErr)
}

func (p *Pinata) upload(ctx context.Context, filename string, canonical []byte) (*pinataResponse, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(canonical); err != nil {
		return nil, err
	}

	// cidVersion 1 is requested because a CIDv0 is dag-pb by definition and could never match the
	// derivation. Asking for v1 at least makes agreement possible.
	if err := mw.WriteField("pinataOptions", `{"cidVersion":1}`); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pinataPinFileURL, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.jwt.Reveal())
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &pinataError{retryable: true, err: err}
	}
	defer resp.Body.Close()

	// The response is length-limited before parsing. An unbounded ReadAll on a remote body is a
	// memory exhaustion path controlled by someone else.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &pinataError{retryable: true, err: err}
	}

	if resp.StatusCode != http.StatusOK {
		// The body is included but the JWT never is, and Secret's String method means an
		// accidental %v on the config cannot put it in a log line either.
		return nil, &pinataError{
			retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			err:       fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 400)),
		}
	}

	var out pinataResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &pinataError{err: fmt.Errorf("decoding pinata response: %w", err)}
	}
	if out.IpfsHash == "" {
		return nil, &pinataError{err: errors.New("pinata returned HTTP 200 with no IpfsHash")}
	}
	return &out, nil
}

// Fetch retrieves content through the configured gateway.
func (p *Pinata) Fetch(ctx context.Context, cid string) ([]byte, error) {
	// The CID is not validated against our strict rule here, unlike in the mock.
	//
	// Deliberate: the address being fetched may be a dag-pb CID that Pinata assigned, which the
	// strict decoder rejects by design. Rejecting it here would make VerifyRoundTrip impossible in
	// exactly the case it is most needed. Integrity is still enforced, just one level up, by
	// hashing what comes back and comparing against the anchored digest.
	if cid == "" {
		return nil, fmt.Errorf("%w: empty address", ErrNotFound)
	}
	if strings.ContainsAny(cid, "/?#") {
		return nil, fmt.Errorf("ipfs: %q is not a bare CID", cid)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GatewayURL(p.gateway, cid), nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: gateway returned 404 for %s", ErrNotFound, cid)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ipfs: gateway returned http %d for %s", resp.StatusCode, cid)
	}

	return io.ReadAll(io.LimitReader(resp.Body, ipfsguard.MaxDocumentBytes+1))
}

type pinataError struct {
	retryable bool
	err       error
}

func (e *pinataError) Error() string { return e.err.Error() }
func (e *pinataError) Unwrap() error { return e.err }

func isRetryablePinataErr(err error) bool {
	var pe *pinataError
	if errors.As(err, &pe) {
		return pe.retryable
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
