package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/store"
)

// meServer builds a server with authentication and the investor surface over a rolled-back transaction.
func meServer(t *testing.T) (*Server, context.Context, pgx.Tx) {
	t.Helper()

	ctx := context.Background()
	tx, err := apiPool(t).Begin(ctx)
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	srv := New(Deps{
		Env:           config.EnvLocal,
		Wall:          fixedClock{at: tokenNow},
		SessionSecret: testSecret,
		Schemes:       store.NewSchemes(tx),
		Offers:        store.NewOffers(tx),
		Periods:       store.NewPeriods(tx),
		Me:            store.NewMe(tx),
	})
	return srv, ctx, tx
}

// investorToken mints a session token for a seeded investor.
func investorToken(t *testing.T, s *Server, investorID string) string {
	t.Helper()
	return tokenFor(t, s, Claims{
		Subject:    "web3auth|" + investorID,
		Kind:       PrincipalInvestor,
		InvestorID: investorID,
		IssuedAt:   tokenNow.Unix(),
		ExpiresAt:  tokenNow.Add(30 * time.Minute).Unix(),
	})
}

// getAs issues an authenticated GET.
func getAs(t *testing.T, s *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// TestMeEndpointsRequireAToken is the first thing to be sure of.
func TestMeEndpointsRequireAToken(t *testing.T) {
	srv, _, _ := meServer(t)

	for _, path := range []string{"/v1/me", "/v1/me/holdings", "/v1/me/entitlements", "/v1/me/payouts", "/v1/me/bids"} {
		t.Run(path, func(t *testing.T) {
			rec := getAs(t, srv, path, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401. body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestMeEndpointsRefuseAnOperatorToken keeps the audience boundary.
func TestMeEndpointsRefuseAnOperatorToken(t *testing.T) {
	srv, _, _ := meServer(t)
	token := tokenFor(t, srv, operatorClaims(RoleManager))

	for _, path := range []string{"/v1/me", "/v1/me/holdings", "/v1/me/entitlements", "/v1/me/payouts", "/v1/me/bids"} {
		t.Run(path, func(t *testing.T) {
			rec := getAs(t, srv, path, token)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403. body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestGetMeServesTheCallersOwnProfile covers the happy path and the masking rules.
func TestGetMeServesTheCallersOwnProfile(t *testing.T) {
	srv, ctx, tx := meServer(t)
	investorID, dematID, _ := seedAPIInvestor(t, ctx, tx)
	seedAPIWallet(t, ctx, tx, investorID, "0xaaaa000000000000000000000000000000000001", true)
	seedAPIKYC(t, ctx, tx, investorID, "VERIFIED")
	_ = dematID

	rec := getAs(t, srv, "/v1/me", investorToken(t, srv, investorID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	got := decodeBody(t, rec)

	if got["investorId"] != investorID {
		t.Errorf("investorId = %v, want %q", got["investorId"], investorID)
	}
	if got["investorClass"] != "RESIDENT_IND" {
		t.Errorf("investorClass = %v", got["investorClass"])
	}
	if got["kycStatus"] != "VERIFIED" {
		t.Errorf("kycStatus = %v, want VERIFIED", got["kycStatus"])
	}
	if got["walletAddress"] != "0xaaaa000000000000000000000000000000000001" {
		t.Errorf("walletAddress = %v", got["walletAddress"])
	}

	// The PAN is encrypted and this service holds no key, so the honest answer is null rather than a
	// fabricated mask.
	if v, present := got["panMasked"]; !present || v != nil {
		t.Errorf("panMasked = %v, want null; the column is ciphertext and there is no decryption path", v)
	}

	demats, ok := got["dematAccounts"].([]any)
	if !ok || len(demats) == 0 {
		t.Fatalf("dematAccounts = %v, want the seeded account", got["dematAccounts"])
	}
	first := demats[0].(map[string]any)
	masked, _ := first["maskedClientId"].(string)
	if !strings.HasPrefix(masked, "****") {
		t.Errorf("maskedClientId = %q, want a masked value", masked)
	}
	if len(masked) > 8 {
		t.Errorf("maskedClientId = %q reveals too much", masked)
	}

	// A credentialled response must not be cached.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// TestKycDefaultsToPendingWithNoRecord covers the coalesce.
//
// An investor with no KYC record must read as PENDING, not as an empty string. An empty status is not a value
// any client can branch on, and defaulting to VERIFIED would be catastrophic.
func TestKycDefaultsToPendingWithNoRecord(t *testing.T) {
	srv, ctx, tx := meServer(t)
	investorID, _, _ := seedAPIInvestor(t, ctx, tx)

	got := decodeBody(t, getAs(t, srv, "/v1/me", investorToken(t, srv, investorID)))

	if got["kycStatus"] != "PENDING" {
		t.Fatalf("kycStatus = %v, want PENDING with no record on file", got["kycStatus"])
	}
}

// TestMostRecentKycWins is why the subquery orders.
func TestMostRecentKycWins(t *testing.T) {
	srv, ctx, tx := meServer(t)
	investorID, _, _ := seedAPIInvestor(t, ctx, tx)

	seedAPIKYCAt(t, ctx, tx, investorID, "REJECTED", tokenNow.Add(-48*time.Hour))
	seedAPIKYCAt(t, ctx, tx, investorID, "VERIFIED", tokenNow.Add(-time.Hour))

	got := decodeBody(t, getAs(t, srv, "/v1/me", investorToken(t, srv, investorID)))

	if got["kycStatus"] != "VERIFIED" {
		t.Fatalf("kycStatus = %v, want the most recent record", got["kycStatus"])
	}
}

// TestAnInvestorWithNoWalletReadsAsNull covers the nullable address.
func TestAnInvestorWithNoWalletReadsAsNull(t *testing.T) {
	srv, ctx, tx := meServer(t)
	investorID, _, _ := seedAPIInvestor(t, ctx, tx)

	got := decodeBody(t, getAs(t, srv, "/v1/me", investorToken(t, srv, investorID)))

	v, present := got["walletAddress"]
	if !present {
		t.Fatal("walletAddress should be present and null")
	}
	if v != nil {
		t.Fatalf("walletAddress = %v, want null", v)
	}
}

// TestARotatedWalletIsNotShownAsCurrent keeps the profile truthful.
func TestARotatedWalletIsNotShownAsCurrent(t *testing.T) {
	srv, ctx, tx := meServer(t)
	investorID, _, _ := seedAPIInvestor(t, ctx, tx)
	seedAPIWallet(t, ctx, tx, investorID, "0xbbbb000000000000000000000000000000000001", false)
	seedAPIWallet(t, ctx, tx, investorID, "0xcccc000000000000000000000000000000000001", true)

	got := decodeBody(t, getAs(t, srv, "/v1/me", investorToken(t, srv, investorID)))

	if got["walletAddress"] != "0xcccc000000000000000000000000000000000001" {
		t.Fatalf("walletAddress = %v, want the active wallet", got["walletAddress"])
	}
}

// TestHoldingsAreScopedToTheCaller is the test this whole surface exists to pass.
//
// Two investors, each holding units in the same scheme. Neither may see the other's position. This is the
// failure that matters most on an investor API, and it is why every query filters in SQL on the id from the
// token rather than on anything a caller supplies.
func TestHoldingsAreScopedToTheCaller(t *testing.T) {
	srv, ctx, tx := meServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)

	alice, _, _ := seedAPIInvestor(t, ctx, tx)
	bob, _, _ := seedAPIInvestor(t, ctx, tx)

	seedAPIHolding(t, ctx, tx, schemeID, alice, "0xa11ce00000000000000000000000000000000001", 7, false)
	seedAPIHolding(t, ctx, tx, schemeID, bob, "0xb0b0000000000000000000000000000000000001", 13, false)

	aliceBody := decodeBody(t, getAs(t, srv, "/v1/me/holdings", investorToken(t, srv, alice)))
	items, ok := aliceBody["items"].([]any)
	if !ok {
		t.Fatalf("items is %T", aliceBody["items"])
	}
	if len(items) != 1 {
		t.Fatalf("alice sees %d holdings, want exactly her own", len(items))
	}

	holding := items[0].(map[string]any)
	if int64(holding["units"].(float64)) != 7 {
		t.Errorf("units = %v, want alice's 7", holding["units"])
	}
	if holding["walletAddress"] != "0xa11ce00000000000000000000000000000000001" {
		t.Errorf("walletAddress = %v, not alice's", holding["walletAddress"])
	}

	// Bob's wallet and unit count must appear nowhere in alice's response.
	raw := getAs(t, srv, "/v1/me/holdings", investorToken(t, srv, alice)).Body.String()
	if strings.Contains(raw, "0xb0b0") {
		t.Fatalf("bob's wallet leaked into alice's holdings: %s", raw)
	}
	if strings.Contains(raw, bob) {
		t.Fatalf("bob's investor id leaked into alice's holdings: %s", raw)
	}
}

// TestEveryMeEndpointIsScopedToTheCaller widens that check across the surface.
//
// One forgotten WHERE clause on one endpoint is the whole failure, so each is checked rather than trusting that
// the pattern was followed.
func TestEveryMeEndpointIsScopedToTheCaller(t *testing.T) {
	srv, ctx, tx := meServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)

	alice, _, _ := seedAPIInvestor(t, ctx, tx)
	bob, bobDemat, bobBank := seedAPIInvestor(t, ctx, tx)

	// Give bob a full set of records and alice nothing.
	seedAPIHolding(t, ctx, tx, schemeID, bob, "0xb0b0000000000000000000000000000000000002", 13, false)
	seedAPIBidFor(t, ctx, tx, offerID, bob, bobDemat, bobBank, 3)

	aliceToken := investorToken(t, srv, alice)

	for _, path := range []string{"/v1/me/holdings", "/v1/me/entitlements", "/v1/me/payouts", "/v1/me/bids"} {
		t.Run(path, func(t *testing.T) {
			rec := getAs(t, srv, path, aliceToken)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
			}

			body := decodeBody(t, rec)
			items, ok := body["items"].([]any)
			if !ok {
				t.Fatalf("items is %T", body["items"])
			}
			if len(items) != 0 {
				t.Fatalf("alice has no records, yet %s returned %d: %s", path, len(items), rec.Body.String())
			}

			raw := rec.Body.String()
			if strings.Contains(raw, bob) || strings.Contains(raw, "0xb0b0") {
				t.Fatalf("%s leaked bob's data to alice: %s", path, raw)
			}
		})
	}
}

// TestEmptyMeCollectionsAreArraysNotNull stops a client faulting.
func TestEmptyMeCollectionsAreArraysNotNull(t *testing.T) {
	srv, ctx, tx := meServer(t)
	investorID, _, _ := seedAPIInvestor(t, ctx, tx)
	token := investorToken(t, srv, investorID)

	for _, path := range []string{"/v1/me/holdings", "/v1/me/entitlements", "/v1/me/payouts", "/v1/me/bids"} {
		t.Run(path, func(t *testing.T) {
			rec := getAs(t, srv, path, token)
			if strings.Contains(rec.Body.String(), `"items":null`) {
				t.Fatalf("an empty collection is null rather than []: %s", rec.Body.String())
			}
		})
	}

	// And on /me itself, the account arrays.
	body := getAs(t, srv, "/v1/me", token).Body.String()
	if strings.Contains(body, `"bankAccounts":null`) {
		t.Errorf("bankAccounts is null rather than []: %s", body)
	}
}

// TestHoldingsCarryTheManagerExclusionFlag covers a distinction that is easy to get backwards.
//
// The manager is excluded from the statutory holder count, not from the distribution: the entitlement
// denominator is all units. Reporting the flag lets a reader see which it is.
func TestHoldingsCarryTheManagerExclusionFlag(t *testing.T) {
	srv, ctx, tx := meServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	manager, _, _ := seedAPIInvestor(t, ctx, tx)
	seedAPIHolding(t, ctx, tx, schemeID, manager, "0xd0d0000000000000000000000000000000000001", 25, true)

	body := decodeBody(t, getAs(t, srv, "/v1/me/holdings", investorToken(t, srv, manager)))
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("got %d holdings", len(items))
	}

	holding := items[0].(map[string]any)
	if holding["excludedFromHolderCount"] != true {
		t.Errorf("excludedFromHolderCount = %v, want true for the manager", holding["excludedFromHolderCount"])
	}
	if int64(holding["units"].(float64)) != 25 {
		t.Errorf("units = %v, want the manager's 25", holding["units"])
	}
}

// TestBidsShowTheirFundsBlock joins the two tables an investor cares about together.
func TestBidsShowTheirFundsBlock(t *testing.T) {
	srv, ctx, tx := meServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)
	investorID, demat, bank := seedAPIInvestor(t, ctx, tx)

	bidID := seedAPIBidFor(t, ctx, tx, offerID, investorID, demat, bank, 4)
	seedAPIBlock(t, ctx, tx, bidID, 4*100000000, "BLOCKED")

	body := decodeBody(t, getAs(t, srv, "/v1/me/bids", investorToken(t, srv, investorID)))
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("got %d bids", len(items))
	}

	bid := items[0].(map[string]any)
	if int64(bid["unitsBid"].(float64)) != 4 {
		t.Errorf("unitsBid = %v, want 4", bid["unitsBid"])
	}
	block, ok := bid["block"].(map[string]any)
	if !ok {
		t.Fatalf("block = %v, want the nested AsbaBlock the contract publishes", bid["block"])
	}
	if block["status"] != "BLOCKED" {
		t.Errorf("block.status = %v, want BLOCKED", block["status"])
	}
	if int64(block["blockedAmountPaise"].(float64)) != 400000000 {
		t.Errorf("block.blockedAmountPaise = %v, want 4 units at ₹10 lakh", block["blockedAmountPaise"])
	}
	// The reference is the investor's own receipt, and the bid book is ordered by it.
	ref, _ := bid["bidRef"].(string)
	if len(ref) != 32 {
		t.Errorf("bidRef = %q, want 32 hex characters", ref)
	}
}

// TestABidWithNoBlockReadsAsNull covers the left join.
func TestABidWithNoBlockReadsAsNull(t *testing.T) {
	srv, ctx, tx := meServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)
	investorID, demat, bank := seedAPIInvestor(t, ctx, tx)
	seedAPIBidFor(t, ctx, tx, offerID, investorID, demat, bank, 2)

	body := decodeBody(t, getAs(t, srv, "/v1/me/bids", investorToken(t, srv, investorID)))
	bid := body["items"].([]any)[0].(map[string]any)

	if _, present := bid["block"]; present {
		t.Errorf("block = %v, want it absent with no block requested", bid["block"])
	}
}

// TestUnpayableSmallAmountsAreFlagged is the paise-level honesty check.
//
// An amount between 1 and 99 paise is storable and unpayable. Reporting it as payable would produce an
// instruction the provider refuses, and the holder would see a failure instead of a carry-forward.
func TestUnpayableSmallAmountsAreFlagged(t *testing.T) {
	cases := []struct {
		net  int64
		want bool
	}{
		{0, false},
		{1, false},
		{99, false},
		{100, true}, // exactly one rupee is payable
		{101, true},
		{27550000000, true},
	}

	for _, tc := range cases {
		got := entitlementToWire(store.Entitlement{NetPayablePaise: paise(tc.net)})
		if got.Payable != tc.want {
			t.Errorf("%d paise: payable = %v, want %v", tc.net, got.Payable, tc.want)
		}
	}
}

// TestPayoutAlwaysDeclaresWhetherItIsSimulated is the project's central honesty commitment.
//
// The distribution arithmetic and the on-chain record are real. The final fiat hop is simulated while corporate
// banking KYC is outstanding, and every payout says so rather than leaving it to a footnote somebody may not
// read.
func TestPayoutAlwaysDeclaresWhetherItIsSimulated(t *testing.T) {
	mock := payoutToWire(store.Payout{Provider: "MOCK"})
	if !mock.Simulated {
		t.Error("a MOCK payout must be marked simulated")
	}

	real := payoutToWire(store.Payout{Provider: "RAZORPAYX_SANDBOX"})
	if real.Simulated {
		t.Error("a provider payout must not be marked simulated")
	}

	// The field is never omitted, so a client cannot read its absence as false by accident.
	encoded, err := json.Marshal(mock)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"simulated":true`) {
		t.Errorf("simulated is not serialised: %s", encoded)
	}
}

// TestPayoutStatusIsOursNotTheProviders keeps a third party's vocabulary out of the contract.
func TestPayoutStatusIsOursNotTheProviders(t *testing.T) {
	got := payoutToWire(store.Payout{Status: "SETTLED"})

	if got.Status != "SETTLED" {
		t.Fatalf("status = %q", got.Status)
	}
	if got.Status == strings.ToLower(got.Status) {
		t.Errorf("status %q looks like the provider's lowercase vocabulary", got.Status)
	}
}

// paise is a tiny helper so the table above reads as amounts.
func paise(n int64) money.Paise { return money.Paise(n) }

// --- fixtures ---

func seedAPIWallet(t *testing.T, ctx context.Context, q store.Querier, investorID, address string, active bool) {
	t.Helper()

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO wallets (investor_id, address, provider, is_active, deactivated_at, rotation_reason)
		VALUES ($1, $2, 'WEB3AUTH', $3,
			CASE WHEN $3 THEN NULL ELSE now() END,
			CASE WHEN $3 THEN NULL ELSE 'rotated in a test' END)
		RETURNING id`, investorID, address, active).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a wallet: %v", err)
	}
}

func seedAPIKYC(t *testing.T, ctx context.Context, q store.Querier, investorID, status string) {
	t.Helper()
	seedAPIKYCAt(t, ctx, q, investorID, status, tokenNow.Add(-time.Hour))
}

// seedAPIKYCAt inserts a KYC record at a given time.
//
// kyc_verified_consistency requires verified_at and expires_at when the status is VERIFIED, so both are
// supplied for that case.
func seedAPIKYCAt(t *testing.T, ctx context.Context, q store.Querier, investorID, status string, at time.Time) {
	t.Helper()

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO kyc_records (investor_id, provider, kyc_ref, status, verified_at, expires_at, created_at)
		VALUES ($1, 'MOCK', $2, $3::kyc_status,
			CASE WHEN $3 = 'VERIFIED' THEN $4::timestamptz ELSE NULL END,
			CASE WHEN $3 = 'VERIFIED' THEN $4::timestamptz + interval '1 year' ELSE NULL END,
			$4::timestamptz)
		RETURNING id`, investorID, apiUniq("KYC"), status, at).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a kyc record: %v", err)
	}
}

func seedAPIHolding(t *testing.T, ctx context.Context, q store.Querier, schemeID, investorID, wallet string, units int32, excluded bool) {
	t.Helper()

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO unit_holdings (scheme_id, investor_id, wallet_address, units, is_excluded_from_holder_count)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`, schemeID, investorID, wallet, units, excluded).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a holding: %v", err)
	}
}

// seedAPIBidFor inserts a bid for a specific investor, reusing their accounts.
func seedAPIBidFor(t *testing.T, ctx context.Context, q store.Querier, offerID, investorID, dematID, bankID string, units int32) string {
	t.Helper()

	price := int64(100000000)
	sum := sha256.Sum256([]byte(apiUniq("bidref")))
	bidRef := fmt.Sprintf("%x", sum)[:32]

	var bidID string
	err := q.QueryRow(ctx, `
		INSERT INTO bids (
			offer_id, investor_id, investor_anchor_hash, demat_account_id, bank_account_id,
			units_bid, price_per_unit_paise, total_amount_paise, bid_reference, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		offerID, investorID, apiIdem("anchor"), dematID, bankID,
		units, price, int64(units)*price, bidRef, apiIdem("bid")).Scan(&bidID)
	if err != nil {
		t.Fatalf("seeding a bid: %v", err)
	}
	return bidID
}

func seedAPIBlock(t *testing.T, ctx context.Context, q store.Querier, bidID string, amount int64, status string) {
	t.Helper()

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO asba_blocks (
			bid_id, provider, requested_amount_paise, blocked_amount_paise,
			block_status, blocked_at, idempotency_key
		) VALUES ($1, 'RAZORPAYX_SANDBOX', $2, $2, $3::asba_block_status, now(), $4)
		RETURNING id`, bidID, amount, status, apiIdem("asba")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding an asba block: %v", err)
	}
}
