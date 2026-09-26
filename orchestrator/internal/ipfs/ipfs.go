package ipfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/ipfsguard"
)

var (
	ErrDigestMismatch  = errors.New("ipfs: the provider stored bytes that hash to a different digest")
	ErrEmptyDocument   = errors.New("ipfs: refusing to pin an empty document")
	ErrUnknownProvider = errors.New("ipfs: unknown provider")
	ErrNotFound        = errors.New("ipfs: no content at that address")
)

// SingleBlockMaxBytes is the size below which a document occupies one IPFS block.
//
// This constant is the boundary of the CID reconstruction rule, and it exists because the rule is
// not universally true. The raw multicodec describes leaf blocks. A document larger than the
// chunking size is split, and IPFS builds a dag-pb root node over the leaves, so the root CID is
// the hash of that node rather than of the document. Derive a raw CID from the document digest in
// that regime and it addresses nothing.
//
// 256 KiB is the default chunk size across IPFS implementations. Staying under it keeps every
// AcreSync document a single raw block, which is what makes the derived CID resolvable. A 500-unit
// register snapshot is roughly 75 KB, so the working documents sit well inside the boundary; the
// constant is here to make the edge explicit rather than assumed.
const SingleBlockMaxBytes = 256 << 10

// Pin is the record of one published document.
type Pin struct {
	DocType  ipfsguard.DocType
	ByteSize int

	// Digest is SHA-256 of the canonical bytes, and it is the only value anchored on-chain.
	//
	// It is computed locally, never taken from the provider. That is the whole point: the
	// commitment is a property of the document, not a claim by a pinning service. A verifier
	// fetches the file, hashes it, and compares against the chain. No part of that requires
	// understanding IPFS block layout, or trusting Pinata, or trusting us.
	Digest [32]byte

	// DerivedCID is the content address implied by Digest under the published rule.
	DerivedCID string

	// ProviderCID is the address the pinning service actually reported.
	//
	// Stored separately from DerivedCID because they can legitimately differ: a service that wraps
	// content in dag-pb, or emits CIDv0, produces a valid address for the same document under a
	// different encoding. Collapsing the two into one column would hide that, and the first sign
	// of trouble would be a verifier unable to fetch the anchored document.
	ProviderCID string

	// CodecVerified is true when ProviderCID decodes to exactly DerivedCID.
	//
	// When true, the published derivation resolves on any public gateway and a verifier needs
	// nothing from AcreSync but the on-chain digest. When false, verification still holds through
	// ProviderCID plus the digest comparison, but the stronger claim does not, and the operator
	// should know which regime they are in before telling anyone the documents are independently
	// addressable.
	CodecVerified bool

	// SingleBlock records whether the document is under SingleBlockMaxBytes.
	SingleBlock bool

	Provider    string
	ProviderRef string

	// CanonicalBytes are the exact bytes pinned, returned so the caller persists what was
	// published rather than re-serialising and risking a second encoding of the same content.
	CanonicalBytes []byte

	// PinCount is how many independent services hold this content.
	//
	// Recorded because IPFS is addressing, not storage: unpinned content is garbage collected.
	// "Permanently verifiable" is a statement about pin redundancy, and a single pinning account is
	// an availability dependency. Storing the count keeps that visible instead of implied.
	PinCount int
}

// Provider publishes bytes and returns their content address.
type Provider interface {
	// Pin publishes already-canonical, already-validated bytes.
	Pin(ctx context.Context, docType ipfsguard.DocType, canonical []byte) (*Pin, error)

	// Fetch retrieves content by address. Needed by the verification path, which has to be able to
	// prove the round trip rather than assume it.
	Fetch(ctx context.Context, cid string) ([]byte, error)

	// Name identifies the provider in the ipfs_pins record.
	Name() string
}

// Publisher validates a document and then pins it.
//
// The two steps are one call deliberately, so no code path can pin without validating. Publishing
// to IPFS is irreversible in a way writing to a private bucket is not: content is addressed by its
// hash, anyone who fetches it can re-pin it, and there is no delete. A personal identifier that
// reaches IPFS once has reached it permanently, and an erasure request under the DPDP Act against
// it cannot be honoured. So the guard runs first, always, and the bytes pinned are the canonical
// bytes the guard returned rather than whatever the caller happened to hold.
type Publisher struct {
	provider Provider
}

func NewPublisher(p Provider) *Publisher {
	return &Publisher{provider: p}
}

func (pub *Publisher) ProviderName() string { return pub.provider.Name() }

// Publish validates raw against the schema for docType, then pins the canonical form.
func (pub *Publisher) Publish(ctx context.Context, docType ipfsguard.DocType, raw []byte) (*Pin, error) {
	if len(raw) == 0 {
		return nil, ErrEmptyDocument
	}

	// Fail closed. ValidateAndCanonicalize rejects any field not on the allowlist, any value not
	// matching its declared machine format, and anything shaped like an Indian personal
	// identifier. It returns canonical bytes, so the validated bytes and the pinned bytes are
	// necessarily identical.
	canonical, err := ipfsguard.ValidateAndCanonicalize(docType, raw)
	if err != nil {
		return nil, fmt.Errorf("ipfs: refusing to pin: %w", err)
	}

	digest := sha256.Sum256(canonical)
	derived := CIDFromDigest(digest)

	pin, err := pub.provider.Pin(ctx, docType, canonical)
	if err != nil {
		return nil, err
	}

	// The digest is ours, not the provider's. Overwrite whatever came back so there is no way for
	// a provider response to influence the value that gets anchored on-chain.
	pin.Digest = digest
	pin.DerivedCID = derived
	pin.ByteSize = len(canonical)
	pin.SingleBlock = len(canonical) <= SingleBlockMaxBytes
	pin.CanonicalBytes = canonical
	pin.DocType = docType

	// Compare the provider's address against the derivation.
	//
	// If the provider returned a raw sha2-256 CIDv1, its embedded digest has to equal ours, since
	// both are SHA-256 of the same bytes. A difference there means the service stored something
	// other than what we sent, which is fatal: we would anchor a commitment to bytes nobody can
	// retrieve. If instead the provider used a different codec, the addresses differ for a benign
	// reason and we record that rather than failing.
	if pin.ProviderCID == derived {
		pin.CodecVerified = true
	} else if pd, decErr := DigestFromCID(pin.ProviderCID); decErr == nil {
		if pd != digest {
			return nil, fmt.Errorf("%w: sent bytes hashing to %x, provider reported a raw CID for %x",
				ErrDigestMismatch, digest, pd)
		}
		pin.CodecVerified = true
	} else {
		pin.CodecVerified = false
	}

	return pin, nil
}

// VerifyRoundTrip fetches a pinned document back and confirms it hashes to the anchored digest.
//
// This is the check that turns "we pinned it" into "it is retrievable and it is the document the
// chain commits to". Those are different claims, and only the second one is worth anything during
// diligence. Called after every pin in the period pipeline, because a pin that succeeded but is
// unretrievable is indistinguishable from a working one until someone tries to audit.
func (pub *Publisher) VerifyRoundTrip(ctx context.Context, pin *Pin) error {
	addr := pin.ProviderCID
	if addr == "" {
		addr = pin.DerivedCID
	}

	got, err := pub.provider.Fetch(ctx, addr)
	if err != nil {
		return fmt.Errorf("ipfs: %s pinned at %s but could not be fetched back: %w", pin.DocType, addr, err)
	}

	actual := sha256.Sum256(got)
	if actual != pin.Digest {
		return fmt.Errorf("%w: %s at %s fetched back as %x, anchored digest is %x",
			ErrDigestMismatch, pin.DocType, addr, actual, pin.Digest)
	}
	return nil
}

// Fetch exposes retrieval for the verification tooling.
func (pub *Publisher) Fetch(ctx context.Context, cid string) ([]byte, error) {
	return pub.provider.Fetch(ctx, cid)
}

// NewProvider builds the configured provider.
func NewProvider(cfg config.IPFSConfig, mockDir string) (Provider, error) {
	switch cfg.Provider {
	case config.IPFSMock:
		return NewMockProvider(mockDir)
	case config.IPFSPinata:
		return NewPinataProvider(cfg)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, cfg.Provider)
	}
}
