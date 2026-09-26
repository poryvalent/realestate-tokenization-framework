package ballotrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
)

// SchemaVersion is the document schema generation.
const SchemaVersion = 1

var (
	ErrNoPin          = errors.New("ballotrun: no pin to anchor")
	ErrWrongDocType   = errors.New("ballotrun: the pin is not an allotment file")
	ErrPinDoesNotBind = errors.New("ballotrun: the pinned document is not this draw")
)

// Document renders the allotment file as JSON for publishing.
//
// # What a verifier can do with this
//
// Everything needed to reproduce the draw. The final seed, the bid book root and the algorithm version
// are all here, and the bid book itself was pinned earlier. A reader fetches both documents, reruns the
// published algorithm over the book with this seed, and compares their allocations line by line against
// these. If the result root they compute matches the one anchored on-chain, the draw was not steered.
//
// # Why the seed is published in plaintext
//
// It is the one value that makes the draw checkable, and it is safe to publish precisely because of the
// order things happened in: the secret was committed before the target block existed, and the target
// block's hash was fixed by the chain. Publishing the seed afterwards gives away nothing, because there
// is nothing left to influence.
//
// # Why losing bids are here too
//
// A bidder who won nothing is the one with the strongest reason to check the draw, and the only way they
// can is by finding their own leaf, recomputing their rank key, and seeing that it really did fall below
// the cut. Publishing winners alone would make the outcome unfalsifiable for exactly those people.
func (r *Run) Document() ([]byte, error) {
	if !isCanonicalUUID(r.OfferID) {
		return nil, fmt.Errorf("ballotrun: offerId %q is not a lowercase canonical UUID", r.OfferID)
	}
	if err := r.Validate(); err != nil {
		// A document that describes an unanchorable draw would be pinned, immutable and useless.
		return nil, fmt.Errorf("ballotrun: refusing to render a document for an invalid draw: %w", err)
	}

	allotments := make([]map[string]any, 0, len(r.Lines))
	for _, l := range r.Lines {
		allotments = append(allotments, map[string]any{
			"leafIndex": l.LeafIndex,

			// The anchor, not a name. The same value published in the bid book, so a reader can join
			// the two documents without either of them naming anybody.
			"investorAnchor": hexBytes32(l.InvestorAnchor),

			"unitsAllotted": l.UnitsAllotted,

			"outcome": string(l.Outcome),

			// The rank the draw produced. Publishing it is what lets a losing bidder recompute their own
			// key from the seed and their anchor, and confirm their position was not invented.
			"ballotRank": l.BallotRank,
		})

		// AmountPayable and RefundAmount are deliberately absent. Both are units times the published
		// price, so a reader derives them, and a stated figure that disagreed with the arithmetic would
		// give two answers with no rule for choosing. BidRef is absent too: the bid book already binds
		// reference to leaf index, and repeating it here would let anyone holding one investor's receipt
		// read their allotment out of a document they have no other reason to be able to index.
	}

	doc := map[string]any{
		"docType":       string(ipfsguard.DocAllotmentFile),
		"schemaVersion": SchemaVersion,
		"schemeRef":     hexBytes32(r.SchemeRef),

		"offerId": r.OfferID,

		"finalSeed":   r.Result.FinalSeed.Hex(),
		"bidbookRoot": r.Result.BidbookRoot.Hex(),
		"resultRoot":  r.Result.ResultRoot.Hex(),

		"unitsOnOffer":      r.UnitsOnOffer,
		"unitsAllotted":     r.Result.UnitsAllotted,
		"distinctAllottees": r.Result.DistinctAllottees,

		"algoVersion": r.Result.AlgoVersion,

		"allotments": allotments,
	}

	return json.Marshal(doc)
}

// ResultAnchorRequest is the argument list for AcreSyncBallot.anchorBallotResult.
type ResultAnchorRequest struct {
	Root      merkle.Hash
	CIDDigest [32]byte

	UnitsAllotted      uint32
	DistinctAllottees  uint32
	UnitsOnOffer       uint32
	MinDistinctHolders uint32
	AlgoVersion        uint32

	CID           string
	ByteSize      int
	SingleBlock   bool
	CodecVerified bool
}

// AnchorRequest validates a pin against this draw and produces the anchor arguments.
//
// The pin is re-derived rather than trusted, for the same reason the bid book anchor re-derives its own:
// drawing, pinning, re-drawing after a correction, then anchoring with the earlier pin still in hand
// produces a permanent anchor whose published document does not yield the anchored root. Every individual
// value passes its own check. Nobody notices until someone tries to reproduce the draw.
func (r *Run) AnchorRequest(pin *ipfs.Pin) (*ResultAnchorRequest, error) {
	if pin == nil {
		return nil, ErrNoPin
	}
	if pin.DocType != ipfsguard.DocAllotmentFile {
		return nil, fmt.Errorf("%w: it is a %s", ErrWrongDocType, pin.DocType)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}

	raw, err := r.Document()
	if err != nil {
		return nil, fmt.Errorf("ballotrun: re-rendering the document to check the pin: %w", err)
	}
	canonical, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocAllotmentFile, raw)
	if err != nil {
		return nil, fmt.Errorf("ballotrun: re-canonicalising the document to check the pin: %w", err)
	}

	digest := sha256.Sum256(canonical)
	if digest != pin.Digest {
		return nil, fmt.Errorf("%w: this draw's document hashes to %x, the pin reports %x",
			ErrPinDoesNotBind, digest, pin.Digest)
	}

	cid := pin.ProviderCID
	if cid == "" {
		cid = pin.DerivedCID
	}

	return &ResultAnchorRequest{
		Root:               r.Result.ResultRoot,
		CIDDigest:          digest,
		UnitsAllotted:      r.Result.UnitsAllotted,
		DistinctAllottees:  r.Result.DistinctAllottees,
		UnitsOnOffer:       r.UnitsOnOffer,
		MinDistinctHolders: r.MinDistinctHolders,
		AlgoVersion:        r.Result.AlgoVersion,
		CID:                cid,
		ByteSize:           pin.ByteSize,
		SingleBlock:        pin.SingleBlock,
		CodecVerified:      pin.CodecVerified,
	}, nil
}

// OutboxPayload renders the call arguments for the relayer.
func (q *ResultAnchorRequest) OutboxPayload() ([]byte, error) {
	return json.Marshal(map[string]any{
		"root":               q.Root[:],
		"cidDigest":          q.CIDDigest[:],
		"allotted":           q.UnitsAllotted,
		"allottees":          q.DistinctAllottees,
		"unitsOnOffer":       q.UnitsOnOffer,
		"minDistinctHolders": q.MinDistinctHolders,
		"algo":               q.AlgoVersion,
		"ipfsCid":            q.CID,
		"byteSize":           q.ByteSize,
		"singleBlock":        q.SingleBlock,
		"codecVerified":      q.CodecVerified,
	})
}

// CommitIdempotencyInput describes the key for anchoring the seed commitment.
//
// # Why the attempt number is absent
//
// The commitment is immutable and a recommit reuses it, so there is exactly one commitSeed per offer for
// all time. Including an attempt would make a retry after a dropped connection derive a fresh key and
// commit twice, which the contract would reject on stage but which would waste an attempt from a cap of
// three.
func (c *Ceremony) CommitIdempotencyInput() idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionCommitSeed,
		SchemeID: c.SchemeID,
		ScopeID:  c.OfferID,
		Payload: map[string]any{
			"commitment":  c.Commitment.Hex(),
			"bidbookRoot": c.BidbookRoot.Hex(),
		},
	}
}

// RecommitIdempotencyInput describes the key for rerolling the target block.
//
// # Why the attempt number is present here
//
// The opposite case to the commit. Each recommit is a genuinely distinct act that must be allowed
// through, and there can be up to two of them. A key without the attempt would make the second recommit
// a duplicate of the first and leave the ceremony stuck at a lapsed window with attempts nominally
// remaining.
func (c *Ceremony) RecommitIdempotencyInput() idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionRecommitSeed,
		SchemeID: c.SchemeID,
		ScopeID:  c.OfferID,
		Payload: map[string]any{
			"commitment":  c.Commitment.Hex(),
			"attempt":     c.Attempt,
			"targetBlock": c.TargetBlock,
		},
	}
}

// RevealIdempotencyInput describes the key for revealing the secret.
//
// Carries the target block, because a reveal after a recommit is against a different block and is a
// different act. Without it a ceremony that lapsed once could never reveal: the retry would derive a key
// already spent on the abandoned attempt.
func (c *Ceremony) RevealIdempotencyInput() idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionRevealSeed,
		SchemeID: c.SchemeID,
		ScopeID:  c.OfferID,
		Payload: map[string]any{
			"commitment":  c.Commitment.Hex(),
			"targetBlock": c.TargetBlock,
		},
	}
}

// RunIdempotencyInput describes the key for recording the draw.
func (r *Run) RunIdempotencyInput() idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionRunBallot,
		SchemeID: r.SchemeID,
		ScopeID:  r.OfferID,
		Payload: map[string]any{
			"finalSeed":  r.Result.FinalSeed.Hex(),
			"resultRoot": r.Result.ResultRoot.Hex(),
		},
	}
}

// AnchorIdempotencyInput describes the key for anchoring the result.
func (q *ResultAnchorRequest) AnchorIdempotencyInput(schemeID, offerID string) idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionFinaliseAllotment,
		SchemeID: schemeID,
		ScopeID:  offerID,
		Payload: map[string]any{
			"resultRoot": q.Root.Hex(),
			"cidDigest":  merkle.Hash(q.CIDDigest).Hex(),
		},
	}
}

func hexBytes32(h merkle.Hash) string {
	return "0x" + strings.ToLower(hex.EncodeToString(h[:]))
}

func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isDigit := c >= '0' && c <= '9'
			isLowerHex := c >= 'a' && c <= 'f'
			if !isDigit && !isLowerHex {
				return false
			}
		}
	}
	return true
}
