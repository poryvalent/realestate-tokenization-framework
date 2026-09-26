// Package payout instructs and tracks fiat transfers to unitholders.
//
// The state model here mirrors RazorpayX rather than inventing a tidier one, because the whole
// point of this layer is that the mock and the live provider behave identically. A simplified model
// would make the mock pass and the real integration fail, which is the opposite of useful.
//
// Source: https://razorpay.com/docs/x/payouts/states-life-cycle
// (Content rephrased for compliance with licensing restrictions.)
package payout

import (
	"errors"
	"fmt"
)

// Status is a RazorpayX payout state.
type Status string

const (
	// StatusPending applies when an approval workflow is enabled and the payout awaits approval.
	StatusPending Status = "pending"

	// StatusQueued means the business account lacked balance. Razorpay documents that a payout left
	// queued for more than three months is failed automatically.
	StatusQueued Status = "queued"

	// StatusScheduled means the payout waits for its scheduled time.
	StatusScheduled Status = "scheduled"

	// StatusProcessing means the partner bank or beneficiary bank has it.
	StatusProcessing Status = "processing"

	// StatusProcessed means the beneficiary's bank processed the transfer.
	//
	// Not a safe terminal state, despite reading like one. See IsTerminal.
	StatusProcessed Status = "processed"

	// StatusReversed means the transfer failed and the amount, with fees and tax, was credited back
	// to the business account.
	StatusReversed Status = "reversed"

	// StatusCancelled means an operator cancelled a queued or scheduled payout.
	StatusCancelled Status = "cancelled"

	// StatusRejected means an approver rejected it, or the approval window lapsed.
	StatusRejected Status = "rejected"

	// StatusFailed applies to current-account payouts failed by the partner bank, and to scheduled
	// payouts with insufficient balance at their scheduled time.
	StatusFailed Status = "failed"
)

var (
	ErrUnknownStatus    = errors.New("payout: unknown status")
	ErrIllegalTransition = errors.New("payout: illegal status transition")
)

// transitions is the permitted state graph, transcribed from Razorpay's documented lifecycle.
//
// Encoded as data rather than as branching logic so the graph can be asserted directly in a test
// and compared line by line against the published lifecycle. A transition the provider performs
// but we reject would stall a real payout; one we allow that the provider never performs would let a
// bug hide.
var transitions = map[Status][]Status{
	StatusPending:   {StatusQueued, StatusScheduled, StatusProcessing, StatusRejected},
	StatusQueued:    {StatusProcessing, StatusCancelled, StatusFailed},
	StatusScheduled: {StatusProcessing, StatusCancelled, StatusFailed},
	StatusProcessing: {StatusProcessed, StatusReversed, StatusFailed},

	// The transition that shapes everything downstream.
	//
	// Razorpay documents that a processed payout can still move to reversed, because a failure may
	// surface days later when the customer's bank or the clearing house reverses the transaction.
	// So "processed" is a statement about the present, not a guarantee about the future.
	//
	// The consequence for AcreSync is direct: the on-chain distribution anchor asserts how much was
	// distributed, and a late reversal makes an already-anchored figure untrue. That is why the
	// reversal and adjustment path exists in the contract at all. Modelling processed as terminal
	// here would have made that path look like defensive over-engineering, and the system would
	// have had no answer the first time a bank reversed a credit after settlement.
	StatusProcessed: {StatusReversed},

	// Reversed can move to failed, but only on RazorpayX-powered current accounts.
	StatusReversed: {StatusFailed},

	StatusCancelled: {},
	StatusRejected:  {},
	StatusFailed:    {},
}

// AllStatuses lists every state.
func AllStatuses() []Status {
	return []Status{
		StatusPending, StatusQueued, StatusScheduled, StatusProcessing,
		StatusProcessed, StatusReversed, StatusCancelled, StatusRejected, StatusFailed,
	}
}

// Valid reports whether s is a state the provider can report.
//
// Unknown states are rejected rather than tolerated. If Razorpay introduces a state we do not model,
// silently treating it as benign could mean reporting a payout as complete when it is not, and that
// error runs straight into a regulatory filing.
func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// CanTransitionTo reports whether the provider could legitimately move s to next.
func (s Status) CanTransitionTo(next Status) bool {
	allowed, ok := transitions[s]
	if !ok {
		return false
	}
	for _, a := range allowed {
		if a == next {
			return true
		}
	}
	return false
}

// CheckTransition validates a reported status change.
func CheckTransition(from, to Status) error {
	if !from.Valid() {
		return fmt.Errorf("%w: from %q", ErrUnknownStatus, from)
	}
	if !to.Valid() {
		return fmt.Errorf("%w: to %q", ErrUnknownStatus, to)
	}
	if from == to {
		return nil // Idempotent restatement of the current state.
	}
	if !from.CanTransitionTo(to) {
		return fmt.Errorf("%w: %s to %s is not in the documented lifecycle", ErrIllegalTransition, from, to)
	}
	return nil
}

// IsTerminal reports whether no further transition is possible.
//
// StatusProcessed is deliberately excluded. Razorpay describes processed as terminal in prose while
// also documenting that it can move to reversed, and between those two the conservative reading is
// the only safe one. Treating processed as terminal would let the period close and the register
// settle while a reversal was still possible, leaving the ledger asserting a distribution that did
// not hold.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusReversed, StatusCancelled, StatusRejected, StatusFailed:
		return true
	default:
		return false
	}
}

// IsCreditConfirmed reports whether the beneficiary has been credited as far as the provider knows.
//
// This is what the distribution total should be computed from, and it is separate from IsTerminal
// precisely because the two answer different questions: "did the money arrive" and "can this still
// change".
func (s Status) IsCreditConfirmed() bool {
	return s == StatusProcessed
}

// MayStillReverse reports whether a confirmed credit could still be clawed back.
func (s Status) MayStillReverse() bool {
	return s == StatusProcessed
}

// IsInFlight reports whether the provider or a bank still holds the instruction.
//
// A payout in flight must never be reissued. For IMPS and UPI the processing state can persist for
// up to T+3 working days, because NPCI may return a deemed-success outcome where it is genuinely
// unknown whether the beneficiary was credited. Razorpay holds the payout in processing rather than
// reporting a state it cannot stand behind.
//
// A timeout-and-retry policy over that window would double-pay a unitholder, and recovering the
// second credit means asking an investor to return money, which is a conversation no amount of
// reconciliation logic makes acceptable. So there is no duration after which this package treats
// processing as failure. It waits for the provider.
func (s Status) IsInFlight() bool {
	switch s {
	case StatusPending, StatusQueued, StatusScheduled, StatusProcessing:
		return true
	default:
		return false
	}
}

// NeedsReissue reports whether the money never moved and a fresh instruction is required.
//
// Deliberately excludes StatusReversed on its own merits being ambiguous: a reversal means the
// amount came back, so a reissue is arguably correct, but a reversal arriving after the period was
// anchored has to be handled as an adjustment against the ledger rather than as a quiet retry.
// Routing it through the reversal path keeps the chain record and the bank record explainable
// against each other.
func (s Status) NeedsReissue() bool {
	switch s {
	case StatusCancelled, StatusRejected, StatusFailed:
		return true
	default:
		return false
	}
}

// FailureSource classifies where a payout failure originated, per Razorpay's status_details.
type FailureSource string

const (
	SourceGateway         FailureSource = "gateway"          // Partner bank technical error.
	SourceBeneficiaryBank FailureSource = "beneficiary_bank" // Beneficiary bank technical error.
	SourceBusiness        FailureSource = "business"         // Action required by us.
	SourceInternal        FailureSource = "internal"         // Razorpay-side technical error.
)

// IsOurFault reports whether the failure requires action on the AcreSync side.
//
// Worth separating because the operational response differs sharply. A beneficiary bank error means
// the unitholder's bank details or their bank's availability is the problem, and reissuing to the
// same account will fail again. A business-source error means our own data or balance is wrong and
// is fixable without contacting anyone.
func (f FailureSource) IsOurFault() bool { return f == SourceBusiness }

// Mode is the transfer rail.
type Mode string

const (
	ModeIMPS Mode = "IMPS"
	ModeNEFT Mode = "NEFT"
	ModeRTGS Mode = "RTGS"
)

// Valid reports whether the mode is one the API accepts. Razorpay documents these as case-sensitive
// and uppercase, so a lowercase value is rejected here rather than silently upcased: quietly
// repairing input hides the fact that a caller is constructing requests incorrectly.
func (m Mode) Valid() bool {
	switch m {
	case ModeIMPS, ModeNEFT, ModeRTGS:
		return true
	default:
		return false
	}
}

// SettlesSameDay reports whether the rail is expected to settle immediately.
//
// NEFT settles in batches and RTGS operates in a fixed window, so a period whose payouts go out by
// NEFT cannot be expected to reach a terminal state within the hour. This is the input to the
// orchestrator's decision to wait rather than to escalate.
func (m Mode) SettlesSameDay() bool { return m == ModeIMPS }

// MayReportDeemedSuccess reports whether the rail can leave a payout in processing for up to T+3.
func (m Mode) MayReportDeemedSuccess() bool { return m == ModeIMPS }

// Purpose is the payout classification.
type Purpose string

const (
	// PurposePayout is the generic classification, used for unitholder distributions.
	//
	// Razorpay's built-in set covers refund, cashback, payout, salary, utility bill and vendor bill,
	// and new purposes can only be created from the dashboard, not via the API. So there is no
	// "distribution" purpose to reach for, and inventing a string here would be rejected at
	// request time.
	PurposePayout Purpose = "payout"

	PurposeRefund Purpose = "refund"
	PurposeVendor Purpose = "vendor bill"
)

func (p Purpose) Valid() bool {
	switch p {
	case PurposePayout, PurposeRefund, PurposeVendor:
		return true
	default:
		return false
	}
}
