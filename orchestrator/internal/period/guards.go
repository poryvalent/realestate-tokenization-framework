package period

import (
	"errors"
	"fmt"

	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/snapshot"
)

var (
	ErrPreconditionFailed  = errors.New("period: precondition not satisfied")
	ErrFourEyes            = errors.New("period: four-eyes approval not satisfied")
	ErrDiverged            = errors.New("period: unresolved reconciliation divergence")
	ErrAnchorNotConfirmed  = errors.New("period: anchor transaction not confirmed to the required depth")
	ErrPaused              = errors.New("period: scheme is paused")
	ErrPayoutsInFlight     = errors.New("period: payouts are in flight")
	ErrFiatSettled         = errors.New("period: fiat has settled; use a carry-forward adjustment")
	ErrEntitlementsPartial = errors.New("period: entitlements do not cover every unit")
)

// AnchorRef is the outbox row a period's on-chain anchor lives in.
type AnchorRef struct {
	// OutboxID identifies the row. Payout instructions carry it as gated_on_anchor_tx.
	OutboxID string

	// Status is the outbox state, one of QUEUED, SIGNED, BROADCAST, CONFIRMING, CONFIRMED,
	// FAILED, DEAD_LETTER.
	Status string

	Confirmations int
	TxHash        string
	BlockNumber   *uint64
}

// IsConfirmed reports whether the anchor has reached the depth fiat movement requires.
func (a *AnchorRef) IsConfirmed() bool {
	return a != nil && a.Status == "CONFIRMED" && a.Confirmations >= MinAnchorConfirmations
}

// PayoutTally summarises the payout population for a period.
type PayoutTally struct {
	Total int

	// InFlight counts payouts the provider or a bank still holds.
	//
	// A period cannot close or reverse while any of these exist. For IMPS this can persist for up to
	// three working days under a deemed-success outcome, and there is deliberately no timeout: the
	// wait is the correct behaviour, because reissuing during that window pays twice.
	InFlight int

	// CreditConfirmed counts payouts the provider reports as processed.
	CreditConfirmed int

	// NeedingReissue counts payouts that terminally failed without money moving.
	NeedingReissue int

	// Reversed counts credits that were clawed back.
	Reversed int

	// ConfirmedPaise is the sum of amounts for credit-confirmed payouts.
	ConfirmedPaise money.Paise
}

// Resolved reports whether every payout has reached a state that will not change on its own.
func (t PayoutTally) Resolved() bool { return t.Total > 0 && t.InFlight == 0 }

// Evidence is the accumulated state a transition is judged against.
//
// Passed as one value rather than read from a database inside the guards, so the rules are pure
// functions of stated facts and can be exercised exhaustively in a test. A guard that queried for
// itself could only be tested against a live schema, and the interesting cases are the ones that are
// awkward to construct in SQL.
type Evidence struct {
	Paused bool

	// RentVariancesExplained reports whether every receipt with a variance carries a reason. The
	// database requires it; checked here so the failure names the period rather than a constraint.
	RentVariancesExplained bool
	RentReceiptCount       int

	// Plan is the computed NDCF and the distribution decision.
	PlanPresent       bool
	NDCFPaise         money.Paise
	DistributedPaise  money.Paise
	DistributionBps   uint16
	StatementDigestOK bool

	// IMApprovedBy and TrusteeApprovedBy are the two actors of the four-eyes rule.
	IMApprovedBy      string
	TrusteeApprovedBy string

	RecordDateDeclared bool

	// Snapshot is the frozen register.
	Snapshot        *snapshot.Snapshot
	SnapshotPinned  bool
	StatementPinned bool

	// BlockingDivergences counts unresolved reconciliation runs flagged blocks_payout.
	BlockingDivergences int

	// Anchor is the outbox row carrying anchorPeriod.
	Anchor *AnchorRef

	// EntitledUnits and EntitledHolders are what has been anchored so far.
	EntitledUnits   uint32
	EntitledHolders uint32

	Payouts PayoutTally

	// RequireMinimumHolders enforces the 200-unitholder floor. False only for fixtures.
	RequireMinimumHolders bool
}

// Guard checks the precondition for entering a status.
//
// Separate from CheckTransition because legality and readiness are different questions. The graph says
// SNAPSHOT_TAKEN can follow RECORD_DATE_DECLARED; the guard says whether a snapshot actually exists
// and whether its register adds up. Conflating them would make "the move is allowed" and "the work is
// done" indistinguishable.
func Guard(from, to Status, ev Evidence) error {
	// The fiat boundary is explained before the transition graph is consulted.
	//
	// Both refuse the same thing, so the order looks arbitrary. It is not. No status with settled fiat
	// has REVERSED as a legal successor, which means the graph rejects it first and the refusal an
	// operator sees is "illegal status transition" — true, and useless at the one moment they need to be
	// told that the remedy is a carry-forward adjustment.
	//
	// Checking here makes that message reachable. Before this, the ErrFiatSettled branch below could
	// never fire for PAYOUTS_CONFIRMED or CLOSED, so the most carefully worded error in this file was
	// dead code for exactly the two states it was written for.
	if to == StatusReversed && from.FiatHasSettled() {
		return fmt.Errorf("%w: cannot reverse from %s, because money has already left the escrow. "+
			"Unwinding the period now would assert that payments which happened did not; record a "+
			"carry-forward adjustment against a later period instead", ErrFiatSettled, from)
	}

	if err := CheckTransition(from, to); err != nil {
		return err
	}
	if from == to {
		return nil
	}

	// Pause blocks the lifecycle but not reconciliation or reversal.
	//
	// Deliberate asymmetry. Blinding the mirror to the depository during an emergency compounds the
	// emergency, and refusing to unwind a bad period while paused would trap the one action most
	// likely to be needed.
	if ev.Paused && to != StatusChainStale && to != StatusReversed {
		return fmt.Errorf("%w: cannot advance to %s while paused", ErrPaused, to)
	}

	switch to {
	case StatusChainStale:
		// Entered on discovery of a divergence, so it requires evidence of one rather than the
		// absence of one.
		if ev.BlockingDivergences == 0 {
			return fmt.Errorf("%w: CHAIN_STALE requires an unresolved blocking divergence", ErrPreconditionFailed)
		}
		return nil

	case StatusRentCollected:
		if ev.RentReceiptCount == 0 {
			return fmt.Errorf("%w: no rent receipts recorded", ErrPreconditionFailed)
		}
		if !ev.RentVariancesExplained {
			// A silent shortfall is how a distribution quietly stops matching the lease schedule.
			return fmt.Errorf("%w: a receipt variance has no explanation", ErrPreconditionFailed)
		}
		return nil

	case StatusNDCFDrafted:
		if !ev.PlanPresent {
			return fmt.Errorf("%w: no NDCF computation", ErrPreconditionFailed)
		}
		if ev.NDCFPaise <= 0 {
			return fmt.Errorf("%w: NDCF is %d; outflows meet or exceed inflows and there is nothing "+
				"to distribute", ErrPreconditionFailed, ev.NDCFPaise)
		}
		if ev.DistributedPaise > ev.NDCFPaise {
			return fmt.Errorf("%w: distributing %d exceeds NDCF of %d",
				ErrPreconditionFailed, ev.DistributedPaise, ev.NDCFPaise)
		}
		// The floor is checked here rather than at anchoring, so the figures are refused while they
		// are still a draft that can be corrected.
		ok, err := money.MeetsFloorBps(ev.DistributedPaise, ev.NDCFPaise, 9500)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %d of %d is below the 9500 bps floor",
				ErrPreconditionFailed, ev.DistributedPaise, ev.NDCFPaise)
		}
		return nil

	case StatusNDCFApproved:
		// Four eyes, enforced as two present and distinct actors.
		//
		// The database requires the same thing, and the contract requires a separate trustee approval
		// for reversals. One operator approving both halves is not four-eyes, and the check has to be
		// on identity rather than on two timestamps existing.
		if ev.IMApprovedBy == "" {
			return fmt.Errorf("%w: no investment manager approval", ErrFourEyes)
		}
		if ev.TrusteeApprovedBy == "" {
			return fmt.Errorf("%w: no trustee approval", ErrFourEyes)
		}
		if ev.IMApprovedBy == ev.TrusteeApprovedBy {
			return fmt.Errorf("%w: %q approved both halves", ErrFourEyes, ev.IMApprovedBy)
		}
		return nil

	case StatusRecordDateDeclared:
		if !ev.RecordDateDeclared {
			return fmt.Errorf("%w: record date not set", ErrPreconditionFailed)
		}
		return nil

	case StatusSnapshotTaken:
		if ev.Snapshot == nil {
			return fmt.Errorf("%w: register not frozen", ErrPreconditionFailed)
		}
		if ev.Snapshot.MerkleRoot.IsZero() {
			return fmt.Errorf("%w: snapshot root is zero", ErrPreconditionFailed)
		}
		if ev.RequireMinimumHolders && !ev.Snapshot.MeetsHolderMinimum() {
			return fmt.Errorf("%w: %d countable unitholders, minimum is %d",
				ErrPreconditionFailed, ev.Snapshot.DistinctHolders, snapshot.MinUnitholders)
		}
		return nil

	case StatusReconciled:
		if ev.BlockingDivergences > 0 {
			return fmt.Errorf("%w: %d unresolved divergence(s); the depository is the legal register",
				ErrDiverged, ev.BlockingDivergences)
		}
		if ev.Snapshot == nil {
			return fmt.Errorf("%w: nothing to reconcile without a snapshot", ErrPreconditionFailed)
		}
		return nil

	case StatusAnchored:
		// Everything the contract's anchorPeriod will check, checked before a transaction is signed.
		// A revert costs gas and produces an error naming a Solidity selector; refusing here names the
		// missing artefact.
		if ev.BlockingDivergences > 0 {
			return fmt.Errorf("%w: refusing to anchor over %d unresolved divergence(s)",
				ErrDiverged, ev.BlockingDivergences)
		}
		if !ev.PlanPresent || ev.NDCFPaise <= 0 {
			return fmt.Errorf("%w: no NDCF figures to anchor", ErrPreconditionFailed)
		}
		if !ev.StatementDigestOK {
			return fmt.Errorf("%w: NDCF statement digest missing", ErrPreconditionFailed)
		}
		if !ev.StatementPinned {
			// Anchoring a digest whose document is not retrievable commits to evidence nobody can
			// fetch, which is worse than not anchoring: it looks verifiable and is not.
			return fmt.Errorf("%w: NDCF statement not pinned", ErrPreconditionFailed)
		}
		if ev.Snapshot == nil || ev.Snapshot.MerkleRoot.IsZero() {
			return fmt.Errorf("%w: no snapshot root to anchor", ErrPreconditionFailed)
		}
		if !ev.SnapshotPinned {
			return fmt.Errorf("%w: snapshot document not pinned", ErrPreconditionFailed)
		}
		if ev.IMApprovedBy == "" || ev.TrusteeApprovedBy == "" {
			return fmt.Errorf("%w: anchoring requires both approvals", ErrFourEyes)
		}
		if ev.IMApprovedBy == ev.TrusteeApprovedBy {
			return fmt.Errorf("%w: %q approved both halves", ErrFourEyes, ev.IMApprovedBy)
		}
		if ev.DistributionBps < 9500 || ev.DistributionBps > 10_000 {
			return fmt.Errorf("%w: distribution is %d bps, the permitted range is 9500 to 10000",
				ErrPreconditionFailed, ev.DistributionBps)
		}
		return nil

	case StatusEntitlementsAnchored:
		// The contract refuses unless accrued units equal the snapshot total, and a Postgres trigger
		// runs assert_period_entitlements_exact on the same transition. Partial coverage means some
		// holder has no entitlement recorded, and they are the one person who cannot prove what they
		// were owed.
		if ev.Snapshot == nil {
			return fmt.Errorf("%w: no snapshot to measure entitlements against", ErrPreconditionFailed)
		}
		want := ev.Snapshot.EntitlementDenominator()
		if ev.EntitledUnits != want {
			return fmt.Errorf("%w: %d of %d units have entitlements",
				ErrEntitlementsPartial, ev.EntitledUnits, want)
		}
		if ev.EntitledHolders != uint32(len(ev.Snapshot.Lines)) {
			return fmt.Errorf("%w: %d of %d holders have entitlements",
				ErrEntitlementsPartial, ev.EntitledHolders, len(ev.Snapshot.Lines))
		}
		return nil

	case StatusPayoutInstructed:
		// The gate that stops a reorg from costing real money.
		//
		// Mirrors enforce_payout_anchor_confirmed: the anchor row must be CONFIRMED with at least five
		// confirmations. Reading chain state and then irreversibly transferring is the single most
		// dangerous sequence in the architecture, because a reorg after the transfer clears cannot be
		// undone.
		if ev.Anchor == nil {
			return fmt.Errorf("%w: no anchor transaction recorded", ErrAnchorNotConfirmed)
		}
		if ev.Anchor.Status != "CONFIRMED" {
			return fmt.Errorf("%w: anchor %s is %s, not CONFIRMED",
				ErrAnchorNotConfirmed, ev.Anchor.OutboxID, ev.Anchor.Status)
		}
		if ev.Anchor.Confirmations < MinAnchorConfirmations {
			return fmt.Errorf("%w: anchor %s has %d confirmations, %d required",
				ErrAnchorNotConfirmed, ev.Anchor.OutboxID, ev.Anchor.Confirmations, MinAnchorConfirmations)
		}
		// Mirrors enforce_no_payout_while_diverged.
		if ev.BlockingDivergences > 0 {
			return fmt.Errorf("%w: %d unresolved divergence(s); resolve before distributing",
				ErrDiverged, ev.BlockingDivergences)
		}
		return nil

	case StatusPayoutsConfirmed:
		if ev.Payouts.Total == 0 {
			return fmt.Errorf("%w: no payouts to confirm", ErrPreconditionFailed)
		}
		if ev.Payouts.InFlight > 0 {
			// Confirming while instructions are outstanding would commit a settled figure that is
			// still moving, and the anchored number would be wrong the moment the next webhook lands.
			return fmt.Errorf("%w: %d of %d payouts are still in flight",
				ErrPayoutsInFlight, ev.Payouts.InFlight, ev.Payouts.Total)
		}
		return nil

	case StatusClosed:
		if ev.Payouts.InFlight > 0 {
			return fmt.Errorf("%w: %d payouts are still in flight", ErrPayoutsInFlight, ev.Payouts.InFlight)
		}
		return nil

	case StatusReversed:
		// Reversal is legal only before fiat settles.
		//
		// The graph already excludes PAYOUTS_CONFIRMED and CLOSED, and the contract and a trigger both
		// refuse the same thing. Repeated here because the message is the useful part: the remedy
		// after settlement is a carry-forward adjustment, and an operator hitting this needs to be
		// told that rather than left looking for a way to force it.
		if from.FiatHasSettled() {
			return fmt.Errorf("%w: cannot reverse from %s", ErrFiatSettled, from)
		}
		if ev.Payouts.InFlight > 0 || ev.Payouts.CreditConfirmed > 0 {
			return fmt.Errorf("%w: %d in flight and %d settled; use a carry-forward adjustment",
				ErrFiatSettled, ev.Payouts.InFlight, ev.Payouts.CreditConfirmed)
		}
		if ev.TrusteeApprovedBy == "" {
			return fmt.Errorf("%w: reversal requires trustee approval", ErrFourEyes)
		}
		return nil

	default:
		return fmt.Errorf("%w: no guard defined for %s", ErrPreconditionFailed, to)
	}
}

// CanAdvance reports whether a transition would be accepted, without performing it.
func CanAdvance(from, to Status, ev Evidence) bool { return Guard(from, to, ev) == nil }

// NextExpected returns the ordinary next state in the happy path.
//
// Used for progress reporting and for a runbook to say what is outstanding. Deliberately does not
// consider readiness: it answers "what comes next", not "can it happen yet".
func NextExpected(s Status) (Status, bool) {
	switch s {
	case StatusOpen:
		return StatusRentCollected, true
	case StatusRentCollected:
		return StatusNDCFDrafted, true
	case StatusNDCFDrafted:
		return StatusNDCFApproved, true
	case StatusNDCFApproved:
		return StatusRecordDateDeclared, true
	case StatusRecordDateDeclared:
		return StatusSnapshotTaken, true
	case StatusSnapshotTaken:
		return StatusReconciled, true
	case StatusReconciled:
		return StatusAnchored, true
	case StatusChainStale:
		return StatusSnapshotTaken, true
	case StatusAnchored:
		return StatusEntitlementsAnchored, true
	case StatusEntitlementsAnchored:
		return StatusPayoutInstructed, true
	case StatusPayoutInstructed:
		return StatusPayoutsConfirmed, true
	case StatusPayoutsConfirmed:
		return StatusClosed, true
	default:
		return "", false
	}
}
