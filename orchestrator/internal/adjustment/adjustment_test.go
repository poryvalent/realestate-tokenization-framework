package adjustment

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/period"
)

const (
	holderA  = "0x00000000000000000000000000000000000000a1"
	investor = "1e7d4c6a-0000-4000-8000-000000000001"
	sourceID = "5a11c0de-0000-4000-8000-000000000001"
	targetID = "5a11c0de-0000-4000-8000-000000000002"
	schemeID = "8d7c6b5a-4e3f-4a2b-8c1d-0e9f8a7b6c5d"
)

var createdAt = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

func narrative(b byte) [32]byte {
	var n [32]byte
	n[0], n[31] = b, b
	return n
}

func base(delta money.Paise) Input {
	return Input{
		InvestorID:      investor,
		WalletAddress:   holderA,
		SourcePeriodID:  sourceID,
		SourcePeriodSeq: 3,
		DeltaPaise:      delta,
		Reason:          period.ReasonDataEntryError,
		NarrativeSHA256: narrative(0x11),
		CreatedAt:       createdAt,
	}
}

func mustNew(t *testing.T, delta money.Paise) *Adjustment {
	t.Helper()
	a, err := New(base(delta))
	if err != nil {
		t.Fatalf("a well-formed adjustment must be accepted: %v", err)
	}
	return a
}

func withTarget(t *testing.T, delta money.Paise) *Adjustment {
	t.Helper()
	a := mustNew(t, delta)
	if err := a.AssignTarget(targetID, 4); err != nil {
		t.Fatal(err)
	}
	return a
}

// --- direction is derived, never asserted --------------------------------------------------------

// TestDirectionFollowsTheSignOfTheError is why Input takes a signed delta.
//
// Accepting an amount and a direction separately would let them disagree, and a correction recorded the
// wrong way round moves money wrongly twice: it fails to fix the original error and then repeats it.
func TestDirectionFollowsTheSignOfTheError(t *testing.T) {
	underpaid := mustNew(t, 5_000)
	if underpaid.Direction != PayToHolder {
		t.Fatalf("a positive delta means we owe the holder, got %s", underpaid.Direction)
	}
	if underpaid.AmountPaise != 5_000 {
		t.Fatalf("amount %d, want 5000", underpaid.AmountPaise)
	}

	overpaid := mustNew(t, -5_000)
	if overpaid.Direction != RecoverFromHolder {
		t.Fatalf("a negative delta means we overpaid, got %s", overpaid.Direction)
	}
	if overpaid.AmountPaise != 5_000 {
		t.Fatalf("the stored amount must be positive, got %d", overpaid.AmountPaise)
	}
}

func TestZeroDeltaIsNotACorrection(t *testing.T) {
	if _, err := New(base(0)); !errors.Is(err, ErrNothingToAdjust) {
		t.Fatalf("want ErrNothingToAdjust, got %v", err)
	}
}

func TestNarrativeIsRequired(t *testing.T) {
	in := base(1_000)
	in.NarrativeSHA256 = [32]byte{}
	if _, err := New(in); !errors.Is(err, ErrNoNarrative) {
		t.Fatalf("want ErrNoNarrative, got %v", err)
	}
}

func TestReasonMustBeKnown(t *testing.T) {
	in := base(1_000)
	in.Reason = period.ReversalReason("INVENTED")
	if _, err := New(in); err == nil {
		t.Fatal("an unknown reason must be refused")
	}
}

// --- the codes are a frozen wire format ----------------------------------------------------------

func TestDirectionCodesAreStableAndOneBased(t *testing.T) {
	want := map[Direction]uint8{RecoverFromHolder: 1, PayToHolder: 2}

	for d, expected := range want {
		got, err := d.Code()
		if err != nil {
			t.Fatal(err)
		}
		if got != expected {
			t.Errorf("%s has code %d, want %d; renumbering reinterprets adjustments already emitted",
				d, got, expected)
		}
	}
	if len(want) != len(AllDirections()) {
		t.Fatalf("%d directions declared, %d have codes", len(AllDirections()), len(want))
	}
}

// TestZeroIsNotADirection is the more dangerous of the two defaults.
//
// An unset uint8 reading as RECOVER_FROM_HOLDER would turn a forgotten assignment into an instruction to
// take money off a holder.
func TestZeroIsNotADirection(t *testing.T) {
	_, err := DirectionFromCode(0)
	if !errors.Is(err, ErrUnknownDirection) {
		t.Fatalf("want ErrUnknownDirection, got %v", err)
	}
	if !strings.Contains(err.Error(), "recover") {
		t.Errorf("the error should say what the reservation prevents: %v", err)
	}
}

func TestDirectionCodesRoundTrip(t *testing.T) {
	for _, d := range AllDirections() {
		c, err := d.Code()
		if err != nil {
			t.Fatal(err)
		}
		back, err := DirectionFromCode(c)
		if err != nil {
			t.Fatal(err)
		}
		if back != d {
			t.Errorf("%s -> %d -> %s", d, c, back)
		}
	}
}

// --- the asymmetry that shapes the arithmetic ----------------------------------------------------

// TestAPaymentAlwaysAppliesInFull is the easy direction.
func TestAPaymentAlwaysAppliesInFull(t *testing.T) {
	a := withTarget(t, 5_000)

	app, err := a.ApplyTo(2_000)
	if err != nil {
		t.Fatal(err)
	}
	if app.After != 7_000 || app.AppliedPaise != 5_000 || app.RemainingPaise != 0 {
		t.Fatalf("got after=%d applied=%d remaining=%d, want 7000/5000/0",
			app.After, app.AppliedPaise, app.RemainingPaise)
	}
	if !app.FullyApplied {
		t.Fatal("paying more is always possible, so a payment is always fully applied")
	}

	// Even against nothing owed.
	app, err = a.ApplyTo(0)
	if err != nil {
		t.Fatal(err)
	}
	if app.After != 5_000 || !app.FullyApplied {
		t.Fatalf("a payment against a zero entitlement still pays: after=%d", app.After)
	}
}

// TestARecoveryIsCappedByWhatIsOwed is the rule the whole package is shaped around.
//
// A distribution is a payment, not an invoice. There is no mechanism to bill a unitholder, so a recovery
// withholds up to what the scheme owes and no further.
func TestARecoveryIsCappedByWhatIsOwed(t *testing.T) {
	a := withTarget(t, -15_000) // we overpaid by 150 rupees

	app, err := a.ApplyTo(10_000) // only 100 rupees owed this period
	if err != nil {
		t.Fatal(err)
	}

	if app.After != 0 {
		t.Fatalf("after=%d, want 0; a recovery must never produce a negative payout", app.After)
	}
	if app.AppliedPaise != 10_000 {
		t.Fatalf("applied=%d, want 10000", app.AppliedPaise)
	}
	if app.RemainingPaise != 5_000 {
		t.Fatalf("remaining=%d, want 5000 still outstanding", app.RemainingPaise)
	}
	if app.FullyApplied {
		t.Fatal("5000 paise is still outstanding, so this is not fully applied")
	}
}

func TestARecoveryAgainstNothingOwedRecoversNothing(t *testing.T) {
	a := withTarget(t, -15_000)

	app, err := a.ApplyTo(0)
	if err != nil {
		t.Fatal(err)
	}
	if app.After != 0 || app.AppliedPaise != 0 || app.RemainingPaise != 15_000 {
		t.Fatalf("got after=%d applied=%d remaining=%d, want 0/0/15000",
			app.After, app.AppliedPaise, app.RemainingPaise)
	}
}

func TestAnExactRecoveryClosesOut(t *testing.T) {
	a := withTarget(t, -10_000)

	app, err := a.ApplyTo(10_000)
	if err != nil {
		t.Fatal(err)
	}
	if app.After != 0 || app.RemainingPaise != 0 || !app.FullyApplied {
		t.Fatalf("an exact recovery should net to zero and close: after=%d remaining=%d",
			app.After, app.RemainingPaise)
	}
}

// TestTheCorrectionRestoresTheRightFigure is the pair reconciling to zero.
//
// The point of a carry-forward: across the two periods the holder ends up with what they should have had.
func TestTheCorrectionRestoresTheRightFigure(t *testing.T) {
	const correct = 30_000
	const paid = 22_000 // underpaid by 8000

	a := withTarget(t, correct-paid)

	// The next period owes them 30000 on its own account.
	app, err := a.ApplyTo(correct)
	if err != nil {
		t.Fatal(err)
	}

	totalReceived := money.Paise(paid) + app.After
	totalOwed := money.Paise(correct) + money.Paise(correct)

	if totalReceived != totalOwed {
		t.Fatalf("the holder received %d across both periods but was owed %d; the pair must net to zero",
			totalReceived, totalOwed)
	}
}

func TestNegativeEntitlementIsRefused(t *testing.T) {
	a := withTarget(t, 1_000)
	if _, err := a.ApplyTo(-1); err == nil {
		t.Fatal("a negative entitlement is not a thing that can exist")
	}
}

// --- ordering when several corrections land together ---------------------------------------------

// TestRecoveriesApplyBeforePayments keeps each correction answerable on its own terms.
//
// Applying a payment first would inflate the balance a recovery can draw down, making a recovery look
// satisfied out of money that only existed because of an unrelated correction.
func TestRecoveriesApplyBeforePayments(t *testing.T) {
	recover := withTarget(t, -10_000)
	pay := withTarget(t, 10_000)

	// Nothing owed on the period's own account.
	total, per, err := ApplyAll(0, []*Adjustment{pay, recover})
	if err != nil {
		t.Fatal(err)
	}

	// The recovery went first against zero, so it recovered nothing and stays outstanding.
	if per[1].AppliedPaise != 0 || per[1].RemainingPaise != 10_000 {
		t.Fatalf("the recovery should have found nothing to take: applied=%d remaining=%d",
			per[1].AppliedPaise, per[1].RemainingPaise)
	}
	// The payment still pays.
	if per[0].AppliedPaise != 10_000 {
		t.Fatalf("the payment should apply in full, got %d", per[0].AppliedPaise)
	}
	if total.After != 10_000 {
		t.Fatalf("the holder should end up owed 10000, got %d", total.After)
	}
	if total.RemainingPaise != 10_000 {
		t.Fatalf("the recovery is still outstanding, want remaining 10000, got %d", total.RemainingPaise)
	}
}

func TestApplyAllOnAnEmptySetChangesNothing(t *testing.T) {
	total, per, err := ApplyAll(7_777, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total.After != 7_777 || len(per) != 0 || !total.FullyApplied {
		t.Fatalf("no corrections means no change, got after=%d", total.After)
	}
}

// --- the residue carries forward again -----------------------------------------------------------

// TestAnExhaustedRecoveryCarriesTheRemainder is why the chain of corrections is walkable.
func TestAnExhaustedRecoveryCarriesTheRemainder(t *testing.T) {
	a := withTarget(t, -15_000)

	app, err := a.ApplyTo(10_000)
	if err != nil {
		t.Fatal(err)
	}

	next, err := a.CarryForward(app, createdAt.AddDate(0, 3, 0))
	if err != nil {
		t.Fatal(err)
	}

	if next.Direction != RecoverFromHolder {
		t.Fatalf("the remainder is still a recovery, got %s", next.Direction)
	}
	if next.AmountPaise != 5_000 {
		t.Fatalf("carried amount %d, want 5000", next.AmountPaise)
	}

	// The follow-on starts from where the attempt fell short, not from the original error.
	if next.SourcePeriodID != targetID || next.SourcePeriodSeq != 4 {
		t.Fatalf("the follow-on should originate in the period that fell short, got %s/%d",
			next.SourcePeriodID, next.SourcePeriodSeq)
	}
	if next.State != StatePending {
		t.Fatalf("a carried remainder is outstanding, got %s", next.State)
	}
}

func TestNothingToCarryIsRefused(t *testing.T) {
	a := withTarget(t, -10_000)
	app, err := a.ApplyTo(10_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CarryForward(app, createdAt); !errors.Is(err, ErrNothingToAdjust) {
		t.Fatalf("a fully applied recovery has nothing to carry, got %v", err)
	}
}

// --- the target period ---------------------------------------------------------------------------

// TestCarryingBackwardsIsRefused is a rule the database cannot express.
//
// The table enforces that source and target differ, but it stores identifiers rather than sequence
// numbers, so it cannot tell which came first. Netting into an earlier period would be correcting a
// distribution that has already been made.
func TestCarryingBackwardsIsRefused(t *testing.T) {
	a := mustNew(t, -1_000) // source is period 3

	if err := a.AssignTarget(targetID, 2); !errors.Is(err, ErrTargetNotLater) {
		t.Fatalf("want ErrTargetNotLater, got %v", err)
	}
	if err := a.AssignTarget(targetID, 3); !errors.Is(err, ErrTargetNotLater) {
		t.Fatalf("the same sequence is not later, got %v", err)
	}
	if err := a.AssignTarget(targetID, 4); err != nil {
		t.Fatalf("period 4 is later than 3 and must be accepted: %v", err)
	}
}

func TestCarryingIntoItselfIsRefused(t *testing.T) {
	a := mustNew(t, -1_000)
	if err := a.AssignTarget(sourceID, 9); !errors.Is(err, ErrSamePeriod) {
		t.Fatalf("want ErrSamePeriod, got %v", err)
	}
}

// --- closing out ---------------------------------------------------------------------------------

func TestMarkAppliedRequiresATarget(t *testing.T) {
	a := mustNew(t, 1_000)
	if err := a.MarkApplied(); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("want ErrNoTarget, got %v", err)
	}

	if err := a.AssignTarget(targetID, 4); err != nil {
		t.Fatal(err)
	}
	if err := a.MarkApplied(); err != nil {
		t.Fatal(err)
	}
	if a.State != StateApplied || !a.State.IsTerminal() {
		t.Fatalf("state %s, want a terminal APPLIED", a.State)
	}
}

func TestAClosedAdjustmentCannotBeReopened(t *testing.T) {
	a := withTarget(t, 1_000)
	if err := a.MarkApplied(); err != nil {
		t.Fatal(err)
	}

	if err := a.MarkApplied(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("want ErrNotOpen, got %v", err)
	}
	if err := a.AssignTarget(targetID, 5); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a closed adjustment cannot be retargeted, got %v", err)
	}
	if err := a.WriteOff(narrative(0x22)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a closed adjustment cannot be written off, got %v", err)
	}
}

// TestOnlyARecoveryCanBeWrittenOff draws the line at the right place.
//
// Writing off money owed TO a holder would be deciding not to pay them, which is not a bookkeeping act.
func TestOnlyARecoveryCanBeWrittenOff(t *testing.T) {
	owed := withTarget(t, 5_000)
	err := owed.WriteOff(narrative(0x22))
	if !errors.Is(err, ErrNotRecoverable) {
		t.Fatalf("want ErrNotRecoverable, got %v", err)
	}
	if !strings.Contains(err.Error(), "not a") {
		t.Errorf("the error should explain the distinction: %v", err)
	}

	owing := withTarget(t, -5_000)
	if err := owing.WriteOff(narrative(0x22)); err != nil {
		t.Fatalf("an uncollectable recovery must be closeable: %v", err)
	}
	if owing.State != StateWrittenOff {
		t.Fatalf("state %s, want WRITTEN_OFF", owing.State)
	}
}

// TestAWriteOffNeedsItsOwnJustification keeps the decision owned.
func TestAWriteOffNeedsItsOwnJustification(t *testing.T) {
	a := withTarget(t, -5_000)
	if err := a.WriteOff([32]byte{}); !errors.Is(err, ErrNoNarrative) {
		t.Fatalf("want ErrNoNarrative, got %v", err)
	}
}

func TestStatePredicates(t *testing.T) {
	for _, s := range AllStates() {
		open := s == StatePending
		if s.IsOpen() != open {
			t.Errorf("%s: IsOpen = %v, want %v", s, s.IsOpen(), open)
		}
		if s.IsTerminal() == open {
			t.Errorf("%s: IsTerminal and IsOpen must be opposites", s)
		}
	}
	if len(AllStates()) != 3 {
		t.Fatalf("the status CHECK allows 3 values, AllStates has %d", len(AllStates()))
	}
}

// --- the anchor ----------------------------------------------------------------------------------

func TestAnchorRequestCarriesWhatTheContractTakes(t *testing.T) {
	a := withTarget(t, -5_000)

	req, err := a.AnchorRequest()
	if err != nil {
		t.Fatal(err)
	}
	if req.SourcePeriodSeq != 3 || req.TargetPeriodSeq != 4 {
		t.Fatalf("periods %d -> %d, want 3 -> 4", req.SourcePeriodSeq, req.TargetPeriodSeq)
	}
	if req.DirectionCode != 1 || req.ReasonCode != 1 {
		t.Fatalf("direction %d reason %d, want 1 and 1", req.DirectionCode, req.ReasonCode)
	}
	if req.Holder != holderA {
		t.Fatalf("holder %s, want %s", req.Holder, holderA)
	}
}

// TestTheAnchorCarriesNoAmount records a real limitation rather than implying otherwise.
//
// recordPayoutAdjustment takes no amount. A third party can verify from the chain that an adjustment was
// made against a holder, in which direction and why, but the figure is only in the database and the
// published statements. Asserted so nobody later assumes the amount is on-chain.
func TestTheAnchorCarriesNoAmount(t *testing.T) {
	a := withTarget(t, -5_000)
	req, err := a.AnchorRequest()
	if err != nil {
		t.Fatal(err)
	}
	if req.DirectionCode == 0 {
		t.Fatal("the request should be populated")
	}

	// A compile-time assertion on the exact shape. Adding an amount field to AnchorRequest breaks this
	// conversion, which forces whoever does it to revisit the package comment claiming the chain records
	// no figure.
	var _ = struct {
		SourcePeriodSeq uint32
		TargetPeriodSeq uint32
		Holder          string
		DirectionCode   uint8
		ReasonCode      uint8
	}(*req)
}

func TestAnchorRequiresATarget(t *testing.T) {
	a := mustNew(t, -5_000)
	if _, err := a.AnchorRequest(); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("want ErrNoTarget, got %v", err)
	}
}

func TestAnchorRefusesAZeroHolder(t *testing.T) {
	a := withTarget(t, -5_000)
	a.WalletAddress = "0x0000000000000000000000000000000000000000"
	if _, err := a.AnchorRequest(); err == nil {
		t.Fatal("recordPayoutAdjustment reverts ZeroValue on a zero holder")
	}
}

// --- idempotency ---------------------------------------------------------------------------------

func TestACorrectedAmountDerivesADifferentKey(t *testing.T) {
	first := withTarget(t, -5_000)
	second := withTarget(t, -6_000)

	k1, err := idempotency.Derive(first.IdempotencyInput(schemeID))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := idempotency.Derive(second.IdempotencyInput(schemeID))
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("a revised figure must derive a new key, or the correction is swallowed as a " +
			"duplicate of the wrong one")
	}
}

func TestTheSameAdjustmentReusesItsKey(t *testing.T) {
	a := withTarget(t, -5_000)

	k1, err := idempotency.Derive(a.IdempotencyInput(schemeID))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := idempotency.Derive(a.IdempotencyInput(schemeID))
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("a retry must reuse the key")
	}
}

func TestOppositeDirectionsDeriveDifferentKeys(t *testing.T) {
	pay := withTarget(t, 5_000)
	rec := withTarget(t, -5_000)

	k1, err := idempotency.Derive(pay.IdempotencyInput(schemeID))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := idempotency.Derive(rec.IdempotencyInput(schemeID))
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("paying and recovering the same amount are opposite acts and must not share a key")
	}
}

// --- reporting -----------------------------------------------------------------------------------

func TestSummaryReadsCorrectly(t *testing.T) {
	rec := withTarget(t, -5_000)
	if s := rec.Summary(); !strings.Contains(s, "recover") || !strings.Contains(s, "from") {
		t.Errorf("a recovery should read as taking from the holder: %s", s)
	}

	pay := withTarget(t, 5_000)
	if s := pay.Summary(); !strings.Contains(s, "pay") || !strings.Contains(s, "to") {
		t.Errorf("a payment should read as paying to the holder: %s", s)
	}

	untargeted := mustNew(t, 5_000)
	if s := untargeted.Summary(); !strings.Contains(s, "unassigned") {
		t.Errorf("an untargeted adjustment should say so: %s", s)
	}
}
