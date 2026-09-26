package e2e

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/outbox"
	"github.com/acresync/orchestrator/internal/period"
)

// These tests attack the gates rather than exercising them.
//
// The happy path passing proves the pipeline works when everything is in order. It says nothing about
// what happens when it is not, and the whole architecture rests on the claim that the database refuses
// to move fiat over a wrong register or an unconfirmed anchor even if the application layer asks it to.
// That claim is only worth something if somebody has actually asked.
//
// Each test here bypasses the Go guards entirely and issues the SQL directly, which is what a
// mistaken migration, a hand-run psql session, or a second process would do.

// minimalPeriod creates an anchored period with one entitlement, ready for a payout attempt.
func minimalPeriod(t *testing.T, ctx context.Context, f *fixture) (periodID, entitlementID, anchorID string) {
	t.Helper()

	key := idemKey(t, idempotency.ActionCreatePeriod, f.schemeID, "", map[string]any{"periodSeq": 1})
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO distribution_periods (
			scheme_id, period_seq, period_label, period_start, period_end, record_date,
			status, ndcf_paise, distributed_paise, distribution_bps,
			ndcf_statement_sha256, im_approved_by, im_approved_at,
			trustee_approved_by, trustee_approved_at, idempotency_key
		) VALUES ($1, 1, 'FY27-Q2', $2, $3, $3, 'ANCHORED',
		          29000000000, 27550000000, 9500, $4,
		          'im@acresync', $5, 'trustee@independent', $5, $6)
		RETURNING id`,
		f.schemeID, periodStart, periodEnd, digest("statement"), takenAt, key,
	).Scan(&periodID); err != nil {
		t.Fatalf("creating the period: %v", err)
	}

	// A snapshot and one line, so an entitlement can hang off it.
	var snapshotID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO register_snapshots (
			scheme_id, distribution_period_id, record_date, taken_at, simulated_clock_value,
			total_units, distinct_holders, snapshot_merkle_root, snapshot_cid_digest,
			algo_version, idempotency_key
		) VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, 1, $9)
		RETURNING id`,
		f.schemeID, periodID, recordDate, takenAt, totalUnits, publicHolders-1,
		digest("root"), digest("cid"),
		idemKey(t, idempotency.ActionTakeSnapshot, f.schemeID, periodID, map[string]any{"v": 1}),
	).Scan(&snapshotID); err != nil {
		t.Fatalf("creating the snapshot: %v", err)
	}

	anchor := f.anchors[0]
	var lineID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO register_snapshot_lines (
			snapshot_id, leaf_index, investor_id, investor_anchor_hash,
			wallet_address, units, is_excluded
		) VALUES ($1, 0, $2, $3, $4, $5, TRUE)
		RETURNING id`,
		snapshotID, f.investorIDs[0], anchor[:], f.wallets[0], int(f.units[0]),
	).Scan(&lineID); err != nil {
		t.Fatalf("creating the snapshot line: %v", err)
	}

	if err := f.pool.QueryRow(ctx, `
		INSERT INTO entitlements (
			distribution_period_id, investor_id, snapshot_line_id,
			units, snapshot_total_units, gross_entitlement_paise,
			residue_paise_awarded, remainder_numerator
		) VALUES ($1, $2, $3, $4, $5, 1377500000, 0, 0)
		RETURNING id`,
		periodID, f.investorIDs[0], lineID, int(f.units[0]), totalUnits,
	).Scan(&entitlementID); err != nil {
		t.Fatalf("creating the entitlement: %v", err)
	}

	// A confirmed anchor for the payout to rest on.
	store := outbox.NewPostgresStore(f.pool)
	k, err := idempotency.Derive(idempotency.Input{
		Action: idempotency.ActionAnchorPeriod, SchemeID: f.schemeID, ScopeID: periodID,
		Payload: map[string]any{"periodSeq": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := store.Enqueue(ctx, outbox.NewEntry{
		SchemeID: f.schemeID, TargetContract: "0xa656a42974b40cf64f32e758abb0689a2a178391",
		FunctionName: "anchorPeriod", Payload: []byte(`{}`),
		IdempotencyKey: k, EnvironmentTag: "LOCAL",
	})
	if err != nil {
		t.Fatalf("enqueuing the anchor: %v", err)
	}
	if _, err := store.ClaimNextQueued(ctx, f.schemeID, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBroadcast(ctx, e.ID, "0x"+strings.Repeat("cd", 32), "1000000000"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkConfirming(ctx, e.ID, 900_000, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkConfirmed(ctx, e.ID, 900_000, period.MinAnchorConfirmations); err != nil {
		t.Fatal(err)
	}

	return periodID, entitlementID, e.ID
}

func digest(s string) []byte {
	d := sha256.Sum256([]byte(s))
	return d[:]
}

// insertPayout attempts a payout instruction directly, bypassing every Go guard.
func insertPayout(ctx context.Context, f *fixture, entitlementID, bankID, anchorID string, key []byte) error {
	_, err := f.pool.Exec(ctx, `
		INSERT INTO payout_instructions (
			entitlement_id, bank_account_id, amount_paise, gated_on_anchor_tx, idempotency_key
		) VALUES ($1, $2, 1239750000, $3, $4)`,
		entitlementID, bankID, anchorID, key)
	return err
}

// TestDatabaseBlocksPayoutWhileDiverged is the defence that matters most.
//
// The depository is the legal register. When our mirror disagrees with it, our view of who owns what is
// wrong, and a distribution computed on a wrong register pays the wrong people. The Go guard refuses,
// and so does a trigger, because the guard can be bypassed by a migration, a script, or a second
// process and the trigger cannot.
func TestDatabaseBlocksPayoutWhileDiverged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	periodID, entitlementID, anchorID := minimalPeriod(t, ctx, f)
	_ = periodID

	// An unresolved divergence. The CHECK constraint on this table already guarantees that a DIVERGED
	// run must block payouts, so recording one is enough to arm the gate.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO reconciliation_runs (
			scheme_id, run_at, simulated_clock_value,
			depository_total_units, chain_total_units,
			depository_holder_count, chain_holder_count, divergence_count,
			status, blocks_payout, idempotency_key
		) VALUES ($1, $2, $2, 500, 498, 201, 200, 1, 'DIVERGED', TRUE, $3)`,
		f.schemeID, takenAt,
		idemKey(t, idempotency.ActionRunReconciliation, f.schemeID, "", map[string]any{"v": 99}),
	); err != nil {
		t.Fatalf("recording the divergence: %v", err)
	}

	err := insertPayout(ctx, f, entitlementID, f.bankIDs[0], anchorID,
		digest("payout-while-diverged"))
	if err == nil {
		t.Fatal("the database must refuse a payout while a divergence is unresolved")
	}
	if !strings.Contains(err.Error(), "payout blocked") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The message has to say why, and name the depository as authoritative. An operator who sees only a
	// constraint name will look for a way around it.
	if !strings.Contains(err.Error(), "depository is the legal register") {
		t.Errorf("the refusal should explain the depository is authoritative, got: %v", err)
	}

	// Resolving it opens the gate, which proves the block was the divergence and not something else.
	if _, err := f.pool.Exec(ctx, `
		UPDATE reconciliation_runs SET status = 'RESOLVED', resolved_at = $2, blocks_payout = FALSE
		 WHERE scheme_id = $1`, f.schemeID, takenAt,
	); err != nil {
		t.Fatalf("resolving the divergence: %v", err)
	}

	if err := insertPayout(ctx, f, entitlementID, f.bankIDs[0], anchorID,
		digest("payout-after-resolution")); err != nil {
		t.Fatalf("once resolved, the payout must be accepted: %v", err)
	}
}

// TestDatabaseBlocksPayoutOnUnconfirmedAnchor covers the reorg gate.
//
// Reading chain state and then irreversibly transferring money is the most dangerous sequence in the
// architecture, because a reorg after the transfer has cleared cannot be undone by any amount of later
// reconciliation. Five confirmations is the depth chosen, and it is enforced where it cannot be argued
// with.
func TestDatabaseBlocksPayoutOnUnconfirmedAnchor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, entitlementID, _ := minimalPeriod(t, ctx, f)

	// A second anchor, deliberately left short of the required depth.
	store := outbox.NewPostgresStore(f.pool)
	k, err := idempotency.Derive(idempotency.Input{
		Action: idempotency.ActionAnchorPeriod, SchemeID: f.schemeID,
		Payload: map[string]any{"periodSeq": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	shallow, err := store.Enqueue(ctx, outbox.NewEntry{
		SchemeID: f.schemeID, TargetContract: "0xa656a42974b40cf64f32e758abb0689a2a178391",
		FunctionName: "anchorPeriod", Payload: []byte(`{}`),
		IdempotencyKey: k, EnvironmentTag: "LOCAL",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Nonce 1: nonce 0 belongs to the anchor minimalPeriod created. Nonces are a single sequence per
	// signing address, which is also why the relayer is one worker per scheme.
	if _, err := store.ClaimNextQueued(ctx, f.schemeID, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBroadcast(ctx, shallow.ID, "0x"+strings.Repeat("ef", 32), "1000000000"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkConfirming(ctx, shallow.ID, 900_001, 3); err != nil {
		t.Fatal(err)
	}

	err = insertPayout(ctx, f, entitlementID, f.bankIDs[0], shallow.ID, digest("payout-shallow"))
	if err == nil {
		t.Fatal("the database must refuse a payout resting on an anchor that is not CONFIRMED")
	}
	if !strings.Contains(err.Error(), "payout blocked") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "CONFIRMED") {
		t.Errorf("the refusal should name the required state, got: %v", err)
	}
}

// TestDatabaseBlocksPayoutOnNonExistentAnchor covers a dangling reference.
//
// gated_on_anchor_tx is not a foreign key, because the check it needs is about state rather than
// existence. So the trigger has to verify existence too, or a payout could name an anchor that was
// never created.
func TestDatabaseBlocksPayoutOnNonExistentAnchor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, entitlementID, _ := minimalPeriod(t, ctx, f)

	err := insertPayout(ctx, f, entitlementID, f.bankIDs[0],
		"00000000-0000-4000-8000-000000000000", digest("payout-ghost"))
	if err == nil {
		t.Fatal("a payout naming an anchor that does not exist must be refused")
	}
	if !strings.Contains(err.Error(), "non-existent anchor") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDatabaseRefusesReversalAfterFiatSettles is the rule that shapes the remedy model.
//
// Before settlement a mistake is corrected by unwinding the period. After it, money has left the
// escrow, and claiming the payments did not happen would be false. The only honest remedy is a
// compensating entry that is itself visible, which is what carry_forward_adjustments exists for. The
// contract refuses the same transition, so the database and the chain agree.
func TestDatabaseRefusesReversalAfterFiatSettles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	periodID, _, _ := minimalPeriod(t, ctx, f)

	for _, from := range []string{"PAYOUTS_CONFIRMED", "CLOSED"} {
		t.Run(from, func(t *testing.T) {
			if _, err := f.pool.Exec(ctx,
				`UPDATE distribution_periods SET status = $2 WHERE id = $1`, periodID, from,
			); err != nil {
				t.Fatalf("setting up %s: %v", from, err)
			}

			_, err := f.pool.Exec(ctx, `
				UPDATE distribution_periods
				   SET status = 'REVERSED', reversal_reason = 'DATA_ENTRY_ERROR',
				       reversal_narrative_sha256 = $2, reversed_at = $3
				 WHERE id = $1`, periodID, digest("narrative"), takenAt)

			if err == nil {
				t.Fatalf("reversing from %s must be refused; fiat has settled", from)
			}
			if !strings.Contains(err.Error(), "fiat has settled") {
				t.Fatalf("unexpected error: %v", err)
			}
			// The message must point at the alternative, or an operator has nowhere to go.
			if !strings.Contains(err.Error(), "carry-forward") {
				t.Errorf("the refusal should name the carry-forward remedy, got: %v", err)
			}
		})
	}
}

// TestDatabaseRefusesReversalWhilePayoutsExist covers the second half of the same trigger.
func TestDatabaseRefusesReversalWhilePayoutsExist(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	periodID, entitlementID, anchorID := minimalPeriod(t, ctx, f)

	if err := insertPayout(ctx, f, entitlementID, f.bankIDs[0], anchorID, digest("payout-inflight")); err != nil {
		t.Fatalf("creating the payout: %v", err)
	}

	reverse := func() error {
		_, err := f.pool.Exec(ctx, `
			UPDATE distribution_periods
			   SET status = 'REVERSED', reversal_reason = 'DATA_ENTRY_ERROR',
			       reversal_narrative_sha256 = $2, reversed_at = $3
			 WHERE id = $1`, periodID, digest("narrative"), takenAt)
		return err
	}

	// A QUEUED instruction does not block reversal, and that is the right line to draw.
	//
	// The trigger counts only SETTLED, PROCESSING and SUBMITTED. A queued instruction has been written
	// down and not handed to the provider, so no money is moving and unwinding the period is still
	// safe. Drawing the line at "a row exists" would trap an operator who created instructions and then
	// spotted the error before sending them, which is exactly when reversal is most useful.
	if err := reverse(); err != nil {
		t.Fatalf("a queued instruction must not block reversal: %v", err)
	}

	// Put it back and submit the payout, which is the point money starts moving.
	if _, err := f.pool.Exec(ctx,
		`UPDATE distribution_periods SET status = 'ANCHORED', reversal_reason = NULL,
		   reversal_narrative_sha256 = NULL, reversed_at = NULL WHERE id = $1`, periodID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE payout_instructions SET status = 'SUBMITTED', submitted_at = $2
		   WHERE entitlement_id = $1`, entitlementID, takenAt,
	); err != nil {
		t.Fatal(err)
	}

	err := reverse()
	if err == nil {
		t.Fatal("reversing a period with a submitted payout must be refused: the instruction is with " +
			"the provider and the money may already be moving")
	}
	if !strings.Contains(err.Error(), "in flight or settled") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "carry-forward") {
		t.Errorf("the refusal should name the remedy, got: %v", err)
	}
}

// TestGoGuardIsStricterThanTheDatabaseOnQueuedPayouts records a deliberate difference between the two
// layers, so it is not mistaken for a bug later.
//
// The database permits reversal while instructions sit in QUEUED. The Go guard does not: its PayoutTally
// counts a queued provider payout as in flight, so it refuses. Being stricter is the safe direction, and
// the operational path is to cancel the queued instructions first and then reverse, which leaves a
// clearer record than reversing around them.
func TestGoGuardIsStricterThanTheDatabaseOnQueuedPayouts(t *testing.T) {
	ev := period.Evidence{
		TrusteeApprovedBy: "trustee@independent",
		Payouts:           period.PayoutTally{Total: 1, InFlight: 1},
	}

	if err := period.Guard(period.StatusAnchored, period.StatusReversed, ev); err == nil {
		t.Fatal("the Go guard is expected to refuse while anything is in flight")
	}

	// With nothing in flight, both layers agree.
	ev.Payouts = period.PayoutTally{}
	if err := period.Guard(period.StatusAnchored, period.StatusReversed, ev); err != nil {
		t.Fatalf("with no payouts outstanding the guard must permit reversal: %v", err)
	}
}

// TestSnapshotLinesAreAppendOnly covers the immutability of a frozen register.
//
// The mirror's history is the only thing that makes a later divergence diagnosable. If a snapshot line
// could be edited, a register that was reconciled late would retroactively change who was owed money
// for a period already anchored, and nothing would record that it had happened.
func TestSnapshotLinesAreAppendOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, entitlementID, _ := minimalPeriod(t, ctx, f)

	var lineID string
	if err := f.pool.QueryRow(ctx,
		`SELECT snapshot_line_id FROM entitlements WHERE id = $1`, entitlementID).Scan(&lineID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.pool.Exec(ctx,
		`UPDATE register_snapshot_lines SET units = units + 1 WHERE id = $1`, lineID,
	); err == nil {
		t.Fatal("a frozen snapshot line must not be updatable")
	}

	if _, err := f.pool.Exec(ctx,
		`DELETE FROM register_snapshot_lines WHERE id = $1`, lineID,
	); err == nil {
		t.Fatal("a frozen snapshot line must not be deletable")
	}
}

// TestDistributionFloorEnforcedByDatabase confirms the regulatory minimum is not only a Go check.
func TestDistributionFloorEnforcedByDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	key := idemKey(t, idempotency.ActionCreatePeriod, f.schemeID, "", map[string]any{"periodSeq": 7})

	// 94.99% of NDCF: one basis point under the floor.
	_, err := f.pool.Exec(ctx, `
		INSERT INTO distribution_periods (
			scheme_id, period_seq, period_label, period_start, period_end,
			status, ndcf_paise, distributed_paise, distribution_bps, idempotency_key
		) VALUES ($1, 7, 'FY27-Q3', $2, $3, 'NDCF_DRAFTED',
		          29000000000, 27547100000, 9499, $4)`,
		f.schemeID, periodStart, periodEnd, key)

	if err == nil {
		t.Fatal("the database must refuse a distribution below the 95% floor")
	}
	if !strings.Contains(err.Error(), "periods_distribution_floor") &&
		!strings.Contains(err.Error(), "periods_bps_range") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSchemeUnitsMustBalance covers the constraint the settlement invariant checks itself against.
func TestSchemeUnitsMustBalance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 25 plus 476 is 501, not 500.
	_, err := f.pool.Exec(ctx, `
		INSERT INTO schemes (
			sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
			total_units, im_units, public_units, environment_tag
		) VALUES ($1, 'Unbalanced', 50000000000, 100000000, 500, 25, 476, 'LOCAL')`,
		"SEBI/E2E/UNBALANCED/"+f.schemeID)

	if err == nil {
		t.Fatal("a cap table that does not add up must be refused")
	}
	if !strings.Contains(err.Error(), "schemes_units_balance") {
		t.Fatalf("unexpected error: %v", err)
	}
}
