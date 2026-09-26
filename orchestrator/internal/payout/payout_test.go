package payout

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/money"
)

// fixedClock is a deterministic business clock. Real time is never read in these tests: a payout
// suite that depends on elapsed time becomes slow, then flaky, and a flaky test over fiat movement
// gets muted rather than fixed.
type fixedClock struct{ t time.Time }

func (f fixedClock) Now(context.Context) (time.Time, error) { return f.t, nil }

const (
	srcAccount = "7878780080316316"
	beneA      = "fa_00000000000001"
	beneB      = "fa_00000000000002"

	schemeUUID      = "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"
	entitlementUUID = "0123abcd-4567-89ef-0123-456789abcdef"
)

func newMock(t *testing.T, opening money.Paise) *Mock {
	t.Helper()
	clk := fixedClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	return NewMock(clk, srcAccount, opening)
}

func validRequest(key string, amount money.Paise) Request {
	return Request{
		IdempotencyKey: key,
		AccountNumber:  srcAccount,
		FundAccountID:  beneA,
		AmountPaise:    amount,
		Mode:           ModeIMPS,
		Purpose:        PurposePayout,
		ReferenceID:    "ACRESYNC-P1-0001",
		Narration:      "AcreSync Q2 payout",
	}
}

// ---------------------------------------------------------------------------
// The state graph
// ---------------------------------------------------------------------------

// TestProcessedIsNotTerminal is the most consequential assertion in this package.
//
// Razorpay's documentation calls processed a terminal state and, a few lines later, describes a
// processed payout moving to reversed when the customer's bank or the clearing house reverses the
// transaction. Between those two readings only the conservative one is safe.
//
// If processed were treated as final, a distribution period would close and the on-chain anchor
// would assert a distributed total while a reversal was still possible. The ledger would then be
// stating something untrue, permanently, with no mechanism to correct it. The contract's reversal and
// adjustment path exists because of this single transition.
func TestProcessedIsNotTerminal(t *testing.T) {
	if StatusProcessed.IsTerminal() {
		t.Fatal("processed must not be treated as terminal: a bank can reverse a credit after " +
			"settlement, which would make an already-anchored distribution figure false")
	}
	if !StatusProcessed.CanTransitionTo(StatusReversed) {
		t.Error("processed must be able to move to reversed")
	}
	if !StatusProcessed.MayStillReverse() {
		t.Error("MayStillReverse must be true for processed")
	}
	if !StatusProcessed.IsCreditConfirmed() {
		t.Error("processed is still the state that confirms a credit")
	}
}

// TestTerminalStatesAreExactlyTheIrreversibleOnes pins the set down, since everything downstream
// keys off it: period closure waits for terminal states, and a wrong set either hangs the period or
// closes it early.
func TestTerminalStatesAreExactlyTheIrreversibleOnes(t *testing.T) {
	want := map[Status]bool{
		StatusReversed: true, StatusCancelled: true, StatusRejected: true, StatusFailed: true,
	}
	for _, s := range AllStatuses() {
		if got := s.IsTerminal(); got != want[s] {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, got, want[s])
		}
	}
}

// TestDocumentedLifecycleTransitions transcribes Razorpay's published graph and compares it against
// the implementation edge by edge. Encoded as data so a divergence names the exact edge.
func TestDocumentedLifecycleTransitions(t *testing.T) {
	legal := map[Status][]Status{
		StatusPending:    {StatusQueued, StatusScheduled, StatusProcessing, StatusRejected},
		StatusQueued:     {StatusProcessing, StatusCancelled, StatusFailed},
		StatusScheduled:  {StatusProcessing, StatusCancelled, StatusFailed},
		StatusProcessing: {StatusProcessed, StatusReversed, StatusFailed},
		StatusProcessed:  {StatusReversed},
		StatusReversed:   {StatusFailed},
		StatusCancelled:  {},
		StatusRejected:   {},
		StatusFailed:     {},
	}

	for from, allowed := range legal {
		allow := map[Status]bool{}
		for _, a := range allowed {
			allow[a] = true
		}
		for _, to := range AllStatuses() {
			if from == to {
				continue
			}
			got := from.CanTransitionTo(to)
			if got != allow[to] {
				t.Errorf("%s -> %s: got %v, want %v", from, to, got, allow[to])
			}
		}
	}
}

func TestUnknownStatusRejected(t *testing.T) {
	if Status("settled").Valid() {
		t.Error("an unmodelled status must not validate; treating one as benign could report an " +
			"incomplete payout as done")
	}
	if err := CheckTransition(StatusProcessing, Status("settled")); !errors.Is(err, ErrUnknownStatus) {
		t.Errorf("want ErrUnknownStatus, got %v", err)
	}
}

func TestInFlightStatesNeverNeedReissue(t *testing.T) {
	for _, s := range AllStatuses() {
		if s.IsInFlight() && s.NeedsReissue() {
			t.Errorf("%s is both in flight and marked for reissue, which would double-pay", s)
		}
	}
	// Processed is not in flight and must not be reissued either.
	if StatusProcessed.NeedsReissue() {
		t.Error("a processed payout must never be reissued")
	}
	if StatusReversed.NeedsReissue() {
		t.Error("a reversal must go through the adjustment path, not a quiet retry, so the chain " +
			"record and the bank record stay explainable against each other")
	}
}

// ---------------------------------------------------------------------------
// Idempotency: the double-payment boundary
// ---------------------------------------------------------------------------

// TestRetryAfterTransportFailureDoesNotPayTwice is the test this whole package exists for.
//
// The first attempt fails at the transport layer after the provider has already created the payout.
// The caller cannot distinguish that from a request that never arrived, so it retries with the same
// key. The retry has to return the original payout rather than create a second one.
//
// Getting this wrong means a unitholder is paid twice and recovering it means asking an investor to
// send money back.
func TestRetryAfterTransportFailureDoesNotPayTwice(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx := context.Background()

	req := validRequest("0xkey-transport", money.Paise(50_000_000))

	// The first call succeeds server-side, then the response is lost.
	first, err := m.CreatePayout(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	// The caller, having seen a timeout, retries blind with the identical request.
	second, err := m.CreatePayout(ctx, req)
	if err != nil {
		t.Fatalf("a retry with the same key must succeed: %v", err)
	}

	if second.ProviderID != first.ProviderID {
		t.Fatalf("the retry created a second payout: %s then %s. A unitholder was paid twice.",
			first.ProviderID, second.ProviderID)
	}
	if got := m.count(); got != 1 {
		t.Fatalf("%d payouts exist, want 1", got)
	}
}

// TestIdempotencyCheckedBeforeInjectedFailure covers the ordering that makes the above work.
//
// If the provider evaluated a transport failure before resolving the key, the retry after a dropped
// connection would create a second payout. The check has to come first.
func TestIdempotencyCheckedBeforeInjectedFailure(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx := context.Background()
	req := validRequest("0xkey-order", money.Paise(50_000_000))

	first, err := m.CreatePayout(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	m.FailNextCreate(errors.New("connection reset"))
	again, err := m.CreatePayout(ctx, req)
	if err != nil {
		t.Fatalf("an already-created payout must be returned even when the next call is armed to "+
			"fail: %v", err)
	}
	if again.ProviderID != first.ProviderID {
		t.Fatal("the retry must return the original payout")
	}
}

// TestReusedKeyWithDifferentAmountRejected covers the inverse mistake.
//
// This is stricter than Razorpay's documentation spells out, chosen deliberately. Returning the
// original payout for an altered request would leave a caller believing a corrected amount had been
// sent while the original stood, and nothing would surface it. A loud rejection turns that into a
// bug report.
func TestReusedKeyWithDifferentAmountRejected(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx := context.Background()

	if _, err := m.CreatePayout(ctx, validRequest("0xkey-reuse", money.Paise(50_000_000))); err != nil {
		t.Fatal(err)
	}

	altered := validRequest("0xkey-reuse", money.Paise(60_000_000))
	if _, err := m.CreatePayout(ctx, altered); !errors.Is(err, ErrIdempotencyReuse) {
		t.Fatalf("want ErrIdempotencyReuse, got %v", err)
	}

	// A changed beneficiary is equally a different instruction.
	other := validRequest("0xkey-reuse", money.Paise(50_000_000))
	other.FundAccountID = beneB
	if _, err := m.CreatePayout(ctx, other); !errors.Is(err, ErrIdempotencyReuse) {
		t.Fatalf("a different beneficiary under the same key must be rejected, got %v", err)
	}
}

// TestAttemptGenerationEnablesReissue is the other half of the idempotency requirement.
//
// A key derived only from the payout's identity would make a retry safe and a legitimate reissue
// impossible: after a failure the provider would keep returning the failed record and the unitholder
// would never be paid. Incrementing the attempt produces a distinct key.
func TestAttemptGenerationEnablesReissue(t *testing.T) {
	k1, err := DeriveIdempotencyKey(schemeUUID, entitlementUUID, 1, 1, money.Paise(50_000_000), beneA)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := DeriveIdempotencyKey(schemeUUID, entitlementUUID, 1, 2, money.Paise(50_000_000), beneA)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("incrementing the attempt must change the key, or a failed payout can never be reissued")
	}

	// And the same attempt must be stable, or every retry becomes a new payment.
	again, err := DeriveIdempotencyKey(schemeUUID, entitlementUUID, 1, 1, money.Paise(50_000_000), beneA)
	if err != nil {
		t.Fatal(err)
	}
	if again != k1 {
		t.Fatal("the key must be deterministic for a given attempt")
	}
}

func TestKeyVariesWithEveryIdentifyingField(t *testing.T) {
	base, err := DeriveIdempotencyKey(schemeUUID, entitlementUUID, 1, 1, money.Paise(50_000_000), beneA)
	if err != nil {
		t.Fatal(err)
	}

	variants := map[string]struct {
		period uint32
		amount money.Paise
		bene   string
	}{
		"period":      {2, money.Paise(50_000_000), beneA},
		"amount":      {1, money.Paise(50_000_001), beneA},
		"beneficiary": {1, money.Paise(50_000_000), beneB},
	}

	for name, v := range variants {
		got, err := DeriveIdempotencyKey(schemeUUID, entitlementUUID, v.period, 1, v.amount, v.bene)
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Errorf("changing the %s must change the key", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Deemed success
// ---------------------------------------------------------------------------

// TestDeemedSuccessNeverLooksLikeFailure guards the most dangerous behaviour in the fiat path.
//
// IMPS and UPI payouts can sit in processing for up to T+3 working days, because NPCI may return a
// deemed-success outcome where it is genuinely unknown whether the beneficiary was credited.
// Razorpay holds the payout in processing rather than reporting a state it cannot stand behind.
//
// Any timeout-and-retry policy over that window double-pays. So there must be no elapsed-time
// condition under which a processing payout is reported as needing reissue, and this test asserts
// that across repeated observation.
func TestDeemedSuccessNeverLooksLikeFailure(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx := context.Background()

	// Three observation rounds stand in for the T+3 working day window.
	const lingerRounds = 3
	m.ScriptOutcome(beneA, DeemedSuccessOutcome(lingerRounds))

	p, err := m.CreatePayout(ctx, validRequest("0xkey-deemed", money.Paise(50_000_000)))
	if err != nil {
		t.Fatal(err)
	}

	for round := 1; round <= lingerRounds; round++ {
		if _, err := m.Advance(ctx); err != nil {
			t.Fatal(err)
		}
		cur, err := m.FetchPayout(ctx, p.ProviderID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status != StatusProcessing {
			t.Fatalf("round %d: status %s, expected to remain processing", round, cur.Status)
		}
		if cur.NeedsReissueNow() {
			t.Fatal("a payout held in processing must never be marked for reissue; reissuing " +
				"during a deemed-success window pays the unitholder twice")
		}
		if !cur.Status.IsInFlight() {
			t.Fatal("processing must count as in flight")
		}
		if cur.UTR != "" {
			t.Fatal("no UTR should exist before the credit is confirmed")
		}
	}

	// The final observation resolves it.
	if _, err := m.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := m.FetchPayout(ctx, p.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != StatusProcessed {
		t.Fatalf("status %s, want processed", cur.Status)
	}
	if cur.UTR == "" {
		t.Error("a confirmed credit must carry a UTR, the only reference shared with the " +
			"unitholder's own bank statement")
	}
}

// ---------------------------------------------------------------------------
// Late reversal
// ---------------------------------------------------------------------------

// TestLateReversalAfterProcessed drives the scenario the on-chain reversal path was built for.
func TestLateReversalAfterProcessed(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx := context.Background()

	amount := money.Paise(50_000_000)
	p, err := m.CreatePayout(ctx, validRequest("0xkey-late", amount))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Advance(ctx); err != nil { // processing -> processed
		t.Fatal(err)
	}
	cur, _ := m.FetchPayout(ctx, p.ProviderID)
	if cur.Status != StatusProcessed {
		t.Fatalf("status %s, want processed", cur.Status)
	}
	balAfterProcessed, _ := m.Balance(ctx)

	// The reversal lands later, after the period would already have been anchored.
	if err := m.ForceReversal(ctx, p.ProviderID, SourceBeneficiaryBank,
		"Credit reversed by the clearing house"); err != nil {
		t.Fatalf("a reversal after processed must be permitted: %v", err)
	}

	cur, _ = m.FetchPayout(ctx, p.ProviderID)
	if cur.Status != StatusReversed {
		t.Fatalf("status %s, want reversed", cur.Status)
	}
	if cur.StatusDetails.Source != SourceBeneficiaryBank {
		t.Errorf("failure source %q not recorded", cur.StatusDetails.Source)
	}
	if cur.StatusDetails.Source.IsOurFault() {
		t.Error("a beneficiary bank failure is not ours to fix by reissuing to the same account")
	}

	// The reversal credits the amount with fees and tax back to the business account.
	balAfterReversal, _ := m.Balance(ctx)
	expected := balAfterProcessed + amount + cur.FeesPaise + cur.TaxPaise
	if balAfterReversal != expected {
		t.Errorf("balance %d after reversal, want %d (amount plus fees and tax returned)",
			balAfterReversal, expected)
	}
}

// ---------------------------------------------------------------------------
// Balance accounting
// ---------------------------------------------------------------------------

// TestFeesAreChargedToTheSchemeNotTheUnitholder pins down arithmetic the 95% floor depends on.
//
// Fees and tax are debited from the business account on top of the transfer, so the unitholder
// receives the full amount. Netting fees out of the distributed figure would understate what was
// distributed and misstate the floor.
func TestFeesAreChargedToTheSchemeNotTheUnitholder(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx := context.Background()

	amount := money.Paise(50_000_000)
	before, _ := m.Balance(ctx)

	p, err := m.CreatePayout(ctx, validRequest("0xkey-fees", amount))
	if err != nil {
		t.Fatal(err)
	}
	if p.AmountPaise != amount {
		t.Fatalf("the beneficiary amount must be untouched by fees: %d, want %d", p.AmountPaise, amount)
	}
	if p.FeesPaise == 0 {
		t.Fatal("the fixture should charge a fee so the accounting is actually exercised")
	}

	after, _ := m.Balance(ctx)
	debited := before - after
	want := amount + p.FeesPaise + p.TaxPaise
	if debited != want {
		t.Errorf("debited %d, want %d (amount plus fees plus tax)", debited, want)
	}
}

func TestInsufficientBalanceFailsRatherThanQueues(t *testing.T) {
	m := newMock(t, money.Paise(1_000))
	ctx := context.Background()

	_, err := m.CreatePayout(ctx, validRequest("0xkey-broke", money.Paise(50_000_000)))
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	if m.count() != 0 {
		t.Error("no payout should exist after a balance failure")
	}
}

// TestCancelledQueuedPayoutDoesNotCreditPhantomMoney covers a bug this code had.
//
// A queued payout is never debited, since the reason it queued is that the balance could not cover
// it. Refunding a recomputed amount on cancellation would have credited money that was never spent,
// and the simulated balance would drift upward every time a payout queued and was cancelled.
func TestCancelledQueuedPayoutDoesNotCreditPhantomMoney(t *testing.T) {
	m := newMock(t, money.Paise(1_000))
	ctx := context.Background()

	req := validRequest("0xkey-queued", money.Paise(50_000_000))
	req.QueueIfLowBalance = true

	// Validate refuses the flag as policy, so the provider path is exercised directly to confirm the
	// accounting rather than the policy.
	p, err := m.createQueuedForTest(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusQueued {
		t.Fatalf("status %s, want queued", p.Status)
	}

	before, _ := m.Balance(ctx)
	if err := m.Cancel(ctx, p.ProviderID); err != nil {
		t.Fatal(err)
	}
	after, _ := m.Balance(ctx)

	if after != before {
		t.Errorf("balance moved from %d to %d on cancelling a queued payout; nothing was ever "+
			"debited so nothing should be credited", before, after)
	}
}

func TestCancelOnlyFromQueued(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx := context.Background()

	p, err := m.CreatePayout(ctx, validRequest("0xkey-cancel", money.Paise(50_000_000)))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(ctx, p.ProviderID); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("cancelling a processing payout must be refused, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// The mock refuses to teach wrong lessons
// ---------------------------------------------------------------------------

// TestMockRejectsIllegalScript confirms the mock will not perform a transition the real provider
// never performs. A permissive mock would let the orchestrator depend on impossible behaviour, and
// the discrepancy would surface against live money.
func TestMockRejectsIllegalScript(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx := context.Background()

	// processing -> cancelled is not in the documented graph.
	m.ScriptOutcome(beneA, Outcome{Path: []Status{StatusProcessing, StatusCancelled}})

	if _, err := m.CreatePayout(ctx, validRequest("0xkey-illegal", money.Paise(50_000_000))); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Advance(ctx); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("the mock must refuse an impossible path, got %v", err)
	}
}

func TestAdvanceIsStableOnceScriptsAreExhausted(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx := context.Background()

	p, err := m.CreatePayout(ctx, validRequest("0xkey-stable", money.Paise(50_000_000)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := m.Advance(ctx); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := m.FetchPayout(ctx, p.ProviderID)
	if cur.Status != StatusProcessed {
		t.Fatalf("status %s, want processed and stable", cur.Status)
	}
}

func TestFetchUnknownPayout(t *testing.T) {
	m := newMock(t, money.Paise(1))
	if _, err := m.FetchPayout(context.Background(), "pout_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Request validation
// ---------------------------------------------------------------------------

func TestValidationMirrorsProviderConstraints(t *testing.T) {
	cases := map[string]func(*Request){
		"amount below the ₹1 minimum": func(r *Request) { r.AmountPaise = 99 },
		"zero amount":                 func(r *Request) { r.AmountPaise = 0 },
		"negative amount":             func(r *Request) { r.AmountPaise = -1 },
		"lowercase mode":              func(r *Request) { r.Mode = Mode("imps") },
		"unknown mode":                func(r *Request) { r.Mode = Mode("UPI") },
		"invented purpose":            func(r *Request) { r.Purpose = Purpose("distribution") },
		"missing idempotency key":     func(r *Request) { r.IdempotencyKey = "" },
		"missing fund account":        func(r *Request) { r.FundAccountID = "" },
		"fund account without prefix": func(r *Request) { r.FundAccountID = "00000000000001" },
		"reference id too long":       func(r *Request) { r.ReferenceID = strings.Repeat("x", 41) },
		"narration too long":          func(r *Request) { r.Narration = strings.Repeat("a", 31) },
		"narration with a hyphen":     func(r *Request) { r.Narration = "AcreSync P1-0001" },
		"narration with punctuation":  func(r *Request) { r.Narration = "AcreSync payout." },
		"queue on low balance":        func(r *Request) { r.QueueIfLowBalance = true },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := validRequest("0xkey", money.Paise(50_000_000))
			mutate(&r)
			if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}
}

func TestValidRequestPasses(t *testing.T) {
	if err := validRequest("0xkey", money.Paise(50_000_000)).Validate(); err != nil {
		t.Fatalf("the fixture must be valid or every negative case above is vacuous: %v", err)
	}
}

// TestNarrationSanitiserProducesAcceptableText matters because the obvious thing to put on a bank
// statement is a period or holder reference, and those carry hyphens, which the provider rejects.
func TestNarrationSanitiser(t *testing.T) {
	cases := map[string]string{
		"AcreSync P1-0001":                   "AcreSync P1 0001",
		"scheme/period.2":                    "scheme period 2",
		"  multiple   spaces  ":              "multiple spaces",
		"AcreSync Distribution Period Two 2026": "AcreSync Distribution Period",
	}
	for in, want := range cases {
		got := SanitiseNarration(in)
		if got != want {
			t.Errorf("SanitiseNarration(%q) = %q, want %q", in, got, want)
		}
		r := validRequest("0xkey", money.Paise(50_000_000))
		r.Narration = got
		if err := r.Validate(); err != nil {
			t.Errorf("sanitised narration %q still fails validation: %v", got, err)
		}
	}
}

func TestNotesLimits(t *testing.T) {
	r := validRequest("0xkey", money.Paise(50_000_000))
	r.Notes = map[string]string{}
	for i := 0; i < MaxNotes+1; i++ {
		r.Notes[string(rune('a'+i))] = "v"
	}
	if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("more than %d notes must be rejected, got %v", MaxNotes, err)
	}

	r.Notes = map[string]string{"k": strings.Repeat("x", MaxNoteValueLen+1)}
	if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("an oversized note value must be rejected, got %v", err)
	}
}

func TestWrongSourceAccountRejected(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	r := validRequest("0xkey-src", money.Paise(50_000_000))
	r.AccountNumber = "9999999999999999"

	if _, err := m.CreatePayout(context.Background(), r); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest for a foreign source account, got %v", err)
	}
}

func TestModeSettlementExpectations(t *testing.T) {
	if !ModeIMPS.SettlesSameDay() {
		t.Error("IMPS settles immediately")
	}
	if ModeNEFT.SettlesSameDay() {
		t.Error("NEFT settles in batches, so a period must not expect same-day terminal states")
	}
	if !ModeIMPS.MayReportDeemedSuccess() {
		t.Error("IMPS can report deemed success")
	}
}

func TestContextCancellation(t *testing.T) {
	m := newMock(t, money.Paise(1_000_000_000))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.CreatePayout(ctx, validRequest("0xkey-ctx", money.Paise(50_000_000))); !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}
