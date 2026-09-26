package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/entitlement"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/ndcf"
	"github.com/acresync/orchestrator/internal/outbox"
	"github.com/acresync/orchestrator/internal/payout"
	"github.com/acresync/orchestrator/internal/period"
	"github.com/acresync/orchestrator/internal/snapshot"
)

// run carries the state accumulated as a period advances.
type run struct {
	f *fixture

	periodID  string
	periodSeq uint32
	status    period.Status

	plan        ndcf.Plan
	ndcfItems   []ndcf.LineItem
	statementPin *ipfs.Pin

	snap        *snapshot.Snapshot
	snapshotID  string
	snapshotPin *ipfs.Pin

	recon *period.Reconciliation

	anchor *outbox.Entry

	batch          *entitlement.Batch
	entitlementIDs map[string]string

	payouts map[string]*payout.Payout // investorID -> provider payout

	publisher *ipfs.Publisher
	ipfsMock  *ipfs.MockProvider
	payMock   *payout.Mock
	store     *outbox.PostgresStore
}

// advance moves the period, checking the guard and then writing the new status.
//
// Both halves matter. The guard refuses on stated evidence and names the missing artefact; the database
// then applies its own triggers to the same transition. A stage that passed the guard and failed the
// database would mean the two layers disagree, which is worth finding here rather than in a rehearsal.
func (r *run) advance(t *testing.T, ctx context.Context, to period.Status, ev period.Evidence) {
	t.Helper()

	if err := period.Guard(r.status, to, ev); err != nil {
		t.Fatalf("guard refused %s -> %s: %v", r.status, to, err)
	}
	if _, err := r.f.pool.Exec(ctx,
		`UPDATE distribution_periods SET status = $2 WHERE id = $1`, r.periodID, string(to),
	); err != nil {
		t.Fatalf("database refused %s -> %s: %v", r.status, to, err)
	}
	r.status = to
}

func idemKey(t *testing.T, action idempotency.Action, schemeID, scopeID string, payload map[string]any) []byte {
	t.Helper()
	k, err := idempotency.Derive(idempotency.Input{
		Action: action, SchemeID: schemeID, ScopeID: scopeID, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return k[:]
}

// TestFullPeriodEndToEnd is the M5 gate.
//
// One period from rent collection to closure, every figure written to Postgres, every document pinned
// and fetched back, every transition passed through both the Go guard and the database triggers. The
// final stage rebuilds the snapshot root from the pinned document alone, which is the claim the
// on-chain anchor makes to an investor who has no access to our systems.
func TestFullPeriodEndToEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	ipfsDir := t.TempDir()
	mockIPFS, err := ipfs.NewMockProvider(ipfsDir)
	if err != nil {
		t.Fatal(err)
	}

	r := &run{
		f:              f,
		periodSeq:      1,
		status:         period.StatusOpen,
		publisher:      ipfs.NewPublisher(mockIPFS),
		ipfsMock:       mockIPFS,
		payMock:        payout.NewMock(fixedClock{takenAt}, srcAccount, money.Paise(60_000_000_000)),
		store:          outbox.NewPostgresStore(f.pool),
		entitlementIDs: map[string]string{},
		payouts:        map[string]*payout.Payout{},
	}

	stageOpenPeriod(t, ctx, r)
	stageCollectRent(t, ctx, r)
	stageDraftNDCF(t, ctx, r)
	stageApproveNDCF(t, ctx, r)
	stageDeclareRecordDate(t, ctx, r)
	stageTakeSnapshot(t, ctx, r)
	stageReconcile(t, ctx, r)
	stageAnchor(t, ctx, r)
	stageEntitlements(t, ctx, r)
	stageInstructPayouts(t, ctx, r)
	stageConfirmPayouts(t, ctx, r)
	stageClose(t, ctx, r)

	assertVerifierCanProveEntitlementFromPublishedDataAlone(t, ctx, r)
	assertInvariants(t, ctx, r)
}

const srcAccount = "7878780080316316"

type fixedClock struct{ t time.Time }

func (c fixedClock) Now(context.Context) (time.Time, error) { return c.t, nil }

// ---------------------------------------------------------------------------
// Stages
// ---------------------------------------------------------------------------

func stageOpenPeriod(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	key := idemKey(t, idempotency.ActionCreatePeriod, r.f.schemeID, "",
		map[string]any{"periodSeq": r.periodSeq})

	err := r.f.pool.QueryRow(ctx, `
		INSERT INTO distribution_periods (
			scheme_id, period_seq, period_label, period_start, period_end, status, idempotency_key
		) VALUES ($1, $2, 'FY27-Q2', $3, $4, 'OPEN', $5)
		RETURNING id`,
		r.f.schemeID, r.periodSeq, periodStart, periodEnd, key,
	).Scan(&r.periodID)
	if err != nil {
		t.Fatalf("opening the period: %v", err)
	}
}

func stageCollectRent(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	// Two receipts, one with a variance. The database requires a variance to carry a reason, because a
	// silent shortfall is how a distribution quietly stops matching the lease schedule.
	receipts := []struct {
		expected, received money.Paise
		reason             *string
	}{
		{expected: 20_000_000_000, received: 20_000_000_000},
		{expected: 13_000_000_000, received: 13_000_000_000},
	}

	for i, rc := range receipts {
		key := idemKey(t, idempotency.ActionInjectRent, r.f.schemeID, r.periodID,
			map[string]any{"seq": i, "amountPaise": rc.received})

		if _, err := r.f.pool.Exec(ctx, `
			INSERT INTO rent_receipts (
				distribution_period_id, lease_id, received_at, escrow_bank_ref,
				expected_amount_paise, received_amount_paise,
				variance_paise, variance_reason, idempotency_key
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			r.periodID, r.f.leaseID, periodEnd,
			fmt.Sprintf("ESCROW/FY27Q2/%d", i),
			int64(rc.expected), int64(rc.received),
			int64(rc.received-rc.expected), rc.reason, key,
		); err != nil {
			t.Fatalf("recording receipt %d: %v", i, err)
		}
	}

	r.advance(t, ctx, period.StatusRentCollected, period.Evidence{
		RentReceiptCount: len(receipts), RentVariancesExplained: true,
	})
}

func stageDraftNDCF(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	ev := func(s string) [32]byte { return sha256.Sum256([]byte(s)) }

	// Gross rent ₹3.3 crore plus CAM ₹10 lakh, less ₹50 lakh of deductions, so NDCF is ₹2.9 crore.
	r.ndcfItems = []ndcf.LineItem{
		{LineType: ndcf.LineGrossRent, AmountPaise: 33_000_000_000, EvidenceSHA256: ev("rent")},
		{LineType: ndcf.LineCAMRecovery, AmountPaise: 1_000_000_000, EvidenceSHA256: ev("cam")},
		{LineType: ndcf.LinePropertyTax, AmountPaise: 2_000_000_000, EvidenceSHA256: ev("tax")},
		{LineType: ndcf.LineInsurance, AmountPaise: 300_000_000, EvidenceSHA256: ev("ins")},
		{LineType: ndcf.LineMaintenance, AmountPaise: 500_000_000, EvidenceSHA256: ev("maint")},
		{LineType: ndcf.LineTrusteeFee, AmountPaise: 200_000_000, EvidenceSHA256: ev("trustee")},
		{LineType: ndcf.LineIMFee, AmountPaise: 800_000_000, EvidenceSHA256: ev("im")},
		{LineType: ndcf.LineAuditFee, AmountPaise: 100_000_000, EvidenceSHA256: ev("audit")},
		{LineType: ndcf.LineStatutoryReserve, AmountPaise: 600_000_000, EvidenceSHA256: ev("statres")},
		{LineType: ndcf.LineWorkingCapitalReserve, AmountPaise: 500_000_000, EvidenceSHA256: ev("wcres")},
	}

	res, err := ndcf.Compute(r.ndcfItems)
	if err != nil {
		t.Fatalf("computing NDCF: %v", err)
	}
	if res.NDCFPaise != money.Paise(29_000_000_000) {
		t.Fatalf("NDCF = %d, want 29000000000", res.NDCFPaise)
	}

	// Distribute at the floor, which is the tightest case: one paise less and the period is
	// non-compliant.
	plan, err := ndcf.PlanAtFloor(res)
	if err != nil {
		t.Fatalf("planning the distribution: %v", err)
	}
	r.plan = plan

	for i, it := range r.ndcfItems {
		dir, err := it.LineType.Direction()
		if err != nil {
			t.Fatal(err)
		}
		key := idemKey(t, idempotency.ActionAddNDCFLineItem, r.f.schemeID, r.periodID,
			map[string]any{"seq": i, "lineType": string(it.LineType)})

		if _, err := r.f.pool.Exec(ctx, `
			INSERT INTO ndcf_line_items (
				distribution_period_id, line_type, direction, amount_paise,
				evidence_sha256, idempotency_key
			) VALUES ($1, $2, $3, $4, $5, $6)`,
			r.periodID, string(it.LineType), string(dir), int64(it.AmountPaise),
			it.EvidenceSHA256[:], key,
		); err != nil {
			t.Fatalf("recording line item %d (%s): %v", i, it.LineType, err)
		}
	}

	// The statement document, validated and pinned before its digest can be anchored.
	raw, err := ndcf.BuildStatement(ndcf.StatementInput{
		SchemeRef: r.f.schemeRef, PeriodID: r.periodSeq,
		PeriodStart: periodStart, PeriodEnd: periodEnd,
		Plan: plan, Items: r.ndcfItems,
	})
	if err != nil {
		t.Fatalf("building the statement: %v", err)
	}

	pin, err := r.publisher.Publish(ctx, ipfsguard.DocNDCFStatement, raw)
	if err != nil {
		t.Fatalf("pinning the statement: %v", err)
	}
	if err := r.publisher.VerifyRoundTrip(ctx, pin); err != nil {
		t.Fatalf("the statement is not retrievable: %v", err)
	}
	r.statementPin = pin

	if _, err := r.f.pool.Exec(ctx, `
		UPDATE distribution_periods
		   SET ndcf_paise = $2, distributed_paise = $3, distribution_bps = $4,
		       ndcf_statement_sha256 = $5, ndcf_cid_digest = $5,
		       ndcf_ipfs_cid = $6, ndcf_statement_uri = $7
		 WHERE id = $1`,
		r.periodID, int64(plan.NDCFPaise), int64(plan.DistributedPaise), int(plan.DistributionBps),
		pin.Digest[:], pin.ProviderCID, "ipfs://"+pin.ProviderCID,
	); err != nil {
		t.Fatalf("persisting the NDCF figures: %v", err)
	}

	r.advance(t, ctx, period.StatusNDCFDrafted, period.Evidence{
		PlanPresent: true, NDCFPaise: plan.NDCFPaise,
		DistributedPaise: plan.DistributedPaise, DistributionBps: plan.DistributionBps,
	})
}

func stageApproveNDCF(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	const imApprover = "im.operator@acresync"
	const trusteeApprover = "trustee@independent"

	if _, err := r.f.pool.Exec(ctx, `
		UPDATE distribution_periods
		   SET im_approved_by = $2, im_approved_at = $3,
		       trustee_approved_by = $4, trustee_approved_at = $5
		 WHERE id = $1`,
		r.periodID, imApprover, takenAt, trusteeApprover, takenAt,
	); err != nil {
		t.Fatalf("recording approvals: %v", err)
	}

	r.advance(t, ctx, period.StatusNDCFApproved, period.Evidence{
		IMApprovedBy: imApprover, TrusteeApprovedBy: trusteeApprover,
	})
}

func stageDeclareRecordDate(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	if _, err := r.f.pool.Exec(ctx,
		`UPDATE distribution_periods SET record_date = $2 WHERE id = $1`, r.periodID, recordDate,
	); err != nil {
		t.Fatalf("declaring the record date: %v", err)
	}

	r.advance(t, ctx, period.StatusRecordDateDeclared, period.Evidence{RecordDateDeclared: true})
}

func stageTakeSnapshot(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	snap, err := snapshot.Build(snapshot.BuildInput{
		SchemeRef: r.f.schemeRef, PeriodID: r.periodSeq,
		RecordDate: recordDate, PeriodEnd: periodEnd, TakenAt: takenAt,
		Holdings:              r.f.holdings(),
		ExpectedTotalUnits:    totalUnits,
		RequireMinimumHolders: true,
	})
	if err != nil {
		t.Fatalf("freezing the register: %v", err)
	}
	r.snap = snap

	if snap.TotalUnits != totalUnits {
		t.Fatalf("snapshot totals %d units, want %d", snap.TotalUnits, totalUnits)
	}
	if snap.DistinctHolders < snapshot.MinUnitholders {
		t.Fatalf("%d countable holders, the minimum is %d", snap.DistinctHolders, snapshot.MinUnitholders)
	}

	raw, err := snap.Document()
	if err != nil {
		t.Fatalf("building the snapshot document: %v", err)
	}
	pin, err := r.publisher.Publish(ctx, ipfsguard.DocSnapshot, raw)
	if err != nil {
		t.Fatalf("pinning the snapshot: %v", err)
	}
	if err := r.publisher.VerifyRoundTrip(ctx, pin); err != nil {
		t.Fatalf("the snapshot is not retrievable: %v", err)
	}
	r.snapshotPin = pin

	key := idemKey(t, idempotency.ActionTakeSnapshot, r.f.schemeID, r.periodID,
		map[string]any{"recordDate": recordDate.Format("2006-01-02")})

	root := snap.MerkleRoot
	if err := r.f.pool.QueryRow(ctx, `
		INSERT INTO register_snapshots (
			scheme_id, distribution_period_id, record_date, taken_at, simulated_clock_value,
			total_units, distinct_holders, snapshot_merkle_root, snapshot_cid_digest,
			ipfs_cid, algo_version, idempotency_key
		) VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id`,
		r.f.schemeID, r.periodID, recordDate, takenAt,
		int(snap.TotalUnits), int(snap.DistinctHolders),
		root[:], pin.Digest[:], pin.ProviderCID, snap.AlgoVersion, key,
	).Scan(&r.snapshotID); err != nil {
		t.Fatalf("persisting the snapshot: %v", err)
	}

	for _, l := range snap.Lines {
		anchor := l.InvestorAnchor
		if _, err := r.f.pool.Exec(ctx, `
			INSERT INTO register_snapshot_lines (
				snapshot_id, leaf_index, investor_id, investor_anchor_hash,
				wallet_address, units, is_excluded
			) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			r.snapshotID, int(l.LeafIndex), l.InvestorID, anchor[:],
			l.WalletAddress, int(l.Units), l.ExcludedFromHolderCount,
		); err != nil {
			t.Fatalf("persisting snapshot line %d: %v", l.LeafIndex, err)
		}
	}

	r.advance(t, ctx, period.StatusSnapshotTaken, period.Evidence{
		Snapshot: snap, RequireMinimumHolders: true,
	})
}

func stageReconcile(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	dep := r.f.readDepository(ctx, t)
	mir := r.f.readMirror(ctx, t)

	depPos := make([]period.Position, 0, len(dep))
	for _, p := range dep {
		depPos = append(depPos, period.Position{InvestorID: p.investorID, WalletAddress: p.wallet, Units: p.units})
	}
	mirPos := make([]period.Position, 0, len(mir))
	for _, p := range mir {
		mirPos = append(mirPos, period.Position{InvestorID: p.investorID, WalletAddress: p.wallet, Units: p.units})
	}

	rec, err := period.Reconcile(depPos, mirPos)
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	if rec.Status != period.ReconMatched {
		t.Fatalf("the seeded fixture should reconcile cleanly, got %s: %s", rec.Status, rec.Summary())
	}
	r.recon = rec

	if _, err := r.f.pool.Exec(ctx, `
		INSERT INTO reconciliation_runs (
			scheme_id, run_at, simulated_clock_value,
			depository_total_units, chain_total_units,
			depository_holder_count, chain_holder_count, divergence_count,
			status, blocks_payout, idempotency_key
		) VALUES ($1, $2, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		r.f.schemeID, takenAt,
		int(rec.DepositoryTotalUnits), int(rec.ChainTotalUnits),
		rec.DepositoryHolderCount, rec.ChainHolderCount,
		rec.DivergenceCount(), string(rec.Status), rec.BlocksPayout,
		idemKey(t, idempotency.ActionRunReconciliation, r.f.schemeID, r.periodID,
			map[string]any{"runAt": takenAt.Format(time.RFC3339)}),
	); err != nil {
		t.Fatalf("persisting the reconciliation run: %v", err)
	}

	r.advance(t, ctx, period.StatusReconciled, period.Evidence{
		Snapshot: r.snap, BlockingDivergences: 0,
	})
}

func stageAnchor(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	// The anchor goes through the outbox, which is the only path to the chain. Payload is the calldata
	// the relayer will sign.
	payload, err := json.Marshal(map[string]any{
		"periodId":           r.periodSeq,
		"recordDate":         recordDate.Unix(),
		"ndcfPaise":          int64(r.plan.NDCFPaise),
		"distributedPaise":   int64(r.plan.DistributedPaise),
		"snapshotTotalUnits": r.snap.TotalUnits,
		"snapshotHolders":    r.snap.DistinctHolders,
		"statementHash":      r.statementPin.Digest[:],
		"snapshotRoot":       r.snap.MerkleRoot[:],
		"ndcfCidDigest":      r.statementPin.Digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}

	k, err := idempotency.Derive(idempotency.Input{
		Action: idempotency.ActionAnchorPeriod, SchemeID: r.f.schemeID, ScopeID: r.periodID,
		Payload: map[string]any{"periodSeq": r.periodSeq},
	})
	if err != nil {
		t.Fatal(err)
	}

	entry, err := r.store.Enqueue(ctx, outbox.NewEntry{
		SchemeID:       r.f.schemeID,
		TargetContract: "0xa656a42974b40cf64f32e758abb0689a2a178391",
		FunctionName:   "anchorPeriod",
		Payload:        payload,
		IdempotencyKey: k,
		EnvironmentTag: "LOCAL",
	})
	if err != nil {
		t.Fatalf("enqueuing the anchor: %v", err)
	}

	claimed, err := r.store.ClaimNextQueued(ctx, r.f.schemeID, 0)
	if err != nil {
		t.Fatalf("claiming the anchor: %v", err)
	}
	txHash := "0x" + strings.Repeat("ab", 32)
	if err := r.store.MarkBroadcast(ctx, claimed.ID, txHash, "1000000000"); err != nil {
		t.Fatalf("broadcasting: %v", err)
	}

	// The outbox will not jump from BROADCAST to CONFIRMED. A transaction that has been seen in a block
	// but not yet buried is genuinely in a third state, and collapsing it into either neighbour would
	// mean either treating an unconfirmed transaction as final or losing the fact that it landed at all.
	if err := r.store.MarkConfirming(ctx, claimed.ID, 900_000, 1); err != nil {
		t.Fatalf("moving to CONFIRMING: %v", err)
	}

	// Below five confirmations the database refuses to call it CONFIRMED at all, which is the
	// constraint the whole fiat gate rests on. Asserted here rather than assumed.
	if err := r.store.MarkConfirmed(ctx, claimed.ID, 900_000, 4); err == nil {
		t.Fatal("Postgres must refuse a CONFIRMED anchor with only four confirmations")
	}

	if err := r.store.MarkConfirmed(ctx, claimed.ID, 900_000, period.MinAnchorConfirmations); err != nil {
		t.Fatalf("confirming the anchor: %v", err)
	}

	confirmed, err := r.store.Get(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.anchor = confirmed

	if _, err := r.f.pool.Exec(ctx,
		`UPDATE distribution_periods SET anchored_tx = $2 WHERE id = $1`, r.periodID, txHash,
	); err != nil {
		t.Fatalf("recording the anchor transaction: %v", err)
	}
	_ = entry

	r.advance(t, ctx, period.StatusAnchored, period.Evidence{
		PlanPresent: true, NDCFPaise: r.plan.NDCFPaise,
		DistributedPaise: r.plan.DistributedPaise, DistributionBps: r.plan.DistributionBps,
		StatementDigestOK: true, StatementPinned: true,
		Snapshot: r.snap, SnapshotPinned: true,
		IMApprovedBy: "im.operator@acresync", TrusteeApprovedBy: "trustee@independent",
		BlockingDivergences: 0,
	})
}

func stageEntitlements(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	batch, err := entitlement.Build(entitlement.BuildInput{
		PeriodID:             r.periodSeq,
		Snapshot:             r.snap,
		DistributedPaise:     r.plan.DistributedPaise,
		Details:              r.f.details(),
		TDS:                  entitlement.SampleTDSTable(),
		ProviderMinimumPaise: payout.MinAmountPaise,
	})
	if err != nil {
		t.Fatalf("building entitlements: %v", err)
	}
	r.batch = batch

	if batch.GrossTotalPaise != r.plan.DistributedPaise {
		t.Fatalf("entitlements total %d, distribution is %d",
			batch.GrossTotalPaise, r.plan.DistributedPaise)
	}

	lineIDs := map[string]string{}
	rows, err := r.f.pool.Query(ctx,
		`SELECT investor_id, id FROM register_snapshot_lines WHERE snapshot_id = $1`, r.snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var inv, id string
		if err := rows.Scan(&inv, &id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		lineIDs[inv] = id
	}
	rows.Close()

	for _, l := range batch.Lines {
		lineID, ok := lineIDs[l.InvestorID]
		if !ok {
			t.Fatalf("no snapshot line for investor %s", l.InvestorID)
		}

		var entID string
		if err := r.f.pool.QueryRow(ctx, `
			INSERT INTO entitlements (
				distribution_period_id, investor_id, snapshot_line_id,
				units, snapshot_total_units, gross_entitlement_paise,
				residue_paise_awarded, remainder_numerator
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id`,
			r.periodID, l.InvestorID, lineID,
			int(l.Units), int(l.SnapshotTotalUnits), int64(l.GrossPaise),
			int(l.ResiduePaise), l.RemainderNumerator.String(),
		).Scan(&entID); err != nil {
			t.Fatalf("persisting the entitlement for %s: %v", l.InvestorID, err)
		}
		r.entitlementIDs[l.InvestorID] = entID

		if _, err := r.f.pool.Exec(ctx, `
			INSERT INTO tax_deductions (
				entitlement_id, investor_class, tds_section, tds_rate_bps,
				tds_amount_paise, net_payable_paise, form_15g_h_on_file, lower_deduction_cert_ref
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			entID, string(l.TDS.Class), l.TDS.Section, int(l.TDS.RateBps),
			int64(l.TDS.AmountPaise), int64(l.TDS.NetPayablePaise),
			l.TDS.Form15GHOnFile, nullIfEmpty(l.TDS.LowerDeductionCertRef),
		); err != nil {
			t.Fatalf("persisting the withholding for %s: %v", l.InvestorID, err)
		}
	}

	// The transition into ENTITLEMENTS_ANCHORED fires assert_period_entitlements_exact, so the
	// database independently verifies what the guard already checked.
	r.advance(t, ctx, period.StatusEntitlementsAnchored, period.Evidence{
		Snapshot:        r.snap,
		EntitledUnits:   r.snap.TotalUnits,
		EntitledHolders: uint32(len(r.snap.Lines)),
	})
}

func stageInstructPayouts(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	reqs, skipped, err := r.batch.PayoutRequests(entitlement.PayoutRequestInput{
		SchemeID:       r.f.schemeID,
		AnchorOutboxID: r.anchor.ID,
		AccountNumber:  srcAccount,
		Mode:           payout.ModeIMPS,
		Purpose:        payout.PurposePayout,
		Narration:      "AcreSync FY27 Q2 payout",
	})
	if err != nil {
		t.Fatalf("building payout requests: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("%d lines were unpayable: %s", len(skipped), skipped[0].NotPayableReason)
	}
	if len(reqs) != len(r.batch.Lines) {
		t.Fatalf("%d requests for %d lines", len(reqs), len(r.batch.Lines))
	}

	// One payout per line, in the batch's order, so the request and the line correspond by index.
	for i, req := range reqs {
		line := r.batch.Lines[i]

		p, err := r.payMock.CreatePayout(ctx, req)
		if err != nil {
			t.Fatalf("instructing the payout for %s: %v", line.InvestorID, err)
		}
		r.payouts[line.InvestorID] = p

		persisted, err := payout.ToDBStatus(p, takenAt)
		if err != nil {
			t.Fatalf("mapping the payout status for %s: %v", line.InvestorID, err)
		}

		if _, err := r.f.pool.Exec(ctx, `
			INSERT INTO payout_instructions (
				entitlement_id, bank_account_id, amount_paise, provider, provider_ref,
				status, gated_on_anchor_tx, idempotency_key, submitted_at
			) VALUES ($1, $2, $3, 'RAZORPAYX_SANDBOX', $4, $5, $6, $7, $8)`,
			r.entitlementIDs[line.InvestorID], line.Detail.BankAccountID,
			int64(line.TDS.NetPayablePaise), persisted.ProviderRef, string(persisted.Status),
			r.anchor.ID, mustParseKey(t, req.IdempotencyKey), takenAt,
		); err != nil {
			t.Fatalf("persisting the payout instruction for %s: %v", line.InvestorID, err)
		}
	}

	r.advance(t, ctx, period.StatusPayoutInstructed, period.Evidence{
		Anchor: &period.AnchorRef{
			OutboxID: r.anchor.ID, Status: string(r.anchor.Status),
			Confirmations: r.anchor.Confirmations,
		},
		BlockingDivergences: 0,
	})
}

func stageConfirmPayouts(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	// Advance until nothing is in flight. Driven explicitly rather than on a timer, which is how
	// webhooks actually arrive and what keeps the test deterministic.
	for round := 0; round < 10; round++ {
		changed, err := r.payMock.Advance(ctx)
		if err != nil {
			t.Fatalf("advancing payouts: %v", err)
		}
		if len(changed) == 0 {
			break
		}
	}

	var tally period.PayoutTally
	var settled []*payout.Payout

	for investorID, p := range r.payouts {
		cur, err := r.payMock.FetchPayout(ctx, p.ProviderID)
		if err != nil {
			t.Fatal(err)
		}
		r.payouts[investorID] = cur

		persisted, err := payout.ToDBStatus(cur, takenAt)
		if err != nil {
			t.Fatalf("mapping %s: %v", cur.ProviderID, err)
		}

		if _, err := r.f.pool.Exec(ctx, `
			UPDATE payout_instructions
			   SET status = $2, utr = $3, settled_at = $4, failure_code = $5
			 WHERE entitlement_id = $1`,
			r.entitlementIDs[investorID], string(persisted.Status),
			nullIfEmpty(persisted.UTR), persisted.SettledAt, nullIfEmpty(persisted.FailureCode),
		); err != nil {
			t.Fatalf("persisting the settled payout for %s: %v", investorID, err)
		}

		tally.Total++
		switch {
		case cur.Status.IsInFlight():
			tally.InFlight++
		case cur.Status.IsCreditConfirmed():
			tally.CreditConfirmed++
			settled = append(settled, cur)
		case cur.Status.NeedsReissue():
			tally.NeedingReissue++
		}
	}

	count, total, err := payout.SettledCountAndAmount(settled)
	if err != nil {
		t.Fatal(err)
	}
	if count != len(r.batch.Lines) {
		t.Fatalf("%d payouts settled, want %d", count, len(r.batch.Lines))
	}

	// The settled total must equal the net total, which is the distributed amount less withholding.
	netTotal, err := r.batch.NetTotalForPayableLines()
	if err != nil {
		t.Fatal(err)
	}
	if money.Paise(total) != netTotal {
		t.Fatalf("settled %d paise, expected the net total of %d", total, netTotal)
	}

	tally.ConfirmedPaise = money.Paise(total)
	r.advance(t, ctx, period.StatusPayoutsConfirmed, period.Evidence{Payouts: tally})
}

func stageClose(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	if _, err := r.f.pool.Exec(ctx,
		`UPDATE distribution_periods SET closed_at = $2 WHERE id = $1`, r.periodID, takenAt,
	); err != nil {
		t.Fatalf("recording closure: %v", err)
	}

	r.advance(t, ctx, period.StatusClosed, period.Evidence{
		Payouts: period.PayoutTally{Total: len(r.batch.Lines), CreditConfirmed: len(r.batch.Lines)},
	})
}

// ---------------------------------------------------------------------------
// The claim the anchor makes
// ---------------------------------------------------------------------------

// assertVerifierCanProveEntitlementFromPublishedDataAlone is the point of the whole architecture.
//
// Nothing here reads the Snapshot object. The document is fetched from the pinning layer by its content
// address, parsed as an outside party would parse it, every leaf recomputed under the published rule,
// the tree rebuilt, and the root compared against the value persisted as the anchored root. Then one
// holder's inclusion proof is verified.
//
// If this passes, an investor with the pinned document and a block explorer can confirm their own
// entitlement without trusting us and without access to anything of ours.
func assertVerifierCanProveEntitlementFromPublishedDataAlone(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	// The anchored root, read back from storage rather than from memory.
	var storedRoot, storedCIDDigest []byte
	var storedCID string
	if err := r.f.pool.QueryRow(ctx, `
		SELECT snapshot_merkle_root, snapshot_cid_digest, ipfs_cid
		  FROM register_snapshots WHERE id = $1`, r.snapshotID,
	).Scan(&storedRoot, &storedCIDDigest, &storedCID); err != nil {
		t.Fatal(err)
	}

	// Fetch by content address. This is the only input a verifier has beyond the chain.
	raw, err := r.publisher.Fetch(ctx, storedCID)
	if err != nil {
		t.Fatalf("the anchored document is not retrievable at %s: %v", storedCID, err)
	}

	// The document must hash to the digest that was anchored, or it is not the document committed to.
	got := sha256.Sum256(raw)
	if !bytesEqual(got[:], storedCIDDigest) {
		t.Fatalf("the fetched document hashes to %x, the anchored digest is %x", got, storedCIDDigest)
	}

	var doc struct {
		TotalUnits      uint32 `json:"totalUnits"`
		DistinctHolders uint32 `json:"distinctHolders"`
		MerkleRoot      string `json:"merkleRoot"`
		AlgoVersion     int    `json:"algoVersion"`
		Lines           []struct {
			LeafIndex      uint32 `json:"leafIndex"`
			Holder         string `json:"holder"`
			Units          uint32 `json:"units"`
			Excluded       bool   `json:"excluded"`
			InvestorAnchor string `json:"investorAnchor"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.AlgoVersion != snapshot.AlgoVersion {
		t.Fatalf("algoVersion %d; a verifier needs it to know which leaf rule applies", doc.AlgoVersion)
	}

	leaves := make([]merkle.Hash, 0, len(doc.Lines))
	var sumUnits, countable uint32
	for _, l := range doc.Lines {
		addr, err := snapshot.ParseAddress(l.Holder)
		if err != nil {
			t.Fatalf("leaf %d: %v", l.LeafIndex, err)
		}
		anchor, err := merkle.ParseHash(l.InvestorAnchor)
		if err != nil {
			t.Fatalf("leaf %d: %v", l.LeafIndex, err)
		}
		leaves = append(leaves, snapshot.EntitlementLeaf(l.LeafIndex, addr, anchor, l.Units, l.Excluded))

		sumUnits += l.Units
		if !l.Excluded {
			countable++
		}
	}

	rebuilt, err := merkle.RootOf(leaves)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(rebuilt[:], storedRoot) {
		t.Fatalf("the root rebuilt from the published document is %x, the anchored root is %x",
			rebuilt, storedRoot)
	}
	if rebuilt.Hex() != doc.MerkleRoot {
		t.Fatalf("the document states root %s, its own lines produce %s", doc.MerkleRoot, rebuilt.Hex())
	}

	// The header figures must be recomputable from the lines, or a verifier has to take them on trust.
	if sumUnits != doc.TotalUnits {
		t.Errorf("lines sum to %d units, the header says %d", sumUnits, doc.TotalUnits)
	}
	if countable != doc.DistinctHolders {
		t.Errorf("%d countable lines, the header says %d", countable, doc.DistinctHolders)
	}
	if countable < snapshot.MinUnitholders {
		t.Errorf("%d countable holders is below the statutory minimum", countable)
	}

	// And one holder's own proof, the operation verifyEntitlement performs on-chain.
	tree, err := merkle.New(leaves)
	if err != nil {
		t.Fatal(err)
	}
	const checkIndex = 7
	proof, err := tree.Proof(checkIndex)
	if err != nil {
		t.Fatal(err)
	}
	if !merkle.Verify(rebuilt, leaves[checkIndex], proof) {
		t.Fatal("a holder's inclusion proof did not verify against the anchored root")
	}

	// A restated unit count must not verify, or inclusion proves nothing about the amount.
	line := doc.Lines[checkIndex]
	addr, _ := snapshot.ParseAddress(line.Holder)
	anchor, _ := merkle.ParseHash(line.InvestorAnchor)
	tampered := snapshot.EntitlementLeaf(line.LeafIndex, addr, anchor, line.Units+1, line.Excluded)
	if merkle.Verify(rebuilt, tampered, proof) {
		t.Fatal("a leaf with an inflated unit count verified against the anchored root")
	}
}

// ---------------------------------------------------------------------------
// Invariants
// ---------------------------------------------------------------------------

func assertInvariants(t *testing.T, ctx context.Context, r *run) {
	t.Helper()

	type check struct {
		name string
		run  func(t *testing.T)
	}

	checks := []check{
		{"period reached CLOSED", func(t *testing.T) {
			var status string
			if err := r.f.pool.QueryRow(ctx,
				`SELECT status FROM distribution_periods WHERE id = $1`, r.periodID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "CLOSED" {
				t.Fatalf("status = %s", status)
			}
		}},

		{"entitlements sum to the distributed amount exactly", func(t *testing.T) {
			var sum int64
			if err := r.f.pool.QueryRow(ctx, `
				SELECT COALESCE(SUM(gross_entitlement_paise), 0) FROM entitlements
				 WHERE distribution_period_id = $1`, r.periodID).Scan(&sum); err != nil {
				t.Fatal(err)
			}
			if money.Paise(sum) != r.plan.DistributedPaise {
				t.Fatalf("entitlements sum to %d, distributed is %d", sum, r.plan.DistributedPaise)
			}
		}},

		{"entitlement units equal the snapshot total", func(t *testing.T) {
			var sum int64
			if err := r.f.pool.QueryRow(ctx, `
				SELECT COALESCE(SUM(units), 0) FROM entitlements WHERE distribution_period_id = $1`,
				r.periodID).Scan(&sum); err != nil {
				t.Fatal(err)
			}
			if sum != int64(totalUnits) {
				t.Fatalf("entitlements cover %d units, want %d", sum, totalUnits)
			}
		}},

		{"every snapshot line has exactly one entitlement", func(t *testing.T) {
			var lines, ents int
			if err := r.f.pool.QueryRow(ctx,
				`SELECT count(*) FROM register_snapshot_lines WHERE snapshot_id = $1`,
				r.snapshotID).Scan(&lines); err != nil {
				t.Fatal(err)
			}
			if err := r.f.pool.QueryRow(ctx,
				`SELECT count(*) FROM entitlements WHERE distribution_period_id = $1`,
				r.periodID).Scan(&ents); err != nil {
				t.Fatal(err)
			}
			if lines != ents {
				t.Fatalf("%d snapshot lines but %d entitlements", lines, ents)
			}
		}},

		{"withholding plus net reconstructs gross", func(t *testing.T) {
			var mismatches int
			if err := r.f.pool.QueryRow(ctx, `
				SELECT count(*) FROM entitlements e
				  JOIN tax_deductions t ON t.entitlement_id = e.id
				 WHERE e.distribution_period_id = $1
				   AND t.tds_amount_paise + t.net_payable_paise <> e.gross_entitlement_paise`,
				r.periodID).Scan(&mismatches); err != nil {
				t.Fatal(err)
			}
			if mismatches != 0 {
				t.Fatalf("%d holders where withholding plus net does not equal gross", mismatches)
			}
		}},

		{"the 95% floor holds on the stored figures", func(t *testing.T) {
			var ndcfPaise, distributed int64
			var bps int
			if err := r.f.pool.QueryRow(ctx, `
				SELECT ndcf_paise, distributed_paise, distribution_bps
				  FROM distribution_periods WHERE id = $1`, r.periodID,
			).Scan(&ndcfPaise, &distributed, &bps); err != nil {
				t.Fatal(err)
			}
			ok, err := money.MeetsFloorBps(money.Paise(distributed), money.Paise(ndcfPaise), 9500)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatalf("%d of %d does not meet the floor", distributed, ndcfPaise)
			}
			if bps < 9500 || bps > 10_000 {
				t.Fatalf("stored bps = %d", bps)
			}
		}},

		{"every payout is gated on the confirmed anchor", func(t *testing.T) {
			var ungated int
			if err := r.f.pool.QueryRow(ctx, `
				SELECT count(*) FROM payout_instructions pi
				  JOIN entitlements e ON e.id = pi.entitlement_id
				  JOIN chain_outbox o ON o.id = pi.gated_on_anchor_tx
				 WHERE e.distribution_period_id = $1
				   AND (o.status <> 'CONFIRMED' OR o.confirmations < 5)`,
				r.periodID).Scan(&ungated); err != nil {
				t.Fatal(err)
			}
			if ungated != 0 {
				t.Fatalf("%d payouts rest on an insufficiently confirmed anchor", ungated)
			}
		}},

		{"settled payouts carry a UTR", func(t *testing.T) {
			var missing int
			if err := r.f.pool.QueryRow(ctx, `
				SELECT count(*) FROM payout_instructions pi
				  JOIN entitlements e ON e.id = pi.entitlement_id
				 WHERE e.distribution_period_id = $1
				   AND pi.status = 'SETTLED' AND (pi.utr IS NULL OR pi.settled_at IS NULL)`,
				r.periodID).Scan(&missing); err != nil {
				t.Fatal(err)
			}
			if missing != 0 {
				t.Fatalf("%d settled payouts have no bank reference", missing)
			}
		}},

		{"payouts total the net amount", func(t *testing.T) {
			var sum int64
			if err := r.f.pool.QueryRow(ctx, `
				SELECT COALESCE(SUM(pi.amount_paise), 0) FROM payout_instructions pi
				  JOIN entitlements e ON e.id = pi.entitlement_id
				 WHERE e.distribution_period_id = $1`, r.periodID).Scan(&sum); err != nil {
				t.Fatal(err)
			}
			want, err := r.batch.NetTotalForPayableLines()
			if err != nil {
				t.Fatal(err)
			}
			if money.Paise(sum) != want {
				t.Fatalf("payouts total %d, net total is %d", sum, want)
			}
		}},

		{"the countable holder count clears the statutory minimum", func(t *testing.T) {
			var holders int
			if err := r.f.pool.QueryRow(ctx,
				`SELECT distinct_holders FROM register_snapshots WHERE id = $1`,
				r.snapshotID).Scan(&holders); err != nil {
				t.Fatal(err)
			}
			if holders < snapshot.MinUnitholders {
				t.Fatalf("%d countable holders, minimum is %d", holders, snapshot.MinUnitholders)
			}
		}},

		{"the excluded holder was paid", func(t *testing.T) {
			// The manager is excluded from the holder count and entitled to distributions. A
			// denominator of the public float would have paid it nothing while still satisfying the
			// floor and reconciling against itself.
			var gross int64
			if err := r.f.pool.QueryRow(ctx, `
				SELECT e.gross_entitlement_paise
				  FROM entitlements e
				  JOIN register_snapshot_lines l ON l.id = e.snapshot_line_id
				 WHERE e.distribution_period_id = $1 AND l.is_excluded`,
				r.periodID).Scan(&gross); err != nil {
				t.Fatal(err)
			}
			if gross <= 0 {
				t.Fatal("the excluded holder received nothing")
			}

			perUnit, rem, err := r.batch.PerUnitRate()
			if err != nil {
				t.Fatal(err)
			}
			if rem == 0 && money.Paise(gross) != perUnit*imUnits {
				t.Fatalf("the manager received %d, the uniform rate gives %d", gross, perUnit*imUnits)
			}
		}},

		{"residue is bounded at one paise per holder", func(t *testing.T) {
			var outOfRange int
			if err := r.f.pool.QueryRow(ctx, `
				SELECT count(*) FROM entitlements
				 WHERE distribution_period_id = $1 AND residue_paise_awarded NOT BETWEEN 0 AND 1`,
				r.periodID).Scan(&outOfRange); err != nil {
				t.Fatal(err)
			}
			if outOfRange != 0 {
				t.Fatalf("%d entitlements have an out-of-range residue", outOfRange)
			}
		}},

		{"no unresolved divergence blocks the scheme", func(t *testing.T) {
			var blocking int
			if err := r.f.pool.QueryRow(ctx, `
				SELECT count(*) FROM reconciliation_runs
				 WHERE scheme_id = $1 AND blocks_payout AND resolved_at IS NULL`,
				r.f.schemeID).Scan(&blocking); err != nil {
				t.Fatal(err)
			}
			if blocking != 0 {
				t.Fatalf("%d unresolved blocking divergences", blocking)
			}
		}},

		{"the pinned documents are still retrievable", func(t *testing.T) {
			for _, pin := range []*ipfs.Pin{r.statementPin, r.snapshotPin} {
				if err := r.publisher.VerifyRoundTrip(ctx, pin); err != nil {
					t.Fatalf("%s: %v", pin.DocType, err)
				}
			}
		}},
	}

	for _, c := range checks {
		t.Run(c.name, c.run)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mustParseKey(t *testing.T, hexKey string) []byte {
	t.Helper()
	k, err := idempotency.ParseKey(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	return k[:]
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
