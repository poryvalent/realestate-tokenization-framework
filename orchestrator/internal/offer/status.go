// Package offer orchestrates one primary-market offer from configuration to a settled cap table.
//
// # The boundary that shapes this lifecycle
//
// Under ASBA an investor's money is blocked in their own bank account, not debited to ours. Nothing
// moves until allotment. That makes abandoning an offer genuinely cheap right up until settlement: an
// abort unblocks funds that never left anybody's account, and no investor is out of pocket for a day.
//
// After settlement, units exist on the register and in a Demat account. There is no unwind.
//
// So the graph below has an abort edge from every pre-settlement state and none from SETTLED, which is
// the same shape as the distribution period's reversal boundary and for the same underlying reason:
// the point where value actually moves is the point where remedies stop being reversals and start
// being compensations.
package offer

import (
	"errors"
	"fmt"
)

// Status is an offer lifecycle state, mirroring the offer_status Postgres enum exactly.
type Status string

const (
	// StatusConfigured is an offer whose terms are set and which is not yet accepting bids.
	StatusConfigured Status = "CONFIGURED"

	// StatusOpen is accepting bids.
	StatusOpen Status = "OPEN"

	// StatusClosed has stopped accepting bids. The book is not yet fixed.
	StatusClosed Status = "CLOSED"

	// StatusBookFrozen means the set of bids in the book is final.
	//
	// Distinct from CLOSED because closing stops intake while validation continues: a bid submitted
	// before the deadline may still fail its funds block or its KYC check afterwards. Freezing is the
	// moment the membership of the book stops changing, and it has to happen before the root is
	// computed or the root would describe a book that could still move.
	StatusBookFrozen Status = "BOOK_FROZEN"

	// StatusFeasibilityChecked means the book has been tested against the subscription and holder
	// floors, and the answer recorded before anything was anchored.
	StatusFeasibilityChecked Status = "FEASIBILITY_CHECKED"

	// StatusBidbookAnchored means the frozen book's root is committed on-chain.
	StatusBidbookAnchored Status = "BIDBOOK_ANCHORED"

	// StatusSeedCommitted means the operator has published a commitment to a secret they cannot now
	// change.
	StatusSeedCommitted Status = "SEED_COMMITTED"

	// StatusSeedRevealed means the secret is public and the final seed is derived from it together
	// with a blockhash nobody could predict at commitment time.
	StatusSeedRevealed Status = "SEED_REVEALED"

	// StatusBallotDrawn means the allocation has been computed from the revealed seed.
	StatusBallotDrawn Status = "BALLOT_DRAWN"

	// StatusAllotmentFinalised means the result root is anchored and the allotment file pinned.
	StatusAllotmentFinalised Status = "ALLOTMENT_FINALISED"

	// StatusSettled is the terminal success state: units credited, cap table closed.
	StatusSettled Status = "SETTLED"

	// StatusAborted is the terminal failure state.
	//
	// Reachable from every state before SETTLED. Not an error condition: an offer that fails its
	// minimum subscription or its unitholder floor has had a normal commercial outcome, and the
	// contract treats Undersubscribed the same way.
	StatusAborted Status = "ABORTED"
)

var (
	ErrUnknownStatus     = errors.New("offer: unknown status")
	ErrIllegalTransition = errors.New("offer: illegal status transition")
)

// transitions is the permitted graph.
//
// Every pre-settlement state carries an abort edge, because under ASBA abandoning an offer costs an
// unblock rather than a refund. Adding those edges is not defensive clutter: without them, an offer
// that failed its holder floor at the feasibility check would have no legal way to stop.
var transitions = map[Status][]Status{
	StatusConfigured:         {StatusOpen, StatusAborted},
	StatusOpen:               {StatusClosed, StatusAborted},
	StatusClosed:             {StatusBookFrozen, StatusAborted},
	StatusBookFrozen:         {StatusFeasibilityChecked, StatusAborted},
	StatusFeasibilityChecked: {StatusBidbookAnchored, StatusAborted},
	StatusBidbookAnchored:    {StatusSeedCommitted, StatusAborted},

	// Seed committed can return to itself on a recommit.
	//
	// A recommit is a real transition in the ceremony but not a change of state: the stored commitment
	// is immutable and only the target block moves, so the offer is still waiting to reveal. Modelled
	// as a self-transition, which CheckTransition treats as an idempotent restatement, rather than as
	// a separate state that would have to be explained to anyone reading the enum.
	StatusSeedCommitted: {StatusSeedRevealed, StatusAborted},

	StatusSeedRevealed:       {StatusBallotDrawn, StatusAborted},
	StatusBallotDrawn:        {StatusAllotmentFinalised, StatusAborted},
	StatusAllotmentFinalised: {StatusSettled, StatusAborted},

	StatusSettled: {},
	StatusAborted: {},
}

// AllStatuses lists every state in lifecycle order.
func AllStatuses() []Status {
	return []Status{
		StatusConfigured, StatusOpen, StatusClosed, StatusBookFrozen,
		StatusFeasibilityChecked, StatusBidbookAnchored, StatusSeedCommitted,
		StatusSeedRevealed, StatusBallotDrawn, StatusAllotmentFinalised,
		StatusSettled, StatusAborted,
	}
}

func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

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

func (s Status) IsTerminal() bool { return s == StatusSettled || s == StatusAborted }

// IsAbortable reports whether the offer can still be abandoned.
//
// True everywhere except the two terminal states. The cost of an abort here is an ASBA unblock, so the
// answer stays yes for far longer than it would if subscription money had been collected up front.
func (s Status) IsAbortable() bool { return !s.IsTerminal() }

// UnitsIssued reports whether units exist on the register.
//
// The boundary that ends reversibility. Before SETTLED nothing has been credited and an abort unblocks
// funds that never moved. After it, units are in Demat accounts and the only remedies are secondary
// transfers or a scheme wind-down.
func (s Status) UnitsIssued() bool { return s == StatusSettled }

// AcceptsBids reports whether new bids may be recorded.
func (s Status) AcceptsBids() bool { return s == StatusOpen }

// BookIsFixed reports whether the membership of the bid book can still change.
//
// Once true, no bid may be added, removed or have its units or price altered. Anchoring the root
// before this point would commit to a book that could still move, which is the one thing that would
// make every published inclusion proof worthless.
func (s Status) BookIsFixed() bool {
	switch s {
	case StatusConfigured, StatusOpen, StatusClosed:
		return false
	default:
		return true
	}
}

// CeremonyInProgress reports whether the commit-reveal draw is underway.
func (s Status) CeremonyInProgress() bool {
	return s == StatusSeedCommitted || s == StatusSeedRevealed
}

// SchemeStatus is the contract's Status enum.
//
// Ordinals transcribed from AcreSyncScheme. Kept as named constants rather than raw numbers because a
// transposed ordinal would advance the scheme to the wrong state and the call would still succeed.
type SchemeStatus uint8

const (
	SchemeDraft SchemeStatus = iota
	SchemeSpvFormed
	SchemeValued
	SchemeFiled
	SchemeOfferApproved
	SchemeOfferOpen
	SchemeOfferClosed
	SchemeAllocated
	SchemeSettled
	SchemeListed
	SchemeOperational
	SchemeUndersubscribed
	SchemeRefunding
	SchemeAborted
	SchemeWindDown
	SchemeDissolved
)

// SchemeStatusFor returns the scheme status an offer state drives, when it drives one.
//
// # Why most states map to nothing
//
// The offer lifecycle is finer-grained than the scheme's. Freezing a book, checking feasibility and
// running a draw are all steps the contract has no opinion about, because none of them changes what
// the scheme is. Only four offer states correspond to a scheme transition, and mapping the rest to a
// plausible-looking neighbour would mean issuing chain calls that assert something untrue.
//
// SETTLED is deliberately absent. The contract refuses to let advanceStatus reach Settled at all: only
// finaliseSettlement can set it, and only once its five invariants hold. Returning a mapping here
// would invite a caller to try, and the attempt would revert.
func (s Status) SchemeStatusFor() (SchemeStatus, bool) {
	switch s {
	case StatusOpen:
		return SchemeOfferOpen, true
	case StatusClosed:
		return SchemeOfferClosed, true
	case StatusAllotmentFinalised:
		// Allocated is the precondition beginSettlement checks, so this is the transition that opens
		// settlement rather than a cosmetic status update.
		return SchemeAllocated, true
	default:
		return 0, false
	}
}

// AbortPath returns the scheme transitions an abort must walk, in order.
//
// # Why this is three steps and not one
//
// The contract will not jump to Aborted. It requires Undersubscribed, then Refunding, then Aborted,
// and the middle state is the point of the sequence: refunding is where blocked funds are released,
// and a scheme that reached Aborted without passing through it would be claiming an outcome it had not
// carried out. Returning the whole path means a caller cannot accidentally skip it.
func AbortPath() []SchemeStatus {
	return []SchemeStatus{SchemeUndersubscribed, SchemeRefunding, SchemeAborted}
}

// NextExpected returns the ordinary next state on the happy path.
//
// Answers "what comes next", not "can it happen yet". Readiness is the guard's job.
func NextExpected(s Status) (Status, bool) {
	switch s {
	case StatusConfigured:
		return StatusOpen, true
	case StatusOpen:
		return StatusClosed, true
	case StatusClosed:
		return StatusBookFrozen, true
	case StatusBookFrozen:
		return StatusFeasibilityChecked, true
	case StatusFeasibilityChecked:
		return StatusBidbookAnchored, true
	case StatusBidbookAnchored:
		return StatusSeedCommitted, true
	case StatusSeedCommitted:
		return StatusSeedRevealed, true
	case StatusSeedRevealed:
		return StatusBallotDrawn, true
	case StatusBallotDrawn:
		return StatusAllotmentFinalised, true
	case StatusAllotmentFinalised:
		return StatusSettled, true
	default:
		return "", false
	}
}
