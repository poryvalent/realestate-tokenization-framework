package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/db"
	"github.com/acresync/orchestrator/internal/devsim"
	"github.com/acresync/orchestrator/internal/entitlement"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/ndcf"
	"github.com/acresync/orchestrator/internal/outbox"
	"github.com/acresync/orchestrator/internal/payout"
	"github.com/acresync/orchestrator/internal/period"
	"github.com/acresync/orchestrator/internal/snapshot"
)

// A paid distribution period on a settled scheme.
//
// There is no API for the distribution pipeline yet, so this drives it the way internal/e2e/period_test.go
// does: every figure computed by the domain packages, every transition passed through period.Guard and then
// written, so the database's own triggers (the 95% floor, dual approval, exact entitlement sums, payouts only
// against a confirmed anchor) judge the same data. The only simulated parts are the ones that are simulated
// everywhere in LOCAL: the chain confirmation (devsim), the IPFS pins (the mock provider) and the bank (the
// payout mock, recorded with provider MOCK so the API reports every payout as simulated).
//
// # The figures
//
// A quarter of a ₹50 crore scheme at a plausible yield: ₹12 crore of inflows less ₹2 crore of costs and
// reserves is ₹10 crore of NDCF... scaled down a hundredfold, so ₹1.2 crore in and ₹1 crore NDCF. Distributed at
// the 95% floor that is ₹95 lakh, or ₹19,000 a unit, about 7.6% a year on a ₹10 lakh unit. 95,00,000 rupees
// divides exactly across 500 units, so no holder is owed a residue paisa.

const payoutSourceAccount = "7878780080316316"

type paidPeriod struct {
	PeriodID    string    `json:"periodId"`
	PeriodSeq   uint32    `json:"periodSeq"`
	RecordDate  time.Time `json:"recordDate"`
	NDCFPaise   int64     `json:"ndcfPaise"`
	Distributed int64     `json:"distributedPaise"`
	Payouts     int       `json:"payoutsSettled"`
}

type periodSeed struct {
	pool     *db.Pool
	schemeID string
	ref      [32]byte
	leaseID  string
	head     devsim.Head
	pub      *ipfs.Publisher

	// details is keyed by investor id: tax class, bank account and a fund account handle.
	details map[string]entitlement.HolderDetail

	id    string
	seq   uint32
	state period.Status
}

func (p *periodSeed) key(action idempotency.Action, scope string, payload map[string]any) ([]byte, error) {
	k, err := idempotency.Derive(idempotency.Input{Action: action, SchemeID: p.schemeID, ScopeID: scope, Payload: payload})
	if err != nil {
		return nil, err
	}
	return k[:], nil
}

// advance checks the guard, then writes the status so the triggers judge it too.
func (p *periodSeed) advance(ctx context.Context, to period.Status, ev period.Evidence) error {
	if err := period.Guard(p.state, to, ev); err != nil {
		return fmt.Errorf("guard refused %s -> %s: %w", p.state, to, err)
	}
	if _, err := p.pool.Exec(ctx, `UPDATE distribution_periods SET status = $2 WHERE id = $1`, p.id, string(to)); err != nil {
		return fmt.Errorf("database refused %s -> %s: %w", p.state, to, err)
	}
	p.state = to
	return nil
}

func seedPaidPeriod(ctx context.Context, cfg *config.Config, pool *db.Pool, head devsim.Head, m *manifest,
	details map[string]entitlement.HolderDetail, leaseID string) (*paidPeriod, error) {

	provider, err := ipfs.NewProvider(cfg.IPFS, getEnv("ACRESYNC_IPFS_MOCK_DIR", "var/ipfs"))
	if err != nil {
		return nil, err
	}
	var sebiRef, schemeAddress string
	if err := pool.QueryRow(ctx, `SELECT sebi_scheme_ref, scheme_address FROM schemes WHERE id = $1`, m.SchemeID).
		Scan(&sebiRef, &schemeAddress); err != nil {
		return nil, err
	}
	p := &periodSeed{
		pool: pool, schemeID: m.SchemeID, ref: sha256.Sum256([]byte(sebiRef)), leaseID: leaseID, head: head,
		pub: ipfs.NewPublisher(provider), details: details, seq: 1, state: period.StatusOpen,
	}

	// The quarter starts the day after settlement and the business clock is moved past its record date, so
	// the scheme's calendar agrees with the period it has just paid.
	ctrl := clock.For(clock.NewPostgresStore(pool.Pool), m.SchemeID)
	now, err := ctrl.Now(ctx)
	if err != nil {
		return nil, err
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	end := start.AddDate(0, 3, -1)
	record := end
	takenAt := record.Add(36 * time.Hour)
	if _, err := ctrl.AdvanceTo(ctx, takenAt, "devseed"); err != nil {
		return nil, fmt.Errorf("advancing the business clock: %w", err)
	}

	// --- open, rent ---------------------------------------------------------------------------------------
	k, err := p.key(idempotency.ActionCreatePeriod, "", map[string]any{"periodSeq": p.seq})
	if err != nil {
		return nil, err
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO distribution_periods (scheme_id, period_seq, period_label, period_start, period_end, status, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, 'OPEN', $6) RETURNING id`,
		m.SchemeID, p.seq, "DEMO "+start.Format("Jan 2006")+" quarter", start, end, k).Scan(&p.id); err != nil {
		return nil, fmt.Errorf("opening the period: %w", err)
	}
	const monthlyRent = 380_000_000 // ₹38 lakh
	for i := 0; i < 3; i++ {
		k, err := p.key(idempotency.ActionInjectRent, p.id, map[string]any{"seq": i, "amountPaise": monthlyRent})
		if err != nil {
			return nil, err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO rent_receipts (distribution_period_id, lease_id, received_at, escrow_bank_ref,
				expected_amount_paise, received_amount_paise, variance_paise, variance_reason, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $5, 0, NULL, $6)`,
			p.id, leaseID, start.AddDate(0, i, 4), fmt.Sprintf("ESCROW/DEMO/%d", i), monthlyRent, k); err != nil {
			return nil, fmt.Errorf("recording rent: %w", err)
		}
	}
	if err := p.advance(ctx, period.StatusRentCollected, period.Evidence{RentReceiptCount: 3, RentVariancesExplained: true}); err != nil {
		return nil, err
	}

	// --- NDCF ----------------------------------------------------------------------------------------------
	ev := func(s string) [32]byte { return sha256.Sum256([]byte("devseed/evidence/" + s)) }
	items := []ndcf.LineItem{
		{LineType: ndcf.LineGrossRent, AmountPaise: 3 * monthlyRent, EvidenceSHA256: ev("rent")},
		{LineType: ndcf.LineCAMRecovery, AmountPaise: 60_000_000, EvidenceSHA256: ev("cam")},
		{LineType: ndcf.LinePropertyTax, AmountPaise: 60_000_000, EvidenceSHA256: ev("tax")},
		{LineType: ndcf.LineInsurance, AmountPaise: 10_000_000, EvidenceSHA256: ev("ins")},
		{LineType: ndcf.LineMaintenance, AmountPaise: 40_000_000, EvidenceSHA256: ev("maint")},
		{LineType: ndcf.LineTrusteeFee, AmountPaise: 10_000_000, EvidenceSHA256: ev("trustee")},
		{LineType: ndcf.LineIMFee, AmountPaise: 50_000_000, EvidenceSHA256: ev("im")},
		{LineType: ndcf.LineAuditFee, AmountPaise: 5_000_000, EvidenceSHA256: ev("audit")},
		{LineType: ndcf.LineStatutoryReserve, AmountPaise: 15_000_000, EvidenceSHA256: ev("statres")},
		{LineType: ndcf.LineWorkingCapitalReserve, AmountPaise: 10_000_000, EvidenceSHA256: ev("wcres")},
	}
	res, err := ndcf.Compute(items)
	if err != nil {
		return nil, err
	}
	plan, err := ndcf.PlanAtFloor(res)
	if err != nil {
		return nil, err
	}
	for i, it := range items {
		dir, err := it.LineType.Direction()
		if err != nil {
			return nil, err
		}
		k, err := p.key(idempotency.ActionAddNDCFLineItem, p.id, map[string]any{"seq": i, "lineType": string(it.LineType)})
		if err != nil {
			return nil, err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO ndcf_line_items (distribution_period_id, line_type, direction, amount_paise, evidence_sha256, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			p.id, string(it.LineType), string(dir), int64(it.AmountPaise), it.EvidenceSHA256[:], k); err != nil {
			return nil, fmt.Errorf("recording NDCF line %s: %w", it.LineType, err)
		}
	}
	raw, err := ndcf.BuildStatement(ndcf.StatementInput{
		SchemeRef: p.ref, PeriodID: p.seq, PeriodStart: start, PeriodEnd: end, Plan: plan, Items: items,
	})
	if err != nil {
		return nil, err
	}
	stmt, err := p.pub.Publish(ctx, ipfsguard.DocNDCFStatement, raw)
	if err != nil {
		return nil, fmt.Errorf("pinning the NDCF statement: %w", err)
	}
	stmtCID := stmt.ProviderCID
	if stmtCID == "" {
		stmtCID = stmt.DerivedCID
	}
	if _, err := pool.Exec(ctx, `
		UPDATE distribution_periods
		   SET ndcf_paise = $2, distributed_paise = $3, distribution_bps = $4,
		       ndcf_statement_sha256 = $5, ndcf_cid_digest = $5, ndcf_ipfs_cid = $6, ndcf_statement_uri = $7
		 WHERE id = $1`,
		p.id, int64(plan.NDCFPaise), int64(plan.DistributedPaise), int(plan.DistributionBps),
		stmt.Digest[:], stmtCID, "ipfs://"+stmtCID); err != nil {
		return nil, err
	}
	planEv := period.Evidence{PlanPresent: true, NDCFPaise: plan.NDCFPaise, DistributedPaise: plan.DistributedPaise,
		DistributionBps: plan.DistributionBps}
	if err := p.advance(ctx, period.StatusNDCFDrafted, planEv); err != nil {
		return nil, err
	}

	const imApprover, trusteeApprover = "devseed|manager", "devseed|trustee"
	if _, err := pool.Exec(ctx, `
		UPDATE distribution_periods
		   SET im_approved_by = $2, im_approved_at = $4, trustee_approved_by = $3, trustee_approved_at = $4
		 WHERE id = $1`, p.id, imApprover, trusteeApprover, takenAt); err != nil {
		return nil, err
	}
	if err := p.advance(ctx, period.StatusNDCFApproved, period.Evidence{IMApprovedBy: imApprover, TrusteeApprovedBy: trusteeApprover}); err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `UPDATE distribution_periods SET record_date = $2 WHERE id = $1`, p.id, record); err != nil {
		return nil, err
	}
	if err := p.advance(ctx, period.StatusRecordDateDeclared, period.Evidence{RecordDateDeclared: true}); err != nil {
		return nil, err
	}

	// --- snapshot and reconciliation ---------------------------------------------------------------------
	holdings, err := p.holdings(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := snapshot.Build(snapshot.BuildInput{
		SchemeRef: p.ref, PeriodID: p.seq, RecordDate: record, PeriodEnd: end, TakenAt: takenAt,
		Holdings: holdings, ExpectedTotalUnits: 500, RequireMinimumHolders: true,
	})
	if err != nil {
		return nil, fmt.Errorf("freezing the register: %w", err)
	}
	doc, err := snap.Document()
	if err != nil {
		return nil, err
	}
	snapPin, err := p.pub.Publish(ctx, ipfsguard.DocSnapshot, doc)
	if err != nil {
		return nil, fmt.Errorf("pinning the snapshot: %w", err)
	}
	snapCID := snapPin.ProviderCID
	if snapCID == "" {
		snapCID = snapPin.DerivedCID
	}
	k, err = p.key(idempotency.ActionTakeSnapshot, p.id, map[string]any{"recordDate": record.Format("2006-01-02")})
	if err != nil {
		return nil, err
	}
	var snapshotID string
	root := snap.MerkleRoot
	if err := pool.QueryRow(ctx, `
		INSERT INTO register_snapshots (scheme_id, distribution_period_id, record_date, taken_at, simulated_clock_value,
			total_units, distinct_holders, snapshot_merkle_root, snapshot_cid_digest, ipfs_cid, algo_version, idempotency_key)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		m.SchemeID, p.id, record, takenAt, int(snap.TotalUnits), int(snap.DistinctHolders),
		root[:], snapPin.Digest[:], snapCID, snap.AlgoVersion, k).Scan(&snapshotID); err != nil {
		return nil, fmt.Errorf("recording the snapshot: %w", err)
	}
	lineIDs := make(map[string]string, len(snap.Lines))
	for _, l := range snap.Lines {
		anchor := l.InvestorAnchor
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO register_snapshot_lines (snapshot_id, leaf_index, investor_id, investor_anchor_hash, wallet_address, units, is_excluded)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			snapshotID, int(l.LeafIndex), l.InvestorID, anchor[:], l.WalletAddress, int(l.Units), l.ExcludedFromHolderCount).Scan(&id); err != nil {
			return nil, fmt.Errorf("recording snapshot line %d: %w", l.LeafIndex, err)
		}
		lineIDs[l.InvestorID] = id
	}
	if err := p.advance(ctx, period.StatusSnapshotTaken, period.Evidence{Snapshot: snap, RequireMinimumHolders: true}); err != nil {
		return nil, err
	}

	if err := p.reconcile(ctx, takenAt); err != nil {
		return nil, err
	}
	if err := p.advance(ctx, period.StatusReconciled, period.Evidence{Snapshot: snap}); err != nil {
		return nil, err
	}

	// --- anchor --------------------------------------------------------------------------------------------
	payload, err := json.Marshal(map[string]any{
		"periodId": p.seq, "recordDate": record.Unix(),
		"ndcfPaise": int64(plan.NDCFPaise), "distributedPaise": int64(plan.DistributedPaise),
		"snapshotTotalUnits": snap.TotalUnits, "snapshotHolders": snap.DistinctHolders,
		"statementHash": stmt.Digest[:], "snapshotRoot": root[:], "ndcfCidDigest": stmt.Digest[:],
	})
	if err != nil {
		return nil, err
	}
	anchorKey, err := idempotency.Derive(idempotency.Input{Action: idempotency.ActionAnchorPeriod, SchemeID: m.SchemeID,
		ScopeID: p.id, Payload: map[string]any{"periodSeq": p.seq}})
	if err != nil {
		return nil, err
	}
	anchor, err := outbox.EnqueueWith(ctx, pool, outbox.NewEntry{
		SchemeID: m.SchemeID, TargetContract: schemeAddress, FunctionName: "anchorPeriod", Payload: payload,
		IdempotencyKey: anchorKey, RelatedEntityType: "distribution_period", RelatedEntityID: p.id, EnvironmentTag: "LOCAL",
	})
	if err != nil {
		return nil, fmt.Errorf("queueing anchorPeriod: %w", err)
	}
	if err := confirmChain(ctx, pool, head, m.SchemeID); err != nil {
		return nil, err
	}
	var txHash, status string
	var confs int
	if err := pool.QueryRow(ctx, `SELECT coalesce(tx_hash, ''), status::text, confirmations FROM chain_outbox WHERE id = $1`, anchor.ID).
		Scan(&txHash, &status, &confs); err != nil {
		return nil, err
	}
	if status != "CONFIRMED" {
		return nil, fmt.Errorf("anchorPeriod is %s after confirming", status)
	}
	if _, err := pool.Exec(ctx, `UPDATE distribution_periods SET anchored_tx = $2 WHERE id = $1`, p.id, txHash); err != nil {
		return nil, err
	}
	anchorEv := planEv
	anchorEv.StatementDigestOK, anchorEv.StatementPinned = true, true
	anchorEv.Snapshot, anchorEv.SnapshotPinned = snap, true
	anchorEv.IMApprovedBy, anchorEv.TrusteeApprovedBy = imApprover, trusteeApprover
	if err := p.advance(ctx, period.StatusAnchored, anchorEv); err != nil {
		return nil, err
	}

	// --- entitlements --------------------------------------------------------------------------------------
	batch, err := entitlement.Build(entitlement.BuildInput{
		PeriodID: p.seq, Snapshot: snap, DistributedPaise: plan.DistributedPaise, Details: details,
		TDS: entitlement.SampleTDSTable(), ProviderMinimumPaise: payout.MinAmountPaise,
	})
	if err != nil {
		return nil, fmt.Errorf("computing entitlements: %w", err)
	}
	entIDs := make(map[string]string, len(batch.Lines))
	for _, l := range batch.Lines {
		// Stored as the e2e stores it: gross already includes the residue. The exactness trigger adds
		// residue_paise_awarded again, which agrees only when every residue is zero, as it is for these
		// figures. Refused rather than written if that ever stops being true.
		if l.ResiduePaise != 0 {
			return nil, errors.New("a residue paisa was awarded; the demo figures must divide exactly across 500 units")
		}
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO entitlements (distribution_period_id, investor_id, snapshot_line_id, units, snapshot_total_units,
				gross_entitlement_paise, residue_paise_awarded, remainder_numerator)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
			p.id, l.InvestorID, lineIDs[l.InvestorID], int(l.Units), int(l.SnapshotTotalUnits),
			int64(l.GrossPaise), int(l.ResiduePaise), l.RemainderNumerator.String()).Scan(&id); err != nil {
			return nil, fmt.Errorf("recording the entitlement for %s: %w", l.InvestorID, err)
		}
		entIDs[l.InvestorID] = id
		var cert any
		if l.TDS.LowerDeductionCertRef != "" {
			cert = l.TDS.LowerDeductionCertRef
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO tax_deductions (entitlement_id, investor_class, tds_section, tds_rate_bps, tds_amount_paise,
				net_payable_paise, form_15g_h_on_file, lower_deduction_cert_ref)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, string(l.TDS.Class), l.TDS.Section, int(l.TDS.RateBps), int64(l.TDS.AmountPaise),
			int64(l.TDS.NetPayablePaise), l.TDS.Form15GHOnFile, cert); err != nil {
			return nil, err
		}
	}
	if err := p.advance(ctx, period.StatusEntitlementsAnchored, period.Evidence{
		Snapshot: snap, EntitledUnits: snap.TotalUnits, EntitledHolders: uint32(len(snap.Lines)),
	}); err != nil {
		return nil, err
	}

	// --- payouts -------------------------------------------------------------------------------------------
	reqs, skipped, err := batch.PayoutRequests(entitlement.PayoutRequestInput{
		SchemeID: m.SchemeID, AnchorOutboxID: anchor.ID, AccountNumber: payoutSourceAccount,
		Mode: payout.ModeIMPS, Purpose: payout.PurposePayout, Narration: "AcreSync demo quarterly distribution",
	})
	if err != nil {
		return nil, err
	}
	if len(skipped) != 0 {
		return nil, fmt.Errorf("%d holders are unpayable: %s", len(skipped), skipped[0].NotPayableReason)
	}
	bank := payout.NewMock(clock.Fixed(takenAt), payoutSourceAccount, money.Paise(10_000_000_000))
	providerIDs := make(map[string]string, len(reqs))
	for i, req := range reqs {
		line := batch.Lines[i]
		po, err := bank.CreatePayout(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("instructing the payout for %s: %w", line.InvestorID, err)
		}
		persisted, err := payout.ToDBStatus(po, takenAt)
		if err != nil {
			return nil, err
		}
		key, err := idempotency.ParseKey(req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO payout_instructions (entitlement_id, bank_account_id, amount_paise, provider, provider_ref,
				status, gated_on_anchor_tx, idempotency_key, submitted_at)
			VALUES ($1, $2, $3, 'MOCK', $4, $5, $6, $7, $8)`,
			entIDs[line.InvestorID], line.Detail.BankAccountID, int64(line.TDS.NetPayablePaise), persisted.ProviderRef,
			string(persisted.Status), anchor.ID, key[:], takenAt); err != nil {
			return nil, fmt.Errorf("recording the payout for %s: %w", line.InvestorID, err)
		}
		providerIDs[line.InvestorID] = po.ProviderID
	}
	if err := p.advance(ctx, period.StatusPayoutInstructed, period.Evidence{
		Anchor: &period.AnchorRef{OutboxID: anchor.ID, Status: status, Confirmations: confs},
	}); err != nil {
		return nil, err
	}

	for round := 0; round < 10; round++ {
		changed, err := bank.Advance(ctx)
		if err != nil {
			return nil, err
		}
		if len(changed) == 0 {
			break
		}
	}
	var tally period.PayoutTally
	var settled []*payout.Payout
	for investorID, pid := range providerIDs {
		cur, err := bank.FetchPayout(ctx, pid)
		if err != nil {
			return nil, err
		}
		persisted, err := payout.ToDBStatus(cur, takenAt)
		if err != nil {
			return nil, err
		}
		if _, err := pool.Exec(ctx, `
			UPDATE payout_instructions SET status = $2, utr = nullif($3, ''), settled_at = $4, failure_code = nullif($5, '')
			 WHERE entitlement_id = $1`,
			entIDs[investorID], string(persisted.Status), persisted.UTR, persisted.SettledAt, persisted.FailureCode); err != nil {
			return nil, err
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
	_, total, err := payout.SettledCountAndAmount(settled)
	if err != nil {
		return nil, err
	}
	tally.ConfirmedPaise = money.Paise(total)
	if err := p.advance(ctx, period.StatusPayoutsConfirmed, period.Evidence{Payouts: tally}); err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `UPDATE distribution_periods SET closed_at = $2 WHERE id = $1`, p.id, takenAt); err != nil {
		return nil, err
	}
	if err := p.advance(ctx, period.StatusClosed, period.Evidence{Payouts: tally}); err != nil {
		return nil, err
	}

	return &paidPeriod{PeriodID: p.id, PeriodSeq: p.seq, RecordDate: record, NDCFPaise: int64(plan.NDCFPaise),
		Distributed: int64(plan.DistributedPaise), Payouts: len(settled)}, nil
}

// holdings reads the settled register with each holder's anchor.
func (p *periodSeed) holdings(ctx context.Context) ([]snapshot.Holding, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT h.investor_id::text, h.wallet_address, h.units, h.is_excluded_from_holder_count, a.anchor_hash
		  FROM unit_holdings h
		  JOIN investor_anchors a ON a.investor_id = h.investor_id AND a.scheme_id = h.scheme_id
		 WHERE h.scheme_id = $1 AND h.units > 0`, p.schemeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []snapshot.Holding
	for rows.Next() {
		var h snapshot.Holding
		var units int32
		var anchor []byte
		if err := rows.Scan(&h.InvestorID, &h.WalletAddress, &units, &h.ExcludedFromHolderCount, &anchor); err != nil {
			return nil, err
		}
		h.Units = uint32(units)
		copy(h.InvestorAnchor[:], anchor)
		out = append(out, h)
	}
	return out, rows.Err()
}

// reconcile writes the depository's copy of the allotment and reconciles the mirror against it.
//
// The API never writes depository_register: it stands for the depository's own books. In a real allotment the
// depository credits each allottee's demat account, so here the simulated depository records the same
// positions, and the reconciliation then genuinely compares two tables.
func (p *periodSeed) reconcile(ctx context.Context, at time.Time) error {
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO depository_register (scheme_id, investor_id, wallet_address, units)
		SELECT scheme_id, investor_id, wallet_address, units FROM unit_holdings WHERE scheme_id = $1
		ON CONFLICT (scheme_id, investor_id) DO NOTHING`, p.schemeID); err != nil {
		return err
	}
	read := func(table string) ([]period.Position, error) {
		rows, err := p.pool.Query(ctx, `SELECT investor_id::text, wallet_address, units FROM `+table+` WHERE scheme_id = $1`, p.schemeID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []period.Position
		for rows.Next() {
			var pos period.Position
			var units int32
			if err := rows.Scan(&pos.InvestorID, &pos.WalletAddress, &units); err != nil {
				return nil, err
			}
			pos.Units = uint32(units)
			out = append(out, pos)
		}
		return out, rows.Err()
	}
	dep, err := read("depository_register")
	if err != nil {
		return err
	}
	mir, err := read("unit_holdings")
	if err != nil {
		return err
	}
	rec, err := period.Reconcile(dep, mir)
	if err != nil {
		return err
	}
	if rec.Status != period.ReconMatched {
		return fmt.Errorf("the register does not reconcile: %s", rec.Summary())
	}
	k, err := p.key(idempotency.ActionRunReconciliation, p.id, map[string]any{"runAt": at.Format(time.RFC3339)})
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO reconciliation_runs (scheme_id, run_at, simulated_clock_value, depository_total_units, chain_total_units,
			depository_holder_count, chain_holder_count, divergence_count, status, blocks_payout, idempotency_key)
		VALUES ($1, $2, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		p.schemeID, at, int(rec.DepositoryTotalUnits), int(rec.ChainTotalUnits), rec.DepositoryHolderCount,
		rec.ChainHolderCount, rec.DivergenceCount(), string(rec.Status), rec.BlocksPayout, k)
	return err
}

// confirmChain confirms every queued call for the scheme, as devconfirm would.
func confirmChain(ctx context.Context, pool *db.Pool, head devsim.Head, schemeID string) error {
	n, err := head.Head(ctx)
	if err != nil {
		return fmt.Errorf("reading the chain head: %w", err)
	}
	_, err = devsim.Confirm(ctx, pool, devsim.Options{Head: n, SchemeID: schemeID})
	return err
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
