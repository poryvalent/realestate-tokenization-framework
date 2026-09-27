package e2e

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/adjustment"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/period"
)

// TestCorrectionLifecycleAgainstPostgres drives the three correction regimes against real rows.
//
// # What this adds over the gate tests
//
// The gates already prove the triggers refuse the wrong thing, and they do it by writing rows by hand.
// That is the right way to test a constraint and says nothing about whether the Go engines produce rows
// the constraints accept, or whether the engines refuse what the database would.
//
// This drives the engines themselves — Reconcile, Resolve, the reversal ceremony and the adjustment
// arithmetic — and stores what they produce. The three regimes, in the order money travels:
//
//  1. Diverged, nothing moved. Resolve it, and payouts unblock.
//  2. Anchored, nothing settled. Reverse the period outright.
//  3. Fiat settled. Neither is available, so record a carry-forward adjustment.
func TestCorrectionLifecycleAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	periodID, entitlementID, anchorID := minimalPeriod(t, ctx, f)

	r := &correctionRun{
		f: f, t: t,
		periodID: periodID, periodSeq: 1,
		entitlementID: entitlementID, anchorID: anchorID,
	}

	// A scheme may have only one period that is not CLOSED or REVERSED, enforced by the
	// periods_one_active_per_scheme partial unique index. So each regime closes its period before the
	// next one opens, which is also how a real scheme runs: one distribution at a time.
	r.detectDivergence(ctx)
	r.proveThePayoutIsBlocked(ctx)
	r.resolveWithTheEngine(ctx)
	r.proveThePayoutIsAccepted(ctx)

	r.closePeriod(ctx, r.periodID)
	r.reverseBeforeFiat(ctx)

	r.proveReversalIsRefusedAfterFiat(ctx)
	r.recordCarryForward(ctx)
}

type correctionRun struct {
	f *fixture
	t *testing.T

	periodID      string
	periodSeq     uint32
	entitlementID string
	anchorID      string

	// The holder whose mirror is deliberately wrong.
	divergedInvestor string
	divergedWallet   string

	original *period.Reconciliation
	runID    string
}

var (
	correctionClock = time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	correctionLater = time.Date(2026, 12, 1, 15, 0, 0, 0, time.UTC)
)

// --- regime one: a divergence, before anything moved ----------------------------------------------

// detectDivergence introduces a real disagreement and finds it with the engine.
//
// The mirror is edited rather than the depository, because that is the direction reality takes: the
// depository is the legal register and settles transfers we have not yet mirrored.
func (r *correctionRun) detectDivergence(ctx context.Context) {
	t := r.t

	// Index 0 is the manager, so take a public holder.
	r.divergedInvestor = r.f.investorIDs[1]
	r.divergedWallet = r.f.wallets[1]

	if _, err := r.f.pool.Exec(ctx, `
		UPDATE unit_holdings SET units = units - 1
		 WHERE scheme_id = $1 AND investor_id = $2`,
		r.f.schemeID, r.divergedInvestor,
	); err != nil {
		t.Fatalf("introducing the divergence: %v", err)
	}

	// Both sides are read back out of Postgres. Comparing what we believe against what we believe would
	// prove nothing.
	rec, err := period.Reconcile(
		r.positions(ctx, "depository_register"),
		r.positions(ctx, "unit_holdings"),
	)
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("the reconciliation must satisfy its own invariant: %v", err)
	}
	if rec.Status != period.ReconDiverged || rec.DivergenceCount() != 1 {
		t.Fatalf("want exactly one divergence, got %s", rec.Summary())
	}

	d := rec.Diffs[0]
	if d.WalletAddress != r.divergedWallet {
		t.Fatalf("the divergence is on %s, expected %s", d.WalletAddress, r.divergedWallet)
	}
	if !d.FavoursDepository() {
		t.Fatalf("the depository should show more than our mirror, diff is %d", d.DiffUnits)
	}
	r.original = rec

	// Persisting is itself a test: recon_divergence_blocks refuses a DIVERGED run that does not block
	// payouts, so the insert only succeeds if the engine paired the two correctly.
	if err := r.f.pool.QueryRow(ctx, `
		INSERT INTO reconciliation_runs (
			scheme_id, simulated_clock_value, depository_total_units, chain_total_units,
			depository_holder_count, chain_holder_count, divergence_count, status, blocks_payout,
			idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		r.f.schemeID, correctionClock,
		rec.DepositoryTotalUnits, rec.ChainTotalUnits,
		rec.DepositoryHolderCount, rec.ChainHolderCount,
		rec.DivergenceCount(), string(rec.Status), rec.BlocksPayout,
		idemKey(t, idempotency.ActionRunReconciliation, r.f.schemeID, r.periodID,
			map[string]any{"run": "diverged"}),
	).Scan(&r.runID); err != nil {
		t.Fatalf("recording the reconciliation run: %v", err)
	}

	for _, d := range rec.Diffs {
		// recon_diffs_diff_consistent and recon_diffs_is_a_diff both bite here if the engine's
		// arithmetic disagrees with the database's.
		if _, err := r.f.pool.Exec(ctx, `
			INSERT INTO reconciliation_diffs (
				reconciliation_run_id, investor_id, wallet_address,
				depository_units, chain_units, diff_units
			) VALUES ($1, $2, $3, $4, $5, $6)`,
			r.runID, d.InvestorID, d.WalletAddress,
			int(d.DepositoryUnits), int(d.ChainUnits), d.DiffUnits,
		); err != nil {
			t.Fatalf("recording the divergence for %s: %v", d.WalletAddress, err)
		}
	}

	t.Logf("divergence: %s", rec.Summary())
}

// proveThePayoutIsBlocked exercises the trigger, not the Go guard.
func (r *correctionRun) proveThePayoutIsBlocked(ctx context.Context) {
	t := r.t

	err := insertPayout(ctx, r.f, r.entitlementID, r.f.bankIDs[0], r.anchorID,
		idemKey(t, idempotency.ActionInstructPayout, r.f.schemeID, r.periodID,
			map[string]any{"attempt": 0}))

	if err == nil {
		t.Fatal("the database must refuse a payout while a divergence is unresolved; paying against a " +
			"register known to be wrong is the one error that cannot be undone afterwards")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "diverg") {
		t.Errorf("the refusal should name the divergence, got: %v", err)
	}
	t.Logf("payout refused while diverged: %v", err)
}

// --- resolution, driven by the engine -------------------------------------------------------------

func (r *correctionRun) resolveWithTheEngine(ctx context.Context) {
	t := r.t

	// The correction: credit our mirror the unit the depository always showed.
	if _, err := r.f.pool.Exec(ctx, `
		UPDATE unit_holdings SET units = units + 1
		 WHERE scheme_id = $1 AND investor_id = $2`,
		r.f.schemeID, r.divergedInvestor,
	); err != nil {
		t.Fatalf("correcting the mirror: %v", err)
	}

	fresh, err := period.Reconcile(
		r.positions(ctx, "depository_register"),
		r.positions(ctx, "unit_holdings"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != period.ReconMatched {
		t.Fatalf("after the correction the register must match, got %s", fresh.Summary())
	}

	// The half a hand-written UPDATE cannot test: the engine refuses a resolution that explains nothing,
	// even though the re-run matches.
	if _, err := period.Resolve(period.ResolveInput{
		Original:   r.original,
		Fresh:      fresh,
		ApprovedBy: "ops@acresync",
		ResolvedAt: correctionLater,
	}); !errors.Is(err, period.ErrDiffUnaccounted) {
		t.Fatalf("a matching re-run is not sufficient on its own, got %v", err)
	}

	res, err := period.Resolve(period.ResolveInput{
		Original: r.original,
		Fresh:    fresh,
		Outcomes: []period.DiffOutcome{{
			WalletAddress: r.divergedWallet,
			Resolution:    period.ResolutionChainUpdated,
			ChainTx:       "0x" + strings.Repeat("ab", 32),
			ResolvedAt:    correctionLater,
		}},
		ApprovedBy: "ops@acresync",
		ResolvedAt: correctionLater,
	})
	if err != nil {
		t.Fatalf("a matching re-run with the divergence explained must resolve: %v", err)
	}

	// Our register moved, so any entitlement computed from the frozen snapshot is now computed against a
	// register that no longer exists.
	if !res.RequiresResnapshot {
		t.Fatal("the mirror was credited a unit, so the snapshot is stale and must be retaken")
	}
	if res.UnitsAfter != res.UnitsBefore+1 {
		t.Fatalf("units went %d -> %d, expected a gain of one", res.UnitsBefore, res.UnitsAfter)
	}
	t.Logf("resolution: %s", res.Summary())

	// The run-level CHECK requires resolved_at whenever the status is RESOLVED.
	if _, err := r.f.pool.Exec(ctx, `
		UPDATE reconciliation_runs
		   SET status = 'RESOLVED', resolved_at = $2, blocks_payout = FALSE, notes = $3
		 WHERE id = $1`,
		r.runID, res.ResolvedAt, res.Summary(),
	); err != nil {
		t.Fatalf("recording the resolution: %v", err)
	}

	for _, o := range res.Outcomes {
		if _, err := r.f.pool.Exec(ctx, `
			UPDATE reconciliation_diffs
			   SET resolution = $2, resolved_at = $3
			 WHERE reconciliation_run_id = $1 AND wallet_address = $4`,
			r.runID, string(o.Resolution), o.ResolvedAt, o.WalletAddress,
		); err != nil {
			t.Fatalf("recording the per-diff resolution: %v", err)
		}
	}

	var pending int
	if err := r.f.pool.QueryRow(ctx, `
		SELECT count(*) FROM reconciliation_diffs
		 WHERE reconciliation_run_id = $1 AND resolution = 'PENDING'`,
		r.runID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d divergences are still PENDING on a run marked RESOLVED", pending)
	}
}

func (r *correctionRun) proveThePayoutIsAccepted(ctx context.Context) {
	t := r.t

	if err := insertPayout(ctx, r.f, r.entitlementID, r.f.bankIDs[0], r.anchorID,
		idemKey(t, idempotency.ActionInstructPayout, r.f.schemeID, r.periodID,
			map[string]any{"attempt": 1})); err != nil {
		t.Fatalf("once resolved, the payout must be accepted: %v", err)
	}
}

// --- regime two: reversal, before fiat settles -----------------------------------------------------

func (r *correctionRun) reverseBeforeFiat(ctx context.Context) {
	t := r.t

	reversibleID := r.makePeriod(ctx, 2, "ANCHORED")
	narrative := sha256.Sum256([]byte("rent receipt 4471 was counted twice"))

	approval, err := period.NewReversalApproval(reversibleID, 2, period.StatusAnchored,
		period.ReasonDuplicateInjection, narrative, "trustee@acresync", correctionClock)
	if err != nil {
		t.Fatalf("the trustee's approval must be accepted: %v", err)
	}

	ev := period.Evidence{TrusteeApprovedBy: "trustee@acresync"}

	// One actor cannot do both halves, whatever roles they hold.
	if _, err := approval.Execute(period.ReversalExecution{
		Reason:          period.ReasonDuplicateInjection,
		NarrativeSHA256: narrative,
		ExecutedBy:      "trustee@acresync",
		ExecutedAt:      correctionLater,
		StatusNow:       period.StatusAnchored,
	}, ev); !errors.Is(err, period.ErrNotFourEyes) {
		t.Fatalf("approval and execution must be different people, got %v", err)
	}

	// Nor can the explanation change between them.
	if _, err := approval.Execute(period.ReversalExecution{
		Reason:          period.ReasonBankFailure,
		NarrativeSHA256: narrative,
		ExecutedBy:      "ops@acresync",
		ExecutedAt:      correctionLater,
		StatusNow:       period.StatusAnchored,
	}, ev); !errors.Is(err, period.ErrApprovalMismatch) {
		t.Fatalf("the reason must match what the trustee approved, got %v", err)
	}

	req, err := approval.Execute(period.ReversalExecution{
		Reason:          period.ReasonDuplicateInjection,
		NarrativeSHA256: narrative,
		ExecutedBy:      "ops@acresync",
		ExecutedAt:      correctionLater,
		StatusNow:       period.StatusAnchored,
	}, ev)
	if err != nil {
		t.Fatalf("a matching execution by a second actor must be accepted: %v", err)
	}

	// The window trigger permits this, because no payout exists for the period.
	if _, err := r.f.pool.Exec(ctx, `
		UPDATE distribution_periods
		   SET status = 'REVERSED', reversal_reason = $2,
		       reversal_narrative_sha256 = $3, reversed_at = $4
		 WHERE id = $1`,
		reversibleID, string(req.Reason), req.NarrativeSHA256[:], req.ExecutedAt,
	); err != nil {
		t.Fatalf("the reversal window trigger must permit a pre-fiat reversal: %v", err)
	}

	// A reversed period stays visible. An audit trail that can hide its own corrections is worth nothing.
	var status, storedReason string
	var reversedAt *time.Time
	if err := r.f.pool.QueryRow(ctx, `
		SELECT status, reversal_reason, reversed_at FROM distribution_periods WHERE id = $1`,
		reversibleID).Scan(&status, &storedReason, &reversedAt); err != nil {
		t.Fatal(err)
	}
	if status != "REVERSED" || storedReason != string(period.ReasonDuplicateInjection) {
		t.Fatalf("stored %s/%s, want REVERSED/DUPLICATE_INJECTION", status, storedReason)
	}
	if reversedAt == nil {
		t.Fatal("a reversal must record when it happened")
	}
	t.Logf("reversal: %s", req.Summary())
}

// --- regime three: after fiat, only a carry-forward ------------------------------------------------

func (r *correctionRun) proveReversalIsRefusedAfterFiat(ctx context.Context) {
	t := r.t

	// The Go guard, with the message that names the remedy.
	err := period.Guard(period.StatusPayoutsConfirmed, period.StatusReversed,
		period.Evidence{TrusteeApprovedBy: "trustee@acresync"})
	if !errors.Is(err, period.ErrFiatSettled) {
		t.Fatalf("the guard must refuse with ErrFiatSettled, got %v", err)
	}
	if !strings.Contains(err.Error(), "carry-forward") {
		t.Errorf("the guard's refusal must name the remedy, got: %v", err)
	}

	// And the trigger, independently of the guard. The reversed period 2 is terminal, so the scheme is
	// free for another.
	settledID := r.makePeriod(ctx, 3, "PAYOUTS_CONFIRMED")
	narrative := sha256.Sum256([]byte("too late"))

	_, dbErr := r.f.pool.Exec(ctx, `
		UPDATE distribution_periods
		   SET status = 'REVERSED', reversal_reason = 'DATA_ENTRY_ERROR',
		       reversal_narrative_sha256 = $2, reversed_at = $3
		 WHERE id = $1`,
		settledID, narrative[:], correctionLater,
	)
	if dbErr == nil {
		t.Fatal("the database must refuse to reverse a period whose payouts are confirmed")
	}
	if !strings.Contains(dbErr.Error(), "carry-forward") {
		t.Errorf("the trigger should name the remedy too, got: %v", dbErr)
	}
	t.Logf("reversal refused after fiat: %v", dbErr)

	// It stayed PAYOUTS_CONFIRMED, so close it to free the scheme for the carry-forward's target.
	r.closePeriod(ctx, settledID)
}

func (r *correctionRun) recordCarryForward(ctx context.Context) {
	t := r.t

	targetID := r.makePeriod(ctx, 4, "OPEN")
	narrative := sha256.Sum256([]byte("overpaid by 150 rupees: duplicate rent receipt"))

	adj, err := adjustment.New(adjustment.Input{
		InvestorID:      r.divergedInvestor,
		WalletAddress:   r.divergedWallet,
		SourcePeriodID:  r.periodID,
		SourcePeriodSeq: r.periodSeq,
		DeltaPaise:      -15_000, // overpaid, so we recover
		Reason:          period.ReasonDuplicateInjection,
		NarrativeSHA256: narrative,
		CreatedAt:       correctionLater,
	})
	if err != nil {
		t.Fatalf("building the adjustment: %v", err)
	}
	if adj.Direction != adjustment.RecoverFromHolder {
		t.Fatalf("a negative delta is a recovery, got %s", adj.Direction)
	}

	// Netting into its own source period is refused by the table's CHECK and by Go.
	if err := adj.AssignTarget(r.periodID, r.periodSeq); err == nil {
		t.Fatal("an adjustment must not carry into the period it came from")
	}
	if err := adj.AssignTarget(targetID, 4); err != nil {
		t.Fatalf("period 4 is later than 1 and must be accepted: %v", err)
	}

	if _, err := r.f.pool.Exec(ctx, `
		INSERT INTO carry_forward_adjustments (
			investor_id, source_period_id, target_period_id, amount_paise,
			direction, reason_code, narrative_sha256, status, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		adj.InvestorID, adj.SourcePeriodID, adj.TargetPeriodID, int64(adj.AmountPaise),
		string(adj.Direction), string(adj.Reason), adj.NarrativeSHA256[:], string(adj.State),
		idemKey(t, idempotency.ActionRecordPayoutAdjustment, r.f.schemeID, r.periodID,
			map[string]any{"holder": adj.WalletAddress, "amount": int64(adj.AmountPaise)}),
	); err != nil {
		t.Fatalf("recording the carry-forward adjustment: %v", err)
	}

	// The table stores a positive amount; the direction carries the sign.
	var stored int64
	var dir, state string
	if err := r.f.pool.QueryRow(ctx, `
		SELECT amount_paise, direction, status FROM carry_forward_adjustments
		 WHERE source_period_id = $1 AND investor_id = $2`,
		r.periodID, adj.InvestorID).Scan(&stored, &dir, &state); err != nil {
		t.Fatal(err)
	}
	if stored != 15_000 {
		t.Fatalf("stored amount %d, want a positive 15000", stored)
	}
	if dir != string(adjustment.RecoverFromHolder) || state != string(adjustment.StatePending) {
		t.Fatalf("stored %s/%s", dir, state)
	}

	// The arithmetic the remedy exists for: a recovery is capped by what the later period owes, because
	// a distribution is a payment and not an invoice.
	app, err := adj.ApplyTo(10_000)
	if err != nil {
		t.Fatal(err)
	}
	if app.After != 0 || app.AppliedPaise != 10_000 || app.RemainingPaise != 5_000 {
		t.Fatalf("got after=%d applied=%d remaining=%d, want 0/10000/5000",
			app.After, app.AppliedPaise, app.RemainingPaise)
	}

	// What could not be recovered carries on, sourced where it fell short.
	next, err := adj.CarryForward(app, correctionLater)
	if err != nil {
		t.Fatal(err)
	}
	if next.AmountPaise != 5_000 || next.SourcePeriodID != targetID {
		t.Fatalf("the remainder should be 5000 sourced at the target period, got %d at %s",
			next.AmountPaise, next.SourcePeriodID)
	}

	t.Logf("adjustment: %s", adj.Summary())
	t.Logf("remainder : %s", next.Summary())
}

// --- helpers --------------------------------------------------------------------------------------

// closePeriod moves a period to CLOSED so the scheme can open another.
func (r *correctionRun) closePeriod(ctx context.Context, id string) {
	t := r.t
	t.Helper()

	if _, err := r.f.pool.Exec(ctx,
		`UPDATE distribution_periods SET status = 'CLOSED', closed_at = $2 WHERE id = $1`,
		id, correctionLater,
	); err != nil {
		t.Fatalf("closing period %s: %v", id, err)
	}
}

// makePeriod creates a bare period in a given status.
func (r *correctionRun) makePeriod(ctx context.Context, seq int, status string) string {
	t := r.t
	t.Helper()

	// The figures and both approvals are always supplied, because every status from ANCHORED onward
	// requires them: periods_anchored_requires_figures and periods_anchored_requires_dual_approval. They
	// are harmless on an OPEN period and save the helper needing to know which statuses demand what.
	//
	// The distributed figure is exactly 95% of NDCF, so periods_distribution_floor is satisfied at the
	// boundary rather than comfortably inside it.
	var id string
	if err := r.f.pool.QueryRow(ctx, `
		INSERT INTO distribution_periods (
			scheme_id, period_seq, period_label, period_start, period_end, status,
			ndcf_paise, distributed_paise, distribution_bps, ndcf_statement_sha256,
			im_approved_by, im_approved_at, trustee_approved_by, trustee_approved_at,
			idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6,
		          29000000000, 27550000000, 9500, $7,
		          'im@acresync', $8, 'trustee@independent', $8, $9)
		RETURNING id`,
		r.f.schemeID, seq, fmt.Sprintf("FY27-Q%d", seq),
		periodStart.AddDate(0, 3*(seq-1), 0), periodEnd.AddDate(0, 3*(seq-1), 0),
		status,
		digest(fmt.Sprintf("statement-%d", seq)),
		correctionClock,
		idemKey(t, idempotency.ActionCreatePeriod, r.f.schemeID, "",
			map[string]any{"periodSeq": seq}),
	).Scan(&id); err != nil {
		t.Fatalf("creating period %d in %s: %v", seq, status, err)
	}
	return id
}

// positions reads one register out of Postgres.
func (r *correctionRun) positions(ctx context.Context, table string) []period.Position {
	t := r.t
	t.Helper()

	// The table name is a compile-time constant at both call sites, never user input.
	q := `SELECT investor_id, wallet_address, units FROM ` + table +
		` WHERE scheme_id = $1 ORDER BY wallet_address`

	rows, err := r.f.pool.Query(ctx, q, r.f.schemeID)
	if err != nil {
		t.Fatalf("reading %s: %v", table, err)
	}
	defer rows.Close()

	var out []period.Position
	for rows.Next() {
		var p period.Position
		var units int32
		if err := rows.Scan(&p.InvestorID, &p.WalletAddress, &units); err != nil {
			t.Fatal(err)
		}
		p.Units = uint32(units)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
