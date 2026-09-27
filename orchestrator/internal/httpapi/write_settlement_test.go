package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/acresync/orchestrator/internal/store"
)

// Settlement through the API: the money, the register mirror, and the three chain calls, each step refused
// until the chain has confirmed the one before it.

// finalisedAllotment drives a feasible offer through the ceremony to ALLOTMENT_FINALISED.
func finalisedAllotment(t *testing.T, h *writeHarness) (schemeID, offerID string) {
	t.Helper()
	schemeID = seedAPIScheme(t, h.ctx, h.tx)
	offerID = closedOffer(t, h, schemeID, feasibleBidders, 2)

	expect(t, h.op(t, offerID, "book/freeze", idem("freeze")), http.StatusOK)
	runID := h.runID(t, offerID)
	h.confirm(t, runID, store.FnAnchorBidbook, 100)
	expect(t, h.op(t, offerID, "ballot/commit", idem("commit")), http.StatusAccepted)
	h.confirm(t, runID, store.FnCommitSeed, 110)
	h.blockHash(120)
	h.chain.head = 121
	expect(t, h.op(t, offerID, "ballot/reveal", idem("reveal")), http.StatusAccepted)
	h.confirm(t, runID, store.FnRevealSeed, 122)
	expect(t, h.op(t, offerID, "ballot/draw", idem("draw")), http.StatusOK)

	// Not before the result anchor confirms.
	expectCode(t, advance(t, h, offerID, RoleManager, "ALLOTMENT_FINALISED", ""), http.StatusConflict, CodeAnchorNotConfirmed)
	h.confirm(t, runID, store.FnAnchorBallotResult, 130)
	expect(t, advance(t, h, offerID, RoleManager, "ALLOTMENT_FINALISED", ""), http.StatusOK)
	return schemeID, offerID
}

// recordManager onboards the scheme's investment manager with a wallet.
func recordManager(t *testing.T, h *writeHarness, schemeID string) (investorID, wallet string) {
	t.Helper()
	investorID, _, _ = seedAPIInvestor(t, h.ctx, h.tx)
	wallet = seedWallet(t, h, investorID)
	if _, err := h.tx.Exec(h.ctx, `UPDATE schemes SET im_investor_id = $2 WHERE id = $1`, schemeID, investorID); err != nil {
		t.Fatal(err)
	}
	return investorID, wallet
}

func (h *writeHarness) settlement(t *testing.T, offerID string) map[string]any {
	t.Helper()
	return expect(t, getAs(t, h.srv, "/v1/admin/offers/"+offerID+"/settlement", h.operator(t, RoleCompliance)), http.StatusOK)
}

func (h *writeHarness) batch(t *testing.T, offerID string, cursor int, key string) map[string]any {
	t.Helper()
	return h.batchRec(t, offerID, cursor, key, http.StatusAccepted)
}

func (h *writeHarness) batchRec(t *testing.T, offerID string, cursor int, key string, status int) map[string]any {
	t.Helper()
	rec := h.send(t, http.MethodPost, "/v1/admin/offers/"+offerID+"/settlement/batches", h.operator(t, RoleManager), key,
		map[string]any{"cursorFrom": cursor})
	if status != http.StatusAccepted {
		return map[string]any{"message": expectCode(t, rec, status, CodePreconditionFailed)}
	}
	return expect(t, rec, status)
}

func num(v any) int { return int(v.(float64)) }

// TestSettlementRunsToFinalisation is the whole of settlement through the API.
func TestSettlementRunsToFinalisation(t *testing.T) {
	doc := loadContract(t)
	h := newWriteHarness(t)
	schemeID, offerID := finalisedAllotment(t, h)

	// Without a manager there is nobody to credit the manager's units to, and finalisation could never pass.
	msg := expectCode(t, getAs(t, h.srv, "/v1/admin/offers/"+offerID+"/settlement", h.operator(t, RoleCompliance)),
		http.StatusConflict, CodePreconditionFailed)
	if !strings.Contains(msg, "investment manager") {
		t.Errorf("message = %q", msg)
	}
	imID, imWallet := recordManager(t, h, schemeID)

	// --- the plan --------------------------------------------------------------------------------------
	rec := getAs(t, h.srv, "/v1/admin/offers/"+offerID+"/settlement", h.operator(t, RoleCompliance))
	plan := expect(t, rec, http.StatusOK)
	assertConforms(t, doc, "SettlementState", rec.Body.Bytes())
	if plan["stage"] != "NOT_STARTED" || plan["nextStep"] != "beginSettlement" {
		t.Fatalf("before begin: stage %v, nextStep %v", plan["stage"], plan["nextStep"])
	}
	if num(plan["expectedUnits"]) != 475 {
		t.Fatalf("expectedUnits = %v, want the public 475 only", plan["expectedUnits"])
	}
	holders := num(plan["expectedHolders"])
	if holders < 200 {
		t.Fatalf("expectedHolders = %d, below the floor", holders)
	}
	batches := plan["batches"].([]any)
	if want := (holders + 99) / 100; len(batches) != want {
		t.Fatalf("%d batches for %d holders, want %d of at most 100", len(batches), holders, want)
	}

	// No batch before settlement is open.
	h.batchRec(t, offerID, 0, idem("early"), http.StatusConflict)

	// --- begin -----------------------------------------------------------------------------------------
	expectCode(t, h.send(t, http.MethodPost, "/v1/admin/offers/"+offerID+"/settlement/begin", h.operator(t, RoleTrustee), idem("b"), nil),
		http.StatusForbidden, CodeForbidden)
	rec = h.op(t, offerID, "settlement/begin", idem("begin"))
	begun := expect(t, rec, http.StatusAccepted)
	assertConforms(t, doc, "SettlementState", rec.Body.Bytes())
	if begun["nextStep"] != nil {
		t.Errorf("nextStep = %v while beginSettlement is queued, want null: there is nothing to do but wait", begun["nextStep"])
	}

	// The money: allottees debited, everyone else released, nobody's funds still held.
	if n := h.count(t, `SELECT count(*) FROM asba_blocks a JOIN bids b ON b.id = a.bid_id WHERE b.offer_id = $1 AND a.block_status = 'DEBITED'`, offerID); n != holders {
		t.Errorf("%d blocks debited, want one per allottee (%d)", n, holders)
	}
	if n := h.count(t, `SELECT count(*) FROM asba_blocks a JOIN bids b ON b.id = a.bid_id WHERE b.offer_id = $1 AND a.block_status = 'BLOCKED'`, offerID); n != 0 {
		t.Errorf("%d blocks still hold funds after settlement opened", n)
	}
	if n := h.count(t, `SELECT coalesce(sum(a.debited_amount_paise), 0) / 100000000 FROM asba_blocks a JOIN bids b ON b.id = a.bid_id WHERE b.offer_id = $1`, offerID); n != 475 {
		t.Errorf("debited %d units' worth, want 475", n)
	}
	if n := h.count(t, `SELECT count(*) FROM bids WHERE offer_id = $1 AND status = 'REJECTED_BALLOT' AND rejection_reason IS NOT NULL`, offerID); n != feasibleBidders-holders {
		t.Errorf("%d losing bids keep their reason, want %d", n, feasibleBidders-holders)
	}
	// The manager's holding, excluded from the count, and its call queued.
	if n := h.count(t, `SELECT count(*) FROM unit_holdings WHERE scheme_id = $1 AND investor_id = $2 AND units = 25 AND is_excluded_from_holder_count`, schemeID, imID); n != 1 {
		t.Fatal("the manager's 25 units are not in the mirror, excluded")
	}
	var imPayload string
	if err := h.tx.QueryRow(h.ctx, `SELECT payload_json->>'wallet' FROM chain_outbox WHERE related_entity_id = $1 AND function_name = 'recordImSubscription'`, offerID).Scan(&imPayload); err != nil || imPayload != imWallet {
		t.Fatalf("recordImSubscription wallet = %q (%v), want the manager's %s", imPayload, err, imWallet)
	}
	expectCode(t, h.op(t, offerID, "settlement/begin", idem("begin-again")), http.StatusConflict, CodePreconditionFailed)

	// --- batches ---------------------------------------------------------------------------------------
	expectCode(t, h.send(t, http.MethodPost, "/v1/admin/offers/"+offerID+"/settlement/batches", h.operator(t, RoleManager), idem("unconfirmed"),
		map[string]any{"cursorFrom": 0}), http.StatusConflict, CodeAnchorNotConfirmed)
	h.confirm(t, offerID, store.FnBeginSettlement, 200)

	after := h.settlement(t, offerID)
	if after["stage"] != "IN_PROGRESS" || after["nextStep"] != "settleBatch" {
		t.Fatalf("after begin confirmed: stage %v, nextStep %v", after["stage"], after["nextStep"])
	}

	// The cursor must be exactly the credited count.
	if m := h.batchRec(t, offerID, 100, idem("skip"), http.StatusConflict)["message"].(string); !strings.Contains(m, "credited 0") {
		t.Errorf("a skipped cursor should name the real one, got %q", m)
	}

	block := int64(201)
	cursor := 0
	for i := range batches {
		got := h.batch(t, offerID, cursor, idem("batch"))
		if !got["batches"].([]any)[i].(map[string]any)["submitted"].(bool) {
			t.Fatalf("batch %d not reported submitted", i)
		}
		// The same cursor again is refused while the first is in flight, and the next cursor is refused
		// until it confirms.
		h.batchRec(t, offerID, cursor, idem("dup"), http.StatusConflict)
		next := num(batches[i].(map[string]any)["cursorTo"])
		if next < holders {
			h.batchRec(t, offerID, next, idem("ahead"), http.StatusConflict)
		}

		// The last batch is still confirming: finalisation is refused, naming the shortfall.
		if next == holders {
			msg := expectCode(t, h.op(t, offerID, "settlement/finalise", idem("fin-early")), http.StatusConflict, CodeSettlementIncomplete)
			if !strings.Contains(msg, "cursor") {
				t.Errorf("an early finalise should name the outstanding holders, got %q", msg)
			}
		}
		h.confirm(t, offerID, store.FnSettleBatch, block)
		block++
		cursor = next
	}

	// Every holder credited, but the manager's subscription has not confirmed.
	msg = expectCode(t, h.op(t, offerID, "settlement/finalise", idem("fin-no-im")), http.StatusConflict, CodeSettlementIncomplete)
	if !strings.Contains(msg, "recordImSubscription") {
		t.Errorf("the refusal should name the missing manager subscription, got %q", msg)
	}
	h.confirm(t, offerID, store.FnRecordIMSubscription, block)

	ready := h.settlement(t, offerID)
	if ready["nextStep"] != "finaliseSettlement" || ready["finalisation"].(map[string]any)["ready"] != true {
		t.Fatalf("with everything confirmed: nextStep %v, finalisation %v", ready["nextStep"], ready["finalisation"])
	}

	// --- finalise --------------------------------------------------------------------------------------
	// Permissionless on-chain, so compliance may ask for it.
	finKey := idem("finalise")
	rec = h.send(t, http.MethodPost, "/v1/admin/offers/"+offerID+"/settlement/finalise", h.operator(t, RoleCompliance), finKey, nil)
	fin := expect(t, rec, http.StatusAccepted)
	assertConforms(t, doc, "SettlementState", rec.Body.Bytes())
	if fin["stage"] != "IN_PROGRESS" || fin["nextStep"] != nil {
		t.Errorf("with finalise queued: stage %v, nextStep %v", fin["stage"], fin["nextStep"])
	}
	if s := h.offerStatus(t, offerID); s != "SETTLED" {
		t.Fatalf("offer is %s, want SETTLED", s)
	}
	replay := h.send(t, http.MethodPost, "/v1/admin/offers/"+offerID+"/settlement/finalise", h.operator(t, RoleCompliance), finKey, nil)
	expect(t, replay, http.StatusAccepted)
	if replay.Header().Get(replayedHeader) != "true" {
		t.Error("a retried finalise was not replayed")
	}
	expectCode(t, h.op(t, offerID, "settlement/finalise", idem("fin-again")), http.StatusConflict, CodePreconditionFailed)

	h.confirm(t, offerID, store.FnFinaliseSettlement, block+1)
	done := h.settlement(t, offerID)
	if done["stage"] != "FINALISED" || done["nextStep"] != nil {
		t.Fatalf("after finalise confirmed: stage %v, nextStep %v", done["stage"], done["nextStep"])
	}

	// --- the register ----------------------------------------------------------------------------------
	if n := h.count(t, `SELECT coalesce(sum(units), 0) FROM unit_holdings WHERE scheme_id = $1`, schemeID); n != 500 {
		t.Errorf("the mirror holds %d units, the scheme is 500", n)
	}
	if n := h.count(t, `SELECT count(*) FROM unit_holdings WHERE scheme_id = $1 AND units > 0 AND NOT is_excluded_from_holder_count`, schemeID); n != holders {
		t.Errorf("%d countable holders, want %d", n, holders)
	}
	if n := h.count(t, `SELECT count(*) FROM holding_ledger WHERE scheme_id = $1 AND anchor_status = 'QUEUED'`, schemeID); n != holders+1 {
		t.Errorf("%d ledger entries, want one per allottee plus the manager", n)
	}
	if n := h.count(t, `SELECT count(*) FROM bids WHERE offer_id = $1 AND status = 'UNITS_CREDITED'`, offerID); n != holders {
		t.Errorf("%d bids credited, want %d", n, holders)
	}
	if n := h.count(t, `SELECT count(*) FROM depository_register WHERE scheme_id = $1`, schemeID); n != 0 {
		t.Errorf("the API wrote %d rows into the depository's own register", n)
	}
	for _, action := range []string{"BEGIN_SETTLEMENT", "FINALISE_SETTLEMENT"} {
		if n := h.count(t, `SELECT count(*) FROM admin_actions WHERE action = $1 AND scheme_id = $2`, action, schemeID); n != 1 {
			t.Errorf("%d %s audit rows, want 1", n, action)
		}
	}
	if n := h.count(t, `SELECT count(*) FROM admin_actions WHERE action = 'SETTLE_BATCH' AND scheme_id = $1`, schemeID); n != len(batches) {
		t.Errorf("%d SETTLE_BATCH audit rows, want %d", n, len(batches))
	}
}

// TestSettlementRefusesAnAllotteeWithoutAWallet is the plan's wallet check through the API.
func TestSettlementRefusesAnAllotteeWithoutAWallet(t *testing.T) {
	h := newWriteHarness(t)
	schemeID, offerID := finalisedAllotment(t, h)
	recordManager(t, h, schemeID)

	if _, err := h.tx.Exec(h.ctx, `
		UPDATE wallets SET is_active = false, deactivated_at = now()
		 WHERE investor_id = (SELECT b.investor_id FROM bids b JOIN allocations a ON a.bid_id = b.id
		                       WHERE b.offer_id = $1 AND a.units_allotted > 0 LIMIT 1)`, offerID); err != nil {
		t.Fatal(err)
	}

	msg := expectCode(t, h.op(t, offerID, "settlement/begin", idem("begin")), http.StatusConflict, CodePreconditionFailed)
	if !strings.Contains(msg, "register address") {
		t.Errorf("message = %q, want the leaf with no wallet named", msg)
	}
	// Refused before the bank was told anything.
	if n := h.count(t, `SELECT count(*) FROM asba_blocks a JOIN bids b ON b.id = a.bid_id WHERE b.offer_id = $1 AND a.block_status <> 'BLOCKED'`, offerID); n != 0 {
		t.Fatalf("%d blocks moved for a settlement that was refused", n)
	}
}

// TestSettlementWaitsForTheAllotment covers the status boundary.
func TestSettlementWaitsForTheAllotment(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)

	expectCode(t, getAs(t, h.srv, "/v1/admin/offers/"+offerID+"/settlement", h.operator(t, RoleManager)), http.StatusConflict, CodePreconditionFailed)
	for _, step := range []string{"settlement/begin", "settlement/finalise"} {
		expectCode(t, h.op(t, offerID, step, idem("early")), http.StatusConflict, CodePreconditionFailed)
	}
	h.batchRec(t, offerID, 0, idem("early"), http.StatusConflict)
}
