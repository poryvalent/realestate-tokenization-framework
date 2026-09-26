package bidbook

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
)

var (
	ErrNoPin          = errors.New("bidbook: no pin to anchor")
	ErrWrongDocType   = errors.New("bidbook: the pin is not a bid book document")
	ErrPinDoesNotBind = errors.New("bidbook: the pinned document is not this book")
)

// AnchorRequest is the exact argument list for AcreSyncBallot.anchorBidbook.
//
// Built as a value rather than assembled at the call site so that everything the contract will check is
// checked before a transaction is signed. A revert costs gas and reports a Solidity selector; a refusal
// here names the artefact that is wrong.
type AnchorRequest struct {
	// Root is the frozen bid book Merkle root.
	Root merkle.Hash

	// CIDDigest is sha256 of the canonical document bytes.
	//
	// Despite the contract's parameter name this is the document digest, not an encoding of the CID.
	// That is the whole point: the commitment is something a verifier computes for themselves by hashing
	// the bytes they fetched. A CID is a locator, and a locator that stopped resolving would take the
	// commitment with it.
	CIDDigest [32]byte

	LeafCount       uint32
	TotalUnitsBid   uint32
	DistinctBidders uint32

	// CID is the address the document can be fetched from. Recorded in the database, not anchored.
	CID string

	// ByteSize and SingleBlock describe the pinned bytes.
	//
	// SingleBlock matters because the derived-CID rule only holds for content that fits one raw block.
	// Above that a provider chunks it, the root block is a dag-pb wrapper, and its digest is not the
	// document digest. Carried so the operator knows which claim they can make.
	ByteSize      int
	SingleBlock   bool
	CodecVerified bool
}

// AnchorRequest validates a pin against this book and produces the anchor arguments.
//
// # Why the pin is re-derived rather than trusted
//
// Anchoring the root of one book alongside the digest of a different document is the failure this guards
// against, and it is not exotic: freeze, pin, correct something, re-freeze, then anchor with the pin
// still in hand from the first attempt. The result passes every individual check, is permanently
// recorded, and leaves a published document whose contents do not produce the anchored root. Nobody
// notices until a verifier tries to reproduce it.
//
// So the book re-renders its own document, canonicalises it through the same allowlist the publisher
// used, hashes that, and requires the answer to equal the digest the pin reports. The pin is accepted as
// belonging to this book only if the bytes say so.
func (b *Book) AnchorRequest(pin *ipfs.Pin) (*AnchorRequest, error) {
	if pin == nil {
		return nil, ErrNoPin
	}
	if pin.DocType != ipfsguard.DocBidbook {
		return nil, fmt.Errorf("%w: it is a %s", ErrWrongDocType, pin.DocType)
	}

	raw, err := b.Document()
	if err != nil {
		return nil, fmt.Errorf("bidbook: re-rendering the document to check the pin: %w", err)
	}
	canonical, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocBidbook, raw)
	if err != nil {
		return nil, fmt.Errorf("bidbook: re-canonicalising the document to check the pin: %w", err)
	}

	digest := sha256.Sum256(canonical)
	if digest != pin.Digest {
		return nil, fmt.Errorf("%w: this book's document hashes to %x, the pin reports %x",
			ErrPinDoesNotBind, digest, pin.Digest)
	}

	// Everything anchorBidbook itself rejects, rejected here with the cause named.
	if b.MerkleRoot.IsZero() {
		return nil, errors.New("bidbook: the root is zero, so the book was never frozen")
	}
	if digest == ([32]byte{}) {
		return nil, errors.New("bidbook: the document digest is zero")
	}
	if b.LeafCount == 0 {
		return nil, errors.New("bidbook: leaf count is zero; anchorBidbook rejects an empty book")
	}
	if b.TotalUnitsBid == 0 {
		return nil, errors.New("bidbook: total units bid is zero")
	}
	if b.DistinctBidders == 0 {
		return nil, errors.New("bidbook: distinct bidders is zero")
	}
	if b.DistinctBidders > b.LeafCount {
		// anchorBidbook reverts with InvalidCounts. Freeze already requires equality, so reaching this
		// means the book was mutated after it was frozen.
		return nil, fmt.Errorf("%w: %d bidders across %d leaves",
			ErrCountsDisagree, b.DistinctBidders, b.LeafCount)
	}

	cid := pin.ProviderCID
	if cid == "" {
		cid = pin.DerivedCID
	}

	return &AnchorRequest{
		Root:            b.MerkleRoot,
		CIDDigest:       digest,
		LeafCount:       b.LeafCount,
		TotalUnitsBid:   b.TotalUnitsBid,
		DistinctBidders: b.DistinctBidders,
		CID:             cid,
		ByteSize:        pin.ByteSize,
		SingleBlock:     pin.SingleBlock,
		CodecVerified:   pin.CodecVerified,
	}, nil
}

// OutboxPayload renders the call arguments for the relayer.
//
// Byte slices here, because this is consumed by the transaction builder rather than by the idempotency
// canonicaliser. The two encodings are deliberately separate; see IdempotencyInput.
func (r *AnchorRequest) OutboxPayload() ([]byte, error) {
	return json.Marshal(map[string]any{
		"root":          r.Root[:],
		"cidDigest":     r.CIDDigest[:],
		"leafCount":     r.LeafCount,
		"unitsBid":      r.TotalUnitsBid,
		"bidders":       r.DistinctBidders,
		"ipfsCid":       r.CID,
		"byteSize":      r.ByteSize,
		"singleBlock":   r.SingleBlock,
		"codecVerified": r.CodecVerified,
	})
}

// IdempotencyInput describes the key for anchoring this book.
//
// # What the payload carries and why
//
// The root and the document digest. Re-anchoring the identical book derives the identical key and is
// rejected as a duplicate, which is what makes a retry after a dropped connection safe. A book that
// changed at all produces a different root, therefore a different key, and is then caught by the
// contract's own stage check rather than silently overwriting an anchored root.
//
// Hex strings rather than raw bytes because the canonical encoder refuses byte slices outright, and the
// counts are omitted because they are functions of the book the root already commits to.
func (r *AnchorRequest) IdempotencyInput(schemeID, offerID string) idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionAnchorBidbook,
		SchemeID: schemeID,
		ScopeID:  offerID,
		Payload: map[string]any{
			"bidbookRoot": r.Root.Hex(),
			"cidDigest":   merkle.Hash(r.CIDDigest).Hex(),
		},
	}
}

// IdempotencyKey derives the anchor key for this book.
func (b *Book) IdempotencyKey(r *AnchorRequest) (idempotency.Key, error) {
	return idempotency.Derive(r.IdempotencyInput(b.SchemeID, b.OfferID))
}

// FreezeIdempotencyInput describes the key for the freeze itself.
//
// Separate from the anchor key because freezing is a database act and anchoring is a chain act. One key
// covering both would make a successful freeze followed by a failed anchor unretryable: the retry would
// derive a key already marked used.
func (b *Book) FreezeIdempotencyInput() idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionFreezeBook,
		SchemeID: b.SchemeID,
		ScopeID:  b.OfferID,
		Payload: map[string]any{
			"bidbookRoot": b.MerkleRoot.Hex(),
			"leafCount":   b.LeafCount,
		},
	}
}
