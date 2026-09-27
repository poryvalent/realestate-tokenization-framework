package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newUndeployedFixtureScheme would be needed for createOffer on a fresh scheme; the fixture scheme has no
// offer yet, so createOffer runs against it directly.

func createOfferBody(schemeID string, opens time.Time) map[string]any {
	return map[string]any{"schemeId": schemeID, "offerType": "INITIAL", "terms": fixtureTerms(opens)}
}

// TestCreateOfferRecordsAConfiguredOffer is the happy path, audited.
func TestCreateOfferRecordsAConfiguredOffer(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)

	rec := h.send(t, http.MethodPost, "/v1/admin/offers", h.operator(t, RoleManager), idem("create"),
		createOfferBody(schemeID, businessNow.Add(-time.Hour)))
	got := expect(t, rec, http.StatusCreated)

	if got["status"] != "CONFIGURED" {
		t.Errorf("status = %v, want CONFIGURED", got["status"])
	}
	if got["schemeId"] != schemeID {
		t.Errorf("schemeId = %v", got["schemeId"])
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a mutation response", cc)
	}

	// The audit trail records who did it, in the same transaction.
	if n := h.count(t, `SELECT count(*) FROM admin_actions WHERE action = 'CREATE_OFFER' AND scheme_id = $1`, schemeID); n != 1 {
		t.Fatalf("%d CREATE_OFFER audit rows, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM admin_actions WHERE action = 'CREATE_OFFER' AND actor_user_id = 'ops|manager-1'`); n < 1 {
		t.Error("the audit row does not name the operator who acted")
	}
}

// TestCreateOfferIsAManagerAction pins the role boundary.
func TestCreateOfferIsAManagerAction(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)

	for _, role := range []Role{RoleTrustee, RoleCompliance} {
		rec := h.send(t, http.MethodPost, "/v1/admin/offers", h.operator(t, role), idem("create-"+string(role)),
			createOfferBody(schemeID, businessNow))
		expectCode(t, rec, http.StatusForbidden, CodeForbidden)
	}
	if n := h.count(t, `SELECT count(*) FROM offers WHERE scheme_id = $1`, schemeID); n != 0 {
		t.Fatalf("%d offers were created by roles that may not create them", n)
	}
}

// TestCreateOfferRefusesTermsThatCannotSettle covers the rules that fail late if they are not caught here.
func TestCreateOfferRefusesTermsThatCannotSettle(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	token := h.operator(t, RoleManager)

	cases := map[string]func(map[string]any){
		// Fewer units than the public side leaves the register short and settlement could never finalise.
		"short of the public units": func(tm map[string]any) { tm["unitsOnOffer"] = 400; tm["minSubscriptionUnits"] = 380 },
		// A cap above units - (floor - 1) makes the holder floor unreachable by construction.
		"a cap that makes the floor unreachable": func(tm map[string]any) { tm["maxBidUnits"] = 300 },
		"a price band below the statutory floor": func(tm map[string]any) { tm["priceBandLowerPaise"] = 99999999 },
		"a closing time before the opening":      func(tm map[string]any) { tm["closesAt"] = businessNow.Add(-48 * time.Hour).Format(time.RFC3339) },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body := createOfferBody(schemeID, businessNow)
			mutate(body["terms"].(map[string]any))
			rec := h.send(t, http.MethodPost, "/v1/admin/offers", token, idem("bad-terms"), body)
			expectCode(t, rec, http.StatusUnprocessableEntity, CodeValidationFailed)
		})
	}
	if n := h.count(t, `SELECT count(*) FROM offers WHERE scheme_id = $1`, schemeID); n != 0 {
		t.Fatalf("%d offers were created from invalid terms", n)
	}
}

// TestOnlyOneInitialOfferPerScheme is the first issuance being a singular event.
func TestOnlyOneInitialOfferPerScheme(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	token := h.operator(t, RoleManager)

	expect(t, h.send(t, http.MethodPost, "/v1/admin/offers", token, idem("first"), createOfferBody(schemeID, businessNow)), http.StatusCreated)

	second := createOfferBody(schemeID, businessNow.Add(time.Hour))
	expectCode(t, h.send(t, http.MethodPost, "/v1/admin/offers", token, idem("second"), second),
		http.StatusConflict, CodePreconditionFailed)
}

// TestARetryReplaysTheOriginalResponse is the Idempotency-Key contract.
//
// Same key, same body: the stored response comes back, marked as a replay, and nothing is written twice.
// Same key, different body: a conflict, because replaying would tell the caller a request succeeded that was
// never executed.
func TestARetryReplaysTheOriginalResponse(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	token := h.operator(t, RoleManager)
	k := idem("replay")
	body := createOfferBody(schemeID, businessNow)

	first := h.send(t, http.MethodPost, "/v1/admin/offers", token, k, body)
	firstBody := expect(t, first, http.StatusCreated)
	if first.Header().Get(replayedHeader) != "" {
		t.Error("the original response must not claim to be a replay")
	}

	again := h.send(t, http.MethodPost, "/v1/admin/offers", token, k, body)
	againBody := expect(t, again, http.StatusCreated)
	if again.Header().Get(replayedHeader) != "true" {
		t.Errorf("%s = %q on the retry, want true", replayedHeader, again.Header().Get(replayedHeader))
	}
	if firstBody["id"] != againBody["id"] {
		t.Fatalf("the retry returned offer %v, the original was %v", againBody["id"], firstBody["id"])
	}
	if n := h.count(t, `SELECT count(*) FROM offers WHERE scheme_id = $1`, schemeID); n != 1 {
		t.Fatalf("%d offers exist after a retried create, want 1", n)
	}

	changed := createOfferBody(schemeID, businessNow.Add(2*time.Hour))
	expectCode(t, h.send(t, http.MethodPost, "/v1/admin/offers", token, k, changed),
		http.StatusConflict, CodeIdempotencyConflict)
}

// TestIdempotencyKeysAreScopedToTheCaller stops one operator's key answering for another.
func TestIdempotencyKeysAreScopedToTheCaller(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	k := idem("shared")
	body := createOfferBody(schemeID, businessNow)

	expect(t, h.send(t, http.MethodPost, "/v1/admin/offers", h.operator(t, RoleManager), k, body), http.StatusCreated)

	other := tokenFor(t, h.srv, Claims{Subject: "ops|manager-2", Kind: PrincipalOperator, Role: RoleManager,
		IssuedAt: tokenNow.Unix(), ExpiresAt: tokenNow.Add(time.Hour).Unix()})
	rec := h.send(t, http.MethodPost, "/v1/admin/offers", other, k, body)
	if rec.Header().Get(replayedHeader) == "true" {
		t.Fatal("a second operator received the first operator's stored response")
	}
	// The second operator's request is executed on its own merits, and the scheme already has its offer.
	expectCode(t, rec, http.StatusConflict, CodePreconditionFailed)
}

// TestAMutationWithoutAKeyIsRefused covers the header rule on the real write path.
func TestAMutationWithoutAKeyIsRefused(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)

	expectCode(t, h.send(t, http.MethodPost, "/v1/admin/offers", h.operator(t, RoleManager), "", createOfferBody(schemeID, businessNow)),
		http.StatusBadRequest, CodeValidationFailed)
	expectCode(t, h.send(t, http.MethodPost, "/v1/admin/offers", h.operator(t, RoleManager), "too-short", createOfferBody(schemeID, businessNow)),
		http.StatusBadRequest, CodeValidationFailed)
}

// createdOffer creates the fixture offer through the API and returns its id.
func createdOffer(t *testing.T, h *writeHarness, schemeID string, opens time.Time) string {
	t.Helper()
	got := expect(t, h.send(t, http.MethodPost, "/v1/admin/offers", h.operator(t, RoleManager), idem("mk"),
		createOfferBody(schemeID, opens)), http.StatusCreated)
	return got["id"].(string)
}

func advance(t *testing.T, h *writeHarness, offerID string, role Role, to, reason string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"to": to}
	if reason != "" {
		body["reason"] = reason
	}
	return h.send(t, http.MethodPost, "/v1/admin/offers/"+offerID+"/transitions", h.operator(t, role), idem("adv-"+to), body)
}

// TestOpeningWaitsForBusinessTime is the guard judging against the scheme's calendar.
func TestOpeningWaitsForBusinessTime(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := createdOffer(t, h, schemeID, businessNow.Add(24*time.Hour))

	msg := expectCode(t, advance(t, h, offerID, RoleManager, "OPEN", ""), http.StatusConflict, CodePreconditionFailed)
	if !strings.Contains(msg, "opens at") {
		t.Errorf("the refusal should say when the offer opens, got %q", msg)
	}

	h.clock.at = businessNow.Add(25 * time.Hour)
	got := expect(t, advance(t, h, offerID, RoleManager, "OPEN", ""), http.StatusOK)
	if got["status"] != "OPEN" {
		t.Fatalf("status = %v, want OPEN", got["status"])
	}
}

// TestStatusesWithTheirOwnEndpointsAreRedirected stops advanceOffer skipping a ceremony step.
func TestStatusesWithTheirOwnEndpointsAreRedirected(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := createdOffer(t, h, schemeID, businessNow.Add(-time.Hour))

	for _, to := range []string{"BOOK_FROZEN", "SEED_COMMITTED", "SEED_REVEALED", "BALLOT_DRAWN", "SETTLED"} {
		msg := expectCode(t, advance(t, h, offerID, RoleManager, to, ""), http.StatusUnprocessableEntity, CodeValidationFailed)
		if !strings.Contains(msg, "POST /admin/offers/") {
			t.Errorf("advancing to %s should name the endpoint that does it, got %q", to, msg)
		}
	}
	expectCode(t, advance(t, h, offerID, RoleManager, "NOT_A_STATUS", ""), http.StatusUnprocessableEntity, CodeValidationFailed)
}

// TestAbortNeedsAReasonAndMayComeFromTheTrustee covers the widened boundary.
func TestAbortNeedsAReasonAndMayComeFromTheTrustee(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := createdOffer(t, h, schemeID, businessNow.Add(-time.Hour))

	expectCode(t, advance(t, h, offerID, RoleTrustee, "ABORTED", ""), http.StatusConflict, CodePreconditionFailed)
	expectCode(t, advance(t, h, offerID, RoleCompliance, "ABORTED", "compliance may not abort"), http.StatusForbidden, CodeForbidden)
	// The trustee cannot run the offer, only abandon it.
	expectCode(t, advance(t, h, offerID, RoleTrustee, "OPEN", ""), http.StatusForbidden, CodeForbidden)

	got := expect(t, advance(t, h, offerID, RoleTrustee, "ABORTED", "valuation withdrawn by the valuer"), http.StatusOK)
	if got["status"] != "ABORTED" {
		t.Fatalf("status = %v, want ABORTED", got["status"])
	}
	if n := h.count(t, `SELECT count(*) FROM admin_actions WHERE action = 'ABORT_OFFER' AND target_entity_id = $1`, offerID); n != 1 {
		t.Fatalf("%d abort audit rows, want 1", n)
	}
}

// TestReadinessAgreesWithTheAdvance is the property that makes readiness worth having.
func TestReadinessAgreesWithTheAdvance(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := createdOffer(t, h, schemeID, businessNow.Add(24*time.Hour))
	token := h.operator(t, RoleCompliance)

	early := expect(t, getAs(t, h.srv, "/v1/admin/offers/"+offerID+"/readiness", token), http.StatusOK)
	if early["canAdvance"] != false {
		t.Fatalf("before the opening time readiness says canAdvance=%v", early["canAdvance"])
	}
	if early["nextExpected"] != "OPEN" {
		t.Errorf("nextExpected = %v, want OPEN", early["nextExpected"])
	}
	blockers := early["blockers"].([]any)
	if len(blockers) == 0 || !strings.Contains(blockers[0].(string), "opens at") {
		t.Errorf("blockers = %v, want the opening-time rule in the domain's words", blockers)
	}

	h.clock.at = businessNow.Add(25 * time.Hour)
	ready := expect(t, getAs(t, h.srv, "/v1/admin/offers/"+offerID+"/readiness", token), http.StatusOK)
	if ready["canAdvance"] != true {
		t.Fatalf("after the opening time readiness says canAdvance=%v, blockers %v", ready["canAdvance"], ready["blockers"])
	}
	if b := ready["blockers"].([]any); len(b) != 0 {
		t.Errorf("blockers must be empty when canAdvance is true, got %v", b)
	}

	// And the advance it predicted succeeds.
	expect(t, advance(t, h, offerID, RoleManager, "OPEN", ""), http.StatusOK)
}

// openOffer creates and opens the fixture offer.
func openOffer(t *testing.T, h *writeHarness, schemeID string) string {
	t.Helper()
	offerID := createdOffer(t, h, schemeID, businessNow.Add(-time.Hour))
	expect(t, advance(t, h, offerID, RoleManager, "OPEN", ""), http.StatusOK)
	return offerID
}

func bidBody(units int, demat, bank string) map[string]any {
	return map[string]any{"unitsBid": units, "pricePerUnitPaise": 100000000, "dematAccountId": demat, "bankAccountId": bank}
}

// TestPlaceBidBlocksTheInvestorsFunds is the happy path of the investor write.
func TestPlaceBidBlocksTheInvestorsFunds(t *testing.T) {
	doc := loadContract(t)
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)
	investorID, demat, bank := seedBidder(t, h, schemeID)

	rec := h.send(t, http.MethodPost, "/v1/offers/"+offerID+"/bids", h.investor(t, investorID), idem("bid"), bidBody(2, demat, bank))
	got := expect(t, rec, http.StatusCreated)

	if got["status"] != "FUNDS_BLOCKED" {
		t.Errorf("status = %v, want FUNDS_BLOCKED", got["status"])
	}
	if int(got["unitsBid"].(float64)) != 2 || int64(got["totalAmountPaise"].(float64)) != 200000000 {
		t.Errorf("unitsBid/total = %v/%v, want 2 units for ₹20 lakh", got["unitsBid"], got["totalAmountPaise"])
	}
	block, ok := got["block"].(map[string]any)
	if !ok || block["status"] != "BLOCKED" {
		t.Fatalf("block = %v, want a BLOCKED funds block", got["block"])
	}
	if ref, _ := got["bidRef"].(string); len(ref) != 32 {
		t.Errorf("bidRef = %q, want 32 hex characters", ref)
	}

	// The bank holds the money; it is still the investor's.
	if held := h.bank.Held(bank); int64(held) != 200000000 {
		t.Errorf("the bank holds %d paise against the account, want 200000000", held)
	}

	assertConforms(t, doc, "Bid", rec.Body.Bytes())
}

// TestPlaceBidRefusesWhatShouldNeverReachTheBank covers each refusal before the block.
func TestPlaceBidRefusesWhatShouldNeverReachTheBank(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)
	investorID, demat, bank := seedBidder(t, h, schemeID)
	_, otherDemat, otherBank := seedBidder(t, h, schemeID)
	token := h.investor(t, investorID)
	path := "/v1/offers/" + offerID + "/bids"

	t.Run("somebody else's accounts", func(t *testing.T) {
		expectCode(t, h.send(t, http.MethodPost, path, token, idem("b1"), bidBody(2, otherDemat, otherBank)),
			http.StatusUnprocessableEntity, CodeValidationFailed)
	})
	t.Run("above the cap", func(t *testing.T) {
		msg := expectCode(t, h.send(t, http.MethodPost, path, token, idem("b2"), bidBody(26, demat, bank)),
			http.StatusUnprocessableEntity, CodeValidationFailed)
		if !strings.Contains(msg, "maximum") {
			t.Errorf("message = %q", msg)
		}
	})
	t.Run("outside the band", func(t *testing.T) {
		body := bidBody(2, demat, bank)
		body["pricePerUnitPaise"] = 200000000
		expectCode(t, h.send(t, http.MethodPost, path, token, idem("b3"), body), http.StatusUnprocessableEntity, CodeValidationFailed)
	})
	t.Run("an unknown field", func(t *testing.T) {
		body := bidBody(2, demat, bank)
		body["units"] = 2
		expectCode(t, h.send(t, http.MethodPost, path, token, idem("b4"), body), http.StatusBadRequest, CodeValidationFailed)
	})
	t.Run("no verified KYC", func(t *testing.T) {
		unverified, d, b := seedAPIInvestor(t, h.ctx, h.tx)
		expectCode(t, h.send(t, http.MethodPost, path, h.investor(t, unverified), idem("b5"), bidBody(2, d, b)),
			http.StatusConflict, CodePreconditionFailed)
	})
	t.Run("an operator token", func(t *testing.T) {
		expectCode(t, h.send(t, http.MethodPost, path, h.operator(t, RoleManager), idem("b6"), bidBody(2, demat, bank)),
			http.StatusForbidden, CodeForbidden)
	})

	if n := h.count(t, `SELECT count(*) FROM bids WHERE offer_id = $1`, offerID); n != 0 {
		t.Fatalf("%d bids were recorded by requests that should all have been refused", n)
	}
	if held := h.bank.Held(bank); held != 0 {
		t.Fatalf("the bank holds %d paise for bids that were refused", held)
	}
}

// TestOneBidPerInvestor is the book's distinct-bidder property.
func TestOneBidPerInvestor(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)
	investorID, demat, bank := seedBidder(t, h, schemeID)
	token := h.investor(t, investorID)
	path := "/v1/offers/" + offerID + "/bids"

	expect(t, h.send(t, http.MethodPost, path, token, idem("one"), bidBody(2, demat, bank)), http.StatusCreated)
	expectCode(t, h.send(t, http.MethodPost, path, token, idem("two"), bidBody(3, demat, bank)), http.StatusConflict, CodePreconditionFailed)

	if held := h.bank.Held(bank); int64(held) != 200000000 {
		t.Fatalf("the bank holds %d paise; the refused second bid must not have blocked more", held)
	}
}

// TestBidsAreRefusedOutsideTheWindow covers a closed or unopened offer.
func TestBidsAreRefusedOutsideTheWindow(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := createdOffer(t, h, schemeID, businessNow.Add(-time.Hour))
	investorID, demat, bank := seedBidder(t, h, schemeID)
	token := h.investor(t, investorID)
	path := "/v1/offers/" + offerID + "/bids"

	// CONFIGURED, not yet open.
	expectCode(t, h.send(t, http.MethodPost, path, token, idem("early"), bidBody(2, demat, bank)), http.StatusConflict, CodePreconditionFailed)

	expect(t, advance(t, h, offerID, RoleManager, "OPEN", ""), http.StatusOK)
	h.clock.at = businessNow.Add(80 * time.Hour) // past closesAt
	expectCode(t, h.send(t, http.MethodPost, path, token, idem("late"), bidBody(2, demat, bank)), http.StatusConflict, CodePreconditionFailed)
}

// TestClosingAdmitsFundedBidsToTheBook joins placeBid to the lifecycle.
func TestClosingAdmitsFundedBidsToTheBook(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)

	for i := 0; i < 3; i++ {
		investorID, demat, bank := seedBidder(t, h, schemeID)
		expect(t, h.send(t, http.MethodPost, "/v1/offers/"+offerID+"/bids", h.investor(t, investorID), idem("c"), bidBody(2, demat, bank)), http.StatusCreated)
	}

	expect(t, advance(t, h, offerID, RoleManager, "CLOSED", ""), http.StatusOK)

	if n := h.count(t, `SELECT count(*) FROM bids WHERE offer_id = $1 AND status = 'IN_BOOK'`, offerID); n != 3 {
		t.Fatalf("%d bids are in the book after closing, want 3", n)
	}
}

// TestClosingWithNoBidsIsRefused is the guard's BidCount rule through the API.
func TestClosingWithNoBidsIsRefused(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)

	expectCode(t, advance(t, h, offerID, RoleManager, "CLOSED", ""), http.StatusConflict, CodePreconditionFailed)
	if s := h.offerStatus(t, offerID); s != "OPEN" {
		t.Fatalf("a refused close left the offer %s", s)
	}
}

// TestAbortReleasesBlockedFunds is the investor-protection half of an abort.
func TestAbortReleasesBlockedFunds(t *testing.T) {
	h := newWriteHarness(t)
	schemeID := seedAPIScheme(t, h.ctx, h.tx)
	offerID := openOffer(t, h, schemeID)
	investorID, demat, bank := seedBidder(t, h, schemeID)
	expect(t, h.send(t, http.MethodPost, "/v1/offers/"+offerID+"/bids", h.investor(t, investorID), idem("a"), bidBody(2, demat, bank)), http.StatusCreated)

	expect(t, advance(t, h, offerID, RoleTrustee, "ABORTED", "the SPV acquisition fell through"), http.StatusOK)

	if held := h.bank.Held(bank); held != 0 {
		t.Fatalf("the bank still holds %d paise after the offer was abandoned", held)
	}
	if n := h.count(t, `SELECT count(*) FROM bids WHERE offer_id = $1 AND status = 'FUNDS_UNBLOCKED'`, offerID); n != 1 {
		t.Fatalf("%d bids released, want 1", n)
	}
}

// TestWriteRoutesAreAbsentWithoutTheirDependencies mirrors the read-side gating.
func TestWriteRoutesAreAbsentWithoutTheirDependencies(t *testing.T) {
	srv := New(Deps{Env: "LOCAL", Wall: fixedClock{at: tokenNow}, SessionSecret: testSecret})
	h := &writeHarness{srv: srv}

	rec := h.send(t, http.MethodPost, "/v1/admin/offers", tokenFor(t, srv, operatorClaims(RoleManager)), idem("x"), `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no database configured", rec.Code)
	}
}
