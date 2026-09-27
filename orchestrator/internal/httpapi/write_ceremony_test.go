package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/store"
)

// The freeze and the commit-reveal ceremony, end to end through the API.
//
// The chain is simulated at exactly one seam: an outbox row is marked CONFIRMED at a block number the test
// chooses, which is what the relayer's confirmation sweep does in production. Everything downstream of that
// (the target block, the reveal window, the draw) is derived from those rows by the handlers, not handed to
// them.

// feasibleBidders is enough distinct bidders to clear the 200-holder floor, each bidding two units, so the
// book is 480 units against 475 on offer: over the 428-unit minimum and oversubscribed, which makes the draw
// a real ballot rather than an allot-everyone.
const feasibleBidders = 240

// closedOffer opens the fixture offer, places one funded bid per bidder through the API, and closes it.
func closedOffer(t *testing.T, h *writeHarness, schemeID string, bidders, units int) string {
	t.Helper()
	offerID := openOffer(t, h, schemeID)
	path := "/v1/offers/" + offerID + "/bids"
	for i := 0; i < bidders; i++ {
		investorID, demat, bank := seedBidder(t, h, schemeID)
		expect(t, h.send(t, http.MethodPost, path, h.investor(t, investorID), idem("bid"), bidBody(units, demat, bank)),
			http.StatusCreated)
	}
	expect(t, advance(t, h, offerID, RoleManager, "CLOSED", ""), http.StatusOK)
	return offerID
}

// op sends a bodiless manager request to one of the offer's ceremony endpoints.
func (h *writeHarness) op(t *testing.T, offerID, step, key string) *httptest.ResponseRecorder {
	t.Helper()
	return h.send(t, http.MethodPost, "/v1/admin/offers/"+offerID+"/"+step, h.operator(t, RoleManager), key, nil)
}

func (h *writeHarness) runID(t *testing.T, offerID string) string {
	t.Helper()
	var id string
	if err := h.tx.QueryRow(h.ctx, `SELECT id FROM ballot_runs WHERE offer_id = $1`, offerID).Scan(&id); err != nil {
		t.Fatalf("reading the ballot run: %v", err)
	}
	return id
}

// confirm marks the queued outbox row for fn against the entity as confirmed in the given block, as the
// relayer's sweep would once it saw the receipt at depth. Only a queued row qualifies: every request in the
// harness shares one outer transaction, so created_at cannot tell two rows apart.
func (h *writeHarness) confirm(t *testing.T, entityID, fn string, block int64) {
	t.Helper()
	tx := sha256.Sum256([]byte(apiUniq("tx")))
	tag, err := h.tx.Exec(h.ctx, `
		UPDATE chain_outbox o
		   SET status = 'CONFIRMED', tx_hash = $3, block_number = $4, confirmed_at = now(), confirmations = 12,
		       submitted_at = now(), attempt_count = 1,
		       nonce = (SELECT coalesce(max(nonce), -1) + 1 FROM chain_outbox WHERE scheme_id = o.scheme_id)
		 WHERE id = (SELECT id FROM chain_outbox
		              WHERE related_entity_id = $1 AND function_name = $2 AND status = 'QUEUED'
		              ORDER BY created_at DESC, id DESC LIMIT 1)`,
		entityID, fn, "0x"+hex.EncodeToString(tx[:]), block)
	if err != nil {
		t.Fatalf("confirming %s: %v", fn, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("no queued %s call against %s to confirm", fn, entityID)
	}
}

// blockHash gives the fake chain a non-zero hash for a block.
func (h *writeHarness) blockHash(n uint64) merkle.Hash {
	sum := sha256.Sum256([]byte{byte(n >> 8), byte(n), 'b', 'l', 'o', 'c', 'k'})
	h.chain.hashes[n] = merkle.Hash(sum)
	return merkle.Hash(sum)
}

// TestAnInfeasibleBookStopsBeforeAnythingIsPublic covers the quiet-abandonment branch of freezeBook.
func TestAnInfeasibleBookStopsBeforeAnythingIsPublic(t *testing.T) {
	doc := loadContract(t)
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := closedOffer(t, h, schemeID, 3, 2)

	rec := h.op(t, offerID, "book/freeze", idem("freeze"))
	got := expect(t, rec, http.StatusOK)
	assertConforms(t, doc, "BookFreezeResult", rec.Body.Bytes())

	f := got["feasibility"].(map[string]any)
	if f["feasible"] != false || f["holderFloorMet"] != false {
		t.Fatalf("feasibility = %v, want infeasible on the holder floor", f)
	}
	if reasons := f["reasons"].([]any); len(reasons) == 0 || !strings.Contains(reasons[0].(string), "floor") {
		t.Errorf("reasons = %v, want the domain's explanation", reasons)
	}
	if _, ok := got["document"]; ok {
		t.Error("an infeasible book must not be published")
	}
	if s := h.offerStatus(t, offerID); s != "FEASIBILITY_CHECKED" {
		t.Fatalf("offer is %s, want FEASIBILITY_CHECKED", s)
	}
	if n := h.count(t, `SELECT count(*) FROM ballot_runs WHERE offer_id = $1`, offerID); n != 0 {
		t.Fatalf("%d ballot runs for an infeasible book", n)
	}
	if n := h.count(t, `SELECT count(*) FROM chain_outbox WHERE scheme_id = $1`, schemeID); n != 0 {
		t.Fatalf("%d chain calls were queued for an infeasible book", n)
	}
	if n := h.count(t, `SELECT count(*) FROM admin_actions WHERE action = 'FREEZE_BOOK' AND scheme_id = $1`, schemeID); n != 1 {
		t.Fatalf("%d FREEZE_BOOK audit rows, want 1", n)
	}

	// Not frozen twice.
	expectCode(t, h.op(t, offerID, "book/freeze", idem("again")), http.StatusConflict, CodePreconditionFailed)
}

// TestFreezeRefusesAnOpenOffer is the order: the book cannot be frozen while bids may still arrive.
func TestFreezeRefusesAnOpenOffer(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)

	expectCode(t, h.op(t, offerID, "book/freeze", idem("open")), http.StatusConflict, CodePreconditionFailed)
	if s := h.offerStatus(t, offerID); s != "OPEN" {
		t.Fatalf("a refused freeze left the offer %s", s)
	}
}

// TestTheCeremonyIsManagerOnly pins the role boundary on every ceremony route.
func TestTheCeremonyIsManagerOnly(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)

	for _, step := range []string{"book/freeze", "ballot/commit", "ballot/reveal", "ballot/recommit", "ballot/draw"} {
		for _, role := range []Role{RoleTrustee, RoleCompliance} {
			rec := h.send(t, http.MethodPost, "/v1/admin/offers/"+offerID+"/"+step, h.operator(t, role), idem("role"), nil)
			expectCode(t, rec, http.StatusForbidden, CodeForbidden)
		}
	}
}

// TestTheCeremonyRunsInOrder drives a feasible offer from the freeze to the draw, refusing each step until
// the chain has confirmed the one before it.
func TestTheCeremonyRunsInOrder(t *testing.T) {
	doc := loadContract(t)
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := closedOffer(t, h, schemeID, feasibleBidders, 2)

	// --- freeze ---------------------------------------------------------------------------------------
	rec := h.op(t, offerID, "book/freeze", idem("freeze"))
	frozen := expect(t, rec, http.StatusOK)
	assertConforms(t, doc, "BookFreezeResult", rec.Body.Bytes())

	if frozen["feasibility"].(map[string]any)["feasible"] != true {
		t.Fatalf("feasibility = %v", frozen["feasibility"])
	}
	if int(frozen["leafCount"].(float64)) != feasibleBidders || int(frozen["totalUnitsBid"].(float64)) != 2*feasibleBidders {
		t.Errorf("leafCount/totalUnitsBid = %v/%v", frozen["leafCount"], frozen["totalUnitsBid"])
	}
	document := frozen["document"].(map[string]any)
	if document["docType"] != "BIDBOOK" {
		t.Errorf("document = %v", document)
	}
	if s := h.offerStatus(t, offerID); s != "BIDBOOK_ANCHORED" {
		t.Fatalf("offer is %s after the freeze, want BIDBOOK_ANCHORED", s)
	}
	runID := h.runID(t, offerID)
	if n := h.count(t, `SELECT count(*) FROM chain_outbox WHERE related_entity_id = $1 AND function_name = 'anchorBidbook' AND status = 'QUEUED'`, runID); n != 1 {
		t.Fatalf("%d anchorBidbook calls queued, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM ipfs_pins WHERE related_entity_id = $1 AND doc_type = 'BIDBOOK'`, runID); n != 1 {
		t.Fatalf("%d bid book pins, want 1", n)
	}
	// The anchored root is the root of the pinned bytes' book, and it is the digest the response names.
	var pinned []byte
	if err := h.tx.QueryRow(h.ctx, `SELECT content_sha256 FROM ipfs_pins WHERE related_entity_id = $1`, runID).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if document["digest"] != "0x"+hex.EncodeToString(pinned) && document["digest"] != hex.EncodeToString(pinned) {
		t.Errorf("document digest %v is not the pinned content hash %x", document["digest"], pinned)
	}

	// --- commit ---------------------------------------------------------------------------------------
	// Not before the book's anchor confirms: a commitment bound to a book the chain may still drop proves
	// nothing about the book.
	expectCode(t, h.op(t, offerID, "ballot/commit", idem("commit-early")), http.StatusConflict, CodeAnchorNotConfirmed)
	h.confirm(t, runID, store.FnAnchorBidbook, 100)

	rec = h.op(t, offerID, "ballot/commit", idem("commit"))
	committed := expect(t, rec, http.StatusAccepted)
	assertConforms(t, doc, "BallotCeremony", rec.Body.Bytes())
	if committed["stage"] != "SEED_COMMITTED" || committed["seedCommitment"] == nil {
		t.Fatalf("after commit: %v", committed)
	}
	if committed["seedPlaintext"] != nil || committed["targetBlock"] != nil {
		t.Fatalf("the secret or a target block is visible before the commitment confirmed: %v", committed)
	}
	// The secret is not on disk anywhere yet.
	if n := h.count(t, `SELECT count(*) FROM ballot_runs WHERE id = $1 AND seed_plaintext IS NOT NULL`, runID); n != 0 {
		t.Fatal("the seed plaintext was stored at commit time")
	}

	// --- reveal ---------------------------------------------------------------------------------------
	h.chain.head = 200
	expectCode(t, h.op(t, offerID, "ballot/reveal", idem("reveal-unconfirmed")), http.StatusConflict, CodeAnchorNotConfirmed)

	// commitSeed lands in block 110, so the contract's target is 120.
	h.confirm(t, runID, store.FnCommitSeed, 110)
	h.chain.head = 120
	msg := expectCode(t, h.op(t, offerID, "ballot/reveal", idem("reveal-at-target")), http.StatusConflict, CodePreconditionFailed)
	if !strings.Contains(msg, "target") {
		t.Errorf("revealing at the target block should say the target is not reached, got %q", msg)
	}

	targetHash := h.blockHash(120)
	h.chain.head = 121
	rec = h.op(t, offerID, "ballot/reveal", idem("reveal"))
	revealed := expect(t, rec, http.StatusAccepted)
	assertConforms(t, doc, "BallotCeremony", rec.Body.Bytes())
	if revealed["stage"] != "SEED_REVEALED" || int(revealed["targetBlock"].(float64)) != 120 {
		t.Fatalf("after reveal: %v", revealed)
	}
	if !strings.HasSuffix(revealed["targetBlockHash"].(string), targetHash.Hex()[2:]) {
		t.Errorf("targetBlockHash = %v, want the chain's hash of block 120 (%s)", revealed["targetBlockHash"], targetHash.Hex())
	}
	if revealed["finalSeed"] == nil || revealed["seedPlaintext"] == nil {
		t.Fatalf("a revealed ceremony shows its secret and seed: %v", revealed)
	}

	// --- draw -----------------------------------------------------------------------------------------
	expectCode(t, h.op(t, offerID, "ballot/draw", idem("draw-early")), http.StatusConflict, CodeAnchorNotConfirmed)
	h.confirm(t, runID, store.FnRevealSeed, 122)

	drawKey := idem("draw")
	rec = h.op(t, offerID, "ballot/draw", drawKey)
	drawn := expect(t, rec, http.StatusOK)
	assertConforms(t, doc, "DrawResult", rec.Body.Bytes())

	if drawn["finalSeed"] != revealed["finalSeed"] {
		t.Errorf("the draw used seed %v, the reveal derived %v", drawn["finalSeed"], revealed["finalSeed"])
	}
	if int(drawn["unitsAllotted"].(float64)) != 475 {
		t.Errorf("unitsAllotted = %v, want every unit on offer", drawn["unitsAllotted"])
	}
	counts := drawn["outcomeCounts"].(map[string]any)
	total := 0
	for _, v := range counts {
		total += int(v.(float64))
	}
	if total != feasibleBidders {
		t.Errorf("outcome counts %v cover %d bids, want all %d", counts, total, feasibleBidders)
	}
	if s := h.offerStatus(t, offerID); s != "BALLOT_DRAWN" {
		t.Fatalf("offer is %s after the draw, want BALLOT_DRAWN", s)
	}

	// Every bid has an allocation row and an outcome, losers included.
	if n := h.count(t, `SELECT count(*) FROM allocations WHERE ballot_run_id = $1`, runID); n != feasibleBidders {
		t.Fatalf("%d allocation rows, want one per bid (%d)", n, feasibleBidders)
	}
	if n := h.count(t, `SELECT coalesce(sum(units_allotted), 0) FROM allocations WHERE ballot_run_id = $1`, runID); n != 475 {
		t.Fatalf("allocations total %d units, want 475", n)
	}
	if n := h.count(t, `SELECT count(*) FROM bids WHERE offer_id = $1 AND status = 'IN_BOOK'`, offerID); n != 0 {
		t.Fatalf("%d bids still IN_BOOK after the draw", n)
	}
	if n := h.count(t, `SELECT count(*) FROM chain_outbox WHERE related_entity_id = $1 AND function_name = 'anchorBallotResult'`, runID); n != 1 {
		t.Fatalf("%d anchorBallotResult calls queued, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM ipfs_pins WHERE related_entity_id = $1 AND doc_type = 'ALLOTMENT_FILE'`, runID); n != 1 {
		t.Fatalf("%d allotment file pins, want 1", n)
	}

	// A retried draw is a replay, not a second draw.
	again := h.op(t, offerID, "ballot/draw", drawKey)
	replayed := expect(t, again, http.StatusOK)
	if again.Header().Get(replayedHeader) != "true" || replayed["resultRoot"] != drawn["resultRoot"] {
		t.Fatalf("a retried draw was not replayed: %v", replayed)
	}
	// And a fresh key cannot draw again.
	expectCode(t, h.op(t, offerID, "ballot/draw", idem("redraw")), http.StatusConflict, CodeCeremonyOrder)

	// The audit trail has one row per step.
	for _, action := range []string{"FREEZE_BOOK", "COMMIT_SEED", "REVEAL_SEED", "RUN_BALLOT"} {
		if n := h.count(t, `SELECT count(*) FROM admin_actions WHERE action = $1 AND scheme_id = $2`, action, schemeID); n != 1 {
			t.Errorf("%d %s audit rows, want 1", n, action)
		}
	}
}

// TestRecommitWaitsForTheWindowToLapse is the rule that stops an operator rerolling a draw they dislike.
func TestRecommitWaitsForTheWindowToLapse(t *testing.T) {
	doc := loadContract(t)
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := closedOffer(t, h, schemeID, feasibleBidders, 2)

	expect(t, h.op(t, offerID, "book/freeze", idem("freeze")), http.StatusOK)
	runID := h.runID(t, offerID)
	h.confirm(t, runID, store.FnAnchorBidbook, 100)
	expect(t, h.op(t, offerID, "ballot/commit", idem("commit")), http.StatusAccepted)
	h.confirm(t, runID, store.FnCommitSeed, 110) // target 120, deadline 320

	// While a reveal is still possible, recommitting is refused.
	h.chain.head = 320
	msg := expectCode(t, h.op(t, offerID, "ballot/recommit", idem("rc-open")), http.StatusConflict, CodePreconditionFailed)
	if !strings.Contains(msg, "reveal is still") {
		t.Errorf("message = %q, want the window-still-open rule", msg)
	}

	// Past the deadline a reveal is refused and a recommit allowed.
	h.blockHash(120)
	h.chain.head = 321
	expectCode(t, h.op(t, offerID, "ballot/reveal", idem("rv-late")), http.StatusConflict, CodePreconditionFailed)

	rec := h.op(t, offerID, "ballot/recommit", idem("rc"))
	got := expect(t, rec, http.StatusAccepted)
	assertConforms(t, doc, "BallotCeremony", rec.Body.Bytes())
	if int(got["attempt"].(float64)) != 2 || got["targetBlock"] != nil {
		t.Fatalf("after recommit: %v, want attempt 2 and no target until it confirms", got)
	}

	// The fresh window has no target until the recommit confirms, so a reveal is refused.
	expectCode(t, h.op(t, offerID, "ballot/reveal", idem("rv-pending")), http.StatusConflict, CodeAnchorNotConfirmed)

	// recommitSeed lands in block 330: the new target is 340, and the reveal proceeds against it.
	h.confirm(t, runID, store.FnRecommitSeed, 330)
	h.blockHash(340)
	h.chain.head = 341
	revealed := expect(t, h.op(t, offerID, "ballot/reveal", idem("rv")), http.StatusAccepted)
	if int(revealed["targetBlock"].(float64)) != 340 || int(revealed["attempt"].(float64)) != 2 {
		t.Fatalf("after reveal: %v, want target 340 on attempt 2", revealed)
	}

	// The commitment never moved.
	if n := h.count(t, `SELECT count(*) FROM chain_outbox WHERE related_entity_id = $1 AND function_name = 'commitSeed'`, runID); n != 1 {
		t.Fatalf("%d commitSeed calls, want 1: a recommit reuses the commitment", n)
	}
}

// TestCommittingTwiceIsRefused covers the ceremony's own ordering through the API.
func TestCommittingTwiceIsRefused(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := closedOffer(t, h, schemeID, feasibleBidders, 2)

	expect(t, h.op(t, offerID, "book/freeze", idem("freeze")), http.StatusOK)
	h.confirm(t, h.runID(t, offerID), store.FnAnchorBidbook, 100)
	expect(t, h.op(t, offerID, "ballot/commit", idem("c1")), http.StatusAccepted)

	expectCode(t, h.op(t, offerID, "ballot/commit", idem("c2")), http.StatusConflict, CodeCeremonyOrder)
}

// TestCeremonyStepsNeedAFrozenBook covers the steps before there is a ballot run.
func TestCeremonyStepsNeedAFrozenBook(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)

	for _, step := range []string{"ballot/commit", "ballot/reveal", "ballot/recommit", "ballot/draw"} {
		expectCode(t, h.op(t, offerID, step, idem("nobook")), http.StatusConflict, CodeCeremonyOrder)
	}
}
