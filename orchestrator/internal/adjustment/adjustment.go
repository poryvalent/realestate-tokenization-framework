// Package adjustment records carry-forward corrections, the only remedy once fiat has settled.
//
// # Where this sits
//
// A distribution period has three correction regimes, separated by how far the money has travelled.
//
// Before the register is trusted, a divergence between the depository and our mirror is resolved and
// nothing has moved. Before money leaves the escrow, a bad period is reversed outright. After a payout
// settles, neither is available: unwinding the period would assert that payments which happened did not,
// and no amount of ledger writing recalls rupees from somebody's bank account.
//
// So the remedy becomes a compensating entry against a later period. We do not pretend the error did not
// happen; we net it off the next distribution and leave both halves visible.
//
// # What the chain records, and what it does not
//
// recordPayoutAdjustment anchors the source period, the target period, the holder, the direction and the
// reason. It does NOT anchor the amount, and that is worth stating plainly rather than implying more
// verifiability than exists. What a third party can check on-chain is that an adjustment was made against
// a named holder, in which direction and why. The figure itself lives in the database and in the published
// distribution statements for the two periods, which is where it is auditable.
//
// # Why a recovery can never produce a negative payout
//
// The asymmetry that shapes the arithmetic. Paying a holder more is always possible. Recovering from one
// is only possible to the extent the scheme already owes them, because a distribution is a payment and not
// an invoice: there is no mechanism by which a unitholder is billed. A recovery therefore withholds up to
// what is owed and no further, and whatever is left over stays outstanding for the period after that.
package adjustment

import (
	"errors"
	"fmt"
	"time"

	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/period"
)

// Direction mirrors the adjustment_direction Postgres enum.
type Direction string

const (
	// RecoverFromHolder means we paid too much and are netting it back.
	RecoverFromHolder Direction = "RECOVER_FROM_HOLDER"

	// PayToHolder means we paid too little and owe the difference.
	PayToHolder Direction = "PAY_TO_HOLDER"
)

func AllDirections() []Direction { return []Direction{RecoverFromHolder, PayToHolder} }

// directionCodes maps each direction to the uint8 the contract records.
//
// A frozen wire format for the same reason the reversal reasons are: the contract takes an opaque uint8
// and emits it, so the meaning lives only here and renumbering would reinterpret adjustments already on an
// immutable ledger.
//
// Zero is reserved. An unset direction field would otherwise read as a confident instruction to recover
// money from a holder, which is the more damaging of the two directions to get wrong by accident.
var directionCodes = map[Direction]uint8{
	RecoverFromHolder: 1,
	PayToHolder:       2,
}

// State mirrors the status vocabulary the carry_forward_adjustments CHECK allows.
type State string

const (
	// StatePending is outstanding and not yet netted against anything.
	StatePending State = "PENDING"

	// StateApplied means it was netted against its target period in full.
	StateApplied State = "APPLIED"

	// StateWrittenOff means it will never be recovered.
	//
	// Reachable only for a recovery, and only deliberately. A holder who has exited the scheme will
	// receive no further distribution, so there is nothing left to net against, and an adjustment that
	// can never be applied has to be closed by somebody putting their name to that rather than sitting
	// PENDING forever and quietly inflating what the scheme believes it is owed.
	StateWrittenOff State = "WRITTEN_OFF"
)

func AllStates() []State { return []State{StatePending, StateApplied, StateWrittenOff} }

// IsOpen reports whether the adjustment is still outstanding.
func (s State) IsOpen() bool { return s == StatePending }

// IsTerminal reports whether the adjustment will not change again.
func (s State) IsTerminal() bool { return s == StateApplied || s == StateWrittenOff }

var (
	ErrUnknownDirection = errors.New("adjustment: unknown direction")
	ErrNothingToAdjust  = errors.New("adjustment: the delta is zero, so there is nothing to correct")
	ErrNoNarrative      = errors.New("adjustment: a correction must record why in writing")
	ErrTargetNotLater   = errors.New("adjustment: the target period must come after the source")
	ErrSamePeriod       = errors.New("adjustment: a period cannot carry forward into itself")
	ErrNoTarget         = errors.New("adjustment: no target period is assigned")
	ErrNotOpen          = errors.New("adjustment: this adjustment is already closed")
	ErrNotRecoverable   = errors.New("adjustment: only a recovery can be written off")
)

// Code returns the uint8 recorded on-chain.
func (d Direction) Code() (uint8, error) {
	c, ok := directionCodes[d]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownDirection, d)
	}
	return c, nil
}

// DirectionFromCode is the inverse, for reading an event back.
func DirectionFromCode(code uint8) (Direction, error) {
	for d, c := range directionCodes {
		if c == code {
			return d, nil
		}
	}
	if code == 0 {
		return "", fmt.Errorf("%w: zero is reserved, so an unset field cannot read as an instruction "+
			"to recover money", ErrUnknownDirection)
	}
	return "", fmt.Errorf("%w: code %d", ErrUnknownDirection, code)
}

// ReducesPayout reports whether applying this direction withholds money.
func (d Direction) ReducesPayout() bool { return d == RecoverFromHolder }

// Adjustment is one holder's correction, carried from one period into a later one.
type Adjustment struct {
	InvestorID    string
	WalletAddress string

	// SourcePeriodID and SourcePeriodSeq identify where the error occurred.
	//
	// Both, because the database keys periods by UUID and the contract takes a uint32 sequence.
	SourcePeriodID  string
	SourcePeriodSeq uint32

	// TargetPeriodID and TargetPeriodSeq identify where it is netted. Unset until a target exists.
	TargetPeriodID  string
	TargetPeriodSeq uint32

	// AmountPaise is always positive. The direction carries the sign.
	//
	// Split this way because the table constrains the amount to be positive and stores the direction
	// separately, and because a signed amount plus a direction is two places to encode one fact.
	AmountPaise money.Paise

	Direction Direction

	Reason          period.ReversalReason
	NarrativeSHA256 [32]byte

	State State

	CreatedAt time.Time
}

// Input describes a correction as a signed difference.
type Input struct {
	InvestorID    string
	WalletAddress string

	SourcePeriodID  string
	SourcePeriodSeq uint32

	// DeltaPaise is what the holder should have received minus what they did receive.
	//
	// Signed, and the direction is derived from it rather than supplied. Accepting both an amount and a
	// direction would allow them to disagree, and a correction recorded with the wrong direction moves
	// money the wrong way twice: once by not fixing the original error and once by repeating it.
	DeltaPaise money.Paise

	Reason          period.ReversalReason
	NarrativeSHA256 [32]byte

	CreatedAt time.Time
}

// New records a correction, deriving the direction from the sign of the difference.
func New(in Input) (*Adjustment, error) {
	if in.InvestorID == "" || in.WalletAddress == "" {
		return nil, errors.New("adjustment: a correction needs a holder")
	}
	if in.SourcePeriodID == "" {
		return nil, errors.New("adjustment: a correction needs the period it came from")
	}
	if in.CreatedAt.IsZero() {
		return nil, errors.New("adjustment: a correction needs a timestamp")
	}
	if _, err := in.Reason.Code(); err != nil {
		return nil, err
	}
	if in.NarrativeSHA256 == ([32]byte{}) {
		return nil, fmt.Errorf("%w: reason %s carries no narrative digest", ErrNoNarrative, in.Reason)
	}

	// Zero is not a correction, and the table's amount_positive CHECK would refuse it anyway. Caught
	// here so the error says what is wrong rather than naming a constraint.
	if in.DeltaPaise == 0 {
		return nil, ErrNothingToAdjust
	}

	dir := PayToHolder
	amount := in.DeltaPaise
	if in.DeltaPaise < 0 {
		dir = RecoverFromHolder
		amount = -in.DeltaPaise
	}

	return &Adjustment{
		InvestorID:      in.InvestorID,
		WalletAddress:   in.WalletAddress,
		SourcePeriodID:  in.SourcePeriodID,
		SourcePeriodSeq: in.SourcePeriodSeq,
		AmountPaise:     amount,
		Direction:       dir,
		Reason:          in.Reason,
		NarrativeSHA256: in.NarrativeSHA256,
		State:           StatePending,
		CreatedAt:       in.CreatedAt,
	}, nil
}

// AssignTarget nominates the period this correction is netted against.
//
// Separate from construction because the target usually does not exist yet: an error found while closing
// one period is carried into the next, which has not been opened.
func (a *Adjustment) AssignTarget(periodID string, seq uint32) error {
	if !a.State.IsOpen() {
		return fmt.Errorf("%w: it is %s", ErrNotOpen, a.State)
	}
	if periodID == "" {
		return ErrNoTarget
	}
	if periodID == a.SourcePeriodID {
		// Mirrors the adjustments_periods_distinct CHECK.
		return ErrSamePeriod
	}

	// The database can enforce distinctness but not ordering, because it stores identifiers rather than
	// sequence numbers. Netting an error backwards into a period that has already been distributed would
	// be a second correction to a closed period, which is the situation this whole package exists to
	// avoid.
	if seq <= a.SourcePeriodSeq {
		return fmt.Errorf("%w: source is period %d and the target is %d",
			ErrTargetNotLater, a.SourcePeriodSeq, seq)
	}

	a.TargetPeriodID = periodID
	a.TargetPeriodSeq = seq
	return nil
}

// Application is the result of netting corrections against one holder's entitlement.
type Application struct {
	Before money.Paise
	After  money.Paise

	// AppliedPaise is how much of the correction took effect.
	AppliedPaise money.Paise

	// RemainingPaise is what could not be applied and stays outstanding.
	//
	// Always zero for a payment and potentially non-zero for a recovery, because a recovery is capped by
	// what the scheme owes.
	RemainingPaise money.Paise

	// FullyApplied is true when nothing remains outstanding.
	FullyApplied bool
}

// ApplyTo nets this correction against a holder's entitlement for the target period.
//
// Returns what the entitlement becomes, how much of the correction landed, and how much is still owed in
// either direction. Does not mutate the adjustment: closing it is a separate act, because the netting is
// arithmetic and the state change is a record of a decision.
func (a *Adjustment) ApplyTo(entitlement money.Paise) (Application, error) {
	if entitlement < 0 {
		return Application{}, fmt.Errorf("adjustment: entitlement cannot be negative, got %d", entitlement)
	}
	if a.AmountPaise <= 0 {
		return Application{}, fmt.Errorf("adjustment: amount must be positive, got %d", a.AmountPaise)
	}

	switch a.Direction {
	case PayToHolder:
		after, err := entitlement.Add(a.AmountPaise)
		if err != nil {
			return Application{}, fmt.Errorf("adjustment: netting a payment: %w", err)
		}
		return Application{
			Before:       entitlement,
			After:        after,
			AppliedPaise: a.AmountPaise,
			FullyApplied: true,
		}, nil

	case RecoverFromHolder:
		// Capped at what is owed. A distribution cannot invoice a unitholder, so the most that can be
		// recovered in any period is the whole of that period's entitlement.
		applied := a.AmountPaise
		if applied > entitlement {
			applied = entitlement
		}
		after, err := entitlement.Sub(applied)
		if err != nil {
			return Application{}, fmt.Errorf("adjustment: netting a recovery: %w", err)
		}
		remaining, err := a.AmountPaise.Sub(applied)
		if err != nil {
			return Application{}, err
		}
		return Application{
			Before:         entitlement,
			After:          after,
			AppliedPaise:   applied,
			RemainingPaise: remaining,
			FullyApplied:   remaining == 0,
		}, nil

	default:
		return Application{}, fmt.Errorf("%w: %q", ErrUnknownDirection, a.Direction)
	}
}

// ApplyAll nets a set of corrections against one entitlement, in order.
//
// Recoveries are applied before payments. The order matters and the choice is deliberate: applying a
// payment first would inflate the balance a recovery can then draw down, so a holder owed 100 and being
// recovered 100 would net to zero either way, but a holder owed nothing and being recovered 100 would
// appear to have had the recovery satisfied out of money that only existed because of a separate
// correction. Taking recoveries against the original entitlement keeps each correction answerable on its
// own terms.
func ApplyAll(entitlement money.Paise, adjustments []*Adjustment) (Application, []Application, error) {
	perAdjustment := make([]Application, len(adjustments))
	running := entitlement

	order := make([]int, 0, len(adjustments))
	for i, adj := range adjustments {
		if adj.Direction.ReducesPayout() {
			order = append(order, i)
		}
	}
	for i, adj := range adjustments {
		if !adj.Direction.ReducesPayout() {
			order = append(order, i)
		}
	}

	var totalApplied, totalRemaining money.Paise
	for _, i := range order {
		app, err := adjustments[i].ApplyTo(running)
		if err != nil {
			return Application{}, nil, fmt.Errorf("adjustment %d: %w", i, err)
		}
		perAdjustment[i] = app
		running = app.After

		if totalApplied, err = totalApplied.Add(app.AppliedPaise); err != nil {
			return Application{}, nil, err
		}
		if totalRemaining, err = totalRemaining.Add(app.RemainingPaise); err != nil {
			return Application{}, nil, err
		}
	}

	return Application{
		Before:         entitlement,
		After:          running,
		AppliedPaise:   totalApplied,
		RemainingPaise: totalRemaining,
		FullyApplied:   totalRemaining == 0,
	}, perAdjustment, nil
}

// MarkApplied closes the adjustment after it has been netted in full.
func (a *Adjustment) MarkApplied() error {
	if !a.State.IsOpen() {
		return fmt.Errorf("%w: it is %s", ErrNotOpen, a.State)
	}
	if a.TargetPeriodID == "" {
		return fmt.Errorf("%w: nothing says which period it was netted against", ErrNoTarget)
	}
	a.State = StateApplied
	return nil
}

// WriteOff closes a recovery that can never be collected.
//
// Requires its own narrative. Writing off money the scheme is owed is a decision with an owner, and the
// alternative is an adjustment sitting PENDING forever while the scheme's books claim a receivable that
// nobody intends to pursue.
func (a *Adjustment) WriteOff(narrative [32]byte) error {
	if !a.State.IsOpen() {
		return fmt.Errorf("%w: it is %s", ErrNotOpen, a.State)
	}
	if !a.Direction.ReducesPayout() {
		// Writing off a payment would mean deciding not to pay somebody what they are owed, which is not
		// a bookkeeping act and is not available here.
		return fmt.Errorf("%w: %s is money owed to the holder, and declining to pay it is not a "+
			"write-off", ErrNotRecoverable, a.Direction)
	}
	if narrative == ([32]byte{}) {
		return fmt.Errorf("%w: a write-off needs its own justification", ErrNoNarrative)
	}
	a.NarrativeSHA256 = narrative
	a.State = StateWrittenOff
	return nil
}

// CarryForward produces a fresh correction for the part that could not be applied.
//
// The residue of a capped recovery. A new record rather than a mutation of the old one, so the history
// shows a recovery partly satisfied in one period and continued into the next, which is what an auditor
// asking "why is this still outstanding" needs to see.
func (a *Adjustment) CarryForward(app Application, at time.Time) (*Adjustment, error) {
	if app.RemainingPaise <= 0 {
		return nil, fmt.Errorf("%w: nothing remains outstanding", ErrNothingToAdjust)
	}
	if a.TargetPeriodID == "" {
		return nil, fmt.Errorf("%w: the exhausted attempt has no target period", ErrNoTarget)
	}

	delta := app.RemainingPaise
	if a.Direction.ReducesPayout() {
		delta = -delta
	}

	// The follow-on's source is the period where the recovery was attempted and fell short, not the
	// original error's period. That keeps the chain of corrections walkable one hop at a time.
	return New(Input{
		InvestorID:      a.InvestorID,
		WalletAddress:   a.WalletAddress,
		SourcePeriodID:  a.TargetPeriodID,
		SourcePeriodSeq: a.TargetPeriodSeq,
		DeltaPaise:      delta,
		Reason:          a.Reason,
		NarrativeSHA256: a.NarrativeSHA256,
		CreatedAt:       at,
	})
}

// AnchorRequest is the argument list for AcreSyncScheme.recordPayoutAdjustment.
//
// No amount, because the contract takes none. See the package comment.
type AnchorRequest struct {
	SourcePeriodSeq uint32
	TargetPeriodSeq uint32
	Holder          string
	DirectionCode   uint8
	ReasonCode      uint8
}

// AnchorRequest builds the call arguments, refusing anything the contract would.
func (a *Adjustment) AnchorRequest() (*AnchorRequest, error) {
	if a.TargetPeriodID == "" {
		return nil, fmt.Errorf("%w: an adjustment is anchored against a target period", ErrNoTarget)
	}
	if a.WalletAddress == "" || a.WalletAddress == "0x0000000000000000000000000000000000000000" {
		// recordPayoutAdjustment reverts ZeroValue on a zero holder.
		return nil, errors.New("adjustment: the holder address is zero")
	}

	dc, err := a.Direction.Code()
	if err != nil {
		return nil, err
	}
	rc, err := a.Reason.Code()
	if err != nil {
		return nil, err
	}

	return &AnchorRequest{
		SourcePeriodSeq: a.SourcePeriodSeq,
		TargetPeriodSeq: a.TargetPeriodSeq,
		Holder:          a.WalletAddress,
		DirectionCode:   dc,
		ReasonCode:      rc,
	}, nil
}

// IdempotencyInput describes the key for recording this adjustment.
//
// Scoped to the source period and keyed on the holder, direction and amount, so a corrected figure derives
// a different key rather than being swallowed as a duplicate of the wrong one.
func (a *Adjustment) IdempotencyInput(schemeID string) idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionRecordPayoutAdjustment,
		SchemeID: schemeID,
		ScopeID:  a.SourcePeriodID,
		Payload: map[string]any{
			"holder":    a.WalletAddress,
			"direction": string(a.Direction),
			"amount":    int64(a.AmountPaise),
			"target":    a.TargetPeriodID,
		},
	}
}

// Summary renders the adjustment for an operator or a log.
func (a *Adjustment) Summary() string {
	verb := "pay"
	if a.Direction.ReducesPayout() {
		verb = "recover"
	}
	target := "unassigned"
	if a.TargetPeriodSeq != 0 {
		target = fmt.Sprintf("period %d", a.TargetPeriodSeq)
	}
	return fmt.Sprintf("%s %d paise %s %s (period %d into %s, %s, %s)",
		verb, a.AmountPaise, directionPreposition(a.Direction), a.WalletAddress,
		a.SourcePeriodSeq, target, a.Reason, a.State)
}

func directionPreposition(d Direction) string {
	if d == RecoverFromHolder {
		return "from"
	}
	return "to"
}
