package period

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/idempotency"
)

var (
	approvedAt = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	executedAt = time.Date(2026, 10, 3, 11, 0, 0, 0, time.UTC)

	testPeriodID = "6b1f0a2c-1111-4000-8000-000000000001"
	testScheme   = "8d7c6b5a-4e3f-4a2b-8c1d-0e9f8a7b6c5d"
)

func narrative(b byte) [32]byte {
	var n [32]byte
	n[0] = b
	n[31] = b
	return n
}

// reversibleEvidence is evidence for a period that may still be reversed.
func reversibleEvidence(trustee string) Evidence {
	return Evidence{TrusteeApprovedBy: trustee}
}

func goodApproval(t *testing.T) *ReversalApproval {
	t.Helper()
	a, err := NewReversalApproval(testPeriodID, 1, StatusAnchored,
		ReasonDataEntryError, narrative(0x11), "trustee@acresync", approvedAt)
	if err != nil {
		t.Fatalf("a well-formed approval must be accepted: %v", err)
	}
	return a
}

// --- reason codes --------------------------------------------------------------------------------

// TestReasonCodesAreStableAndOneBased pins a wire format.
//
// The contract stores the reason as a bare uint8 and emits it. Renumbering would reinterpret every
// reversal already on-chain, so these numbers are frozen.
func TestReasonCodesAreStableAndOneBased(t *testing.T) {
	want := map[ReversalReason]uint8{
		ReasonDataEntryError:       1,
		ReasonDuplicateInjection:   2,
		ReasonValuationRestatement: 3,
		ReasonBankFailure:          4,
		ReasonRegulatoryDirection:  5,
		ReasonOther:                6,
	}

	for reason, expected := range want {
		got, err := reason.Code()
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if got != expected {
			t.Errorf("%s has code %d, want %d. These are frozen: renumbering reinterprets every "+
				"reversal already emitted on-chain", reason, got, expected)
		}
	}

	if len(want) != len(AllReversalReasons()) {
		t.Fatalf("%d reasons declared, %d have codes", len(AllReversalReasons()), len(want))
	}
}

// TestZeroIsNotAReason is why counting starts at one.
//
// A uint8 left unset is zero. If zero meant DATA_ENTRY_ERROR, a forgotten assignment would anchor a
// specific and false explanation instead of failing.
func TestZeroIsNotAReason(t *testing.T) {
	_, err := ReversalReasonFromCode(0)
	if !errors.Is(err, ErrUnknownReason) {
		t.Fatalf("zero must not decode to a reason, got %v", err)
	}
	if !strings.Contains(err.Error(), "unset") {
		t.Errorf("the error should explain the reservation: %v", err)
	}

	for _, r := range AllReversalReasons() {
		c, err := r.Code()
		if err != nil {
			t.Fatal(err)
		}
		if c == 0 {
			t.Errorf("%s maps to zero", r)
		}
	}
}

func TestReasonCodesRoundTrip(t *testing.T) {
	for _, r := range AllReversalReasons() {
		code, err := r.Code()
		if err != nil {
			t.Fatal(err)
		}
		back, err := ReversalReasonFromCode(code)
		if err != nil {
			t.Fatal(err)
		}
		if back != r {
			t.Errorf("%s -> %d -> %s", r, code, back)
		}
	}
}

func TestUnknownReasonIsRefused(t *testing.T) {
	if _, err := ReversalReason("MADE_UP").Code(); !errors.Is(err, ErrUnknownReason) {
		t.Fatalf("want ErrUnknownReason, got %v", err)
	}
	if _, err := ReversalReasonFromCode(99); !errors.Is(err, ErrUnknownReason) {
		t.Fatalf("want ErrUnknownReason, got %v", err)
	}
}

func TestReasonVocabularyMatchesTheSchemaOrder(t *testing.T) {
	// The reversal_reason Postgres enum, in declaration order.
	want := []ReversalReason{
		ReasonDataEntryError, ReasonDuplicateInjection, ReasonValuationRestatement,
		ReasonBankFailure, ReasonRegulatoryDirection, ReasonOther,
	}
	got := AllReversalReasons()
	if len(got) != len(want) {
		t.Fatalf("%d reasons, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d is %s, want %s", i, got[i], want[i])
		}
	}
}

// --- the approval --------------------------------------------------------------------------------

func TestApprovalRequiresANarrative(t *testing.T) {
	_, err := NewReversalApproval(testPeriodID, 1, StatusAnchored,
		ReasonDataEntryError, [32]byte{}, "trustee@acresync", approvedAt)
	if !errors.Is(err, ErrNoNarrative) {
		t.Fatalf("a reversal with no written explanation must be refused, got %v", err)
	}
}

func TestApprovalRequiresANamedTrustee(t *testing.T) {
	_, err := NewReversalApproval(testPeriodID, 1, StatusAnchored,
		ReasonDataEntryError, narrative(0x11), "", approvedAt)
	if !errors.Is(err, ErrFourEyes) {
		t.Fatalf("want ErrFourEyes, got %v", err)
	}
}

// TestApprovalOnlyFromAReversibleStatus mirrors approveReversal.
func TestApprovalOnlyFromAReversibleStatus(t *testing.T) {
	for _, s := range AllStatuses() {
		_, err := NewReversalApproval(testPeriodID, 1, s,
			ReasonDataEntryError, narrative(0x11), "trustee@acresync", approvedAt)

		if s.IsReversible() {
			if err != nil {
				t.Errorf("%s is reversible but approval was refused: %v", s, err)
			}
			continue
		}
		if !errors.Is(err, ErrPeriodNotReversible) {
			t.Errorf("%s is not reversible; want ErrPeriodNotReversible, got %v", s, err)
		}
	}
}

// TestOnlyTwoStatusesAreReversible states the window explicitly.
func TestOnlyTwoStatusesAreReversible(t *testing.T) {
	var reversible []Status
	for _, s := range AllStatuses() {
		if s.IsReversible() {
			reversible = append(reversible, s)
		}
	}
	if len(reversible) != 2 {
		t.Fatalf("expected exactly ANCHORED and ENTITLEMENTS_ANCHORED to be reversible, got %v",
			reversible)
	}
	for _, s := range reversible {
		if s != StatusAnchored && s != StatusEntitlementsAnchored {
			t.Errorf("%s should not be reversible", s)
		}
	}
}

// --- the binding between the two halves ----------------------------------------------------------

func TestExecutionSucceedsWhenItMatches(t *testing.T) {
	a := goodApproval(t)

	req, err := a.Execute(ReversalExecution{
		Reason:          ReasonDataEntryError,
		NarrativeSHA256: narrative(0x11),
		ExecutedBy:      "ops@acresync",
		ExecutedAt:      executedAt,
		StatusNow:       StatusAnchored,
	}, reversibleEvidence("trustee@acresync"))
	if err != nil {
		t.Fatalf("a matching execution must be accepted: %v", err)
	}

	if req.ReasonCode != 1 {
		t.Fatalf("reason code %d, want 1", req.ReasonCode)
	}
	if req.ApprovedByTrustee != "trustee@acresync" || req.ExecutedBy != "ops@acresync" {
		t.Fatal("both actors must be recorded on the request")
	}
	if !strings.Contains(req.Summary(), "approved by trustee@acresync") {
		t.Errorf("the summary should name both actors: %s", req.Summary())
	}
}

// TestChangingTheReasonAtExecutionIsRefused is the contract's ReversalApprovalMismatch, caught earlier.
func TestChangingTheReasonAtExecutionIsRefused(t *testing.T) {
	a := goodApproval(t)

	_, err := a.Execute(ReversalExecution{
		Reason:          ReasonBankFailure, // trustee approved DATA_ENTRY_ERROR
		NarrativeSHA256: narrative(0x11),
		ExecutedBy:      "ops@acresync",
		ExecutedAt:      executedAt,
		StatusNow:       StatusAnchored,
	}, reversibleEvidence("trustee@acresync"))

	if !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("want ErrApprovalMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "DATA_ENTRY_ERROR") {
		t.Errorf("the error should name what was approved: %v", err)
	}
}

// TestChangingTheNarrativeAtExecutionIsRefused is the subtler half.
//
// Swapping the explanation while keeping the reason would leave a reversal whose recorded justification is
// not the one the trustee read.
func TestChangingTheNarrativeAtExecutionIsRefused(t *testing.T) {
	a := goodApproval(t)

	_, err := a.Execute(ReversalExecution{
		Reason:          ReasonDataEntryError,
		NarrativeSHA256: narrative(0x22),
		ExecutedBy:      "ops@acresync",
		ExecutedAt:      executedAt,
		StatusNow:       StatusAnchored,
	}, reversibleEvidence("trustee@acresync"))

	if !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("want ErrApprovalMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "not the one being acted on") {
		t.Errorf("the error should say why it matters: %v", err)
	}
}

// TestOneActorCannotDoBothHalves is why role separation alone is insufficient.
//
// A trustee who also holds operator credentials satisfies onlyTrustee and onlyRelayer by themselves. The
// contract cannot tell; this can.
func TestOneActorCannotDoBothHalves(t *testing.T) {
	a := goodApproval(t)

	_, err := a.Execute(ReversalExecution{
		Reason:          ReasonDataEntryError,
		NarrativeSHA256: narrative(0x11),
		ExecutedBy:      "trustee@acresync", // the same person
		ExecutedAt:      executedAt,
		StatusNow:       StatusAnchored,
	}, reversibleEvidence("trustee@acresync"))

	if !errors.Is(err, ErrNotFourEyes) {
		t.Fatalf("want ErrNotFourEyes, got %v", err)
	}
}

// TestAnApprovalIsNotAReservation is the staleness rule.
//
// If the period advanced to payouts after approval, money has moved and the remedy changed, whatever the
// trustee agreed to earlier.
func TestAnApprovalIsNotAReservation(t *testing.T) {
	a := goodApproval(t)

	_, err := a.Execute(ReversalExecution{
		Reason:          ReasonDataEntryError,
		NarrativeSHA256: narrative(0x11),
		ExecutedBy:      "ops@acresync",
		ExecutedAt:      executedAt,
		StatusNow:       StatusPayoutsConfirmed,
	}, reversibleEvidence("trustee@acresync"))

	if err == nil {
		t.Fatal("a period that has since paid out must not be reversible on an older approval")
	}
	if !errors.Is(err, ErrFiatSettled) && !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("the refusal should be about fiat having settled, got %v", err)
	}
	if !strings.Contains(err.Error(), "carry-forward") {
		t.Errorf("the error should name the remedy that does apply: %v", err)
	}
}

func TestExecutionRequiresANamedOperator(t *testing.T) {
	a := goodApproval(t)
	_, err := a.Execute(ReversalExecution{
		Reason:          ReasonDataEntryError,
		NarrativeSHA256: narrative(0x11),
		ExecutedAt:      executedAt,
		StatusNow:       StatusAnchored,
	}, reversibleEvidence("trustee@acresync"))
	if !errors.Is(err, ErrFourEyes) {
		t.Fatalf("want ErrFourEyes, got %v", err)
	}
}

// --- idempotency ---------------------------------------------------------------------------------

func TestApprovalAndExecutionDeriveDifferentKeys(t *testing.T) {
	a := goodApproval(t)
	req, err := a.Execute(ReversalExecution{
		Reason:          ReasonDataEntryError,
		NarrativeSHA256: narrative(0x11),
		ExecutedBy:      "ops@acresync",
		ExecutedAt:      executedAt,
		StatusNow:       StatusAnchored,
	}, reversibleEvidence("trustee@acresync"))
	if err != nil {
		t.Fatal(err)
	}

	ak, err := idempotency.Derive(a.ApprovalIdempotencyInput(testScheme))
	if err != nil {
		t.Fatal(err)
	}
	ek, err := idempotency.Derive(req.IdempotencyInput(testScheme))
	if err != nil {
		t.Fatal(err)
	}

	if ak == ek {
		t.Fatal("the two halves are separate transactions and must not share a key, or the second " +
			"would be swallowed as a duplicate of the first")
	}
}

// TestADifferentNarrativeDerivesADifferentApprovalKey keeps a re-approval from being silently dropped.
func TestADifferentNarrativeDerivesADifferentApprovalKey(t *testing.T) {
	first := goodApproval(t)

	second, err := NewReversalApproval(testPeriodID, 1, StatusAnchored,
		ReasonDataEntryError, narrative(0x99), "trustee@acresync", approvedAt)
	if err != nil {
		t.Fatal(err)
	}

	k1, err := idempotency.Derive(first.ApprovalIdempotencyInput(testScheme))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := idempotency.Derive(second.ApprovalIdempotencyInput(testScheme))
	if err != nil {
		t.Fatal(err)
	}

	if k1 == k2 {
		t.Fatal("a revised narrative is a different approval; sharing a key would swallow the revision")
	}
}

// TestRetryingTheSameExecutionReusesItsKey keeps a dropped connection recoverable.
func TestRetryingTheSameExecutionReusesItsKey(t *testing.T) {
	a := goodApproval(t)
	in := ReversalExecution{
		Reason:          ReasonDataEntryError,
		NarrativeSHA256: narrative(0x11),
		ExecutedBy:      "ops@acresync",
		ExecutedAt:      executedAt,
		StatusNow:       StatusAnchored,
	}
	ev := reversibleEvidence("trustee@acresync")

	r1, err := a.Execute(in, ev)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Execute(in, ev)
	if err != nil {
		t.Fatal(err)
	}

	k1, err := idempotency.Derive(r1.IdempotencyInput(testScheme))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := idempotency.Derive(r2.IdempotencyInput(testScheme))
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("a verbatim retry must reuse the key")
	}
}

func TestExecuteNeedsAnApproval(t *testing.T) {
	var a *ReversalApproval
	if _, err := a.Execute(ReversalExecution{
		Reason: ReasonDataEntryError, ExecutedBy: "ops", ExecutedAt: executedAt,
		StatusNow: StatusAnchored,
	}, reversibleEvidence("t")); err == nil {
		t.Fatal("executing with no approval must be refused; the contract reverts ReversalNotApproved")
	}
}
