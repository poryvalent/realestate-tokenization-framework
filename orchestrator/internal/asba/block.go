// Package asba blocks subscription money in an investor's own bank account.
//
// # The distinction the whole package exists to hold
//
// ASBA means Application Supported by Blocked Amount. The investor's money never leaves their account
// during the offer: their bank marks it unavailable, and it is debited only if and to the extent that
// units are allotted. Everything not allotted is simply released.
//
// That is not an implementation detail, it is the reason abandoning an offer is cheap. A scheme that
// collected subscription money up front and refunded it would have held hundreds of investors' cash for
// weeks, owed them interest, and made aborting a decision with real victims. Under ASBA an abort costs
// an unblock, and nobody is out of pocket for a day.
//
// So the type below never has a "collected" state, and there is no balance anywhere in this package
// representing money we hold. We hold none.
package asba

import (
	"errors"
	"fmt"
	"time"

	"github.com/acresync/orchestrator/internal/money"
)

// BlockStatus mirrors the asba_block_status Postgres enum.
type BlockStatus string

const (
	// StatusRequested means the block has been asked for and the bank has not yet answered.
	StatusRequested BlockStatus = "REQUESTED"

	// StatusBlocked means the full bid amount is marked unavailable in the investor's account.
	//
	// The money is still theirs and still with their bank. Nothing has moved.
	StatusBlocked BlockStatus = "BLOCKED"

	// StatusDebited means the allotted portion was taken and the remainder released.
	//
	// Terminal, and it covers both halves of the settlement instruction. There is no separate
	// "partially released" state because a debit and its matching release are one operation; see Settle.
	StatusDebited BlockStatus = "DEBITED"

	// StatusUnblocked means the entire amount was released with nothing debited.
	//
	// The outcome for a bid that won nothing, and for every bid when an offer is abandoned.
	StatusUnblocked BlockStatus = "UNBLOCKED"

	// StatusFailed means the bank refused or could not place the block.
	//
	// Nothing is reserved, so the bid cannot enter the book: it would be a bid for units it has no
	// funds behind.
	StatusFailed BlockStatus = "FAILED"
)

var (
	ErrUnknownStatus     = errors.New("asba: unknown block status")
	ErrIllegalTransition = errors.New("asba: illegal block status transition")
	ErrPartialBlock      = errors.New("asba: a block must cover the full requested amount")
	ErrDebitExceedsBlock = errors.New("asba: debit exceeds the blocked amount")
	ErrNotReconciled     = errors.New("asba: debited plus released does not equal blocked")
)

// transitions is the permitted graph.
//
// Narrow on purpose. There is no path from DEBITED or UNBLOCKED back to anything: once the bank has
// acted on a block, the instruction is spent. A failed block is also terminal, because a retry is a new
// request with its own idempotency key rather than a revival of the old one.
var transitions = map[BlockStatus][]BlockStatus{
	StatusRequested: {StatusBlocked, StatusFailed},

	// From blocked, either the allotment debits part or all of it, or the whole amount is released.
	StatusBlocked: {StatusDebited, StatusUnblocked},

	StatusDebited:   {},
	StatusUnblocked: {},
	StatusFailed:    {},
}

func AllStatuses() []BlockStatus {
	return []BlockStatus{StatusRequested, StatusBlocked, StatusDebited, StatusUnblocked, StatusFailed}
}

func (s BlockStatus) Valid() bool {
	_, ok := transitions[s]
	return ok
}

func (s BlockStatus) CanTransitionTo(next BlockStatus) bool {
	for _, a := range transitions[s] {
		if a == next {
			return true
		}
	}
	return false
}

func CheckTransition(from, to BlockStatus) error {
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

// IsTerminal reports whether the bank has finished with this instruction.
func (s BlockStatus) IsTerminal() bool {
	switch s {
	case StatusDebited, StatusUnblocked, StatusFailed:
		return true
	default:
		return false
	}
}

// HoldsFunds reports whether money is currently reserved.
//
// The predicate the book-freeze guard depends on: a bid may only enter the book while its funds are
// genuinely held, because a bid without a block could be allotted units it cannot pay for.
func (s BlockStatus) HoldsFunds() bool { return s == StatusBlocked }

// MoneyMoved reports whether any amount was actually taken from the investor.
//
// Only DEBITED. A released block moved nothing, and a failed one never reserved anything. Used to decide
// whether an abort is still free.
func (s BlockStatus) MoneyMoved() bool { return s == StatusDebited }

// Block is one investor's reserved subscription amount.
type Block struct {
	BidID string

	// Provider and BankRef identify the instruction at the bank.
	Provider string
	BankRef  string

	// RequestedPaise is the full bid amount: units bid times price per unit.
	RequestedPaise money.Paise

	// BlockedPaise is what the bank actually reserved.
	//
	// Equal to RequestedPaise or nothing. The schema enforces the same rule, and the reason is that a
	// partial block is not a smaller valid bid: the bid is for a specific number of units at a specific
	// price, and reserving less would leave it unpayable at exactly the amount it was short. Better to
	// fail the bid and tell the investor than to admit it into the book underfunded.
	BlockedPaise money.Paise

	// DebitedPaise is the allotted portion taken at settlement.
	DebitedPaise money.Paise

	// ReleasedPaise is the unallotted remainder returned to the investor's control.
	//
	// Stored rather than derived, so the reconciliation is provable from what was recorded instead of
	// recomputed from the other two and hoped for.
	ReleasedPaise money.Paise

	Status BlockStatus

	RequestedAt time.Time
	BlockedAt   *time.Time
	DebitedAt   *time.Time
	UnblockedAt *time.Time

	FailureCode string
}

// Reconcile asserts the money identity for one block.
//
// # The invariant
//
//	debited + released == blocked
//
// with no tolerance. Every paise the bank reserved is either taken for units or handed back. A shortfall
// means an investor's money is still frozen with no claim against it; an excess means we released more
// than was ever held, which the bank would refuse and which would surface as a reconciliation break
// nobody could explain.
//
// Checked here rather than only in aggregate because an aggregate that balances can still hide two
// investors whose individual blocks are wrong in opposite directions.
func (b *Block) Reconcile() error {
	switch b.Status {
	case StatusRequested:
		if b.BlockedPaise != 0 || b.DebitedPaise != 0 || b.ReleasedPaise != 0 {
			return fmt.Errorf("%w: %s is still REQUESTED but records movement", ErrNotReconciled, b.BidID)
		}
		return nil

	case StatusFailed:
		// A failed block reserved nothing, so nothing can have moved.
		if b.BlockedPaise != 0 || b.DebitedPaise != 0 || b.ReleasedPaise != 0 {
			return fmt.Errorf("%w: %s FAILED but records movement", ErrNotReconciled, b.BidID)
		}
		if b.FailureCode == "" {
			return fmt.Errorf("asba: %s is FAILED with no failure code", b.BidID)
		}
		return nil

	case StatusBlocked:
		if b.BlockedPaise != b.RequestedPaise {
			return fmt.Errorf("%w: %s blocked %d against a request of %d",
				ErrPartialBlock, b.BidID, b.BlockedPaise, b.RequestedPaise)
		}
		if b.DebitedPaise != 0 || b.ReleasedPaise != 0 {
			return fmt.Errorf("%w: %s is BLOCKED but already records a debit or release",
				ErrNotReconciled, b.BidID)
		}
		if b.BlockedAt == nil {
			return fmt.Errorf("asba: %s is BLOCKED with no timestamp", b.BidID)
		}
		return nil

	case StatusDebited, StatusUnblocked:
		if b.BlockedPaise != b.RequestedPaise {
			return fmt.Errorf("%w: %s blocked %d against a request of %d",
				ErrPartialBlock, b.BidID, b.BlockedPaise, b.RequestedPaise)
		}
		if b.DebitedPaise > b.BlockedPaise {
			return fmt.Errorf("%w: %s debited %d of %d blocked",
				ErrDebitExceedsBlock, b.BidID, b.DebitedPaise, b.BlockedPaise)
		}

		sum, err := b.DebitedPaise.Add(b.ReleasedPaise)
		if err != nil {
			return err
		}
		if sum != b.BlockedPaise {
			return fmt.Errorf("%w: %s debited %d plus released %d is %d, but %d was blocked",
				ErrNotReconciled, b.BidID, b.DebitedPaise, b.ReleasedPaise, sum, b.BlockedPaise)
		}

		if b.Status == StatusUnblocked && b.DebitedPaise != 0 {
			return fmt.Errorf("%w: %s is UNBLOCKED but debited %d; a released block takes nothing",
				ErrNotReconciled, b.BidID, b.DebitedPaise)
		}
		if b.Status == StatusDebited && b.DebitedPaise == 0 {
			return fmt.Errorf("%w: %s is DEBITED but took nothing; it should be UNBLOCKED",
				ErrNotReconciled, b.BidID)
		}
		return nil

	default:
		return fmt.Errorf("%w: %q", ErrUnknownStatus, b.Status)
	}
}

// ReconcileAll checks every block and the aggregate together.
//
// Both, because they catch different things. Per-block catches an individual investor being short;
// the totals catch a systemic error such as a fee being netted somewhere it should not be.
func ReconcileAll(blocks []*Block) (blocked, debited, released money.Paise, err error) {
	for _, b := range blocks {
		if b == nil {
			return 0, 0, 0, errors.New("asba: nil block in the reconciliation set")
		}
		if e := b.Reconcile(); e != nil {
			return 0, 0, 0, e
		}

		if blocked, err = blocked.Add(b.BlockedPaise); err != nil {
			return 0, 0, 0, err
		}
		if debited, err = debited.Add(b.DebitedPaise); err != nil {
			return 0, 0, 0, err
		}
		if released, err = released.Add(b.ReleasedPaise); err != nil {
			return 0, 0, 0, err
		}
	}

	// Blocks still in REQUESTED or FAILED contribute nothing to any total, so the identity holds over
	// the whole set exactly as it does per block.
	sum, err := debited.Add(released)
	if err != nil {
		return 0, 0, 0, err
	}
	if sum != blocked {
		return 0, 0, 0, fmt.Errorf("%w: across %d blocks, debited %d plus released %d is %d against "+
			"%d blocked", ErrNotReconciled, len(blocks), debited, released, sum, blocked)
	}
	return blocked, debited, released, nil
}
