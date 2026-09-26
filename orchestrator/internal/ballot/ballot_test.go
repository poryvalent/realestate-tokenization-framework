package ballot

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func testAnchor(i int) merkle.Hash {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(i)+0x9e3779b97f4a7c15)
	return merkle.Hash(sha256.Sum256(b[:]))
}

func testBidRef(i int) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(i))
	sum := sha256.Sum256(b[:])
	return hex.EncodeToString(sum[:BidRefLen])
}

// book builds a bid book where bidder i bids units[i].
func book(units []uint32) []Bid {
	out := make([]Bid, len(units))
	for i, u := range units {
		out[i] = Bid{
			LeafIndex:         uint32(i),
			BidRef:            testBidRef(i),
			InvestorAnchor:    testAnchor(i),
			UnitsBid:          u,
			PricePerUnitPaise: money.MinUnitPricePaise,
		}
	}
	return out
}

func uniform(n int, each uint32) []Bid {
	units := make([]uint32, n)
	for i := range units {
		units[i] = each
	}
	return book(units)
}

func seed(n byte) merkle.Hash {
	var h merkle.Hash
	for i := range h {
		h[i] = n
	}
	return h
}

// assertInvariants checks every property the on-chain finaliseSettlement will
// later check, plus the per-bid bounds.
func assertInvariants(t *testing.T, r *Result, bids []Bid, p Params) {
	t.Helper()

	if r.UnitsAllotted != p.UnitsOnOffer {
		t.Errorf("allotted %d units, must be exactly %d", r.UnitsAllotted, p.UnitsOnOffer)
	}
	if r.DistinctAllottees < p.MinDistinctHolders {
		t.Errorf("%d distinct allottees, below the floor of %d", r.DistinctAllottees, p.MinDistinctHolders)
	}
	if len(r.Allocations) != len(bids) {
		t.Fatalf("got %d allocations for %d bids", len(r.Allocations), len(bids))
	}

	bidByLeaf := map[uint32]Bid{}
	for _, b := range bids {
		bidByLeaf[b.LeafIndex] = b
	}

	var sum uint32
	var distinct uint32
	seenRank := map[uint32]bool{}

	for i, a := range r.Allocations {
		if i > 0 && r.Allocations[i-1].LeafIndex >= a.LeafIndex {
			t.Fatalf("allocations must be ordered by leaf index; %d follows %d",
				a.LeafIndex, r.Allocations[i-1].LeafIndex)
		}
		b := bidByLeaf[a.LeafIndex]

		if a.UnitsAllotted > b.UnitsBid {
			t.Errorf("leaf %d allotted %d but bid only %d", a.LeafIndex, a.UnitsAllotted, b.UnitsBid)
		}
		if a.UnitsAllotted > p.MaxBidUnits {
			t.Errorf("leaf %d allotted %d, above cap %d", a.LeafIndex, a.UnitsAllotted, p.MaxBidUnits)
		}
		if seenRank[a.BallotRank] {
			t.Errorf("ballot rank %d assigned twice", a.BallotRank)
		}
		seenRank[a.BallotRank] = true

		// Outcome must agree with the units.
		switch a.Outcome {
		case OutcomeFull:
			if a.UnitsAllotted != b.UnitsBid {
				t.Errorf("leaf %d marked FULL but allotted %d of %d", a.LeafIndex, a.UnitsAllotted, b.UnitsBid)
			}
		case OutcomePartial:
			if a.UnitsAllotted == 0 || a.UnitsAllotted >= b.UnitsBid {
				t.Errorf("leaf %d marked PARTIAL but allotted %d of %d", a.LeafIndex, a.UnitsAllotted, b.UnitsBid)
			}
		case OutcomeNilBallot, OutcomeNilTechnical:
			if a.UnitsAllotted != 0 {
				t.Errorf("leaf %d marked %s but allotted %d", a.LeafIndex, a.Outcome, a.UnitsAllotted)
			}
		default:
			t.Errorf("leaf %d has unknown outcome %q", a.LeafIndex, a.Outcome)
		}

		// Money must reconcile: what is taken plus what is returned equals what was
		// blocked under ASBA.
		bidTotal, _, err := b.PricePerUnitPaise.MulDivFloor(int64(b.UnitsBid), 1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.AmountPayable.Add(a.RefundAmount)
		if err != nil {
			t.Fatal(err)
		}
		if got != bidTotal {
			t.Errorf("leaf %d: payable %d + refund %d = %d, but the bid blocked %d",
				a.LeafIndex, a.AmountPayable, a.RefundAmount, got, bidTotal)
		}

		sum += a.UnitsAllotted
		if a.UnitsAllotted > 0 {
			distinct++
		}
	}

	if sum != r.UnitsAllotted {
		t.Errorf("per-allocation sum %d disagrees with reported total %d", sum, r.UnitsAllotted)
	}
	if distinct != r.DistinctAllottees {
		t.Errorf("per-allocation distinct count %d disagrees with reported %d", distinct, r.DistinctAllottees)
	}
	if r.ResultRoot.IsZero() {
		t.Error("result root is unset")
	}
	if r.AlgoVersion != AlgoVersion {
		t.Errorf("algo version %d, want %d", r.AlgoVersion, AlgoVersion)
	}
}

// ---------------------------------------------------------------------------
// The headline case: more bidders than indivisible units
// ---------------------------------------------------------------------------

// 900 investors each want one unit. There are 475. No proportional answer exists,
// which is precisely why this is a ballot.
func TestMoreBiddersThanUnits(t *testing.T) {
	p := V1Params()
	bids := uniform(900, 1)

	r, err := Run(bids, p, seed(0xa1))
	if err != nil {
		t.Fatal(err)
	}
	assertInvariants(t, r, bids, p)

	if r.DistinctAllottees != 475 {
		t.Errorf("expected exactly 475 winners of one unit each, got %d", r.DistinctAllottees)
	}

	var won, lost int
	for _, a := range r.Allocations {
		switch a.Outcome {
		case OutcomeFull:
			won++
			if a.UnitsAllotted != 1 {
				t.Errorf("leaf %d won %d units, expected 1", a.LeafIndex, a.UnitsAllotted)
			}
		case OutcomeNilBallot:
			lost++
			if a.Rejection != RejectionBallot {
				t.Errorf("leaf %d lost the ballot but reason is %q", a.LeafIndex, a.Rejection)
			}
		default:
			t.Errorf("unexpected outcome %s", a.Outcome)
		}
	}
	if won != 475 || lost != 425 {
		t.Errorf("got %d winners and %d losers, want 475 and 425", won, lost)
	}
}

// Winners must be selected strictly in ballot-rank order, so the draw is checkable
// bid by bid rather than only in aggregate.
func TestWinnersAreTheTopRanks(t *testing.T) {
	p := V1Params()
	bids := uniform(900, 1)

	r, err := Run(bids, p, seed(0x5c))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Allocations {
		expectWin := a.BallotRank < p.UnitsOnOffer
		didWin := a.UnitsAllotted > 0
		if expectWin != didWin {
			t.Fatalf("leaf %d rank %d: won=%v, expected %v", a.LeafIndex, a.BallotRank, didWin, expectWin)
		}
	}
}

// Fewer bidders than units: everyone is seated, then the surplus is shared out.
func TestFewerBiddersThanUnits(t *testing.T) {
	p := V1Params()
	bids := uniform(300, 5) // 1500 units demanded, 475 available

	r, err := Run(bids, p, seed(0xb2))
	if err != nil {
		t.Fatal(err)
	}
	assertInvariants(t, r, bids, p)

	if r.DistinctAllottees != 300 {
		t.Errorf("every eligible bidder should be seated, got %d of 300", r.DistinctAllottees)
	}
	for _, a := range r.Allocations {
		if a.UnitsAllotted < 1 {
			t.Fatalf("leaf %d got nothing despite there being room to seat everyone", a.LeafIndex)
		}
	}

	// 300 seats plus 175 surplus: 175 bidders reach two units, 125 stay at one.
	counts := map[uint32]int{}
	for _, a := range r.Allocations {
		counts[a.UnitsAllotted]++
	}
	if counts[2] != 175 || counts[1] != 125 {
		t.Errorf("unit distribution = %v, want 175 holders on 2 units and 125 on 1", counts)
	}
}

// Exactly as many bidders as units is the boundary between the two branches.
func TestExactlyAsManyBiddersAsUnits(t *testing.T) {
	p := V1Params()
	bids := uniform(475, 1)

	r, err := Run(bids, p, seed(0xc3))
	if err != nil {
		t.Fatal(err)
	}
	assertInvariants(t, r, bids, p)

	if r.DistinctAllottees != 475 {
		t.Errorf("all 475 bidders should be seated, got %d", r.DistinctAllottees)
	}
	for _, a := range r.Allocations {
		if a.Outcome != OutcomeFull {
			t.Fatalf("leaf %d should be FULL, got %s", a.LeafIndex, a.Outcome)
		}
	}
}

// Water-filling: when a proportional share would exceed what someone asked for,
// they are capped and the freed units must be redistributed rather than lost.
func TestWaterFillingRedistributesCappedUnits(t *testing.T) {
	p := V1Params()

	// 200 bidders. Most want a single unit so they cap out immediately; a handful
	// want the maximum and must absorb everything that frees up.
	units := make([]uint32, 200)
	for i := range units {
		units[i] = 1
	}
	for i := range 20 {
		units[i] = 25
	}
	// Demand: 20*25 + 180*1 = 680, comfortably above 475.
	bids := book(units)

	r, err := Run(bids, p, seed(0xd4))
	if err != nil {
		t.Fatal(err)
	}
	assertInvariants(t, r, bids, p)

	if r.DistinctAllottees != 200 {
		t.Errorf("all 200 bidders should be seated, got %d", r.DistinctAllottees)
	}
	// Anyone who asked for one unit must have exactly one, never more.
	for _, a := range r.Allocations {
		if a.UnitsBid == 1 && a.UnitsAllotted != 1 {
			t.Fatalf("leaf %d bid 1 unit but was allotted %d", a.LeafIndex, a.UnitsAllotted)
		}
	}
}

// ---------------------------------------------------------------------------
// Determinism
// ---------------------------------------------------------------------------

// The property the entire trust model rests on: an investor recomputing the draw
// from the pinned bid book and the revealed seed must get the same answer we did.
func TestDeterministic(t *testing.T) {
	p := V1Params()
	bids := uniform(600, 3)
	s := seed(0xe5)

	first, err := Run(bids, p, s)
	if err != nil {
		t.Fatal(err)
	}

	for iter := range 100 {
		again, err := Run(bids, p, s)
		if err != nil {
			t.Fatal(err)
		}
		if again.ResultRoot != first.ResultRoot {
			t.Fatalf("iteration %d: result root changed, %s vs %s", iter, again.ResultRoot, first.ResultRoot)
		}
		for i := range first.Allocations {
			if again.Allocations[i] != first.Allocations[i] {
				t.Fatalf("iteration %d: allocation %d differs\n got %+v\nwant %+v",
					iter, i, again.Allocations[i], first.Allocations[i])
			}
		}
	}
}

// Input order must not matter. The book arrives from a database query whose row
// order is not guaranteed, so a result that depended on it would be irreproducible.
func TestDeterministicAcrossBookOrder(t *testing.T) {
	p := V1Params()
	bids := uniform(500, 4)
	s := seed(0xf6)

	reference, err := Run(bids, p, s)
	if err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewPCG(3, 5))
	for range 50 {
		shuffled := make([]Bid, len(bids))
		copy(shuffled, bids)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		got, err := Run(shuffled, p, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.ResultRoot != reference.ResultRoot {
			t.Fatal("result depends on the order bids arrive in")
		}
	}
}

// A different seed must produce a different draw. If it did not, the commit-reveal
// ceremony would be decorative.
func TestSeedChangesTheDraw(t *testing.T) {
	p := V1Params()
	bids := uniform(900, 1)

	a, err := Run(bids, p, seed(0x01))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(bids, p, seed(0x02))
	if err != nil {
		t.Fatal(err)
	}

	if a.ResultRoot == b.ResultRoot {
		t.Fatal("two different seeds produced the same allotment")
	}
	// Totals are structural and must not move.
	if a.UnitsAllotted != b.UnitsAllotted || a.DistinctAllottees != b.DistinctAllottees {
		t.Fatal("the seed must change who wins, never how many units or holders there are")
	}

	differences := 0
	for i := range a.Allocations {
		if a.Allocations[i].UnitsAllotted != b.Allocations[i].UnitsAllotted {
			differences++
		}
	}
	if differences == 0 {
		t.Fatal("no bid changed outcome between seeds")
	}
	t.Logf("%d of %d bids changed outcome between seeds", differences, len(a.Allocations))
}

func TestZeroSeedRejected(t *testing.T) {
	if _, err := Run(uniform(500, 2), V1Params(), merkle.Hash{}); !errors.Is(err, ErrZeroSeed) {
		t.Fatalf("want ErrZeroSeed, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Feasibility, reported test by test
// ---------------------------------------------------------------------------

// The showpiece case. Fifty investors bid ten units each: 500 units, so the offer
// is oversubscribed in rupees and still cannot list, because an SM-REIT scheme needs
// 200 distinct unitholders.
func TestFullySubscribedButHolderFloorFails(t *testing.T) {
	p := V1Params()
	bids := uniform(50, 10)

	f, err := CheckFeasibility(bids, p)
	if err != nil {
		t.Fatal(err)
	}

	if !f.SubscriptionMet {
		t.Error("500 units bid should satisfy the minimum subscription of 428")
	}
	if !f.FullSubscriptionMet {
		t.Error("500 units bid should satisfy full subscription of 475")
	}
	if f.HolderFloorMet {
		t.Error("50 bidders must not satisfy a 200-holder floor")
	}
	if f.Feasible {
		t.Fatal("an offer that cannot reach 200 unitholders is not feasible however well subscribed it is")
	}
	if len(f.Reasons) != 1 || !strings.Contains(f.Reasons[0], "unitholder floor") {
		t.Errorf("expected exactly the holder-floor reason, got %v", f.Reasons)
	}

	if _, err := Run(bids, p, seed(0x11)); !errors.Is(err, ErrInfeasible) {
		t.Fatalf("Run must refuse an infeasible offer, got %v", err)
	}
}

func TestUndersubscribedFails(t *testing.T) {
	p := V1Params()
	// 300 bidders, one unit each: 300 units against 475 on offer and a 428 floor.
	bids := uniform(300, 1)

	f, err := CheckFeasibility(bids, p)
	if err != nil {
		t.Fatal(err)
	}
	if f.SubscriptionMet {
		t.Error("300 units must not satisfy a 428-unit minimum subscription")
	}
	if f.FullSubscriptionMet {
		t.Error("300 units must not satisfy full subscription")
	}
	if !f.HolderFloorMet {
		t.Error("300 bidders should satisfy the 200-holder floor")
	}
	if f.Feasible {
		t.Fatal("an undersubscribed offer is not feasible")
	}
}

// Above the minimum subscription but short of the full offer. This cannot proceed
// because the scheme's asset value is units times price and the band floor leaves no
// room to shrink it.
func TestPartiallySubscribedFailsOnFullSubscription(t *testing.T) {
	p := V1Params()
	// 450 bidders, one unit each: past the 428 floor, short of 475.
	bids := uniform(450, 1)

	f, err := CheckFeasibility(bids, p)
	if err != nil {
		t.Fatal(err)
	}
	if !f.SubscriptionMet {
		t.Error("450 units should clear the 428 minimum subscription")
	}
	if !f.HolderFloorMet {
		t.Error("450 bidders should clear the holder floor")
	}
	if f.FullSubscriptionMet {
		t.Error("450 units must not count as full subscription of 475")
	}
	if f.Feasible {
		t.Fatal("a partially subscribed offer must not proceed while full subscription is required")
	}

	// With the requirement relaxed it becomes feasible, which documents that the
	// constraint is a policy decision and not an accident of the algorithm.
	relaxed := p
	relaxed.RequireFullSubscription = false
	f2, err := CheckFeasibility(bids, relaxed)
	if err != nil {
		t.Fatal(err)
	}
	if !f2.Feasible {
		t.Fatalf("with full subscription not required this should be feasible: %v", f2.Reasons)
	}
}

func TestFeasibilityCountsAreAccurate(t *testing.T) {
	p := V1Params()
	bids := uniform(600, 2)

	f, err := CheckFeasibility(bids, p)
	if err != nil {
		t.Fatal(err)
	}
	if f.TotalBids != 600 || f.EligibleBids != 600 || f.TechnicalRejects != 0 {
		t.Errorf("counts = total %d eligible %d rejects %d", f.TotalBids, f.EligibleBids, f.TechnicalRejects)
	}
	if f.TotalUnitsBid != 1200 {
		t.Errorf("total units bid = %d, want 1200", f.TotalUnitsBid)
	}
	if !f.Feasible {
		t.Errorf("should be feasible: %v", f.Reasons)
	}
}

// ---------------------------------------------------------------------------
// Technical rejections
// ---------------------------------------------------------------------------

// The database rejects out-of-bounds bids at submission, so these should never
// appear in a frozen book. The defensive layer still has to work, because the
// failure it guards against is an investor receiving units they never validly bid
// for.
func TestTechnicalRejections(t *testing.T) {
	p := V1Params()
	bids := uniform(500, 2)

	bids[0].UnitsBid = 26                        // above the cap
	bids[1].UnitsBid = 0                         // below the minimum
	bids[2].PricePerUnitPaise = 99_999_999       // below the band
	bids[3].PricePerUnitPaise = 106_000_000      // above the band

	r, err := Run(bids, p, seed(0x22))
	if err != nil {
		t.Fatal(err)
	}
	assertInvariants(t, r, bids, p)

	byLeaf := map[uint32]Allocation{}
	for _, a := range r.Allocations {
		byLeaf[a.LeafIndex] = a
	}
	for _, leaf := range []uint32{0, 1, 2, 3} {
		a := byLeaf[leaf]
		if a.Outcome != OutcomeNilTechnical {
			t.Errorf("leaf %d should be NIL_TECHNICAL, got %s", leaf, a.Outcome)
		}
		if a.UnitsAllotted != 0 {
			t.Errorf("leaf %d technically rejected but allotted %d units", leaf, a.UnitsAllotted)
		}
		if a.Rejection == RejectionNone {
			t.Errorf("leaf %d technically rejected with no reason recorded", leaf)
		}
	}
	if r.Feasibility.TechnicalRejects != 4 {
		t.Errorf("technical rejects = %d, want 4", r.Feasibility.TechnicalRejects)
	}
}

// ---------------------------------------------------------------------------
// Book and parameter validation
// ---------------------------------------------------------------------------

func TestBookValidation(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if err := ValidateBook(nil); !errors.Is(err, ErrEmptyBook) {
			t.Fatalf("want ErrEmptyBook, got %v", err)
		}
	})

	t.Run("duplicate leaf index", func(t *testing.T) {
		bids := uniform(4, 1)
		bids[2].LeafIndex = 1
		if err := ValidateBook(bids); !errors.Is(err, ErrDuplicateLeafIndex) {
			t.Fatalf("want ErrDuplicateLeafIndex, got %v", err)
		}
	})

	// One investor with two bids could exceed the cap and be counted twice toward
	// the holder floor.
	t.Run("duplicate anchor", func(t *testing.T) {
		bids := uniform(4, 1)
		bids[2].InvestorAnchor = bids[0].InvestorAnchor
		if err := ValidateBook(bids); !errors.Is(err, ErrDuplicateAnchor) {
			t.Fatalf("want ErrDuplicateAnchor, got %v", err)
		}
	})

	t.Run("leaf index gap", func(t *testing.T) {
		bids := uniform(4, 1)
		bids[3].LeafIndex = 99
		if err := ValidateBook(bids); !errors.Is(err, ErrLeafIndexGap) {
			t.Fatalf("want ErrLeafIndexGap, got %v", err)
		}
	})

	t.Run("bad bid reference", func(t *testing.T) {
		for _, ref := range []string{"", "abc", strings.ToUpper(testBidRef(0)), strings.Repeat("z", 32)} {
			bids := uniform(2, 1)
			bids[0].BidRef = ref
			if err := ValidateBook(bids); !errors.Is(err, ErrBadBidRef) {
				t.Errorf("ref %q: want ErrBadBidRef, got %v", ref, err)
			}
		}
	})
}

func TestParamsValidation(t *testing.T) {
	t.Run("v1 params are valid", func(t *testing.T) {
		if err := V1Params().Validate(); err != nil {
			t.Fatal(err)
		}
	})

	// The binding constraint from Phase 1: with 475 units and a 200-holder floor,
	// one bidder can take at most 276 and still leave a unit for 199 others.
	t.Run("cap that breaks the holder floor", func(t *testing.T) {
		p := V1Params()
		p.MaxBidUnits = 277
		err := p.Validate()
		if !errors.Is(err, ErrBadParams) {
			t.Fatalf("want ErrBadParams, got %v", err)
		}
		if !strings.Contains(err.Error(), "unreachable") {
			t.Errorf("error should explain the holder floor becomes unreachable: %v", err)
		}
	})

	t.Run("cap at the exact boundary is allowed", func(t *testing.T) {
		p := V1Params()
		p.MaxBidUnits = 276
		if err := p.Validate(); err != nil {
			t.Fatalf("276 is exactly feasible and must be permitted: %v", err)
		}
	})

	// Guards the uint32 underflow that would otherwise turn a hard failure into a
	// silently permissive ceiling.
	t.Run("holder floor above units on offer", func(t *testing.T) {
		p := V1Params()
		p.UnitsOnOffer = 100
		p.MinDistinctHolders = 200
		err := p.Validate()
		if !errors.Is(err, ErrBadParams) {
			t.Fatalf("want ErrBadParams, got %v", err)
		}
		if !strings.Contains(err.Error(), "exceeds UnitsOnOffer") {
			t.Errorf("expected the holder-floor message, got %v", err)
		}
	})

	t.Run("price band below statutory minimum", func(t *testing.T) {
		p := V1Params()
		p.PriceBandLowerPaise = money.MinUnitPricePaise - 1
		if err := p.Validate(); !errors.Is(err, ErrBadParams) {
			t.Fatalf("want ErrBadParams, got %v", err)
		}
	})

	t.Run("zero minimum bid", func(t *testing.T) {
		p := V1Params()
		p.MinBidUnits = 0
		if err := p.Validate(); !errors.Is(err, ErrBadParams) {
			t.Fatalf("units are indivisible so a zero minimum is invalid, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Seed derivation
// ---------------------------------------------------------------------------

func TestDeriveFinalSeed(t *testing.T) {
	secret := seed(0x11)
	blockHash := seed(0x22)
	root := seed(0x33)

	base := DeriveFinalSeed(secret, blockHash, root)
	if base.IsZero() {
		t.Fatal("derived seed is zero")
	}
	if DeriveFinalSeed(secret, blockHash, root) != base {
		t.Fatal("derivation is not deterministic")
	}

	// Every input must matter. The secret proves intent predated the draw, the block
	// hash removes operator choice, and the bid book root binds the draw to one exact
	// set of bids.
	if DeriveFinalSeed(seed(0x12), blockHash, root) == base {
		t.Error("changing the secret must change the seed")
	}
	if DeriveFinalSeed(secret, seed(0x23), root) == base {
		t.Error("changing the target block hash must change the seed")
	}
	if DeriveFinalSeed(secret, blockHash, seed(0x34)) == base {
		t.Error("changing the bid book root must change the seed: the draw is bound to one book")
	}

	// Argument positions must not be interchangeable.
	if DeriveFinalSeed(blockHash, secret, root) == base {
		t.Error("swapping secret and block hash must change the seed")
	}
}

// Swapping one bid invalidates the draw rather than quietly changing it, because the
// bid book root feeds the seed.
func TestSwappedBidInvalidatesTheSeed(t *testing.T) {
	bids := uniform(500, 2)
	rootA, err := BidbookRoot(bids)
	if err != nil {
		t.Fatal(err)
	}

	tampered := make([]Bid, len(bids))
	copy(tampered, bids)
	tampered[42].UnitsBid = 3
	rootB, err := BidbookRoot(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if rootA == rootB {
		t.Fatal("changing a bid must change the bid book root")
	}

	secret, blockHash := seed(0x44), seed(0x55)
	if DeriveFinalSeed(secret, blockHash, rootA) == DeriveFinalSeed(secret, blockHash, rootB) {
		t.Fatal("a tampered book must produce a different final seed")
	}
}

// ---------------------------------------------------------------------------
// Leaf encodings
// ---------------------------------------------------------------------------

func TestBidLeafIsFieldSensitive(t *testing.T) {
	b := Bid{
		LeafIndex:         7,
		BidRef:            testBidRef(7),
		InvestorAnchor:    testAnchor(7),
		UnitsBid:          3,
		PricePerUnitPaise: money.MinUnitPricePaise,
	}
	base, err := BidLeaf(b)
	if err != nil {
		t.Fatal(err)
	}

	variants := map[string]func(*Bid){
		"leaf index": func(x *Bid) { x.LeafIndex = 8 },
		"bid ref":    func(x *Bid) { x.BidRef = testBidRef(8) },
		"anchor":     func(x *Bid) { x.InvestorAnchor = testAnchor(8) },
		"units":      func(x *Bid) { x.UnitsBid = 4 },
		"price":      func(x *Bid) { x.PricePerUnitPaise = money.MinUnitPricePaise + 1 },
	}
	for name, mutate := range variants {
		v := b
		mutate(&v)
		got, err := BidLeaf(v)
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Errorf("changing the %s did not change the leaf", name)
		}
	}
}

func TestAllotmentLeafIsFieldSensitive(t *testing.T) {
	a := Allocation{
		LeafIndex:      7,
		InvestorAnchor: testAnchor(7),
		UnitsAllotted:  2,
		Outcome:        OutcomePartial,
		BallotRank:     19,
	}
	base, err := AllotmentLeaf(a)
	if err != nil {
		t.Fatal(err)
	}

	variants := map[string]func(*Allocation){
		"leaf index": func(x *Allocation) { x.LeafIndex = 8 },
		"anchor":     func(x *Allocation) { x.InvestorAnchor = testAnchor(8) },
		"units":      func(x *Allocation) { x.UnitsAllotted = 3 },
		"outcome":    func(x *Allocation) { x.Outcome = OutcomeFull },
		"rank":       func(x *Allocation) { x.BallotRank = 20 },
	}
	for name, mutate := range variants {
		v := a
		mutate(&v)
		got, err := AllotmentLeaf(v)
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Errorf("changing the %s did not change the leaf", name)
		}
	}
}

// Outcome codes are part of the leaf preimage and must match the Solidity enum
// ordering, so they cannot be reordered without invalidating anchored roots.
func TestOutcomeCodesAreStable(t *testing.T) {
	want := map[Outcome]byte{
		OutcomeFull:         0,
		OutcomePartial:      1,
		OutcomeNilBallot:    2,
		OutcomeNilTechnical: 3,
	}
	for o, code := range want {
		got, err := o.code()
		if err != nil {
			t.Fatal(err)
		}
		if got != code {
			t.Errorf("%s has code %d, want %d. These codes are part of the allotment leaf "+
				"preimage and must match the Solidity enum order.", o, got, code)
		}
	}
}

// ---------------------------------------------------------------------------
// Randomised property test
// ---------------------------------------------------------------------------

// Generated books across a wide range of shapes. The combinations that break a
// constrained allocation are hard to guess and easy to generate.
func TestRandomisedProperties(t *testing.T) {
	p := V1Params()
	// Fixed seed so a failure is reproducible rather than a story about last Tuesday.
	rng := rand.New(rand.NewPCG(0xacc1_5e7a, 0x5eed_0001))

	feasibleRuns := 0

	for iter := range 400 {
		n := 150 + rng.IntN(900)
		units := make([]uint32, n)
		for i := range units {
			// Skewed toward small bids, which is the realistic shape and also the one
			// that stresses the water-filling loop.
			switch rng.IntN(10) {
			case 0:
				units[i] = 1 + uint32(rng.IntN(25))
			case 1, 2:
				units[i] = 1 + uint32(rng.IntN(5))
			default:
				units[i] = 1
			}
		}
		bids := book(units)

		var s merkle.Hash
		for i := range s {
			s[i] = byte(rng.UintN(256))
		}
		if s.IsZero() {
			s[0] = 1
		}

		f, err := CheckFeasibility(bids, p)
		if err != nil {
			t.Fatalf("iter=%d: feasibility: %v", iter, err)
		}
		if !f.Feasible {
			continue
		}
		feasibleRuns++

		r, err := Run(bids, p, s)
		if err != nil {
			t.Fatalf("iter=%d n=%d: feasible offer failed to run: %v", iter, n, err)
		}
		assertInvariants(t, r, bids, p)
	}

	if feasibleRuns < 100 {
		t.Fatalf("only %d feasible books generated; the generator is not exercising the algorithm", feasibleRuns)
	}
	t.Logf("verified invariants across %d feasible randomised books", feasibleRuns)
}

// ---------------------------------------------------------------------------
// Refund arithmetic
// ---------------------------------------------------------------------------

// ASBA blocks the full bid amount. What is debited plus what is unblocked must equal
// what was blocked, for every bid, or an investor is short-changed or over-refunded.
func TestRefundsReconcileForEveryBid(t *testing.T) {
	p := V1Params()
	units := make([]uint32, 700)
	for i := range units {
		units[i] = uint32(i%7) + 1
	}
	bids := book(units)

	r, err := Run(bids, p, seed(0x66))
	if err != nil {
		t.Fatal(err)
	}

	byLeaf := map[uint32]Bid{}
	for _, b := range bids {
		byLeaf[b.LeafIndex] = b
	}

	var totalPayable, totalRefund, totalBlocked money.Paise
	for _, a := range r.Allocations {
		b := byLeaf[a.LeafIndex]
		blocked, _, err := b.PricePerUnitPaise.MulDivFloor(int64(b.UnitsBid), 1)
		if err != nil {
			t.Fatal(err)
		}
		if totalPayable, err = totalPayable.Add(a.AmountPayable); err != nil {
			t.Fatal(err)
		}
		if totalRefund, err = totalRefund.Add(a.RefundAmount); err != nil {
			t.Fatal(err)
		}
		if totalBlocked, err = totalBlocked.Add(blocked); err != nil {
			t.Fatal(err)
		}
	}

	sum, err := totalPayable.Add(totalRefund)
	if err != nil {
		t.Fatal(err)
	}
	if sum != totalBlocked {
		t.Fatalf("debits %d + refunds %d = %d, but %d was blocked", totalPayable, totalRefund, sum, totalBlocked)
	}

	// And the amount actually collected must be the offer priced at the band.
	wantCollected, _, err := money.MinUnitPricePaise.MulDivFloor(int64(p.UnitsOnOffer), 1)
	if err != nil {
		t.Fatal(err)
	}
	if totalPayable != wantCollected {
		t.Errorf("collected %d paise for %d units, want %d", totalPayable, p.UnitsOnOffer, wantCollected)
	}
}

// ---------------------------------------------------------------------------
// Golden vector
// ---------------------------------------------------------------------------

// Freezes the whole pipeline: leaf encoding, ranking, selection, distribution and
// root construction. If this changes, every allotment AcreSync has published
// becomes unreproducible by a verifier built against the old rules.
func TestGoldenResult(t *testing.T) {
	p := V1Params()
	units := make([]uint32, 600)
	for i := range units {
		units[i] = uint32(i%5) + 1
	}
	bids := book(units)

	secret := seed(0x7a)
	blockHash := seed(0x8b)
	bookRoot, err := BidbookRoot(bids)
	if err != nil {
		t.Fatal(err)
	}
	finalSeed := DeriveFinalSeed(secret, blockHash, bookRoot)

	r, err := Run(bids, p, finalSeed)
	if err != nil {
		t.Fatal(err)
	}

	const (
		wantBook   = "0xe624478ab0c28497f9abff18bb25fa993ad237f36617134d9a887e2eed9d30a5"
		wantSeed   = "0x7d64bb66afe494ce6149acffea3c1e960ed6274cd797769eecd244f78ec84df5"
		wantResult = "0xe603b6200f0d0547c5cad34bb527578b59976219dc5466cee653d0fdd5a4f1ed"
	)

	if bookRoot.Hex() != wantBook || finalSeed.Hex() != wantSeed || r.ResultRoot.Hex() != wantResult {
		t.Errorf("golden vectors changed:\n  bidbook %s\n  seed    %s\n  result  %s\n"+
			"  allotted %d units across %d holders\n\n"+
			"If the algorithm changed deliberately, increment AlgoVersion in the same commit: "+
			"a verifier reproducing a historical allotment needs to know which rules applied.",
			bookRoot.Hex(), finalSeed.Hex(), r.ResultRoot.Hex(), r.UnitsAllotted, r.DistinctAllottees)
	}
}
