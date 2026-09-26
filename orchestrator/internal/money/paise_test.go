package money

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/acresync/orchestrator/internal/canonical"
)

func TestRegulatoryConstants(t *testing.T) {
	// ₹10 lakh per unit, the SM-REIT minimum unit price.
	if MinUnitPricePaise != 100_000_000 {
		t.Errorf("MinUnitPricePaise = %d, want 100000000 (₹10,00,000)", MinUnitPricePaise)
	}
	// ₹50 crore and ₹500 crore scheme band.
	if MinSchemeAssetValuePaise != 50_000_000_000 {
		t.Errorf("MinSchemeAssetValuePaise = %d, want 50000000000 (₹50 crore)", MinSchemeAssetValuePaise)
	}
	if MaxSchemeAssetValuePaise != 500_000_000_000 {
		t.Errorf("MaxSchemeAssetValuePaise = %d, want 500000000000 (₹500 crore)", MaxSchemeAssetValuePaise)
	}

	// The v1 scheme parameters must be internally consistent: 500 units at
	// ₹10 lakh is exactly ₹50 crore, the band floor.
	const totalUnits = 500
	if got := Paise(totalUnits) * MinUnitPricePaise; got != MinSchemeAssetValuePaise {
		t.Errorf("500 units at the minimum price = %d, want %d", got, MinSchemeAssetValuePaise)
	}
}

func TestAddOverflow(t *testing.T) {
	if _, err := Paise(math.MaxInt64).Add(1); !errors.Is(err, ErrOverflow) {
		t.Error("positive overflow not detected")
	}
	if _, err := Paise(math.MinInt64).Add(-1); !errors.Is(err, ErrOverflow) {
		t.Error("negative overflow not detected")
	}
	if got, err := Paise(100).Add(23); err != nil || got != 123 {
		t.Errorf("Add = %d, %v; want 123, nil", got, err)
	}
}

func TestSubOverflow(t *testing.T) {
	if _, err := Paise(math.MinInt64).Sub(1); !errors.Is(err, ErrOverflow) {
		t.Error("underflow not detected")
	}
	if _, err := Paise(math.MaxInt64).Sub(-1); !errors.Is(err, ErrOverflow) {
		t.Error("overflow via negative subtrahend not detected")
	}
	if got, err := Paise(100).Sub(23); err != nil || got != 77 {
		t.Errorf("Sub = %d, %v; want 77, nil", got, err)
	}
}

func TestSumDetectsOverflowMidway(t *testing.T) {
	if _, err := Sum([]Paise{math.MaxInt64, 1, -1}); !errors.Is(err, ErrOverflow) {
		t.Error("Sum should fail at the overflowing element rather than wrapping and recovering")
	}
	got, err := Sum([]Paise{1, 2, 3, 4})
	if err != nil || got != 10 {
		t.Errorf("Sum = %d, %v; want 10, nil", got, err)
	}
}

func TestFromRupees(t *testing.T) {
	got, err := FromRupees(1_000_000) // ₹10 lakh
	if err != nil || got != MinUnitPricePaise {
		t.Errorf("FromRupees(1000000) = %d, %v; want %d, nil", got, err, MinUnitPricePaise)
	}
	if _, err := FromRupees(math.MaxInt64); !errors.Is(err, ErrOverflow) {
		t.Error("FromRupees should detect overflow")
	}
}

// The primitive the entitlement calculation rests on. The quotient is the
// per-holder floor; the remainder is what the largest-remainder pass distributes
// so that the total lands exactly on distributedPaise.
func TestMulDivFloorAndRemainder(t *testing.T) {
	// ₹2.85 crore distributed across 500 units, 3 units held.
	const distributed = Paise(28_500_000_000)
	q, r, err := distributed.MulDivFloor(3, 500)
	if err != nil {
		t.Fatal(err)
	}
	if want := Paise(171_000_000); q != want {
		t.Errorf("quotient = %d, want %d", q, want)
	}
	if r.Sign() != 0 {
		t.Errorf("remainder = %s, want 0 (this division is exact)", r)
	}

	// A case that does not divide evenly.
	q, r, err = Paise(1000).MulDivFloor(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if q != 333 {
		t.Errorf("quotient = %d, want 333", q)
	}
	if r.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("remainder = %s, want 1", r)
	}

	if _, _, err := Paise(1).MulDivFloor(1, 0); !errors.Is(err, ErrZeroDivisor) {
		t.Error("zero denominator not rejected")
	}
	if _, _, err := Paise(-1).MulDivFloor(1, 2); !errors.Is(err, ErrNegative) {
		t.Error("negative operand not rejected")
	}
}

// The property that invariant 7 depends on: floors plus residue must reconstruct
// the distributed total exactly, for every unit split, with no tolerance.
func TestLargestRemainderReconstructsExactly(t *testing.T) {
	totals := []Paise{1, 7, 999, 28_500_000_000, 28_500_000_001, 100_000_000_003}
	splits := [][]int64{
		{500},
		{1, 499},
		{3, 7, 490},
		{250, 250},
		{1, 1, 1, 497},
	}

	for _, distributed := range totals {
		for _, split := range splits {
			var totalUnits int64
			for _, u := range split {
				totalUnits += u
			}

			type holder struct {
				floor     Paise
				remainder *big.Int
				idx       int
			}
			holders := make([]holder, len(split))
			var sumFloors Paise

			for i, units := range split {
				q, r, err := distributed.MulDivFloor(units, totalUnits)
				if err != nil {
					t.Fatal(err)
				}
				holders[i] = holder{floor: q, remainder: r, idx: i}
				s, err := sumFloors.Add(q)
				if err != nil {
					t.Fatal(err)
				}
				sumFloors = s
			}

			residue := int64(distributed - sumFloors)
			if residue < 0 || residue >= int64(len(split)) && residue >= totalUnits {
				t.Fatalf("residue %d out of expected bounds for distributed=%d split=%v",
					residue, distributed, split)
			}

			// Largest remainder: descending remainder, then ascending index for a
			// deterministic tie-break. One paisa each until the residue is spent.
			order := make([]holder, len(holders))
			copy(order, holders)
			for i := range order {
				for j := i + 1; j < len(order); j++ {
					cmp := order[j].remainder.Cmp(order[i].remainder)
					if cmp > 0 || (cmp == 0 && order[j].idx < order[i].idx) {
						order[i], order[j] = order[j], order[i]
					}
				}
			}

			awarded := make([]Paise, len(split))
			for i := range order {
				if int64(i) < residue {
					awarded[order[i].idx] = 1
				}
			}

			var final Paise
			for i := range split {
				v, err := holders[i].floor.Add(awarded[i])
				if err != nil {
					t.Fatal(err)
				}
				f, err := final.Add(v)
				if err != nil {
					t.Fatal(err)
				}
				final = f
			}

			if final != distributed {
				t.Errorf("distributed=%d split=%v: entitlements sum to %d, want exactly %d (off by %d paise)",
					distributed, split, final, distributed, final-distributed)
			}
		}
	}
}

func TestMeetsFloorBps(t *testing.T) {
	cases := []struct {
		name        string
		distributed Paise
		ndcf        Paise
		want        bool
	}{
		{"exactly 9500", 9_500, 10_000, true},
		{"just above", 9_501, 10_000, true},
		{"just below", 9_499, 10_000, false},
		{"full distribution", 10_000, 10_000, true},
		{"realistic pass", 28_500_000_000, 30_000_000_000, true},
		{"realistic fail", 28_499_999_999, 30_000_000_000, false},

		// The value that matters most: a product which overflows uint64 if the
		// implementation multiplies in a narrow type. 2e15 * 10000 = 2e19, past
		// uint64's 1.8e19 ceiling. A wrapped product would report a pass.
		{"overflow guard", 2_000_000_000_000_000, 2_000_000_000_000_000, true},
		{"overflow guard below floor", 1_800_000_000_000_000, 2_000_000_000_000_000, false},
	}

	for _, c := range cases {
		got, err := MeetsFloorBps(c.distributed, c.ndcf, 9500)
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: MeetsFloorBps(%d, %d, 9500) = %v, want %v",
				c.name, c.distributed, c.ndcf, got, c.want)
		}
	}

	if _, err := MeetsFloorBps(1, 0, 9500); !errors.Is(err, ErrZeroDivisor) {
		t.Error("zero NDCF should be an error, not a silent false")
	}
	if _, err := MeetsFloorBps(-1, 100, 9500); !errors.Is(err, ErrNegative) {
		t.Error("negative distribution should be rejected")
	}
}

// Confirms the claim in the MeetsFloorBps doc comment: for an integer threshold,
// truncating division agrees with cross-multiplication. The reason to
// cross-multiply is overflow, not truncation, and the code comment says so.
func TestTruncatingDivisionAgreesAtTheBoundary(t *testing.T) {
	for ndcf := Paise(1); ndcf <= 3000; ndcf++ {
		for distributed := Paise(0); distributed <= ndcf; distributed++ {
			viaCross, err := MeetsFloorBps(distributed, ndcf, 9500)
			if err != nil {
				t.Fatal(err)
			}
			viaDivision := (int64(distributed)*10000)/int64(ndcf) >= 9500
			if viaCross != viaDivision {
				t.Fatalf("disagreement at distributed=%d ndcf=%d: cross=%v division=%v",
					distributed, ndcf, viaCross, viaDivision)
			}
		}
	}
}

func TestBasisPointsOf(t *testing.T) {
	bps, err := Paise(28_500_000_000).BasisPointsOf(30_000_000_000)
	if err != nil || bps != 9500 {
		t.Errorf("BasisPointsOf = %d, %v; want 9500, nil", bps, err)
	}
	if _, err := Paise(1).BasisPointsOf(0); !errors.Is(err, ErrZeroDivisor) {
		t.Error("zero divisor not rejected")
	}
}

func TestRupeesSplit(t *testing.T) {
	whole, rem := Paise(123_456).Rupees()
	if whole != 1234 || rem != 56 {
		t.Errorf("Rupees() = %d, %d; want 1234, 56", whole, rem)
	}
}

// Paise must survive the canonical encoder as a bare integer. If it ever encoded
// as a string or a float, every idempotency key involving an amount would change.
func TestPaiseEncodesCanonicallyAsInteger(t *testing.T) {
	got, err := canonical.Marshal(map[string]any{"amount": Paise(28_500_000_000)})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"amount":28500000000}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestUnmarshalJSONRejectsFractional(t *testing.T) {
	var p Paise
	for _, in := range []string{`1.5`, `1e3`, `"100"`, `1.0`} {
		if err := p.UnmarshalJSON([]byte(in)); !errors.Is(err, ErrNotAnInteger) {
			t.Errorf("UnmarshalJSON(%s): want ErrNotAnInteger, got %v", in, err)
		}
	}
	if err := p.UnmarshalJSON([]byte(`28500000000`)); err != nil || p != 28_500_000_000 {
		t.Errorf("UnmarshalJSON = %d, %v", p, err)
	}
}
