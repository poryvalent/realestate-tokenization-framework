package offer

import (
	"errors"
	"fmt"
	"time"

	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

var (
	ErrPreconditionFailed = errors.New("offer: precondition not satisfied")
	ErrPaused             = errors.New("offer: scheme is paused")
	ErrBookNotFixed       = errors.New("offer: bid book is not frozen")
	ErrAnchorNotConfirmed = errors.New("offer: anchor transaction not confirmed to the required depth")
	ErrNotFeasible        = errors.New("offer: subscription or holder floor not met")
	ErrCeremonyOrder      = errors.New("offer: commit-reveal steps out of order")
	ErrUnitsIssued        = errors.New("offer: units are issued; the offer cannot be aborted")
	ErrSettlementIncomplete = errors.New("offer: settlement has not credited every unit")
)

// MinAnchorConfirmations is the confirmation depth required before the draw proceeds on an anchor.
//
// Five, the same depth the distribution path requires before moving fiat. The reason is the same in
// shape and different in consequence: a reorg that unwound the bid book anchor after the seed was
// committed would leave a commitment bound to a book the chain no longer records, and the draw could
// not be shown to have been fair.
const MinAnchorConfirmations = 5

// AnchorRef is an outbox row carrying one of the offer's chain calls.
type AnchorRef struct {
	OutboxID      string
	Status        string
	Confirmations int
	TxHash        string
}

func (a *AnchorRef) IsConfirmed() bool {
	return a != nil && a.Status == "CONFIRMED" && a.Confirmations >= MinAnchorConfirmations
}

// Terms are the offer's configured parameters.
type Terms struct {
	UnitsOnOffer uint32

	// MinBidUnits and MaxBidUnits bound a single bid.
	MinBidUnits uint32
	MaxBidUnits uint32

	// MinSubscriptionUnits is the floor below which the offer cannot proceed.
	MinSubscriptionUnits uint32

	// MinDistinctHolders is the statutory unitholder floor, at least 200.
	MinDistinctHolders uint32

	PriceBandLowerPaise money.Paise
	PriceBandUpperPaise money.Paise

	OpensAt  time.Time
	ClosesAt time.Time

	// AllotmentDueAt is the deadline for finalising the allotment.
	AllotmentDueAt time.Time
}

// Validate checks the terms against the schema's constraints.
//
// Every rule here is also a CHECK constraint. Duplicated so a misconfigured offer is refused with a
// sentence rather than a constraint name, and so the feasibility arithmetic can be explained at the
// point it is applied.
func (t Terms) Validate() error {
	var problems []string

	if t.UnitsOnOffer == 0 {
		problems = append(problems, "units on offer must be positive")
	}
	if t.MinBidUnits < 1 {
		problems = append(problems, "minimum bid must be at least one whole unit")
	}
	if t.MaxBidUnits < t.MinBidUnits {
		problems = append(problems, fmt.Sprintf(
			"maximum bid %d is below the minimum %d", t.MaxBidUnits, t.MinBidUnits))
	}
	if t.MaxBidUnits > t.UnitsOnOffer {
		problems = append(problems, fmt.Sprintf(
			"maximum bid %d exceeds the %d units on offer", t.MaxBidUnits, t.UnitsOnOffer))
	}
	if t.MinDistinctHolders < 200 {
		problems = append(problems, fmt.Sprintf(
			"holder floor %d is below the statutory minimum of 200", t.MinDistinctHolders))
	}
	if t.MinDistinctHolders > t.UnitsOnOffer {
		problems = append(problems, fmt.Sprintf(
			"holder floor %d exceeds the %d units on offer; every holder needs at least one whole unit",
			t.MinDistinctHolders, t.UnitsOnOffer))
	}
	if t.MinSubscriptionUnits == 0 || t.MinSubscriptionUnits > t.UnitsOnOffer {
		problems = append(problems, fmt.Sprintf(
			"minimum subscription %d must be between 1 and the %d units on offer",
			t.MinSubscriptionUnits, t.UnitsOnOffer))
	}
	if t.PriceBandLowerPaise < money.MinUnitPricePaise {
		problems = append(problems, fmt.Sprintf(
			"price band floor %d paise is below the statutory minimum unit price of %d",
			t.PriceBandLowerPaise, money.MinUnitPricePaise))
	}
	if t.PriceBandUpperPaise < t.PriceBandLowerPaise {
		problems = append(problems, "price band upper bound is below the lower bound")
	}
	if !t.ClosesAt.After(t.OpensAt) {
		problems = append(problems, "the offer must close after it opens")
	}
	if t.AllotmentDueAt.Before(t.ClosesAt) {
		problems = append(problems, "allotment cannot be due before the offer closes")
	}

	// The feasibility arithmetic, stated rather than implied.
	//
	// Reaching the holder floor requires min_distinct_holders - 1 other holders to take at least one
	// unit each, so the largest a single bid can be and still leave room is
	// units_on_offer - (min_distinct_holders - 1). A cap above that makes the floor unreachable by
	// construction: the offer could be fully subscribed in rupees and still be unlistable, and the only
	// way to discover it would be a failed draw after the book was already anchored.
	if t.UnitsOnOffer > 0 && t.MinDistinctHolders > 0 {
		headroom := int64(t.UnitsOnOffer) - int64(t.MinDistinctHolders-1)
		if headroom < 1 {
			problems = append(problems, fmt.Sprintf(
				"a holder floor of %d leaves no room in %d units",
				t.MinDistinctHolders, t.UnitsOnOffer))
		} else if int64(t.MaxBidUnits) > headroom {
			problems = append(problems, fmt.Sprintf(
				"maximum bid %d exceeds %d, the largest cap that still leaves one unit for each of "+
					"the other %d holders needed to reach the floor of %d",
				t.MaxBidUnits, headroom, t.MinDistinctHolders-1, t.MinDistinctHolders))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrPreconditionFailed, joinProblems(problems))
	}
	return nil
}

// MaxBidHeadroom returns the largest bid cap compatible with the holder floor.
func (t Terms) MaxBidHeadroom() int64 {
	return int64(t.UnitsOnOffer) - int64(t.MinDistinctHolders-1)
}

// BookState summarises the bid book at a point in the lifecycle.
type BookState struct {
	// BidCount is every bid recorded, including those that failed validation.
	BidCount int

	// InBookCount is the bids that passed validation and hold a funds block.
	InBookCount int

	// FundsBlockedCount is the bids whose money is confirmed blocked.
	//
	// Tracked separately from InBookCount so the gap is visible. A bid in the book without a confirmed
	// block is a bid that could be allotted units it cannot pay for.
	FundsBlockedCount int

	// UnitsBid is the total demand from in-book bids.
	UnitsBid uint32

	// DistinctBidders is the number of distinct investors in the book.
	DistinctBidders uint32

	// PendingValidation is bids still awaiting a KYC, Demat or funds-block outcome.
	PendingValidation int
}

// Ceremony is the commit-reveal state.
type Ceremony struct {
	BidbookRoot merkle.Hash

	// BidbookAnchor is the outbox row carrying anchorBidbook.
	BidbookAnchor *AnchorRef

	SeedCommitment merkle.Hash

	// CommitmentAnchor carries commitSeed. The database refuses to store the secret until this is
	// recorded, so the ordering is enforced twice.
	CommitmentAnchor *AnchorRef

	// TargetBlock is the block whose hash will be mixed into the seed.
	TargetBlock uint64

	// TargetBlockHash is populated once the target block exists.
	TargetBlockHash merkle.Hash

	// SecretRevealed reports whether the plaintext has been published.
	SecretRevealed bool

	FinalSeed  merkle.Hash
	ResultRoot merkle.Hash

	// ResultAnchor carries anchorBallotResult.
	ResultAnchor *AnchorRef

	// Attempt counts commitment windows used. Bounded at three, after which the draw requires trustee
	// escalation.
	Attempt int

	Escalated bool
}

// Settlement mirrors the contract's settlement cursor.
type Settlement struct {
	Begun bool

	ExpectedHolders uint32
	ExpectedUnits   uint32
	CreditedHolders uint32
	CreditedUnits   uint32

	AllotmentFileHash merkle.Hash

	// IMUnitsRecorded reports whether the manager's subscription is on-chain.
	//
	// One of the five checks finaliseSettlement performs, and the one most easily forgotten because the
	// manager does not bid: its units are recorded directly rather than allotted by ballot.
	IMUnitsRecorded bool
}

// Evidence is the accumulated state a transition is judged against.
type Evidence struct {
	Paused bool

	Terms Terms

	// Now is business time, supplied rather than read so the guards stay pure.
	Now time.Time

	Book BookState

	// Feasible and FeasibilityReason record the outcome of the pre-anchor check.
	Feasible         bool
	FeasibilityReason string

	Ceremony   Ceremony
	Settlement Settlement

	// BidbookPinned and AllotmentFilePinned report IPFS availability.
	BidbookPinned       bool
	AllotmentFilePinned bool

	// AllocationsRecorded is the number of allocation rows written.
	AllocationsRecorded int

	// AbortReason is required to abort.
	AbortReason string
}

// Guard checks the precondition for entering a status.
func Guard(from, to Status, ev Evidence) error {
	if err := CheckTransition(from, to); err != nil {
		return err
	}
	if from == to {
		return nil
	}

	// Pause blocks the lifecycle but never an abort.
	//
	// Refusing to abandon an offer while paused would trap the one action most likely to be needed
	// during an emergency, and an abort under ASBA releases money rather than moving it.
	if ev.Paused && to != StatusAborted {
		return fmt.Errorf("%w: cannot advance to %s while paused", ErrPaused, to)
	}

	switch to {
	case StatusAborted:
		if from.UnitsIssued() {
			return fmt.Errorf("%w: units were credited at settlement and cannot be recalled",
				ErrUnitsIssued)
		}
		if ev.AbortReason == "" {
			return fmt.Errorf("%w: an abort must record why", ErrPreconditionFailed)
		}
		return nil

	case StatusOpen:
		if err := ev.Terms.Validate(); err != nil {
			return err
		}
		if ev.Now.Before(ev.Terms.OpensAt) {
			return fmt.Errorf("%w: the offer opens at %s, business time is %s",
				ErrPreconditionFailed, iso(ev.Terms.OpensAt), iso(ev.Now))
		}
		return nil

	case StatusClosed:
		// Closing early is permitted; closing before opening is not.
		if ev.Book.BidCount == 0 {
			return fmt.Errorf("%w: no bids were recorded", ErrPreconditionFailed)
		}
		return nil

	case StatusBookFrozen:
		// Freezing with validation outstanding would fix a book whose membership is still unresolved.
		if ev.Book.PendingValidation > 0 {
			return fmt.Errorf("%w: %d bid(s) are still awaiting validation; freezing now would fix a "+
				"book that is still changing", ErrPreconditionFailed, ev.Book.PendingValidation)
		}
		if ev.Book.InBookCount == 0 {
			return fmt.Errorf("%w: no bid survived validation", ErrPreconditionFailed)
		}

		// Every bid in the book must hold a confirmed funds block. A bid without one could be allotted
		// units it cannot pay for, and the shortfall would surface only at debit time, after the cap
		// table was anchored.
		if ev.Book.FundsBlockedCount != ev.Book.InBookCount {
			return fmt.Errorf("%w: %d of %d in-book bids have confirmed funds blocked",
				ErrPreconditionFailed, ev.Book.FundsBlockedCount, ev.Book.InBookCount)
		}
		return nil

	case StatusFeasibilityChecked:
		if !from.BookIsFixed() {
			return fmt.Errorf("%w: feasibility must be tested against a frozen book", ErrBookNotFixed)
		}
		// Reaching this state records an answer, not necessarily a pass. The pass is required to
		// proceed to anchoring, which is the next guard.
		return nil

	case StatusBidbookAnchored:
		// The feasibility verdict gates anchoring, deliberately.
		//
		// Anchoring is the first irreversible public act. Testing the floors beforehand means an offer
		// that cannot list is abandoned quietly, rather than anchored, drawn, and then found unlistable
		// with the evidence already permanent.
		if !ev.Feasible {
			return fmt.Errorf("%w: %s", ErrNotFeasible, ev.FeasibilityReason)
		}
		if ev.Ceremony.BidbookRoot.IsZero() {
			return fmt.Errorf("%w: bid book root is unset", ErrPreconditionFailed)
		}
		if !ev.BidbookPinned {
			return fmt.Errorf("%w: the bid book document is not pinned, so the anchored root would "+
				"commit to evidence nobody can fetch", ErrPreconditionFailed)
		}
		return nil

	case StatusSeedCommitted:
		// Mirrors the ballot_runs_enforce_reveal_order trigger: a commitment cannot exist before the
		// bid book root does, so the draw is bound to a bid set nobody can still change.
		if ev.Ceremony.BidbookRoot.IsZero() {
			return fmt.Errorf("%w: the bid book must be anchored before a seed is committed",
				ErrCeremonyOrder)
		}
		if !ev.Ceremony.BidbookAnchor.IsConfirmed() {
			return fmt.Errorf("%w: the bid book anchor needs %d confirmations before the draw is "+
				"bound to it; a reorg afterwards would leave the commitment bound to a book the "+
				"chain no longer records", ErrAnchorNotConfirmed, MinAnchorConfirmations)
		}
		if ev.Ceremony.SeedCommitment.IsZero() {
			return fmt.Errorf("%w: no commitment", ErrPreconditionFailed)
		}
		if ev.Ceremony.Attempt > 3 {
			return fmt.Errorf("%w: %d commitment windows have been used; the draw now requires "+
				"trustee escalation", ErrPreconditionFailed, ev.Ceremony.Attempt)
		}
		return nil

	case StatusSeedRevealed:
		// The same ordering the trigger enforces on the way into the database.
		if ev.Ceremony.SeedCommitment.IsZero() {
			return fmt.Errorf("%w: nothing was committed", ErrCeremonyOrder)
		}
		if !ev.Ceremony.CommitmentAnchor.IsConfirmed() {
			return fmt.Errorf("%w: the commitment must be confirmed on-chain before the secret is "+
				"published, or the operator could claim a different secret was always intended",
				ErrCeremonyOrder)
		}
		if ev.Ceremony.TargetBlock == 0 {
			return fmt.Errorf("%w: no target block", ErrCeremonyOrder)
		}
		if ev.Ceremony.TargetBlockHash.IsZero() {
			return fmt.Errorf("%w: the target block hash is not yet available", ErrPreconditionFailed)
		}
		return nil

	case StatusBallotDrawn:
		if !ev.Ceremony.SecretRevealed {
			return fmt.Errorf("%w: the secret has not been revealed", ErrCeremonyOrder)
		}
		if ev.Ceremony.FinalSeed.IsZero() {
			return fmt.Errorf("%w: no final seed", ErrPreconditionFailed)
		}
		if ev.Ceremony.ResultRoot.IsZero() {
			return fmt.Errorf("%w: the draw produced no result root", ErrPreconditionFailed)
		}
		if ev.AllocationsRecorded != ev.Book.InBookCount {
			// Every bid gets an outcome, including a nil one. A book of 900 bids for 475 units produces
			// 900 allocations, most of them nil, because a losing bidder needs a published result to
			// check their own rank against.
			return fmt.Errorf("%w: %d allocations for %d bids in the book; every bid needs an "+
				"outcome, including a nil one", ErrPreconditionFailed,
				ev.AllocationsRecorded, ev.Book.InBookCount)
		}
		return nil

	case StatusAllotmentFinalised:
		if ev.Ceremony.ResultRoot.IsZero() {
			return fmt.Errorf("%w: no result root to anchor", ErrPreconditionFailed)
		}
		if !ev.Ceremony.ResultAnchor.IsConfirmed() {
			return fmt.Errorf("%w: the ballot result anchor needs %d confirmations before settlement "+
				"binds to it", ErrAnchorNotConfirmed, MinAnchorConfirmations)
		}
		if !ev.AllotmentFilePinned {
			return fmt.Errorf("%w: the allotment file is not pinned, so no bidder could reproduce "+
				"the draw", ErrPreconditionFailed)
		}
		return nil

	case StatusSettled:
		// Everything finaliseSettlement will check, checked before the transaction is signed. A revert
		// costs gas and names a Solidity selector; refusing here names the missing quantity.
		if !ev.Settlement.Begun {
			return fmt.Errorf("%w: settlement has not been opened", ErrSettlementIncomplete)
		}
		if ev.Settlement.AllotmentFileHash.IsZero() {
			return fmt.Errorf("%w: settlement is not bound to an allotment file", ErrSettlementIncomplete)
		}
		if ev.Settlement.CreditedUnits != ev.Settlement.ExpectedUnits {
			return fmt.Errorf("%w: %d of %d units credited", ErrSettlementIncomplete,
				ev.Settlement.CreditedUnits, ev.Settlement.ExpectedUnits)
		}
		if ev.Settlement.CreditedHolders != ev.Settlement.ExpectedHolders {
			return fmt.Errorf("%w: %d of %d holders credited", ErrSettlementIncomplete,
				ev.Settlement.CreditedHolders, ev.Settlement.ExpectedHolders)
		}

		// The manager's units are recorded rather than allotted, which is exactly why this is easy to
		// miss: the manager never appears in the bid book, so a settlement built from ballot output
		// alone would be short by its holding and finaliseSettlement would revert on the third check.
		if !ev.Settlement.IMUnitsRecorded {
			return fmt.Errorf("%w: the manager's subscription is not recorded on-chain. It is not "+
				"allotted by ballot, so it has to be credited separately", ErrSettlementIncomplete)
		}

		if ev.Settlement.ExpectedHolders < ev.Terms.MinDistinctHolders {
			return fmt.Errorf("%w: %d holders against a floor of %d", ErrNotFeasible,
				ev.Settlement.ExpectedHolders, ev.Terms.MinDistinctHolders)
		}
		return nil

	default:
		return fmt.Errorf("%w: no guard defined for %s", ErrPreconditionFailed, to)
	}
}

// CanAdvance reports whether a transition would be accepted.
func CanAdvance(from, to Status, ev Evidence) bool { return Guard(from, to, ev) == nil }

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func joinProblems(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
