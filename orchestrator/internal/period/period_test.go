package period

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/snapshot"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func anchorBytes(b byte) [32]byte {
	var h [32]byte
	for i := range h {
		h[i] = b
	}
	return h
}

func wallet(n int) string { return fmt.Sprintf("0x%040x", n) }

// readyRegister is 500 units across 201 countable holders plus the excluded manager.
func readyRegister(t *testing.T) *snapshot.Snapshot {
	t.Helper()

	holdings := []snapshot.Holding{{
		InvestorID: "00000001-0000-4000-8000-000000000000", WalletAddress: wallet(1),
		InvestorAnchor: anchorBytes(1), Units: 25, ExcludedFromHolderCount: true,
	}}
	for i := 0; i < 200; i++ {
		holdings = append(holdings, snapshot.Holding{
			InvestorID:     fmt.Sprintf("%08d-0000-4000-8000-000000000000", i+2),
			WalletAddress:  wallet(i + 2),
			InvestorAnchor: anchorBytes(byte(i + 2)),
			Units:          2,
		})
	}
	holdings = append(holdings, snapshot.Holding{
		InvestorID: "00000500-0000-4000-8000-000000000000", WalletAddress: wallet(500),
		InvestorAnchor: anchorBytes(250), Units: 75,
	})

	d := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	snap, err := snapshot.Build(snapshot.BuildInput{
		SchemeRef: anchorBytes(0x5c), PeriodID: 1,
		RecordDate: d, PeriodEnd: d, TakenAt: d,
		Holdings: holdings, ExpectedTotalUnits: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// fullyReady is the evidence for a period that has satisfied everything.
func fullyReady(t *testing.T) Evidence {
	t.Helper()
	snap := readyRegister(t)

	return Evidence{
		RentReceiptCount:       4,
		RentVariancesExplained: true,

		PlanPresent:       true,
		NDCFPaise:         money.Paise(29_000_000_000),
		DistributedPaise:  money.Paise(27_550_000_000),
		DistributionBps:   9500,
		StatementDigestOK: true,
		StatementPinned:   true,

		IMApprovedBy:      "im.operator@acresync",
		TrusteeApprovedBy: "trustee@independent",

		RecordDateDeclared: true,
		Snapshot:           snap,
		SnapshotPinned:     true,

		BlockingDivergences: 0,

		Anchor: &AnchorRef{
			OutboxID: "0198f1a0-0000-4000-8000-000000000000",
			Status:   "CONFIRMED", Confirmations: 5,
			TxHash: "0x" + strings.Repeat("ab", 32),
		},

		// 500 units across 202 register lines.
		//
		// 202, not 201. The register has 202 lines and 201 of them are countable toward the statutory
		// minimum, because the manager is excluded from the count. Every line is still entitled to a
		// distribution, so the number of entitlements is the line count. These are three different
		// numbers that are easy to confuse; see TestThreeCountsAreDistinct.
		EntitledUnits:   500,
		EntitledHolders: 202,

		Payouts: PayoutTally{
			Total: 202, InFlight: 0, CreditConfirmed: 202,
			ConfirmedPaise: money.Paise(27_550_000_000),
		},

		RequireMinimumHolders: true,
	}
}

// ---------------------------------------------------------------------------
// The graph
// ---------------------------------------------------------------------------

func TestStatusEnumMatchesPostgres(t *testing.T) {
	pg := []string{
		"OPEN", "RENT_COLLECTED", "NDCF_DRAFTED", "NDCF_APPROVED",
		"RECORD_DATE_DECLARED", "SNAPSHOT_TAKEN", "RECONCILED", "CHAIN_STALE",
		"ANCHORED", "ENTITLEMENTS_ANCHORED", "PAYOUT_INSTRUCTED",
		"PAYOUTS_CONFIRMED", "CLOSED", "REVERSED",
	}
	for _, name := range pg {
		if !Status(name).Valid() {
			t.Errorf("%s is in the period_status enum but unmapped here", name)
		}
	}
	if len(AllStatuses()) != len(pg) {
		t.Errorf("AllStatuses has %d entries, the enum has %d", len(AllStatuses()), len(pg))
	}
}

func TestHappyPathWalksEndToEnd(t *testing.T) {
	ev := fullyReady(t)

	// Every step of the intended sequence must be both legal and satisfied.
	cur := StatusOpen
	var visited []Status
	for {
		visited = append(visited, cur)
		next, ok := NextExpected(cur)
		if !ok || cur == StatusClosed {
			break
		}
		if err := Guard(cur, next, ev); err != nil {
			t.Fatalf("%s -> %s refused with complete evidence: %v", cur, next, err)
		}
		cur = next
		if len(visited) > 20 {
			t.Fatal("the happy path does not terminate")
		}
	}

	if cur != StatusClosed {
		t.Fatalf("the happy path ended at %s, want CLOSED", cur)
	}
	// OPEN through CLOSED is twelve states; CHAIN_STALE and REVERSED are off the happy path.
	if len(visited) != 12 {
		t.Errorf("walked %d states (%v), want 12", len(visited), visited)
	}
}

// TestReversalUnreachableAfterSettlement is the rule the contract and a trigger both enforce.
//
// reversePeriod rejects PayoutsConfirmed and Closed, and a Postgres trigger raises with "use a
// carry-forward adjustment instead". Once money has left the escrow, unwinding the period would be a
// claim that the payments did not happen, and the honest remedy is a compensating entry that is
// itself visible.
func TestReversalUnreachableAfterSettlement(t *testing.T) {
	for _, from := range []Status{StatusPayoutsConfirmed, StatusClosed} {
		if from.CanTransitionTo(StatusReversed) {
			t.Errorf("the graph must not permit %s -> REVERSED", from)
		}
		if from.IsReversible() {
			t.Errorf("%s must not be reported as reversible", from)
		}
		if !from.FiatHasSettled() {
			t.Errorf("%s should be recognised as settled", from)
		}

		err := Guard(from, StatusReversed, fullyReady(t))
		if err == nil {
			t.Fatalf("%s -> REVERSED must be refused", from)
		}
		// The message has to name the alternative, or an operator hunting a way to force it has
		// nowhere to go.
		if !strings.Contains(err.Error(), "carry-forward") && !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("the refusal should point at a carry-forward adjustment, got: %v", err)
		}
	}

	// And it must remain reachable while it is still safe.
	for _, from := range []Status{StatusAnchored, StatusEntitlementsAnchored} {
		if !from.IsReversible() {
			t.Errorf("%s must be reversible", from)
		}
		ev := fullyReady(t)
		ev.Payouts = PayoutTally{} // nothing instructed yet
		if err := Guard(from, StatusReversed, ev); err != nil {
			t.Errorf("%s -> REVERSED must be permitted before settlement: %v", from, err)
		}
	}
}

// TestReversalRefusedWhileAnyPayoutExists mirrors the trigger that counts in-flight and settled rows.
func TestReversalRefusedWhileAnyPayoutExists(t *testing.T) {
	cases := map[string]PayoutTally{
		"in flight": {Total: 5, InFlight: 5},
		"settled":   {Total: 5, CreditConfirmed: 5},
		"mixed":     {Total: 5, InFlight: 2, CreditConfirmed: 3},
	}
	for name, tally := range cases {
		t.Run(name, func(t *testing.T) {
			ev := fullyReady(t)
			ev.Payouts = tally
			if err := Guard(StatusEntitlementsAnchored, StatusReversed, ev); !errors.Is(err, ErrFiatSettled) {
				t.Fatalf("want ErrFiatSettled, got %v", err)
			}
		})
	}
}

func TestReversalRequiresTrusteeApproval(t *testing.T) {
	ev := fullyReady(t)
	ev.Payouts = PayoutTally{}
	ev.TrusteeApprovedBy = ""

	if err := Guard(StatusAnchored, StatusReversed, ev); !errors.Is(err, ErrFourEyes) {
		t.Fatalf("want ErrFourEyes, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// The confirmation gate
// ---------------------------------------------------------------------------

// TestPayoutRequiresFiveConfirmations is the gate that stops a reorg costing real money.
//
// Mirrors enforce_payout_anchor_confirmed. Reading chain state and then irreversibly transferring is
// the most dangerous sequence in the architecture: a reorg after the transfer has cleared cannot be
// undone by any amount of later reconciliation.
func TestPayoutRequiresFiveConfirmations(t *testing.T) {
	for conf := 0; conf < MinAnchorConfirmations; conf++ {
		ev := fullyReady(t)
		ev.Anchor.Confirmations = conf

		err := Guard(StatusEntitlementsAnchored, StatusPayoutInstructed, ev)
		if !errors.Is(err, ErrAnchorNotConfirmed) {
			t.Fatalf("%d confirmations must block payouts, got %v", conf, err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d required", MinAnchorConfirmations)) {
			t.Errorf("the error should state the requirement, got: %v", err)
		}
	}

	ev := fullyReady(t)
	ev.Anchor.Confirmations = MinAnchorConfirmations
	if err := Guard(StatusEntitlementsAnchored, StatusPayoutInstructed, ev); err != nil {
		t.Fatalf("%d confirmations must be sufficient: %v", MinAnchorConfirmations, err)
	}
}

func TestPayoutRequiresConfirmedOutboxStatus(t *testing.T) {
	// Every non-CONFIRMED outbox state must block, even with a deep confirmation count, because the
	// count alone does not mean the row was accepted.
	for _, st := range []string{"QUEUED", "SIGNED", "BROADCAST", "CONFIRMING", "FAILED", "DEAD_LETTER"} {
		ev := fullyReady(t)
		ev.Anchor.Status = st
		ev.Anchor.Confirmations = 50

		if err := Guard(StatusEntitlementsAnchored, StatusPayoutInstructed, ev); !errors.Is(err, ErrAnchorNotConfirmed) {
			t.Errorf("outbox status %s must block payouts, got %v", st, err)
		}
	}
}

func TestPayoutRequiresAnAnchorAtAll(t *testing.T) {
	ev := fullyReady(t)
	ev.Anchor = nil
	if err := Guard(StatusEntitlementsAnchored, StatusPayoutInstructed, ev); !errors.Is(err, ErrAnchorNotConfirmed) {
		t.Fatalf("want ErrAnchorNotConfirmed, got %v", err)
	}
}

func TestAnchorRefIsConfirmed(t *testing.T) {
	var nilRef *AnchorRef
	if nilRef.IsConfirmed() {
		t.Error("a nil anchor is not confirmed")
	}
	if (&AnchorRef{Status: "CONFIRMED", Confirmations: 4}).IsConfirmed() {
		t.Error("four confirmations is not enough")
	}
	if (&AnchorRef{Status: "CONFIRMING", Confirmations: 9}).IsConfirmed() {
		t.Error("CONFIRMING is not CONFIRMED")
	}
	if !(&AnchorRef{Status: "CONFIRMED", Confirmations: 5}).IsConfirmed() {
		t.Error("five confirmations on a CONFIRMED row is the gate")
	}
}

// ---------------------------------------------------------------------------
// CHAIN_STALE
// ---------------------------------------------------------------------------

// TestDivergenceBlocksAnchoringAndPayouts covers the gate in both places it matters.
func TestDivergenceBlocksAnchoringAndPayouts(t *testing.T) {
	t.Run("anchoring", func(t *testing.T) {
		ev := fullyReady(t)
		ev.BlockingDivergences = 1
		if err := Guard(StatusReconciled, StatusAnchored, ev); !errors.Is(err, ErrDiverged) {
			t.Fatalf("want ErrDiverged, got %v", err)
		}
	})

	t.Run("payouts", func(t *testing.T) {
		ev := fullyReady(t)
		ev.BlockingDivergences = 1
		if err := Guard(StatusEntitlementsAnchored, StatusPayoutInstructed, ev); !errors.Is(err, ErrDiverged) {
			t.Fatalf("want ErrDiverged, got %v", err)
		}
	})

	t.Run("reconciled state itself", func(t *testing.T) {
		ev := fullyReady(t)
		ev.BlockingDivergences = 2
		if err := Guard(StatusSnapshotTaken, StatusReconciled, ev); !errors.Is(err, ErrDiverged) {
			t.Fatalf("want ErrDiverged, got %v", err)
		}
	})
}

// TestChainStaleReturnsToSnapshotNotToAnchoring is the recovery path.
//
// After a divergence is resolved the register has changed, so the snapshot has to be retaken and
// reconciled again. Returning straight to RECONCILED would trust a comparison made before the
// divergence was found.
func TestChainStaleReturnsToSnapshotNotToAnchoring(t *testing.T) {
	if StatusChainStale.CanTransitionTo(StatusAnchored) {
		t.Error("CHAIN_STALE must not lead directly to ANCHORED")
	}
	if StatusChainStale.CanTransitionTo(StatusReconciled) {
		t.Error("CHAIN_STALE must not lead directly to RECONCILED; the snapshot has to be retaken")
	}
	if !StatusChainStale.CanTransitionTo(StatusSnapshotTaken) {
		t.Error("CHAIN_STALE must return to SNAPSHOT_TAKEN")
	}
	if !StatusChainStale.BlocksPayout() {
		t.Error("CHAIN_STALE must block payouts")
	}
}

// TestChainStaleReachableFromEveryPreAnchorState matters because a divergence can be discovered at
// any point before the figures are committed.
func TestChainStaleReachableFromEveryPreAnchorState(t *testing.T) {
	preAnchor := []Status{
		StatusOpen, StatusRentCollected, StatusNDCFDrafted, StatusNDCFApproved,
		StatusRecordDateDeclared, StatusSnapshotTaken, StatusReconciled,
	}
	for _, s := range preAnchor {
		if !s.CanTransitionTo(StatusChainStale) {
			t.Errorf("%s must be able to enter CHAIN_STALE", s)
		}
	}

	// Entering it requires evidence of a divergence, not merely a wish to stop.
	ev := fullyReady(t)
	ev.BlockingDivergences = 0
	if err := Guard(StatusOpen, StatusChainStale, ev); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("CHAIN_STALE without a divergence must be refused, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Four eyes
// ---------------------------------------------------------------------------

func TestFourEyesRequiresTwoDistinctActors(t *testing.T) {
	cases := map[string]func(*Evidence){
		"no IM approval":      func(e *Evidence) { e.IMApprovedBy = "" },
		"no trustee approval": func(e *Evidence) { e.TrusteeApprovedBy = "" },
		"same actor twice":    func(e *Evidence) { e.TrusteeApprovedBy = e.IMApprovedBy },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ev := fullyReady(t)
			mutate(&ev)
			if err := Guard(StatusNDCFDrafted, StatusNDCFApproved, ev); !errors.Is(err, ErrFourEyes) {
				t.Fatalf("want ErrFourEyes, got %v", err)
			}
			// And anchoring must refuse the same thing, so approval cannot be bypassed by skipping
			// the approval state.
			if err := Guard(StatusReconciled, StatusAnchored, ev); !errors.Is(err, ErrFourEyes) {
				t.Fatalf("anchoring must also require four eyes, got %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Anchoring preconditions
// ---------------------------------------------------------------------------

// TestAnchoringRequiresRetrievableEvidence covers a subtle failure.
//
// Anchoring a digest whose document was never pinned commits to evidence nobody can fetch. That is
// worse than not anchoring at all: the period looks verifiable and is not, and the gap only surfaces
// when an outside party tries to check it.
func TestAnchoringRequiresRetrievableEvidence(t *testing.T) {
	cases := map[string]func(*Evidence){
		"statement not pinned": func(e *Evidence) { e.StatementPinned = false },
		"snapshot not pinned":  func(e *Evidence) { e.SnapshotPinned = false },
		"no statement digest":  func(e *Evidence) { e.StatementDigestOK = false },
		"no snapshot":          func(e *Evidence) { e.Snapshot = nil },
		"no plan":              func(e *Evidence) { e.PlanPresent = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ev := fullyReady(t)
			mutate(&ev)
			if err := Guard(StatusReconciled, StatusAnchored, ev); err == nil {
				t.Fatal("anchoring must be refused")
			}
		})
	}
}

func TestAnchoringRejectsOutOfRangeBps(t *testing.T) {
	for _, bps := range []uint16{0, 9499, 10_001, 65535} {
		ev := fullyReady(t)
		ev.DistributionBps = bps
		if err := Guard(StatusReconciled, StatusAnchored, ev); !errors.Is(err, ErrPreconditionFailed) {
			t.Errorf("%d bps must be refused, got %v", bps, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Entitlement completeness
// ---------------------------------------------------------------------------

// TestEntitlementsMustCoverEveryUnit mirrors the contract check and the Postgres assertion.
//
// Partial coverage means some holder has no entitlement recorded, and they are precisely the person
// who cannot later prove what they were owed.
func TestEntitlementsMustCoverEveryUnit(t *testing.T) {
	ev := fullyReady(t)
	ev.EntitledUnits = 499

	err := Guard(StatusAnchored, StatusEntitlementsAnchored, ev)
	if !errors.Is(err, ErrEntitlementsPartial) {
		t.Fatalf("want ErrEntitlementsPartial, got %v", err)
	}
	if !strings.Contains(err.Error(), "499 of 500") {
		t.Errorf("the error should state the shortfall, got: %v", err)
	}
}

// TestEntitlementDenominatorIsEveryIssuedUnit re-asserts the excluded-flag rule at this layer.
//
// The manager's 25 units are excluded from the holder count and included in the distribution. If the
// guard compared against 475 it would accept a run that paid the manager nothing, and the figure
// would still satisfy the 95% floor.
func TestEntitlementDenominatorIsEveryIssuedUnit(t *testing.T) {
	ev := fullyReady(t)

	if got := ev.Snapshot.EntitlementDenominator(); got != 500 {
		t.Fatalf("denominator = %d, want 500", got)
	}

	// 475 units is the public float, and it must not satisfy the guard.
	ev.EntitledUnits = 475
	if err := Guard(StatusAnchored, StatusEntitlementsAnchored, ev); !errors.Is(err, ErrEntitlementsPartial) {
		t.Fatal("covering only the public float must be refused; the manager is entitled to " +
			"distributions on its 25 units even though they do not count toward the 200-holder floor")
	}
}

func TestEntitlementsMustCoverEveryHolder(t *testing.T) {
	ev := fullyReady(t)
	ev.EntitledHolders = 200
	if err := Guard(StatusAnchored, StatusEntitlementsAnchored, ev); !errors.Is(err, ErrEntitlementsPartial) {
		t.Fatalf("want ErrEntitlementsPartial, got %v", err)
	}
}

// TestThreeCountsAreDistinct separates three numbers that are easy to conflate and that each control
// something different.
//
// This test exists because writing the fixture above got it wrong on the first attempt: 201 was used
// where 202 belonged, and the guard caught it. Had the guard compared against the countable count
// instead, the manager would have received no entitlement and the run would have looked complete.
//
//	TotalUnits      500  the entitlement denominator, every issued unit
//	DistinctHolders 201  the statutory count, anchored as snapshotHolders, must be at least 200
//	len(Lines)      202  the number of entitlements and payouts to create
//
// The manager is the single line that separates the last two. It is excluded from the count and
// included in the distribution.
func TestThreeCountsAreDistinct(t *testing.T) {
	snap := readyRegister(t)

	if snap.TotalUnits != 500 {
		t.Errorf("TotalUnits = %d, want 500 (the entitlement denominator)", snap.TotalUnits)
	}
	if snap.DistinctHolders != 201 {
		t.Errorf("DistinctHolders = %d, want 201 (the statutory count)", snap.DistinctHolders)
	}
	if len(snap.Lines) != 202 {
		t.Errorf("line count = %d, want 202 (the number of entitlements)", len(snap.Lines))
	}

	if snap.DistinctHolders == uint32(len(snap.Lines)) {
		t.Fatal("the fixture must include an excluded holder, or this test is vacuous")
	}

	// Anchoring entitlements for the countable count only must be refused.
	ev := fullyReady(t)
	ev.EntitledHolders = snap.DistinctHolders
	if err := Guard(StatusAnchored, StatusEntitlementsAnchored, ev); !errors.Is(err, ErrEntitlementsPartial) {
		t.Fatal("creating entitlements for the countable holders only must be refused; the excluded " +
			"manager is still owed a distribution")
	}
}

// ---------------------------------------------------------------------------
// In-flight payouts
// ---------------------------------------------------------------------------

// TestCannotConfirmOrCloseWhilePayoutsInFlight covers the deemed-success window.
//
// An IMPS payout can sit in processing for up to three working days. Confirming during that window
// would commit a settled figure that is still moving, and the anchored number would be wrong the
// moment the next webhook lands.
func TestCannotConfirmOrCloseWhilePayoutsInFlight(t *testing.T) {
	ev := fullyReady(t)
	ev.Payouts = PayoutTally{Total: 201, InFlight: 1, CreditConfirmed: 200}

	if err := Guard(StatusPayoutInstructed, StatusPayoutsConfirmed, ev); !errors.Is(err, ErrPayoutsInFlight) {
		t.Fatalf("confirming with one payout in flight must be refused, got %v", err)
	}

	ev.Payouts.InFlight = 0
	ev.Payouts.CreditConfirmed = 201
	if err := Guard(StatusPayoutInstructed, StatusPayoutsConfirmed, ev); err != nil {
		t.Fatalf("once nothing is in flight, confirmation must proceed: %v", err)
	}
}

// TestCloseIsPermittedWithFailedPayouts is a deliberate asymmetry worth stating.
//
// A terminally failed payout is resolved: the money did not move and the holder is owed a reissue,
// which is tracked separately. Blocking closure on it would leave the period open indefinitely over a
// beneficiary whose bank details are wrong.
func TestCloseIsPermittedWithFailedPayouts(t *testing.T) {
	ev := fullyReady(t)
	ev.Payouts = PayoutTally{Total: 201, CreditConfirmed: 198, NeedingReissue: 3}

	if err := Guard(StatusPayoutsConfirmed, StatusClosed, ev); err != nil {
		t.Fatalf("a resolved-but-failed payout must not block closure: %v", err)
	}
}

func TestPayoutTallyResolved(t *testing.T) {
	if (PayoutTally{}).Resolved() {
		t.Error("an empty tally is not resolved")
	}
	if (PayoutTally{Total: 3, InFlight: 1}).Resolved() {
		t.Error("an in-flight payout means unresolved")
	}
	if !(PayoutTally{Total: 3, CreditConfirmed: 3}).Resolved() {
		t.Error("all confirmed is resolved")
	}
}

// ---------------------------------------------------------------------------
// Pause
// ---------------------------------------------------------------------------

// TestPauseBlocksLifecycleButNotReconciliationOrReversal is a deliberate asymmetry.
//
// Blinding the mirror to the depository during an emergency compounds the emergency, and refusing to
// unwind a bad period while paused would trap the one action most likely to be needed.
func TestPauseBlocksLifecycleButNotReconciliationOrReversal(t *testing.T) {
	ev := fullyReady(t)
	ev.Paused = true
	ev.BlockingDivergences = 1

	if err := Guard(StatusReconciled, StatusAnchored, ev); !errors.Is(err, ErrPaused) {
		t.Errorf("anchoring while paused must be refused, got %v", err)
	}
	if err := Guard(StatusOpen, StatusChainStale, ev); err != nil {
		t.Errorf("entering CHAIN_STALE must work while paused: %v", err)
	}

	rev := fullyReady(t)
	rev.Paused = true
	rev.Payouts = PayoutTally{}
	if err := Guard(StatusAnchored, StatusReversed, rev); err != nil {
		t.Errorf("reversal must work while paused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Illegal transitions
// ---------------------------------------------------------------------------

// TestAnchoringCannotSkipReconciliation is the shortcut that would matter most.
func TestAnchoringCannotSkipReconciliation(t *testing.T) {
	for _, from := range []Status{
		StatusOpen, StatusRentCollected, StatusNDCFDrafted, StatusNDCFApproved,
		StatusRecordDateDeclared, StatusSnapshotTaken,
	} {
		if from.CanTransitionTo(StatusAnchored) {
			t.Errorf("%s must not lead directly to ANCHORED; reconciliation against the "+
				"depository cannot be skipped", from)
		}
	}
	if !StatusReconciled.CanTransitionTo(StatusAnchored) {
		t.Error("RECONCILED is the state anchoring proceeds from")
	}
}

func TestPayoutsCannotSkipEntitlements(t *testing.T) {
	if StatusAnchored.CanTransitionTo(StatusPayoutInstructed) {
		t.Error("payouts must not be instructable straight from ANCHORED")
	}
}

func TestTerminalStatesAreTerminal(t *testing.T) {
	for _, s := range []Status{StatusClosed, StatusReversed} {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
		for _, to := range AllStatuses() {
			if s == to {
				continue
			}
			if s.CanTransitionTo(to) {
				t.Errorf("%s must not transition to %s", s, to)
			}
		}
	}
}

func TestUnknownStatusRejected(t *testing.T) {
	if err := CheckTransition(StatusOpen, Status("SETTLED")); !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("want ErrUnknownStatus, got %v", err)
	}
}

func TestSameStatusIsIdempotent(t *testing.T) {
	for _, s := range AllStatuses() {
		if err := Guard(s, s, Evidence{}); err != nil {
			t.Errorf("restating %s should be a no-op, got %v", s, err)
		}
	}
}

// ---------------------------------------------------------------------------
// On-chain mapping
// ---------------------------------------------------------------------------

// TestOnChainMappingMatchesContractEnum guards against a status mismatch being read as a divergence.
//
// The contract's PeriodStatus is None, Anchored, EntitlementsAnchored, PayoutsConfirmed, Closed,
// Reversed. Off-chain states with no counterpart must report that rather than mapping to zero, which
// would read as None and look like a period that was never anchored.
func TestOnChainMappingMatchesContractEnum(t *testing.T) {
	want := map[Status]uint8{
		StatusAnchored:             1,
		StatusEntitlementsAnchored: 2,
		StatusPayoutsConfirmed:     3,
		StatusClosed:               4,
		StatusReversed:             5,
	}

	for _, s := range AllStatuses() {
		ordinal, ok := s.ChainStatus()
		expected, shouldMap := want[s]

		if ok != shouldMap {
			t.Errorf("%s: ChainStatus mapped = %v, want %v", s, ok, shouldMap)
			continue
		}
		if ok && ordinal != expected {
			t.Errorf("%s maps to ordinal %d, want %d", s, ordinal, expected)
		}
		if s.IsOnChain() != shouldMap {
			t.Errorf("%s: IsOnChain = %v, want %v", s, s.IsOnChain(), shouldMap)
		}
	}

	// PAYOUT_INSTRUCTED deliberately has no chain counterpart: an instruction is not an outcome, and
	// there is nothing worth committing about money that has not moved.
	if _, ok := StatusPayoutInstructed.ChainStatus(); ok {
		t.Error("PAYOUT_INSTRUCTED must not map to a chain status")
	}
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

func TestReconcileMatching(t *testing.T) {
	positions := []Position{
		{InvestorID: "a", WalletAddress: wallet(1), Units: 25},
		{InvestorID: "b", WalletAddress: wallet(2), Units: 3},
	}
	rec, err := Reconcile(positions, positions)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != ReconMatched {
		t.Errorf("status = %s, want MATCHED", rec.Status)
	}
	if rec.BlocksPayout {
		t.Error("a matched run must not block payouts")
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileDetectsTheDangerousDirection covers a holder the depository knows and we do not.
//
// Iterating only our own register would miss them entirely. They are legally entitled and would
// receive nothing, and the run would report a clean match.
func TestReconcileDetectsHolderMissingFromMirror(t *testing.T) {
	depository := []Position{
		{InvestorID: "a", WalletAddress: wallet(1), Units: 25},
		{InvestorID: "b", WalletAddress: wallet(2), Units: 3},
	}
	mirror := []Position{
		{InvestorID: "a", WalletAddress: wallet(1), Units: 25},
	}

	rec, err := Reconcile(depository, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != ReconDiverged {
		t.Fatalf("status = %s, want DIVERGED", rec.Status)
	}
	if !rec.BlocksPayout {
		t.Fatal("a divergence must block payouts")
	}
	if len(rec.Diffs) != 1 {
		t.Fatalf("%d diffs, want 1", len(rec.Diffs))
	}
	d := rec.Diffs[0]
	if d.WalletAddress != wallet(2) || d.DepositoryUnits != 3 || d.ChainUnits != 0 || d.DiffUnits != 3 {
		t.Errorf("unexpected diff: %+v", d)
	}
	if !d.FavoursDepository() {
		t.Error("the depository shows more units, so the mirror is stale")
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileDetectsUnitsWeCreditedThatTheDepositoryDidNot is the worse direction.
func TestReconcileDetectsPhantomUnitsInMirror(t *testing.T) {
	depository := []Position{{InvestorID: "a", WalletAddress: wallet(1), Units: 10}}
	mirror := []Position{{InvestorID: "a", WalletAddress: wallet(1), Units: 14}}

	rec, err := Reconcile(depository, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != ReconDiverged {
		t.Fatalf("status = %s", rec.Status)
	}
	d := rec.Diffs[0]
	if d.DiffUnits != -4 {
		t.Errorf("diff = %d, want -4", d.DiffUnits)
	}
	if d.FavoursDepository() {
		t.Error("we credited more than the depository, so we would pay someone who does not hold the units")
	}
}

// TestReconcileIsOrderStable matters because an operator compares successive reports.
func TestReconcileIsOrderStable(t *testing.T) {
	depository := []Position{
		{InvestorID: "a", WalletAddress: wallet(3), Units: 1},
		{InvestorID: "b", WalletAddress: wallet(1), Units: 2},
		{InvestorID: "c", WalletAddress: wallet(2), Units: 3},
	}
	mirror := []Position{}

	first, err := Reconcile(depository, mirror)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		again, err := Reconcile(depository, mirror)
		if err != nil {
			t.Fatal(err)
		}
		for j := range again.Diffs {
			if again.Diffs[j].WalletAddress != first.Diffs[j].WalletAddress {
				t.Fatal("diff order must not depend on map iteration")
			}
		}
	}
	// And the order is the wallet ordering, not the input ordering.
	if first.Diffs[0].WalletAddress != wallet(1) {
		t.Errorf("first diff is %s, want the lowest wallet", first.Diffs[0].WalletAddress)
	}
}

func TestReconcileRejectsDuplicateWallets(t *testing.T) {
	dup := []Position{
		{InvestorID: "a", WalletAddress: wallet(1), Units: 1},
		{InvestorID: "b", WalletAddress: wallet(1), Units: 2},
	}
	if _, err := Reconcile(dup, nil); err == nil {
		t.Error("a duplicated wallet in the depository must be reported")
	}
	if _, err := Reconcile(nil, dup); err == nil {
		t.Error("a duplicated wallet in the mirror must be reported")
	}
}

// TestValidateRejectsDivergedRunThatDoesNotBlock is the invariant the whole design rests on.
func TestValidateRejectsDivergedRunThatDoesNotBlock(t *testing.T) {
	bad := &Reconciliation{
		Status:       ReconDiverged,
		Diffs:        []Diff{{WalletAddress: wallet(1), DepositoryUnits: 5, ChainUnits: 3, DiffUnits: 2}},
		BlocksPayout: false,
	}
	err := bad.Validate()
	if err == nil {
		t.Fatal("a DIVERGED run that does not block payouts must be rejected")
	}
	if !strings.Contains(err.Error(), "must block payouts") {
		t.Errorf("unexpected message: %v", err)
	}

	alsoBad := &Reconciliation{Status: ReconMatched, BlocksPayout: true}
	if err := alsoBad.Validate(); err == nil {
		t.Error("a MATCHED run must not block payouts")
	}

	inconsistent := &Reconciliation{
		Status:       ReconDiverged,
		Diffs:        []Diff{{WalletAddress: wallet(1), DepositoryUnits: 5, ChainUnits: 3, DiffUnits: 99}},
		BlocksPayout: true,
	}
	if err := inconsistent.Validate(); err == nil {
		t.Error("a diff whose stated difference is wrong must be rejected")
	}
}

func TestReconciliationTotalsAndSummary(t *testing.T) {
	depository := []Position{
		{InvestorID: "a", WalletAddress: wallet(1), Units: 25},
		{InvestorID: "b", WalletAddress: wallet(2), Units: 5},
	}
	mirror := []Position{
		{InvestorID: "a", WalletAddress: wallet(1), Units: 25},
		{InvestorID: "b", WalletAddress: wallet(2), Units: 4},
	}

	rec, err := Reconcile(depository, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if rec.DepositoryTotalUnits != 30 || rec.ChainTotalUnits != 29 {
		t.Errorf("totals: depository %d, mirror %d", rec.DepositoryTotalUnits, rec.ChainTotalUnits)
	}
	if rec.DepositoryHolderCount != 2 || rec.ChainHolderCount != 2 {
		t.Errorf("holder counts: %d and %d", rec.DepositoryHolderCount, rec.ChainHolderCount)
	}
	if rec.DivergenceCount() != 1 {
		t.Errorf("divergence count = %d", rec.DivergenceCount())
	}
	if !strings.Contains(rec.Summary(), "DIVERGED") {
		t.Errorf("summary should name the status: %s", rec.Summary())
	}
}

// TestZeroUnitPositionIsNotAHolder covers a holder who transferred everything away.
func TestZeroUnitPositionIsNotAHolder(t *testing.T) {
	positions := []Position{
		{InvestorID: "a", WalletAddress: wallet(1), Units: 5},
		{InvestorID: "b", WalletAddress: wallet(2), Units: 0},
	}
	rec, err := Reconcile(positions, positions)
	if err != nil {
		t.Fatal(err)
	}
	if rec.DepositoryHolderCount != 1 {
		t.Errorf("holder count = %d; a zero-unit position is not a holder and must not count "+
			"toward the statutory minimum", rec.DepositoryHolderCount)
	}
	if rec.Status != ReconMatched {
		t.Errorf("identical sources must match, got %s", rec.Status)
	}
}

func TestCanAdvance(t *testing.T) {
	ev := fullyReady(t)
	if !CanAdvance(StatusReconciled, StatusAnchored, ev) {
		t.Error("a ready period should be able to anchor")
	}
	ev.BlockingDivergences = 1
	if CanAdvance(StatusReconciled, StatusAnchored, ev) {
		t.Error("a diverged period should not")
	}
}
