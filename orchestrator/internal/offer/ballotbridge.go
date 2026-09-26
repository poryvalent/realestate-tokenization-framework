package offer

import (
	"fmt"
	"strings"

	"github.com/acresync/orchestrator/internal/ballot"
)

// BallotParams converts the offer's terms into the allocation engine's parameters.
//
// # Why this conversion exists rather than one shared struct
//
// Terms and ballot.Params overlap on seven fields, which is exactly the shape where two structs drift:
// someone widens a bid cap on the offer and the draw keeps enforcing the old one, or a price band moves
// and the allocator rejects bids the offer accepted. Neither failure is loud.
//
// They stay separate because they answer to different authorities. Terms mirrors the offers table and
// its CHECK constraints; Params is the allocation engine's own contract and is versioned by AlgoVersion
// because changing it invalidates every anchored result. Merging them would couple a schema migration
// to the reproducibility of historical draws.
//
// So the conversion is explicit and one-directional, and a test asserts the two agree on every shared
// field. Drift then fails a test rather than a distribution.
func (t Terms) BallotParams() ballot.Params {
	return ballot.Params{
		UnitsOnOffer:         t.UnitsOnOffer,
		MinBidUnits:          t.MinBidUnits,
		MaxBidUnits:          t.MaxBidUnits,
		MinDistinctHolders:   t.MinDistinctHolders,
		MinSubscriptionUnits: t.MinSubscriptionUnits,
		PriceBandLowerPaise:  t.PriceBandLowerPaise,
		PriceBandUpperPaise:  t.PriceBandUpperPaise,

		// Always true for a v1 scheme, and not a policy this layer gets to choose.
		//
		// A scheme's asset value is total units times unit price, and the SM-REIT band floor of fifty
		// crore is exactly 500 units at ten lakh. There is no room to shrink the scheme to match a
		// partial subscription without dropping below the statutory floor, so a shortfall has to be
		// abandoned or underwritten and can never be absorbed.
		RequireFullSubscription: true,
	}
}

// TermsFromBallotParams builds terms from allocation parameters.
//
// The reverse direction, used by tests and by the demo seeder so a fixture cannot define an offer the
// allocator would reject. Timestamps are not carried because the allocator has no notion of a window.
func TermsFromBallotParams(p ballot.Params) Terms {
	return Terms{
		UnitsOnOffer:         p.UnitsOnOffer,
		MinBidUnits:          p.MinBidUnits,
		MaxBidUnits:          p.MaxBidUnits,
		MinDistinctHolders:   p.MinDistinctHolders,
		MinSubscriptionUnits: p.MinSubscriptionUnits,
		PriceBandLowerPaise:  p.PriceBandLowerPaise,
		PriceBandUpperPaise:  p.PriceBandUpperPaise,
	}
}

// FeasibilityVerdict turns the engine's finding into the evidence a guard consumes.
//
// # Why the reason is assembled here and not left to the caller
//
// "Fully subscribed in rupees but only 51 distinct bidders" is the finding that stops a scheme, and it
// is the one an audience needs stated in full. A boolean plus a generic message would make a holder
// floor failure indistinguishable from an undersubscription, and those have completely different
// commercial responses: one needs more bidders, the other needs more money.
//
// So each failing test is named with its own numbers, and a passing verdict still carries the counts,
// because the operator approving an anchor should see what they are approving.
func FeasibilityVerdict(f ballot.Feasibility) (feasible bool, reason string) {
	if f.Feasible {
		return true, fmt.Sprintf(
			"feasible: %d eligible bids for %d units, %d technical rejection(s)",
			f.EligibleBids, f.TotalUnitsBid, f.TechnicalRejects)
	}

	// The engine's own reasons first, since they carry the numbers.
	if len(f.Reasons) > 0 {
		return false, strings.Join(f.Reasons, "; ")
	}

	// A fallback naming which of the three tests failed, so a verdict is never just "not feasible".
	var failed []string
	if !f.SubscriptionMet {
		failed = append(failed, "minimum subscription not met")
	}
	if !f.HolderFloorMet {
		failed = append(failed, "unitholder floor not met")
	}
	if !f.FullSubscriptionMet {
		failed = append(failed, "the offer is not fully subscribed and a v1 scheme cannot shrink")
	}
	if len(failed) == 0 {
		failed = append(failed, "the engine reported infeasible without naming a failing test")
	}
	return false, strings.Join(failed, "; ")
}

// ApplyFeasibility records a verdict on the evidence.
func (ev *Evidence) ApplyFeasibility(f ballot.Feasibility) {
	ev.Feasible, ev.FeasibilityReason = FeasibilityVerdict(f)
}
