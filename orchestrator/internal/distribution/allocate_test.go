package distribution

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

func anchor(i int) merkle.Hash {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(i))
	return merkle.Hash(sha256.Sum256(b[:]))
}

func holders(units ...uint32) []Holder {
	out := make([]Holder, len(units))
	for i, u := range units {
		out[i] = Holder{InvestorAnchor: anchor(i), Units: u}
	}
	return out
}

func sumGross(t *testing.T, r *Result) money.Paise {
	t.Helper()
	var total money.Paise
	for _, e := range r.Entitlements {
		got, err := e.Floor.Add(e.ResidueAwarded)
		if err != nil {
			t.Fatal(err)
		}
		if got != e.Gross {
			t.Fatalf("Gross %d != Floor %d + Residue %d", e.Gross, e.Floor, e.ResidueAwarded)
		}
		total, err = total.Add(e.Gross)
		if err != nil {
			t.Fatal(err)
		}
	}
	return total
}

// The v1 numbers, which divide cleanly and are worth pinning as a sanity anchor.
//
// NDCF ₹30 crore, 95% distributed is ₹28.5 crore = 28,500,000,000 paise across 500
// units, which is exactly 57,000,000 paise (₹5.7 lakh) per unit.
func TestV1SchemeDividesCleanly(t *testing.T) {
	const distributed = money.Paise(28_500_000_000)
	hs := holders(25, 3, 472)

	r, err := Allocate(distributed, 500, hs)
	if err != nil {
		t.Fatal(err)
	}
	if r.ResidueDistributed != 0 {
		t.Errorf("expected no residue for an exact division, got %d paise", r.ResidueDistributed)
	}

	perUnit, rem, err := PerUnitRate(distributed, 500)
	if err != nil {
		t.Fatal(err)
	}
	if perUnit != 57_000_000 || rem != 0 {
		t.Fatalf("per-unit rate = %d paise (remainder %d), want 57000000 and 0", perUnit, rem)
	}

	byUnits := map[uint32]money.Paise{}
	for _, e := range r.Entitlements {
		byUnits[e.Units] = e.Gross
	}
	for units, want := range map[uint32]money.Paise{
		25:  1_425_000_000,
		3:   171_000_000,
		472: 26_904_000_000,
	} {
		if got := byUnits[units]; got != want {
			t.Errorf("%d units: got %d paise, want %d", units, got, want)
		}
	}
	if got := sumGross(t, r); got != distributed {
		t.Fatalf("sum %d != distributed %d", got, distributed)
	}
}

// The invariant, exhaustively. Every combination of an awkward amount and an
// awkward unit split must reconcile to the paise with no tolerance.
func TestExactnessAcrossManyShapes(t *testing.T) {
	amounts := []money.Paise{
		0, 1, 2, 7, 99, 100, 101,
		999_999_999,
		28_500_000_000,
		28_500_000_001,
		28_499_999_999,
		1_000_000_000_007,
	}
	splits := [][]uint32{
		{500},
		{1, 499},
		{25, 475},
		{25, 3, 472},
		{1, 1, 1, 497},
		{167, 167, 166},
		{3, 7, 11, 13, 466},
	}

	for _, amount := range amounts {
		for _, split := range splits {
			var total uint32
			for _, u := range split {
				total += u
			}
			r, err := Allocate(amount, total, holders(split...))
			if err != nil {
				t.Fatalf("amount=%d split=%v: %v", amount, split, err)
			}
			if got := sumGross(t, r); got != amount {
				t.Errorf("amount=%d split=%v: entitlements sum to %d, off by %d paise",
					amount, split, got, got-amount)
			}
			if uint64(r.ResidueDistributed) >= uint64(total) {
				t.Errorf("amount=%d split=%v: residue %d should be below total units %d",
					amount, split, r.ResidueDistributed, total)
			}
		}
	}
}

// A 200-holder scheme with a deliberately awkward amount, which is the realistic
// shape and the one where a naive floor would lose a few rupees.
func TestExactnessAtSchemeScale(t *testing.T) {
	// 200 holders sharing 500 units: 199 holders with 2 units, one with 102.
	units := make([]uint32, 200)
	for i := range 199 {
		units[i] = 2
	}
	units[199] = 102

	const distributed = money.Paise(28_499_999_999) // one paisa short of clean
	r, err := Allocate(distributed, 500, holders(units...))
	if err != nil {
		t.Fatal(err)
	}
	if got := sumGross(t, r); got != distributed {
		t.Fatalf("sum %d != distributed %d", got, distributed)
	}
	if r.ResidueDistributed == 0 {
		t.Error("expected a non-zero residue for an amount that does not divide evenly")
	}

	awarded := 0
	for _, e := range r.Entitlements {
		if e.ResidueAwarded > 1 {
			t.Errorf("a holder received %d residue paise; largest-remainder awards at most one", e.ResidueAwarded)
		}
		if e.ResidueAwarded == 1 {
			awarded++
		}
	}
	if money.Paise(awarded) != r.ResidueDistributed {
		t.Errorf("%d holders received a residue paisa but residue is %d", awarded, r.ResidueDistributed)
	}
}

// Randomised, because the combinations that break an apportionment rule are hard to
// guess and easy to generate.
func TestExactnessRandomised(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x4163726553, 0x796e63)) // fixed seed: reproducible failures

	for iter := range 2000 {
		n := 1 + rng.IntN(60)
		units := make([]uint32, n)
		var total uint32
		for i := range units {
			units[i] = 1 + uint32(rng.IntN(30))
			total += units[i]
		}
		amount := money.Paise(rng.Int64N(50_000_000_000))

		r, err := Allocate(amount, total, holders(units...))
		if err != nil {
			t.Fatalf("iter=%d n=%d total=%d amount=%d: %v", iter, n, total, amount, err)
		}
		if got := sumGross(t, r); got != amount {
			t.Fatalf("iter=%d: sum %d != amount %d (off by %d)", iter, got, amount, got-amount)
		}
	}
}

// Determinism: the same inputs must produce byte-identical output regardless of the
// order holders arrive in, because the result is hashed into the snapshot document
// and anchored on-chain.
func TestDeterministicAcrossInputOrder(t *testing.T) {
	const distributed = money.Paise(28_499_999_999)
	base := holders(25, 3, 100, 200, 172)

	reference, err := Allocate(distributed, 500, base)
	if err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewPCG(7, 11))
	for range 200 {
		shuffled := make([]Holder, len(base))
		copy(shuffled, base)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		got, err := Allocate(distributed, 500, shuffled)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Entitlements) != len(reference.Entitlements) {
			t.Fatalf("length changed: %d vs %d", len(got.Entitlements), len(reference.Entitlements))
		}
		for i := range got.Entitlements {
			a, b := reference.Entitlements[i], got.Entitlements[i]
			if a.InvestorAnchor != b.InvestorAnchor || a.Gross != b.Gross || a.ResidueAwarded != b.ResidueAwarded {
				t.Fatalf("entry %d differs: %+v vs %+v", i, a, b)
			}
		}
	}
}

func TestOutputIsSortedByAnchor(t *testing.T) {
	r, err := Allocate(28_500_000_000, 500, holders(25, 3, 472))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(r.Entitlements); i++ {
		prev := r.Entitlements[i-1].InvestorAnchor
		cur := r.Entitlements[i].InvestorAnchor
		if bytes.Compare(prev[:], cur[:]) >= 0 {
			t.Fatal("output must be sorted ascending by investor anchor to be canonical")
		}
	}
}

// The investment manager is excluded from the 200-unitholder count but not from
// economic rights. If exclusion suppressed the entitlement, the manager would be
// underpaid and the per-unit rate would differ between holders, which is the one
// number an investor is most likely to check by hand.
func TestExcludedHoldersStillEarn(t *testing.T) {
	hs := []Holder{
		{InvestorAnchor: anchor(0), Units: 25, Excluded: true}, // investment manager
		{InvestorAnchor: anchor(1), Units: 475, Excluded: false},
	}
	r, err := Allocate(28_500_000_000, 500, hs)
	if err != nil {
		t.Fatal(err)
	}

	var imGross, publicGross money.Paise
	for _, e := range r.Entitlements {
		switch e.Units {
		case 25:
			imGross = e.Gross
		case 475:
			publicGross = e.Gross
		}
	}
	if imGross != 1_425_000_000 {
		t.Errorf("excluded manager gross = %d, want 1425000000", imGross)
	}
	if publicGross != 27_075_000_000 {
		t.Errorf("public gross = %d, want 27075000000", publicGross)
	}

	// The per-unit rate must be identical for both.
	imRate := int64(imGross) / 25
	pubRate := int64(publicGross) / 475
	if imRate != pubRate {
		t.Errorf("per-unit rate differs: excluded %d vs included %d", imRate, pubRate)
	}
}

func TestZeroDistributionIsValid(t *testing.T) {
	// A period can legitimately distribute nothing, for instance when NDCF is
	// consumed entirely by expenses. Every holder gets zero and the sum still
	// reconciles.
	r, err := Allocate(0, 500, holders(25, 475))
	if err != nil {
		t.Fatal(err)
	}
	if got := sumGross(t, r); got != 0 {
		t.Fatalf("sum %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Rejections
// ---------------------------------------------------------------------------

func TestUnitMismatchRejected(t *testing.T) {
	// A snapshot whose lines do not sum to its declared total would silently change
	// every holder's rate.
	if _, err := Allocate(1000, 500, holders(25, 400)); !errors.Is(err, ErrUnitMismatch) {
		t.Fatalf("want ErrUnitMismatch, got %v", err)
	}
}

func TestDuplicateAnchorRejected(t *testing.T) {
	hs := holders(250, 250)
	hs[1].InvestorAnchor = hs[0].InvestorAnchor
	if _, err := Allocate(1000, 500, hs); !errors.Is(err, ErrDuplicateAnchor) {
		t.Fatalf("want ErrDuplicateAnchor, got %v", err)
	}
}

func TestZeroUnitHolderRejected(t *testing.T) {
	hs := []Holder{
		{InvestorAnchor: anchor(0), Units: 0},
		{InvestorAnchor: anchor(1), Units: 500},
	}
	if _, err := Allocate(1000, 500, hs); err == nil {
		t.Fatal("a zero-unit line does not belong in a record-date snapshot and must be rejected")
	}
}

func TestEmptyAndZeroTotalRejected(t *testing.T) {
	if _, err := Allocate(1000, 500, nil); !errors.Is(err, ErrNoHolders) {
		t.Fatalf("want ErrNoHolders, got %v", err)
	}
	if _, err := Allocate(1000, 0, holders(1)); !errors.Is(err, ErrZeroUnits) {
		t.Fatalf("want ErrZeroUnits, got %v", err)
	}
}

func TestNegativeDistributionRejected(t *testing.T) {
	if _, err := Allocate(-1, 500, holders(500)); !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("want ErrNegativeAmount, got %v", err)
	}
}
