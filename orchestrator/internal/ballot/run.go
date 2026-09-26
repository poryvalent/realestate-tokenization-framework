package ballot

import (
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/acresync/orchestrator/internal/merkle"
)

// CheckFeasibility evaluates the three independent tests that decide whether an
// offer may proceed to a draw.
//
// It is separate from Run so the admin surface can report the outcome before
// anything is anchored, and so the "fully subscribed in rupees but short of the
// holder floor" case is visible as its own finding rather than as a failed draw.
func CheckFeasibility(bids []Bid, p Params) (Feasibility, error) {
	if err := p.Validate(); err != nil {
		return Feasibility{}, err
	}
	if err := ValidateBook(bids); err != nil {
		return Feasibility{}, err
	}

	f := Feasibility{TotalBids: uint32(len(bids))}

	for _, b := range bids {
		if rejectionFor(b, p) != RejectionNone {
			f.TechnicalRejects++
			continue
		}
		f.EligibleBids++
		f.TotalUnitsBid += uint64(b.UnitsBid)
	}

	f.SubscriptionMet = f.TotalUnitsBid >= uint64(p.MinSubscriptionUnits)
	f.HolderFloorMet = f.EligibleBids >= p.MinDistinctHolders
	f.FullSubscriptionMet = f.TotalUnitsBid >= uint64(p.UnitsOnOffer)

	if !f.SubscriptionMet {
		f.Reasons = append(f.Reasons, fmt.Sprintf(
			"minimum subscription not met: %d units bid against a floor of %d",
			f.TotalUnitsBid, p.MinSubscriptionUnits))
	}
	if !f.HolderFloorMet {
		f.Reasons = append(f.Reasons, fmt.Sprintf(
			"unitholder floor not met: %d eligible bidders against a floor of %d. "+
				"An SM-REIT scheme needs at least %d distinct unitholders, so this offer cannot list "+
				"even if it is fully subscribed in rupees",
			f.EligibleBids, p.MinDistinctHolders, p.MinDistinctHolders))
	}
	if p.RequireFullSubscription && !f.FullSubscriptionMet {
		f.Reasons = append(f.Reasons, fmt.Sprintf(
			"offer not fully subscribed: %d units bid against %d on offer. The scheme's asset value is "+
				"units times unit price and the statutory band floor leaves no room to shrink it, so a "+
				"shortfall must be abandoned or underwritten rather than absorbed",
			f.TotalUnitsBid, p.UnitsOnOffer))
	}

	f.Feasible = f.SubscriptionMet && f.HolderFloorMet &&
		(!p.RequireFullSubscription || f.FullSubscriptionMet)

	return f, nil
}

// rejectionFor classifies a technically invalid bid.
//
// In practice none of these fire: the database's enforce_bid_bounds trigger rejects
// such a bid at submission, so a frozen book should contain none. They are kept as
// a defensive layer because the cost is a comparison and the failure they guard
// against is an investor receiving units they never validly bid for.
func rejectionFor(b Bid, p Params) Rejection {
	switch {
	case b.UnitsBid < p.MinBidUnits:
		return RejectionBelowMinBid
	case b.UnitsBid > p.MaxBidUnits:
		return RejectionAboveMaxBid
	case b.PricePerUnitPaise < p.PriceBandLowerPaise:
		return RejectionBelowMinBid
	case b.PricePerUnitPaise > p.PriceBandUpperPaise:
		return RejectionAboveMaxBid
	}
	return RejectionNone
}

// ranked pairs a bid with its deterministic ballot position.
type ranked struct {
	bid  Bid
	key  merkle.Hash
	rank uint32
}

// rankBids assigns every bid a ballot rank derived from the final seed.
//
//	key = sha256(domain || seed || investorAnchor || leafIndex)
//
// Sorting on that key yields a permutation that is uniform under the seed and that
// anyone can spot-check one bid at a time. Ties fall back to leaf index, which
// cannot happen for distinct anchors short of a SHA-256 collision but leaves the
// ordering total rather than merely almost-total.
// RankKey derives a single bid's ballot ranking key.
//
//	key = sha256(0x00 || domain || seed || investorAnchor || leafIndex)
//
// Exported because it is the mechanism by which an investor checks their own position:
// recompute one hash, compare against the published rank. The Solidity counterpart in
// BallotEncoding.rankKey must match, and the golden vectors hold them together.
func RankKey(seed, investorAnchor merkle.Hash, leafIndex uint32) merkle.Hash {
	buf := make([]byte, 0, len(rankDomain)+2*merkle.Size+4)
	buf = append(buf, rankDomain...)
	buf = append(buf, seed[:]...)
	buf = append(buf, investorAnchor[:]...)
	buf = binary.BigEndian.AppendUint32(buf, leafIndex)
	return merkle.HashLeaf(buf)
}

func rankBids(bids []Bid, seed merkle.Hash) []ranked {
	out := make([]ranked, len(bids))
	for i, b := range bids {
		out[i] = ranked{bid: b, key: RankKey(seed, b.InvestorAnchor, b.LeafIndex)}
	}

	sort.Slice(out, func(i, j int) bool {
		if c := compareHash(out[i].key, out[j].key); c != 0 {
			return c < 0
		}
		return out[i].bid.LeafIndex < out[j].bid.LeafIndex
	})

	for i := range out {
		out[i].rank = uint32(i)
	}
	return out
}

func compareHash(a, b merkle.Hash) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// Run performs the draw.
//
// The allocation proceeds in two stages, and the order is the point.
//
// Stage one seats the holder floor. Every selected bidder receives exactly one
// unit before anyone receives a second. That is what makes the 200-holder
// requirement a structural outcome rather than something the arithmetic might
// happen to satisfy: if there are more eligible bidders than units, a ballot picks
// exactly UnitsOnOffer of them and each gets one unit, and since UnitsOnOffer is
// necessarily at least MinDistinctHolders the floor is met by construction.
//
// Stage two distributes what remains in proportion to residual demand, using
// largest-remainder so the total lands exactly on UnitsOnOffer, and water-filling
// so that a bidder is never allotted more than they bid.
func Run(bids []Bid, p Params, finalSeed merkle.Hash) (*Result, error) {
	if finalSeed.IsZero() {
		return nil, ErrZeroSeed
	}

	feas, err := CheckFeasibility(bids, p)
	if err != nil {
		return nil, err
	}
	if !feas.Feasible {
		return nil, fmt.Errorf("%w: %v", ErrInfeasible, feas.Reasons)
	}

	bidbookRoot, err := BidbookRoot(bids)
	if err != nil {
		return nil, err
	}

	ranks := rankBids(bids, finalSeed)

	// Allocation state, keyed by leaf index.
	allotted := make(map[uint32]uint32, len(bids))
	rankOf := make(map[uint32]uint32, len(bids))
	eligible := make([]ranked, 0, len(bids))

	for _, r := range ranks {
		rankOf[r.bid.LeafIndex] = r.rank
		if rejectionFor(r.bid, p) == RejectionNone {
			eligible = append(eligible, r)
		}
	}

	// ----- Stage one: one unit each, in rank order -----

	seats := uint32(len(eligible))
	if seats > p.UnitsOnOffer {
		// More eligible bidders than indivisible units. A ballot selects exactly
		// UnitsOnOffer of them; the rest are NIL_BALLOT. This is the case that makes
		// the whole mechanism a ballot rather than a calculation.
		seats = p.UnitsOnOffer
	}
	if seats < p.MinDistinctHolders {
		return nil, fmt.Errorf("%w: only %d seats available against a holder floor of %d",
			ErrInternal, seats, p.MinDistinctHolders)
	}

	selected := eligible[:seats]
	for _, r := range selected {
		allotted[r.bid.LeafIndex] = 1
	}
	remaining := p.UnitsOnOffer - seats

	// ----- Stage two: proportional distribution of what is left -----

	if remaining > 0 {
		if err := distributeRemainder(selected, allotted, remaining); err != nil {
			return nil, err
		}
	}

	// ----- Assemble, in leaf order -----

	sorted := sortedByLeafIndex(bids)
	allocations := make([]Allocation, 0, len(sorted))
	var totalAllotted uint32
	var distinct uint32

	for _, b := range sorted {
		units := allotted[b.LeafIndex]
		rej := rejectionFor(b, p)

		var outcome Outcome
		switch {
		case rej != RejectionNone:
			outcome = OutcomeNilTechnical
		case units == 0:
			outcome = OutcomeNilBallot
			rej = RejectionBallot
		case units == b.UnitsBid:
			outcome = OutcomeFull
		default:
			outcome = OutcomePartial
		}

		// price * units, routed through MulDivFloor with a denominator of one so the
		// product is computed in arbitrary precision and an overflow is an error
		// rather than a wrap. At ₹10 lakh a unit these values are large enough that
		// silent wrapping would be a plausible failure.
		payable, _, err := b.PricePerUnitPaise.MulDivFloor(int64(units), 1)
		if err != nil {
			return nil, fmt.Errorf("ballot: computing payable for leaf %d: %w", b.LeafIndex, err)
		}
		bidTotal, _, err := b.PricePerUnitPaise.MulDivFloor(int64(b.UnitsBid), 1)
		if err != nil {
			return nil, fmt.Errorf("ballot: computing bid total for leaf %d: %w", b.LeafIndex, err)
		}
		refund, err := bidTotal.Sub(payable)
		if err != nil {
			return nil, fmt.Errorf("ballot: computing refund for leaf %d: %w", b.LeafIndex, err)
		}

		allocations = append(allocations, Allocation{
			LeafIndex:      b.LeafIndex,
			BidRef:         b.BidRef,
			InvestorAnchor: b.InvestorAnchor,
			UnitsBid:       b.UnitsBid,
			UnitsAllotted:  units,
			Outcome:        outcome,
			Rejection:      rej,
			BallotRank:     rankOf[b.LeafIndex],
			AmountPayable:  payable,
			RefundAmount:   refund,
		})

		totalAllotted += units
		if units > 0 {
			distinct++
		}
	}

	// ----- Hard invariants -----
	//
	// These are the conditions the on-chain finaliseSettlement will check. Failing
	// here means the algorithm is wrong, so it is an error rather than a result.

	if totalAllotted != p.UnitsOnOffer {
		return nil, fmt.Errorf("%w: allotted %d units, expected exactly %d",
			ErrInternal, totalAllotted, p.UnitsOnOffer)
	}
	if distinct < p.MinDistinctHolders {
		return nil, fmt.Errorf("%w: %d distinct allottees against a floor of %d",
			ErrInternal, distinct, p.MinDistinctHolders)
	}
	for _, a := range allocations {
		if a.UnitsAllotted > a.UnitsBid {
			return nil, fmt.Errorf("%w: leaf %d allotted %d units but bid only %d",
				ErrInternal, a.LeafIndex, a.UnitsAllotted, a.UnitsBid)
		}
		if a.UnitsAllotted > p.MaxBidUnits {
			return nil, fmt.Errorf("%w: leaf %d allotted %d units, above the cap of %d",
				ErrInternal, a.LeafIndex, a.UnitsAllotted, p.MaxBidUnits)
		}
	}

	resultRoot, err := allotmentRoot(allocations)
	if err != nil {
		return nil, err
	}

	return &Result{
		AlgoVersion:       AlgoVersion,
		FinalSeed:         finalSeed,
		BidbookRoot:       bidbookRoot,
		Feasibility:       feas,
		Allocations:       allocations,
		UnitsAllotted:     totalAllotted,
		DistinctAllottees: distinct,
		ResultRoot:        resultRoot,
	}, nil
}

// distributeRemainder allocates `remaining` units across selected bidders in
// proportion to residual demand.
//
// Residual demand is UnitsBid minus the one unit already seated. The loop is a
// water-filling pass because a proportional share can exceed what a bidder asked
// for: capping them frees units that must then be redistributed among those still
// short. Without the loop, the total would land below UnitsOnOffer whenever any
// bidder's proportional share hit their ceiling.
func distributeRemainder(selected []ranked, allotted map[uint32]uint32, remaining uint32) error {
	type slot struct {
		leafIndex uint32
		rank      uint32
		headroom  uint32
	}

	slots := make([]slot, 0, len(selected))
	for _, r := range selected {
		slots = append(slots, slot{
			leafIndex: r.bid.LeafIndex,
			rank:      r.rank,
			headroom:  r.bid.UnitsBid - allotted[r.bid.LeafIndex],
		})
	}

	for remaining > 0 {
		var totalHeadroom uint64
		active := make([]int, 0, len(slots))
		for i := range slots {
			if slots[i].headroom > 0 {
				active = append(active, i)
				totalHeadroom += uint64(slots[i].headroom)
			}
		}
		if len(active) == 0 {
			// Feasibility guaranteed enough demand, so exhausting headroom with units
			// still to place means the capacity check and this loop disagree.
			return fmt.Errorf("%w: %d units left to allot but no bidder has headroom",
				ErrInternal, remaining)
		}

		// Enough headroom for everyone to be filled completely.
		if uint64(remaining) >= totalHeadroom {
			for _, i := range active {
				allotted[slots[i].leafIndex] += slots[i].headroom
				remaining -= slots[i].headroom
				slots[i].headroom = 0
			}
			continue
		}

		// Proportional base. Because remaining < totalHeadroom, each base share is
		// strictly below that bidder's headroom, so every active bidder keeps at
		// least one unit of headroom and the largest-remainder pass below can always
		// place its units.
		before := uint64(remaining)
		type rem struct {
			idx       int
			remainder uint64
			rank      uint32
		}
		remainders := make([]rem, 0, len(active))

		for _, i := range active {
			h := uint64(slots[i].headroom)
			base := before * h / totalHeadroom
			if base > uint64(slots[i].headroom) {
				return fmt.Errorf("%w: proportional base %d exceeds headroom %d", ErrInternal, base, slots[i].headroom)
			}
			allotted[slots[i].leafIndex] += uint32(base)
			slots[i].headroom -= uint32(base)
			remaining -= uint32(base)
			remainders = append(remainders, rem{
				idx:       i,
				remainder: (before * h) % totalHeadroom,
				rank:      slots[i].rank,
			})
		}

		// Largest remainder, ties broken by ballot rank so the result is reproducible
		// from published data rather than dependent on slice order.
		sort.Slice(remainders, func(a, b int) bool {
			if remainders[a].remainder != remainders[b].remainder {
				return remainders[a].remainder > remainders[b].remainder
			}
			return remainders[a].rank < remainders[b].rank
		})

		for _, r := range remainders {
			if remaining == 0 {
				break
			}
			if slots[r.idx].headroom == 0 {
				continue
			}
			allotted[slots[r.idx].leafIndex]++
			slots[r.idx].headroom--
			remaining--
		}
	}
	return nil
}

func allotmentRoot(allocations []Allocation) (merkle.Hash, error) {
	leaves := make([]merkle.Hash, len(allocations))
	for i, a := range allocations {
		leaf, err := AllotmentLeaf(a)
		if err != nil {
			return merkle.Hash{}, err
		}
		leaves[i] = leaf
	}
	return merkle.RootOf(leaves)
}
