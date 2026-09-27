package period

import (
	"errors"
	"fmt"
	"time"

	"github.com/acresync/orchestrator/internal/idempotency"
)

// ReversalReason mirrors the reversal_reason Postgres enum.
type ReversalReason string

const (
	// ReasonDataEntryError is a wrong figure entered by an operator.
	ReasonDataEntryError ReversalReason = "DATA_ENTRY_ERROR"

	// ReasonDuplicateInjection is the same rent receipt counted twice.
	ReasonDuplicateInjection ReversalReason = "DUPLICATE_INJECTION"

	// ReasonValuationRestatement is a valuer revising a figure the period depended on.
	ReasonValuationRestatement ReversalReason = "VALUATION_RESTATEMENT"

	// ReasonBankFailure is money that did not move as the escrow reported.
	ReasonBankFailure ReversalReason = "BANK_FAILURE"

	// ReasonRegulatoryDirection is an instruction from SEBI or the trustee.
	ReasonRegulatoryDirection ReversalReason = "REGULATORY_DIRECTION"

	// ReasonOther is anything else and always needs a narrative to be meaningful.
	ReasonOther ReversalReason = "OTHER"
)

// AllReversalReasons lists the reasons in the order the Postgres enum declares them.
func AllReversalReasons() []ReversalReason {
	return []ReversalReason{
		ReasonDataEntryError,
		ReasonDuplicateInjection,
		ReasonValuationRestatement,
		ReasonBankFailure,
		ReasonRegulatoryDirection,
		ReasonOther,
	}
}

// reasonCodes maps each reason to the uint8 the contract records.
//
// # Why these numbers are frozen
//
// The contract takes the reason as a bare uint8 and never interprets it; it stores the number and puts it
// in an event. So the meaning of each number lives here and nowhere else, and it is a wire format on an
// immutable ledger: renumbering would silently reinterpret every reversal already emitted. Values may be
// appended, never reordered or reused.
//
// # Why counting starts at one
//
// Zero is deliberately not a reason. A uint8 field left unset is zero, and if zero meant
// DATA_ENTRY_ERROR then a forgotten assignment would anchor a confident, specific and false explanation
// for a reversal. Reserving zero turns that same mistake into a refusal before anything is sent.
var reasonCodes = map[ReversalReason]uint8{
	ReasonDataEntryError:       1,
	ReasonDuplicateInjection:   2,
	ReasonValuationRestatement: 3,
	ReasonBankFailure:          4,
	ReasonRegulatoryDirection:  5,
	ReasonOther:                6,
}

var (
	ErrUnknownReason       = errors.New("period: unknown reversal reason")
	ErrApprovalMismatch    = errors.New("period: the execution does not match the trustee's approval")
	ErrNotFourEyes         = errors.New("period: approval and execution must be different people")
	ErrNoNarrative         = errors.New("period: a reversal must record why in writing")
	ErrPeriodNotReversible = errors.New("period: this status cannot be reversed")
)

// Code returns the uint8 recorded on-chain for this reason.
func (r ReversalReason) Code() (uint8, error) {
	c, ok := reasonCodes[r]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownReason, r)
	}
	return c, nil
}

// ReversalReasonFromCode is the inverse, for reading an event back.
func ReversalReasonFromCode(code uint8) (ReversalReason, error) {
	for r, c := range reasonCodes {
		if c == code {
			return r, nil
		}
	}
	if code == 0 {
		return "", fmt.Errorf("%w: zero is reserved so that an unset field is a refusal rather than a "+
			"plausible-looking reason", ErrUnknownReason)
	}
	return "", fmt.Errorf("%w: code %d", ErrUnknownReason, code)
}

// ReversalApproval is the trustee half of a reversal.
//
// # Why this is a separate artefact
//
// The contract splits a reversal across two transactions by two roles, and requires the reason and the
// narrative digest to match between them. That is four-eyes enforced where it cannot be bypassed: an admin
// interface that merely showed two buttons could be satisfied by one person clicking both.
//
// Modelling the approval as its own value makes the same shape explicit off-chain, so the mismatch the
// contract would revert on is caught here with an error that says which field differs.
type ReversalApproval struct {
	PeriodID  string
	PeriodSeq uint32

	Reason ReversalReason

	// NarrativeSHA256 is the digest of the written explanation. Anchored, so the explanation is fixed
	// before the reversal rather than reconstructed afterwards.
	NarrativeSHA256 [32]byte

	// TrusteeID is the approving trustee. Recorded so the executor can be checked against it.
	TrusteeID string

	ApprovedAt time.Time

	// StatusAtApproval is the period status the trustee approved against.
	//
	// Kept because the contract re-checks reversibility at execution: an approval given while a period
	// was reversible does not stay valid if the period moved on.
	StatusAtApproval Status
}

// NewReversalApproval records a trustee's approval, refusing anything the contract would.
func NewReversalApproval(periodID string, periodSeq uint32, status Status, reason ReversalReason,
	narrative [32]byte, trusteeID string, at time.Time) (*ReversalApproval, error) {

	if periodID == "" {
		return nil, errors.New("period: an approval needs a period")
	}
	if trusteeID == "" {
		return nil, fmt.Errorf("%w: no trustee is named", ErrFourEyes)
	}
	if at.IsZero() {
		return nil, errors.New("period: an approval needs a timestamp")
	}
	if _, err := reason.Code(); err != nil {
		return nil, err
	}
	if narrative == ([32]byte{}) {
		return nil, fmt.Errorf("%w: reason %s carries no narrative digest", ErrNoNarrative, reason)
	}

	// Mirrors approveReversal, which accepts only Anchored and EntitlementsAnchored.
	if !status.IsReversible() {
		return nil, fmt.Errorf("%w: %s", ErrPeriodNotReversible, status)
	}

	return &ReversalApproval{
		PeriodID:         periodID,
		PeriodSeq:        periodSeq,
		Reason:           reason,
		NarrativeSHA256:  narrative,
		TrusteeID:        trusteeID,
		ApprovedAt:       at,
		StatusAtApproval: status,
	}, nil
}

// ReversalExecution is what the operator presents when executing the reversal.
//
// The reason and narrative are supplied again rather than read from the approval. That looks redundant and
// is the point: the contract compares them and reverts on any difference, so accepting them a second time
// is what lets the mismatch be caught here, named, before gas is spent.
type ReversalExecution struct {
	Reason          ReversalReason
	NarrativeSHA256 [32]byte

	// ExecutedBy is the operator submitting the reversal. Must not be the approving trustee.
	ExecutedBy string

	ExecutedAt time.Time

	// StatusNow is the period's current status, re-read rather than assumed.
	StatusNow Status
}

// ReversalRequest is the argument list for AcreSyncScheme.reversePeriod.
type ReversalRequest struct {
	PeriodID  string
	PeriodSeq uint32

	Reason     ReversalReason
	ReasonCode uint8

	NarrativeSHA256 [32]byte

	ApprovedByTrustee string
	ExecutedBy        string
	ExecutedAt        time.Time
}

// Execute validates an execution against the approval and produces the call arguments.
//
// Checks, in the order a failure is most likely: the reason and narrative must match the approval, the
// executor must differ from the trustee, and the period must still be reversible now rather than only when
// it was approved.
func (a *ReversalApproval) Execute(in ReversalExecution, ev Evidence) (*ReversalRequest, error) {
	if a == nil {
		return nil, errors.New("period: there is no approval to execute against")
	}
	if in.ExecutedBy == "" {
		return nil, fmt.Errorf("%w: no operator is named", ErrFourEyes)
	}
	if in.ExecutedAt.IsZero() {
		return nil, errors.New("period: an execution needs a timestamp")
	}

	// The binding the contract enforces as ReversalApprovalMismatch.
	if in.Reason != a.Reason {
		return nil, fmt.Errorf("%w: approved for %s, executing as %s",
			ErrApprovalMismatch, a.Reason, in.Reason)
	}
	if in.NarrativeSHA256 != a.NarrativeSHA256 {
		return nil, fmt.Errorf("%w: the narrative digest differs from the one the trustee approved, so "+
			"the explanation on record is not the one being acted on", ErrApprovalMismatch)
	}

	// Four eyes. The contract separates the roles; this separates the people.
	//
	// Role separation alone is not enough: a trustee who also holds operator credentials would satisfy
	// onlyTrustee and onlyRelayer by themselves, and the second signature would be theatre.
	if in.ExecutedBy == a.TrusteeID {
		return nil, fmt.Errorf("%w: %s both approved and executed", ErrNotFourEyes, in.ExecutedBy)
	}

	// Reversibility is re-checked against the status now, not the status at approval.
	//
	// An approval is not a reservation. If the period advanced to payouts in the meantime, the money has
	// moved and the remedy changed, whatever the trustee agreed to earlier.
	if err := Guard(in.StatusNow, StatusReversed, ev); err != nil {
		return nil, err
	}

	code, err := a.Reason.Code()
	if err != nil {
		return nil, err
	}

	return &ReversalRequest{
		PeriodID:          a.PeriodID,
		PeriodSeq:         a.PeriodSeq,
		Reason:            a.Reason,
		ReasonCode:        code,
		NarrativeSHA256:   a.NarrativeSHA256,
		ApprovedByTrustee: a.TrusteeID,
		ExecutedBy:        in.ExecutedBy,
		ExecutedAt:        in.ExecutedAt,
	}, nil
}

// ApprovalIdempotencyInput describes the key for the trustee's approval.
//
// Carries the reason and narrative, so changing either is a different approval rather than a silent
// overwrite of the one the operator is about to act on.
func (a *ReversalApproval) ApprovalIdempotencyInput(schemeID string) idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionApproveReversal,
		SchemeID: schemeID,
		ScopeID:  a.PeriodID,
		Payload: map[string]any{
			"reason":    string(a.Reason),
			"narrative": hex32(a.NarrativeSHA256),
			"trustee":   a.TrusteeID,
		},
	}
}

// IdempotencyInput describes the key for executing the reversal.
func (r *ReversalRequest) IdempotencyInput(schemeID string) idempotency.Input {
	return idempotency.Input{
		Action:   idempotency.ActionReversePeriod,
		SchemeID: schemeID,
		ScopeID:  r.PeriodID,
		Payload: map[string]any{
			"reason":    string(r.Reason),
			"narrative": hex32(r.NarrativeSHA256),
		},
	}
}

// Summary renders the reversal for an operator or a log.
func (r *ReversalRequest) Summary() string {
	return fmt.Sprintf("period %d reversed for %s (code %d), approved by %s, executed by %s",
		r.PeriodSeq, r.Reason, r.ReasonCode, r.ApprovedByTrustee, r.ExecutedBy)
}

func hex32(b [32]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 2, 66)
	out[0], out[1] = '0', 'x'
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}
