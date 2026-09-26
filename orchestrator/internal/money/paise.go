// Package money provides the only monetary type in AcreSync.
//
// Every rupee amount in the system is an exact integer count of paise. There is
// no float anywhere in the money path, by construction rather than by
// convention, because the distribution invariant is exact:
//
//	sum(per-holder gross entitlement) == distributedPaise
//
// Not "within a tolerance". A one-paisa discrepancy in a regulated SM-REIT
// distribution is an audit finding, and floating point arithmetic cannot
// guarantee the equality holds.
//
// All arithmetic here is overflow-checked. A silent int64 wrap in a cap table
// would be indistinguishable from fraud.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// Paise is an exact integer count of Indian paise. 100 paise = 1 rupee.
type Paise int64

const (
	// PaisePerRupee is the fixed minor-unit scale for INR.
	PaisePerRupee = 100

	// OneLakhRupees is the SM-REIT minimum unit price denominator helper.
	OneLakhRupees = 100_000

	// MinUnitPricePaise is the SEBI minimum price for one unit of an SM-REIT
	// scheme: ten lakh rupees.
	MinUnitPricePaise Paise = 10 * OneLakhRupees * PaisePerRupee // 100_000_000

	// MinSchemeAssetValuePaise is the SM-REIT floor: fifty crore rupees.
	MinSchemeAssetValuePaise Paise = 50 * 10_000_000 * PaisePerRupee // 50_00_00_00_000

	// MaxSchemeAssetValuePaise is the SM-REIT ceiling: five hundred crore rupees.
	MaxSchemeAssetValuePaise Paise = 500 * 10_000_000 * PaisePerRupee
)

var (
	ErrOverflow     = errors.New("money: arithmetic overflow")
	ErrNegative     = errors.New("money: negative amount not permitted here")
	ErrZeroDivisor  = errors.New("money: division by zero")
	ErrNotAnInteger = errors.New("money: value is not an integer number of paise")
)

// String renders the raw paise count. It deliberately does not format as
// currency; display formatting belongs in the presentation layer, and a
// currency-formatted value must never be fed back into arithmetic.
func (p Paise) String() string { return strconv.FormatInt(int64(p), 10) }

// Rupees returns the whole rupee part and the remaining paise.
func (p Paise) Rupees() (whole int64, remainder int64) {
	return int64(p) / PaisePerRupee, int64(p) % PaisePerRupee
}

// FromRupees converts a whole rupee amount to paise, checking for overflow.
func FromRupees(rupees int64) (Paise, error) {
	if rupees > math.MaxInt64/PaisePerRupee || rupees < math.MinInt64/PaisePerRupee {
		return 0, fmt.Errorf("%w: %d rupees to paise", ErrOverflow, rupees)
	}
	return Paise(rupees * PaisePerRupee), nil
}

// Add returns p+q, checking for overflow.
func (p Paise) Add(q Paise) (Paise, error) {
	sum := p + q
	// Overflow occurred if the operands share a sign that the result does not.
	if (p > 0 && q > 0 && sum < 0) || (p < 0 && q < 0 && sum >= 0) {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, p, q)
	}
	return sum, nil
}

// Sub returns p-q, checking for overflow.
func (p Paise) Sub(q Paise) (Paise, error) {
	diff := p - q
	if (q < 0 && diff < p) || (q > 0 && diff > p) {
		return 0, fmt.Errorf("%w: %d - %d", ErrOverflow, p, q)
	}
	return diff, nil
}

// Sum adds a slice exactly, checking for overflow at every step.
func Sum(amounts []Paise) (Paise, error) {
	var total Paise
	var err error
	for i, a := range amounts {
		if total, err = total.Add(a); err != nil {
			return 0, fmt.Errorf("summing element %d: %w", i, err)
		}
	}
	return total, nil
}

// MulDivFloor computes floor(p * num / den) exactly, along with the truncated
// remainder, using arbitrary precision intermediates so that the p*num product
// cannot overflow.
//
// This is the primitive behind per-holder entitlement:
//
//	gross_i    = floor(distributedPaise * units_i / totalUnits)
//	remainder_i = distributedPaise * units_i mod totalUnits
//
// The remainders are what the largest-remainder allocation consumes to make the
// per-holder amounts sum to distributedPaise exactly.
func (p Paise) MulDivFloor(num, den int64) (quotient Paise, remainder *big.Int, err error) {
	if den == 0 {
		return 0, nil, ErrZeroDivisor
	}
	if den < 0 {
		return 0, nil, fmt.Errorf("%w: negative denominator %d", ErrNegative, den)
	}
	if p < 0 || num < 0 {
		return 0, nil, fmt.Errorf("%w: MulDivFloor operands %d, %d", ErrNegative, p, num)
	}

	product := new(big.Int).Mul(big.NewInt(int64(p)), big.NewInt(num))
	q, r := new(big.Int).QuoRem(product, big.NewInt(den), new(big.Int))

	if !q.IsInt64() {
		return 0, nil, fmt.Errorf("%w: floor(%d * %d / %d) exceeds int64", ErrOverflow, p, num, den)
	}
	return Paise(q.Int64()), r, nil
}

// BasisPointsOf returns floor(p * 10000 / of), the share of `of` represented by
// p expressed in basis points. Returns an error when `of` is zero.
//
// Used for reporting only. The regulatory floor check itself must use
// MeetsFloorBps, which avoids the truncation this function introduces.
func (p Paise) BasisPointsOf(of Paise) (uint16, error) {
	if of == 0 {
		return 0, ErrZeroDivisor
	}
	bps, _, err := p.MulDivFloor(10_000, int64(of))
	if err != nil {
		return 0, err
	}
	if bps > math.MaxUint16 {
		return 0, fmt.Errorf("%w: %d bps exceeds uint16", ErrOverflow, bps)
	}
	return uint16(bps), nil
}

// MeetsFloorBps reports whether distributed is at least floorBps basis points
// of ndcf, evaluated as a cross-multiplication in arbitrary precision.
//
// This mirrors the on-chain check exactly. Note that the reason is NOT that
// truncating division would give a different answer: since floorBps is an
// integer, floor(x) >= floorBps holds exactly when x >= floorBps, so
// `distributed * 10000 / ndcf >= floorBps` yields the same boolean. The actual
// reasons to cross-multiply are narrower and worth stating precisely:
//
//   - Overflow. In Solidity the operands are uint64 and the product
//     distributed * 10000 overflows that width for large NDCF values, silently
//     wrapping to a small number that passes the check. The on-chain version
//     must widen to uint256 before multiplying; here big.Int removes the
//     question entirely.
//
//   - No division means no division-by-zero branch to get wrong, and no
//     dependence on the language's rounding behaviour for the operand signs.
//
// The zero and negative cases are rejected explicitly rather than being allowed
// to produce a misleading false.
func MeetsFloorBps(distributed, ndcf Paise, floorBps uint16) (bool, error) {
	if ndcf <= 0 {
		return false, fmt.Errorf("%w: ndcf must be positive, got %d", ErrZeroDivisor, ndcf)
	}
	if distributed < 0 {
		return false, fmt.Errorf("%w: distributed %d", ErrNegative, distributed)
	}
	lhs := new(big.Int).Mul(big.NewInt(int64(distributed)), big.NewInt(10_000))
	rhs := new(big.Int).Mul(big.NewInt(int64(ndcf)), big.NewInt(int64(floorBps)))
	return lhs.Cmp(rhs) >= 0, nil
}

// MarshalJSON emits a bare JSON integer.
func (p Paise) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(p), 10)), nil
}

// UnmarshalJSON accepts only an integer literal. A fractional or exponent form
// is rejected rather than rounded.
func (p *Paise) UnmarshalJSON(data []byte) error {
	s := string(data)
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNotAnInteger, s)
	}
	*p = Paise(v)
	return nil
}
