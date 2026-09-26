package ndcf

import (
	"errors"
	"fmt"
	"sort"

	"github.com/acresync/orchestrator/internal/money"
)

var (
	ErrNoLineItems       = errors.New("ndcf: a period needs at least one line item")
	ErrNonPositiveAmount = errors.New("ndcf: line amounts must be positive")
	ErrMissingEvidence   = errors.New("ndcf: every line item needs an evidence digest")
	ErrNotDistributable  = errors.New("ndcf: outflows meet or exceed inflows, nothing is distributable")
	ErrBelowFloor        = errors.New("ndcf: distribution is below the regulatory floor")
	ErrExceedsNDCF       = errors.New("ndcf: distribution exceeds net distributable cash flow")
)

// FloorBps is the regulatory minimum share of NDCF that must be distributed.
//
// 9500 basis points, that is 95%. Expressed in basis points rather than as a fraction so the
// comparison stays in integers end to end; a float here would put a rounding decision between the
// computed figure and a regulatory threshold.
const FloorBps uint16 = 9500

// ReserveConcernBps is the share of gross inflow above which reserve deductions are flagged.
//
// Not a limit, and not enforced. Reserves are the only deduction class with no external counterparty,
// which makes them the natural way to suppress NDCF and distribute 95% of a deliberately small
// number while appearing compliant. Nothing in this package can distinguish prudent reserving from
// that, so it does not pretend to: it computes the ratio and surfaces it, so the figure is in the
// statement an auditor reads rather than buried in a sum.
const ReserveConcernBps uint16 = 1500

// LineItem is one entry in the derivation.
//
// Deliberately carries no Direction field. Direction follows from LineType, so a struct that held
// both could express a contradiction, and the code would then have to choose which to believe.
type LineItem struct {
	LineType    LineType
	AmountPaise money.Paise

	// EvidenceSHA256 is the digest of the supporting document.
	//
	// Required on every line. A deduction without evidence is an assertion, and the deduction chain
	// is what the 95% floor is computed against, so an unevidenced line moves the regulatory number
	// on somebody's word alone.
	EvidenceSHA256 [32]byte

	// SPVRef and PropertyRef are optional UUID references.
	//
	// Properties hang off SPVs rather than schemes, which the database enforces. Both are optional
	// here because scheme-level costs such as the trustee fee belong to neither.
	SPVRef      string
	PropertyRef string

	// Description is free text for the audit trail in Postgres.
	//
	// Never pinned. It is the one field on this struct that could carry a tenant's name or any other
	// personal detail, and IPFS has no delete. The document builder omits it, and the IPFS field
	// allowlist would reject it independently, so two separate mechanisms have to fail before it
	// could leak.
	Description string
}

// Result is the computed derivation.
type Result struct {
	// NDCFPaise is inflows less outflows. Always positive; a non-positive result is an error.
	NDCFPaise money.Paise

	TotalInflowPaise  money.Paise
	TotalOutflowPaise money.Paise

	// ReservePaise is the portion of outflow held back rather than paid to a counterparty.
	ReservePaise money.Paise

	// FeePaise is the portion of outflow paid to service providers.
	FeePaise money.Paise

	// ReserveRatioBps is reserves as a share of gross inflow.
	ReserveRatioBps uint16

	// MinimumDistributablePaise is the smallest amount satisfying the floor.
	MinimumDistributablePaise money.Paise

	// ByType is the total per line type, for the statement and for reconciliation against source
	// records.
	ByType map[LineType]money.Paise

	// LineCount is the number of items the result was computed from.
	LineCount int
}

// ReserveConcern reports whether reserves are large enough to warrant explanation.
func (r Result) ReserveConcern() bool { return r.ReserveRatioBps > ReserveConcernBps }

// Compute derives NDCF from line items.
//
// Every addition is overflow-checked. A wrapped sum does not error, it produces a plausible smaller
// number, and that number would flow into the distribution figure and then onto the chain where it
// cannot be corrected.
func Compute(items []LineItem) (Result, error) {
	if len(items) == 0 {
		return Result{}, ErrNoLineItems
	}

	res := Result{ByType: make(map[LineType]money.Paise, len(items))}

	for i, it := range items {
		dir, err := it.LineType.Direction()
		if err != nil {
			return Result{}, fmt.Errorf("line %d: %w", i, err)
		}

		// The database requires positive amounts, and the IPFS schema types them unsigned. A negative
		// amount is how a deduction would be smuggled in as an inflow, so it is refused here rather
		// than relied upon to fail further down.
		if it.AmountPaise <= 0 {
			return Result{}, fmt.Errorf("%w: line %d (%s) is %d",
				ErrNonPositiveAmount, i, it.LineType, it.AmountPaise)
		}
		if it.EvidenceSHA256 == ([32]byte{}) {
			return Result{}, fmt.Errorf("%w: line %d (%s)", ErrMissingEvidence, i, it.LineType)
		}

		if res.ByType[it.LineType], err = res.ByType[it.LineType].Add(it.AmountPaise); err != nil {
			return Result{}, fmt.Errorf("line %d (%s): %w", i, it.LineType, err)
		}

		if dir == Inflow {
			if res.TotalInflowPaise, err = res.TotalInflowPaise.Add(it.AmountPaise); err != nil {
				return Result{}, fmt.Errorf("total inflow: %w", err)
			}
			continue
		}

		if res.TotalOutflowPaise, err = res.TotalOutflowPaise.Add(it.AmountPaise); err != nil {
			return Result{}, fmt.Errorf("total outflow: %w", err)
		}
		if it.LineType.IsReserve() {
			if res.ReservePaise, err = res.ReservePaise.Add(it.AmountPaise); err != nil {
				return Result{}, fmt.Errorf("total reserves: %w", err)
			}
		}
		if it.LineType.IsFee() {
			if res.FeePaise, err = res.FeePaise.Add(it.AmountPaise); err != nil {
				return Result{}, fmt.Errorf("total fees: %w", err)
			}
		}
	}

	ndcf, err := res.TotalInflowPaise.Sub(res.TotalOutflowPaise)
	if err != nil {
		return Result{}, fmt.Errorf("ndcf: %w", err)
	}

	// A non-positive NDCF is refused rather than reported.
	//
	// The database requires ndcf_paise > 0, and 95% of zero or of a negative number is not a
	// distribution instruction. The period has to stop here and be explained, because the alternative
	// is anchoring a distribution of nothing and calling the quarter settled.
	if ndcf <= 0 {
		return Result{}, fmt.Errorf("%w: inflows %d, outflows %d, net %d",
			ErrNotDistributable, res.TotalInflowPaise, res.TotalOutflowPaise, ndcf)
	}
	res.NDCFPaise = ndcf
	res.LineCount = len(items)

	if res.TotalInflowPaise > 0 {
		if res.ReserveRatioBps, err = res.ReservePaise.BasisPointsOf(res.TotalInflowPaise); err != nil {
			return Result{}, fmt.Errorf("reserve ratio: %w", err)
		}
	}

	if res.MinimumDistributablePaise, err = MinimumDistributable(ndcf, FloorBps); err != nil {
		return Result{}, err
	}

	return res, nil
}

// MinimumDistributable returns the smallest amount that satisfies the floor.
//
// # Why this rounds up
//
// The floor holds when distributed × 10000 ≥ ndcf × floorBps. The smallest such value is
// ndcf × floorBps / 10000 rounded *up*. Rounding down gives a figure that is short by up to one
// paise whenever the division is inexact, and the comparison then fails by exactly that paise.
//
// Which is the kind of defect that survives review, because the number looks right. For an NDCF of
// ₹3,00,00,000.01 the floor share is not an integer, truncation yields a value one paise light, and
// the period is rejected by a constraint asserting a regulatory minimum. The failure would read as a
// compliance breach rather than as a rounding bug.
//
// The arithmetic goes through MulDivFloor, which uses big.Int internally, so the intermediate product
// cannot overflow regardless of scheme size.
func MinimumDistributable(ndcf money.Paise, floorBps uint16) (money.Paise, error) {
	if ndcf <= 0 {
		return 0, fmt.Errorf("%w: ndcf is %d", ErrNotDistributable, ndcf)
	}

	q, rem, err := ndcf.MulDivFloor(int64(floorBps), 10_000)
	if err != nil {
		return 0, fmt.Errorf("minimum distributable: %w", err)
	}
	if rem.Sign() != 0 {
		// Rounding up by one paise. Never more: the remainder is strictly less than the divisor.
		if q, err = q.Add(1); err != nil {
			return 0, fmt.Errorf("minimum distributable: %w", err)
		}
	}

	// Belt and braces. The value returned is the one a regulator's arithmetic is compared against, so
	// it is checked against the floor predicate rather than trusted from its own derivation.
	ok, err := money.MeetsFloorBps(q, ndcf, floorBps)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("ndcf: internal error, %d paise does not satisfy a %d bps floor on %d",
			q, floorBps, ndcf)
	}
	return q, nil
}

// Plan is a proposed distribution for a period.
type Plan struct {
	Result

	DistributedPaise money.Paise

	// DistributionBps is the achieved share, for reporting and for the anchored figure.
	DistributionBps uint16

	// RetainedPaise is NDCF less the distributed amount.
	RetainedPaise money.Paise
}

// PlanDistribution validates a proposed distribution against a computed result.
//
// The distributed amount is an input rather than something this function derives. A scheme may
// distribute more than the floor, and frequently should, so the figure is a decision recorded with
// approvals behind it. What this function does is refuse a decision that breaches the floor or
// exceeds NDCF, which are the two ways the number can be wrong in a way that matters.
func PlanDistribution(res Result, distributed money.Paise) (Plan, error) {
	if distributed > res.NDCFPaise {
		return Plan{}, fmt.Errorf("%w: %d exceeds ndcf of %d",
			ErrExceedsNDCF, distributed, res.NDCFPaise)
	}

	ok, err := money.MeetsFloorBps(distributed, res.NDCFPaise, FloorBps)
	if err != nil {
		return Plan{}, err
	}
	if !ok {
		return Plan{}, fmt.Errorf("%w: %d of %d is below %d bps; the minimum is %d",
			ErrBelowFloor, distributed, res.NDCFPaise, FloorBps, res.MinimumDistributablePaise)
	}

	bps, err := distributed.BasisPointsOf(res.NDCFPaise)
	if err != nil {
		return Plan{}, err
	}
	retained, err := res.NDCFPaise.Sub(distributed)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Result:           res,
		DistributedPaise: distributed,
		DistributionBps:  bps,
		RetainedPaise:    retained,
	}, nil
}

// PlanAtFloor produces the minimum compliant distribution.
func PlanAtFloor(res Result) (Plan, error) {
	return PlanDistribution(res, res.MinimumDistributablePaise)
}

// SortedTypes returns the line types present, in the canonical order.
//
// Ordered so a statement rendered twice from the same data is byte-identical. Map iteration would
// give a different order each run, and since the statement is hashed and the digest anchored, an
// unstable order would mean the same period produced different anchors.
func (r Result) SortedTypes() []LineType {
	order := map[LineType]int{}
	for i, lt := range AllLineTypes() {
		order[lt] = i
	}
	out := make([]LineType, 0, len(r.ByType))
	for lt := range r.ByType {
		out = append(out, lt)
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}
