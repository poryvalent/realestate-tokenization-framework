// Package distribution splits a period's distributable cash across unitholders.
//
// # The invariant this package exists to guarantee
//
//	sum(per-holder gross entitlement) == distributedPaise
//
// Exactly. Not within a tolerance, not within a rupee. A distribution that does
// not reconcile to the paise is an audit finding in a regulated scheme, and the
// database enforces the same equality in assert_period_entitlements_exact, so a
// mismatch here surfaces as a failed period rather than as a rounding note.
//
// Naive proportional division cannot deliver that. floor(distributed * units /
// total) summed over holders falls short by up to total-1 paise, because each
// division throws away a fraction. The shortfall is small in absolute terms, under
// five rupees on a 500-unit scheme, and completely unacceptable as a silent
// discrepancy.
//
// The largest-remainder method closes it: award every holder their floor, then
// hand out the leftover paise one at a time to the holders whose discarded
// fractions were largest. The result sums exactly and is the standard apportionment
// rule, so it is defensible to an auditor rather than ad hoc.
//
// # Determinism
//
// Ties on the discarded fraction are broken by ascending investor anchor. Anchors
// are unique within a scheme, so the ordering is total and the allocation is
// reproducible from published data by anyone, which is what makes the entitlement
// events on-chain independently checkable.
package distribution

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

var (
	ErrNoHolders       = errors.New("distribution: no holders")
	ErrZeroUnits       = errors.New("distribution: total units must be positive")
	ErrNegativeAmount  = errors.New("distribution: distributed amount cannot be negative")
	ErrUnitMismatch    = errors.New("distribution: holder units do not sum to the declared total")
	ErrDuplicateAnchor = errors.New("distribution: duplicate investor anchor")
	ErrNotExact        = errors.New("distribution: entitlements do not sum to the distributed amount")
)

// Holder is one line of the record-date register snapshot.
//
// Excluded holders still receive an entitlement. Exclusion applies to the
// 200-unitholder count, not to economic rights: the investment manager's 25 units
// earn their pro-rata share like any other. Conflating the two would underpay the
// manager and, worse, would make the per-unit rate differ between holders.
type Holder struct {
	InvestorAnchor merkle.Hash
	Units          uint32
	Excluded       bool
}

// Entitlement is one holder's exact share.
type Entitlement struct {
	InvestorAnchor merkle.Hash
	Units          uint32

	// Floor is floor(distributed * units / totalUnits).
	Floor money.Paise

	// ResidueAwarded is 0 or 1 paise from the largest-remainder pass. Recorded
	// separately so the exactness invariant is provable from stored data instead of
	// being recomputed and hoped for.
	ResidueAwarded money.Paise

	// Gross is Floor + ResidueAwarded, the amount anchored against this holder.
	Gross money.Paise

	// RemainderNumerator is (distributed * units) mod totalUnits, the discarded
	// fraction expressed as an exact integer. Kept so the ordering that produced
	// this allocation can be re-derived and audited.
	RemainderNumerator *big.Int
}

// Result is the complete allocation for one period.
type Result struct {
	DistributedPaise money.Paise
	TotalUnits       uint32
	Entitlements     []Entitlement // ordered by ascending investor anchor
	ResidueDistributed money.Paise
}

// Allocate splits distributed across holders exactly.
//
// totalUnits is passed explicitly rather than summed from holders so that a
// snapshot which disagrees with its own declared total is rejected. That
// disagreement would otherwise change every holder's rate silently, and the
// per-unit amount is the one number an investor will check by hand.
func Allocate(distributed money.Paise, totalUnits uint32, holders []Holder) (*Result, error) {
	if len(holders) == 0 {
		return nil, ErrNoHolders
	}
	if totalUnits == 0 {
		return nil, ErrZeroUnits
	}
	if distributed < 0 {
		return nil, fmt.Errorf("%w: %d", ErrNegativeAmount, distributed)
	}

	var unitSum uint64
	seen := make(map[merkle.Hash]struct{}, len(holders))
	for i, h := range holders {
		if h.Units == 0 {
			return nil, fmt.Errorf("distribution: holder %d has zero units; a zero-unit line does not belong in a record-date snapshot", i)
		}
		if _, dup := seen[h.InvestorAnchor]; dup {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateAnchor, h.InvestorAnchor.Hex())
		}
		seen[h.InvestorAnchor] = struct{}{}
		unitSum += uint64(h.Units)
	}
	if unitSum != uint64(totalUnits) {
		return nil, fmt.Errorf("%w: holders sum to %d, snapshot declares %d", ErrUnitMismatch, unitSum, totalUnits)
	}

	// Stage one: exact floors and exact discarded fractions.
	ents := make([]Entitlement, len(holders))
	var floorSum money.Paise

	for i, h := range holders {
		q, r, err := distributed.MulDivFloor(int64(h.Units), int64(totalUnits))
		if err != nil {
			return nil, fmt.Errorf("distribution: holder %s: %w", h.InvestorAnchor.Hex(), err)
		}
		ents[i] = Entitlement{
			InvestorAnchor:     h.InvestorAnchor,
			Units:              h.Units,
			Floor:              q,
			RemainderNumerator: r,
		}
		floorSum, err = floorSum.Add(q)
		if err != nil {
			return nil, fmt.Errorf("distribution: summing floors: %w", err)
		}
	}

	residue, err := distributed.Sub(floorSum)
	if err != nil {
		return nil, fmt.Errorf("distribution: computing residue: %w", err)
	}
	if residue < 0 {
		return nil, fmt.Errorf("%w: floors exceed the distributed amount by %d paise", ErrInternalNegativeResidue, -residue)
	}
	// The residue is bounded by the number of divisions, one paisa short per holder
	// at worst, and cannot reach totalUnits.
	if uint64(residue) >= uint64(totalUnits) {
		return nil, fmt.Errorf("distribution: residue %d paise is not below totalUnits %d; the floor computation is wrong",
			residue, totalUnits)
	}

	// Stage two: order by largest discarded fraction, ties by anchor.
	order := make([]int, len(ents))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if c := ents[ia].RemainderNumerator.Cmp(ents[ib].RemainderNumerator); c != 0 {
			return c > 0
		}
		return bytes.Compare(ents[ia].InvestorAnchor[:], ents[ib].InvestorAnchor[:]) < 0
	})

	for i := range int(residue) {
		ents[order[i]].ResidueAwarded = 1
	}

	// Stage three: totals and the exactness check.
	var grossSum money.Paise
	for i := range ents {
		g, err := ents[i].Floor.Add(ents[i].ResidueAwarded)
		if err != nil {
			return nil, fmt.Errorf("distribution: computing gross: %w", err)
		}
		ents[i].Gross = g
		grossSum, err = grossSum.Add(g)
		if err != nil {
			return nil, fmt.Errorf("distribution: summing gross: %w", err)
		}
	}
	if grossSum != distributed {
		return nil, fmt.Errorf("%w: entitlements sum to %d, distributed is %d, off by %d paise",
			ErrNotExact, grossSum, distributed, grossSum-distributed)
	}

	// Ordering the output by anchor makes the result canonical, which matters
	// because these rows are hashed into the snapshot document and anchored.
	sort.Slice(ents, func(a, b int) bool {
		return bytes.Compare(ents[a].InvestorAnchor[:], ents[b].InvestorAnchor[:]) < 0
	})

	return &Result{
		DistributedPaise:   distributed,
		TotalUnits:         totalUnits,
		Entitlements:       ents,
		ResidueDistributed: residue,
	}, nil
}

// ErrInternalNegativeResidue signals that the sum of floors exceeded the amount
// being distributed, which is arithmetically impossible and therefore a bug.
var ErrInternalNegativeResidue = errors.New("distribution: internal error, negative residue")

// PerUnitRate returns floor(distributed / totalUnits) and the remainder, for
// display and for reconciling against a hand calculation.
func PerUnitRate(distributed money.Paise, totalUnits uint32) (money.Paise, money.Paise, error) {
	if totalUnits == 0 {
		return 0, 0, ErrZeroUnits
	}
	q, r, err := distributed.MulDivFloor(1, int64(totalUnits))
	if err != nil {
		return 0, 0, err
	}
	if !r.IsInt64() {
		return 0, 0 , fmt.Errorf("distribution: remainder does not fit int64")
	}
	return q, money.Paise(r.Int64()), nil
}
