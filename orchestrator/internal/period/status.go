// Package period orchestrates one distribution period from rent collection to closure.
//
// # Three layers enforce the same rules, on purpose
//
// The transition rules here are also enforced by Postgres triggers and, for the subset the chain
// sees, by the scheme contract. That is not redundancy for its own sake. The database is the last
// line and it catches a rogue psql session or a second process, but its error arrives as a trigger
// exception halfway through a distribution run, naming a constraint rather than a decision. This
// layer exists to refuse the same thing earlier and say why, and to be readable by someone deciding
// whether the system is safe before they trust it with money.
//
// Where the three disagree, the database wins by construction. So every guard here is written against
// what migrations 0006 and 0008 actually enforce, and the tests assert that correspondence rather
// than assuming it.
package period

import (
	"errors"
	"fmt"
)

// Status is an off-chain distribution period state.
//
// Mirrors the period_status Postgres enum exactly. Fourteen states off-chain against six on-chain:
// the chain records only what has been committed, while the orchestrator has to track work in
// progress. Collapsing them to match the contract would mean losing the distinction between "NDCF
// drafted" and "NDCF approved by two people", which is the distinction the four-eyes rule is made of.
type Status string

const (
	// StatusOpen is a period accepting rent receipts and cost line items.
	StatusOpen Status = "OPEN"

	// StatusRentCollected means collection is complete and variances are explained.
	StatusRentCollected Status = "RENT_COLLECTED"

	// StatusNDCFDrafted means the deduction chain is assembled and NDCF computed.
	StatusNDCFDrafted Status = "NDCF_DRAFTED"

	// StatusNDCFApproved means two distinct actors have signed off the figures.
	StatusNDCFApproved Status = "NDCF_APPROVED"

	// StatusRecordDateDeclared means the record date is fixed and published.
	StatusRecordDateDeclared Status = "RECORD_DATE_DECLARED"

	// StatusSnapshotTaken means the register is frozen and its root computed.
	StatusSnapshotTaken Status = "SNAPSHOT_TAKEN"

	// StatusReconciled means our register agrees with the depository.
	StatusReconciled Status = "RECONCILED"

	// StatusChainStale means a divergence was found and everything downstream is blocked.
	//
	// Not an error state to be cleared quietly. The depository is the legal register, so a
	// disagreement means our view of who owns what is wrong, and distributing on a wrong register
	// pays the wrong people.
	StatusChainStale Status = "CHAIN_STALE"

	// StatusAnchored means the period figures and snapshot root are committed on-chain.
	StatusAnchored Status = "ANCHORED"

	// StatusEntitlementsAnchored means every unit has an entitlement recorded on-chain.
	StatusEntitlementsAnchored Status = "ENTITLEMENTS_ANCHORED"

	// StatusPayoutInstructed means payout instructions exist with the provider.
	//
	// Has no on-chain counterpart. The chain goes from EntitlementsAnchored straight to
	// PayoutsConfirmed, because an instruction is not an outcome and there is nothing worth
	// committing about money that has not moved.
	StatusPayoutInstructed Status = "PAYOUT_INSTRUCTED"

	// StatusPayoutsConfirmed means the settled count and amount are committed on-chain.
	StatusPayoutsConfirmed Status = "PAYOUTS_CONFIRMED"

	// StatusClosed is the terminal success state.
	StatusClosed Status = "CLOSED"

	// StatusReversed means the period was unwound before any fiat settled.
	//
	// Reachable only from ANCHORED and ENTITLEMENTS_ANCHORED. Both the contract and a Postgres
	// trigger refuse a reversal once payouts are confirmed or closed, because by then money has left
	// the escrow. A late bank reversal after settlement is handled as a carry-forward adjustment in a
	// subsequent period, not by rewriting anchored history.
	StatusReversed Status = "REVERSED"
)

// MinAnchorConfirmations is the confirmation depth required before fiat may move.
//
// Five, matching the outbox_confirmed_has_evidence constraint and the enforce_payout_anchor_confirmed
// trigger. The number exists because the most dangerous operation in the architecture is reading chain
// state and then irreversibly transferring money: a reorg after the transfer has cleared cannot be
// undone by any amount of subsequent reconciliation.
const MinAnchorConfirmations = 5

var (
	ErrUnknownStatus     = errors.New("period: unknown status")
	ErrIllegalTransition = errors.New("period: illegal status transition")
)

// transitions is the permitted graph, mirroring the Postgres enum and triggers.
var transitions = map[Status][]Status{
	StatusOpen:          {StatusRentCollected, StatusChainStale},
	StatusRentCollected: {StatusNDCFDrafted, StatusChainStale},

	// Drafted can go back to drafted is not modelled; a correction re-enters NDCF_DRAFTED from
	// itself, which CheckTransition treats as an idempotent restatement.
	StatusNDCFDrafted: {StatusNDCFApproved, StatusChainStale},

	StatusNDCFApproved:       {StatusRecordDateDeclared, StatusChainStale},
	StatusRecordDateDeclared: {StatusSnapshotTaken, StatusChainStale},
	StatusSnapshotTaken:      {StatusReconciled, StatusChainStale},

	// Reconciled is the only state anchoring can proceed from.
	StatusReconciled: {StatusAnchored, StatusChainStale},

	// CHAIN_STALE returns to SNAPSHOT_TAKEN once the divergence is resolved, so the snapshot is
	// reconciled again rather than being trusted from before the divergence was found.
	StatusChainStale: {StatusSnapshotTaken},

	StatusAnchored: {StatusEntitlementsAnchored, StatusReversed},

	// From entitlements anchored, either payouts are instructed or the period is reversed. This is
	// the last point at which reversal is possible.
	StatusEntitlementsAnchored: {StatusPayoutInstructed, StatusReversed},

	// No reversal edge from here on. Both the contract and a trigger refuse it: fiat has settled.
	StatusPayoutInstructed:   {StatusPayoutsConfirmed},
	StatusPayoutsConfirmed:   {StatusClosed},

	StatusClosed:   {},
	StatusReversed: {},
}

// AllStatuses lists every state in lifecycle order.
func AllStatuses() []Status {
	return []Status{
		StatusOpen, StatusRentCollected, StatusNDCFDrafted, StatusNDCFApproved,
		StatusRecordDateDeclared, StatusSnapshotTaken, StatusReconciled, StatusChainStale,
		StatusAnchored, StatusEntitlementsAnchored, StatusPayoutInstructed,
		StatusPayoutsConfirmed, StatusClosed, StatusReversed,
	}
}

// Valid reports whether the status is one the enum defines.
func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// CanTransitionTo reports whether the graph permits the move.
func (s Status) CanTransitionTo(next Status) bool {
	for _, a := range transitions[s] {
		if a == next {
			return true
		}
	}
	return false
}

// CheckTransition validates a move against the graph, before any precondition is considered.
func CheckTransition(from, to Status) error {
	if !from.Valid() {
		return fmt.Errorf("%w: from %q", ErrUnknownStatus, from)
	}
	if !to.Valid() {
		return fmt.Errorf("%w: to %q", ErrUnknownStatus, to)
	}
	if from == to {
		return nil
	}
	if !from.CanTransitionTo(to) {
		return fmt.Errorf("%w: %s to %s", ErrIllegalTransition, from, to)
	}
	return nil
}

// IsTerminal reports whether the period has finished.
func (s Status) IsTerminal() bool { return s == StatusClosed || s == StatusReversed }

// IsReversible reports whether the period can still be unwound on-chain.
//
// Only ANCHORED and ENTITLEMENTS_ANCHORED, matching approveReversal and reversePeriod in the
// contract. Before settlement a mistake is corrected by unwinding; after it, money has left the
// escrow and the only honest remedy is a compensating entry that is itself visible.
func (s Status) IsReversible() bool {
	return s == StatusAnchored || s == StatusEntitlementsAnchored
}

// FiatHasSettled reports whether money has left the escrow.
func (s Status) FiatHasSettled() bool {
	return s == StatusPayoutsConfirmed || s == StatusClosed
}

// IsOnChain reports whether the state has a counterpart in the contract's PeriodStatus.
//
// The pre-anchor states and PAYOUT_INSTRUCTED do not. Useful when reconciling our status against the
// chain: expecting a chain status for a period that has not been anchored would report a divergence
// where there is none.
func (s Status) IsOnChain() bool {
	switch s {
	case StatusAnchored, StatusEntitlementsAnchored, StatusPayoutsConfirmed,
		StatusClosed, StatusReversed:
		return true
	default:
		return false
	}
}

// ChainStatus maps an off-chain status to the contract's enum ordinal.
//
// Returns false for states the chain does not model, rather than a zero that would read as None.
func (s Status) ChainStatus() (uint8, bool) {
	switch s {
	case StatusAnchored:
		return 1, true // PeriodStatus.Anchored
	case StatusEntitlementsAnchored:
		return 2, true // PeriodStatus.EntitlementsAnchored
	case StatusPayoutsConfirmed:
		return 3, true // PeriodStatus.PayoutsConfirmed
	case StatusClosed:
		return 4, true // PeriodStatus.Closed
	case StatusReversed:
		return 5, true // PeriodStatus.Reversed
	default:
		return 0, false
	}
}

// BlocksPayout reports whether fiat movement is forbidden in this state.
func (s Status) BlocksPayout() bool { return s == StatusChainStale }
