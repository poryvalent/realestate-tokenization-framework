// Package ballot allocates units from an oversubscribed SM-REIT offer.
//
// # Why this is a ballot and not a pro-rata calculation
//
// The arithmetic of the SM-REIT framework forces it. A unit costs ten lakh rupees
// and a fifty crore scheme is therefore exactly 500 units, of which 475 are
// public after the investment manager's 5%. The scheme also needs at least 200
// distinct unitholders. With a one-unit minimum bid and 475 indivisible units,
// there is no proportional answer when 900 investors apply: somebody receives
// zero. This is the same situation Indian IPOs resolve with a lot-based lottery,
// and the regulation does not prescribe an algorithm, so the algorithm has to be
// specified, deterministic, and independently checkable.
//
// # Determinism is the product
//
// Run is a pure function of (bid book, parameters, final seed, algorithm version).
// Nothing here reads a clock, a database, a map iteration order, or a locale. That
// is what lets an investor recompute the entire allotment from the bid book pinned
// to IPFS and the seed revealed on-chain, and confirm that nobody was inserted,
// removed, or favoured after the draw.
//
// # Ranking by key, not by shuffle
//
// Each bid's ballot rank comes from sorting on sha256(domain || seed || anchor ||
// leafIndex). A Fisher-Yates shuffle seeded by a PRNG would also be deterministic,
// but verifying one bid's position would require replaying the whole shuffle and
// reimplementing the exact PRNG. With a per-bid key, a verifier recomputes one
// hash to check one bid, and the ordering is a consequence rather than a procedure.
package ballot

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

// AlgoVersion identifies the allocation rules below.
//
// It is recorded in the pinned allotment file and anchored on-chain. Any change to
// the ranking, selection or distribution logic must increment it, because a
// verifier reproducing a historical allotment needs to know which rules applied.
const AlgoVersion uint32 = 1

// Domain strings separate the three hash uses in this package so that a digest
// computed for one purpose can never be valid for another.
const (
	rankDomain      = "acresync.ballot.rank.v1"
	seedDomain      = "acresync.ballot.finalseed.v1"
	bidLeafDomain   = "acresync.ballot.bidleaf.v1"
	allotLeafDomain = "acresync.ballot.allotleaf.v1"
)

// BidRefLen is the byte length of a bid reference (32 lowercase hex characters).
//
// Deliberately opaque. A human-readable reference would be a personal-data vector
// in the bid book document, which is pinned publicly and permanently.
const BidRefLen = 16

var (
	ErrEmptyBook          = errors.New("ballot: bid book is empty")
	ErrDuplicateLeafIndex = errors.New("ballot: duplicate leaf index")
	ErrDuplicateAnchor    = errors.New("ballot: duplicate investor anchor")
	ErrLeafIndexGap       = errors.New("ballot: leaf indices must be contiguous from zero")
	ErrBadBidRef          = errors.New("ballot: bid reference must be 32 lowercase hex characters")
	ErrBadParams          = errors.New("ballot: invalid parameters")
	ErrInfeasible         = errors.New("ballot: offer is not feasible")
	ErrZeroSeed           = errors.New("ballot: final seed is unset")
	ErrInternal           = errors.New("ballot: internal invariant violated")
)

// Outcome mirrors the allocation_outcome Postgres enum and the Solidity enum, in
// that order, because the numeric code is part of the allotment leaf preimage.
type Outcome string

const (
	OutcomeFull         Outcome = "FULL"
	OutcomePartial      Outcome = "PARTIAL"
	OutcomeNilBallot    Outcome = "NIL_BALLOT"
	OutcomeNilTechnical Outcome = "NIL_TECHNICAL"
)

func (o Outcome) code() (byte, error) {
	switch o {
	case OutcomeFull:
		return 0, nil
	case OutcomePartial:
		return 1, nil
	case OutcomeNilBallot:
		return 2, nil
	case OutcomeNilTechnical:
		return 3, nil
	}
	return 0, fmt.Errorf("%w: unknown outcome %q", ErrInternal, o)
}

// Rejection mirrors the bid_rejection_reason Postgres enum.
type Rejection string

const (
	RejectionNone        Rejection = ""
	RejectionBelowMinBid Rejection = "BELOW_MIN_BID"
	RejectionAboveMaxBid Rejection = "ABOVE_MAX_BID"
	RejectionBallot      Rejection = "BALLOT_NOT_DRAWN"
)

// Bid is one entry in the frozen bid book.
//
// It carries no name, no PAN, and no wallet address. The investor is identified
// only by the HMAC anchor, which is what makes the bid book safe to pin publicly.
type Bid struct {
	LeafIndex         uint32
	BidRef            string
	InvestorAnchor    merkle.Hash
	UnitsBid          uint32
	PricePerUnitPaise money.Paise
}

// Params are the offer parameters the allocation must respect.
type Params struct {
	UnitsOnOffer         uint32
	MinBidUnits          uint32
	MaxBidUnits          uint32
	MinDistinctHolders   uint32
	MinSubscriptionUnits uint32
	PriceBandLowerPaise  money.Paise
	PriceBandUpperPaise  money.Paise

	// RequireFullSubscription must be true for a v1 scheme and defaults to that
	// meaning in Validate.
	//
	// The reason is structural rather than stylistic. A scheme's asset value equals
	// total units times unit price, and the SM-REIT band floor of fifty crore is
	// exactly 500 units at ten lakh. There is no room to shrink the scheme to match
	// a partial subscription without dropping below the statutory floor, so a
	// shortfall has to be abandoned or underwritten, never absorbed.
	RequireFullSubscription bool
}

// V1Params returns the locked parameters for the first AcreSync scheme.
func V1Params() Params {
	return Params{
		UnitsOnOffer:            475,
		MinBidUnits:             1,
		MaxBidUnits:             25,
		MinDistinctHolders:      200,
		MinSubscriptionUnits:    428, // 90% of the fresh issue
		PriceBandLowerPaise:     money.MinUnitPricePaise,
		PriceBandUpperPaise:     105_000_000, // ₹10.5 lakh
		RequireFullSubscription: true,
	}
}

func (p Params) Validate() error {
	var problems []string

	if p.UnitsOnOffer == 0 {
		problems = append(problems, "UnitsOnOffer must be positive")
	}
	if p.MinBidUnits == 0 {
		problems = append(problems, "MinBidUnits must be at least 1: units are indivisible")
	}
	if p.MaxBidUnits < p.MinBidUnits {
		problems = append(problems, "MaxBidUnits is below MinBidUnits")
	}
	if p.MinDistinctHolders == 0 {
		problems = append(problems, "MinDistinctHolders must be positive")
	}
	if p.MinDistinctHolders > p.UnitsOnOffer {
		problems = append(problems, fmt.Sprintf(
			"MinDistinctHolders (%d) exceeds UnitsOnOffer (%d): every holder needs at least one whole unit",
			p.MinDistinctHolders, p.UnitsOnOffer))
	} else if p.MinDistinctHolders > 0 {
		// The cap must leave the holder floor arithmetically reachable. If one bidder
		// could take so much that fewer than MinDistinctHolders units remain for
		// everyone else, the scheme becomes unlistable no matter how the draw falls.
		//
		// Guarded by the branch above because the subtraction is on uint32 and would
		// wrap to an enormous ceiling if the floor exceeded the units on offer,
		// turning a hard failure into a silently permissive check.
		maxFeasible := p.UnitsOnOffer - (p.MinDistinctHolders - 1)
		if p.MaxBidUnits > maxFeasible {
			problems = append(problems, fmt.Sprintf(
				"MaxBidUnits (%d) exceeds %d, which would make the %d-holder floor unreachable",
				p.MaxBidUnits, maxFeasible, p.MinDistinctHolders))
		}
	}
	if p.PriceBandLowerPaise < money.MinUnitPricePaise {
		problems = append(problems, fmt.Sprintf(
			"PriceBandLowerPaise (%d) is below the statutory minimum unit price of %d paise",
			p.PriceBandLowerPaise, money.MinUnitPricePaise))
	}
	if p.PriceBandUpperPaise < p.PriceBandLowerPaise {
		problems = append(problems, "PriceBandUpperPaise is below PriceBandLowerPaise")
	}
	if p.MinSubscriptionUnits > p.UnitsOnOffer {
		problems = append(problems, "MinSubscriptionUnits exceeds UnitsOnOffer")
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrBadParams, strings.Join(problems, "; "))
	}
	return nil
}

// Allocation is the outcome for one bid.
type Allocation struct {
	LeafIndex       uint32
	BidRef          string
	InvestorAnchor  merkle.Hash
	UnitsBid        uint32
	UnitsAllotted   uint32
	Outcome         Outcome
	Rejection       Rejection
	BallotRank      uint32
	AmountPayable   money.Paise
	RefundAmount    money.Paise
}

// Feasibility reports the three independent tests, each separately.
//
// A single boolean would be useless operationally. "Fully subscribed in rupees but
// only 51 distinct bidders" is the finding that stops a scheme, and an audience
// only understands that the regulation was read when the failure names which test
// failed and by how much.
type Feasibility struct {
	TotalBids       uint32
	EligibleBids    uint32
	TechnicalRejects uint32
	TotalUnitsBid   uint64

	SubscriptionMet     bool
	HolderFloorMet      bool
	FullSubscriptionMet bool

	Feasible bool
	Reasons  []string
}

// Result is the complete, reproducible outcome of a draw.
type Result struct {
	AlgoVersion       uint32
	FinalSeed         merkle.Hash
	BidbookRoot       merkle.Hash
	Feasibility       Feasibility
	Allocations       []Allocation // ordered by LeafIndex
	UnitsAllotted     uint32
	DistinctAllottees uint32
	ResultRoot        merkle.Hash
}

// ---------------------------------------------------------------------------
// Seed derivation
// ---------------------------------------------------------------------------

// Commitment returns the value published on-chain when the seed is committed.
//
//	commitment = sha256(secret)
//
// Deliberately a bare digest with no domain prefix, because the contract verifies it
// with a single sha256 of 32 bytes and the simplest possible rule is the easiest to
// reimplement correctly. The secret is 32 bytes of entropy, so there is nothing for a
// domain separator to protect against here.
func Commitment(secret merkle.Hash) merkle.Hash {
	return merkle.Hash(sha256.Sum256(secret[:]))
}

// DeriveFinalSeed computes the seed the draw consumes.
//
//	finalSeed = sha256(domain || secret || targetBlockHash || bidbookRoot)
//
// Each input contributes something the others cannot. The operator's committed
// secret proves the intent predated the draw. The target block hash is unknown to
// everyone at commit time, so the operator cannot choose the outcome. The bid book
// root binds the seed to one exact set of bids, so swapping a bid invalidates the
// draw rather than quietly changing it.
//
// The Solidity implementation must match this byte for byte.
func DeriveFinalSeed(secret, targetBlockHash, bidbookRoot merkle.Hash) merkle.Hash {
	buf := make([]byte, 0, len(seedDomain)+3*merkle.Size)
	buf = append(buf, seedDomain...)
	buf = append(buf, secret[:]...)
	buf = append(buf, targetBlockHash[:]...)
	buf = append(buf, bidbookRoot[:]...)
	return merkle.HashLeaf(buf)
}

// ---------------------------------------------------------------------------
// Leaf encodings
// ---------------------------------------------------------------------------

// BidLeaf computes the bid book Merkle leaf.
//
// Every field is fixed width and big-endian, so the preimage has no length
// ambiguity and no separator to get wrong. A variable-length encoding would let
// two different bids produce the same preimage.
func BidLeaf(b Bid) (merkle.Hash, error) {
	ref, err := decodeBidRef(b.BidRef)
	if err != nil {
		return merkle.Hash{}, err
	}
	if b.PricePerUnitPaise < 0 {
		return merkle.Hash{}, fmt.Errorf("%w: negative price in bid %d", ErrBadParams, b.LeafIndex)
	}

	buf := make([]byte, 0, len(bidLeafDomain)+4+BidRefLen+merkle.Size+4+8)
	buf = append(buf, bidLeafDomain...)
	buf = binary.BigEndian.AppendUint32(buf, b.LeafIndex)
	buf = append(buf, ref...)
	buf = append(buf, b.InvestorAnchor[:]...)
	buf = binary.BigEndian.AppendUint32(buf, b.UnitsBid)
	buf = binary.BigEndian.AppendUint64(buf, uint64(b.PricePerUnitPaise))
	return merkle.HashLeaf(buf), nil
}

// AllotmentLeaf computes the allotment file Merkle leaf.
func AllotmentLeaf(a Allocation) (merkle.Hash, error) {
	code, err := a.Outcome.code()
	if err != nil {
		return merkle.Hash{}, err
	}
	buf := make([]byte, 0, len(allotLeafDomain)+4+merkle.Size+4+1+4)
	buf = append(buf, allotLeafDomain...)
	buf = binary.BigEndian.AppendUint32(buf, a.LeafIndex)
	buf = append(buf, a.InvestorAnchor[:]...)
	buf = binary.BigEndian.AppendUint32(buf, a.UnitsAllotted)
	buf = append(buf, code)
	buf = binary.BigEndian.AppendUint32(buf, a.BallotRank)
	return merkle.HashLeaf(buf), nil
}

// BidbookRoot builds the frozen bid book root, which is anchored on-chain before
// the seed is committed.
func BidbookRoot(bids []Bid) (merkle.Hash, error) {
	if err := ValidateBook(bids); err != nil {
		return merkle.Hash{}, err
	}
	sorted := sortedByLeafIndex(bids)
	leaves := make([]merkle.Hash, len(sorted))
	for i, b := range sorted {
		leaf, err := BidLeaf(b)
		if err != nil {
			return merkle.Hash{}, err
		}
		leaves[i] = leaf
	}
	return merkle.RootOf(leaves)
}

// ---------------------------------------------------------------------------
// Book validation
// ---------------------------------------------------------------------------

// ValidateBook checks structural properties of the book.
//
// These are caller errors, not business outcomes. A duplicate anchor means one
// investor has two bids, which the database already prevents at submission; seeing
// it here means the book was assembled wrongly, and silently allocating against it
// would let one investor exceed the cap and double-count toward the holder floor.
func ValidateBook(bids []Bid) error {
	if len(bids) == 0 {
		return ErrEmptyBook
	}

	seenIndex := make(map[uint32]int, len(bids))
	seenAnchor := make(map[merkle.Hash]uint32, len(bids))
	seenRef := make(map[string]uint32, len(bids))

	for i, b := range bids {
		if prior, dup := seenIndex[b.LeafIndex]; dup {
			return fmt.Errorf("%w: %d appears at positions %d and %d", ErrDuplicateLeafIndex, b.LeafIndex, prior, i)
		}
		seenIndex[b.LeafIndex] = i

		if prior, dup := seenAnchor[b.InvestorAnchor]; dup {
			return fmt.Errorf("%w: leaf %d and leaf %d share an investor", ErrDuplicateAnchor, prior, b.LeafIndex)
		}
		seenAnchor[b.InvestorAnchor] = b.LeafIndex

		if _, err := decodeBidRef(b.BidRef); err != nil {
			return fmt.Errorf("leaf %d: %w", b.LeafIndex, err)
		}
		if prior, dup := seenRef[b.BidRef]; dup {
			return fmt.Errorf("ballot: duplicate bid reference at leaf %d and leaf %d", prior, b.LeafIndex)
		}
		seenRef[b.BidRef] = b.LeafIndex
	}

	// Contiguity from zero, so a verifier reconstructing the tree from the pinned
	// document cannot silently omit a leaf.
	for i := range uint32(len(bids)) {
		if _, ok := seenIndex[i]; !ok {
			return fmt.Errorf("%w: index %d is missing from a book of %d bids", ErrLeafIndexGap, i, len(bids))
		}
	}
	return nil
}

func decodeBidRef(ref string) ([]byte, error) {
	if len(ref) != 2*BidRefLen {
		return nil, fmt.Errorf("%w: got %d characters", ErrBadBidRef, len(ref))
	}
	if ref != strings.ToLower(ref) {
		return nil, fmt.Errorf("%w: contains uppercase", ErrBadBidRef)
	}
	raw, err := hex.DecodeString(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadBidRef, err)
	}
	return raw, nil
}

func sortedByLeafIndex(bids []Bid) []Bid {
	out := make([]Bid, len(bids))
	copy(out, bids)
	sort.Slice(out, func(i, j int) bool { return out[i].LeafIndex < out[j].LeafIndex })
	return out
}
