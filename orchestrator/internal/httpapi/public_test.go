package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/ndcf"
	"github.com/acresync/orchestrator/internal/store"
)

// These tests exercise the whole stack: the real router, the real middleware, the real stores and a real
// Postgres. Everything below the HTTP boundary is genuine.
//
// The point is the seams. A handler can be correct and a store can be correct while the conversion between
// them drops a field, names it differently from the contract, or serialises a date as a timestamp. None of
// those is visible from either side alone.

func apiPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("ACRESYNC_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("ACRESYNC_DATABASE_URL")
	}
	if url == "" {
		t.Skip("no ACRESYNC_DATABASE_URL; skipping API integration tests")
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Skipf("could not connect to Postgres: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("Postgres not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// apiServer builds a server over a rolled-back transaction.
//
// The stores take a Querier, so the whole API can be driven against a transaction that never commits. That
// is what makes it safe to seed a full scheme, offer and register for every test.
func apiServer(t *testing.T) (*Server, context.Context, pgx.Tx) {
	t.Helper()

	ctx := context.Background()
	tx, err := apiPool(t).Begin(ctx)
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	srv := New(Deps{
		Env:     config.EnvLocal,
		Schemes: store.NewSchemes(tx),
		Offers:  store.NewOffers(tx),
		Periods: store.NewPeriods(tx),
	})
	return srv, ctx, tx
}

// get issues a request against the server.
func get(t *testing.T, s *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// decodeBody unmarshals a response into a generic map, which is how a client without Go types sees it.
//
// Deliberately not decoding into the wire structs: that would only prove the structs round-trip through
// themselves. Reading the JSON as data is what catches a field named differently from the contract.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the response is not a JSON object: %v\nbody: %s", err, body)
	}
	return out
}

// TestGetSchemeServesTheContractsFieldNames is the seam between the store and the contract.
func TestGetSchemeServesTheContractsFieldNames(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	id := seedAPIScheme(t, ctx, tx)

	rec := get(t, srv, "/v1/schemes/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	got := decodeBody(t, rec)

	// Every field the contract marks required must be present under exactly that name.
	for _, field := range []string{
		"id", "sebiSchemeRef", "name", "unitPricePaise", "totalUnits",
		"imUnits", "publicUnits", "minPublicHolders", "distributionFloorBps", "environmentTag",
	} {
		if _, ok := got[field]; !ok {
			t.Errorf("the contract requires %q, which is absent. got keys: %v", field, keysOf(got))
		}
	}

	// Money is an integer number of paise, as a JSON number.
	if v, ok := got["unitPricePaise"].(float64); !ok || int64(v) != 100000000 {
		t.Errorf("unitPricePaise = %v, want the integer 100000000 (₹10 lakh in paise)", got["unitPricePaise"])
	}
	if v, ok := got["assetValuePaise"].(float64); !ok || int64(v) != 50000000000 {
		t.Errorf("assetValuePaise = %v, want 50000000000 (₹50 crore)", got["assetValuePaise"])
	}

	// The floor is the constant the distribution maths applies, not a stored copy.
	if v, ok := got["distributionFloorBps"].(float64); !ok || int(v) != int(ndcf.FloorBps) {
		t.Errorf("distributionFloorBps = %v, want %d from ndcf.FloorBps", got["distributionFloorBps"], ndcf.FloorBps)
	}

	// The cap table.
	if int64(got["totalUnits"].(float64)) != 500 ||
		int64(got["imUnits"].(float64)) != 25 ||
		int64(got["publicUnits"].(float64)) != 475 {
		t.Errorf("cap table = %v/%v/%v, want 500/25/475",
			got["totalUnits"], got["imUnits"], got["publicUnits"])
	}
	if int64(got["minPublicHolders"].(float64)) != 200 {
		t.Errorf("minPublicHolders = %v, want the statutory 200", got["minPublicHolders"])
	}
}

// TestDeployedSchemeServesLowercaseAddresses covers the address spelling rule.
//
// One address must have exactly one spelling everywhere. A client comparing an address it read from the chain
// against one from this API should not have to normalise case first.
func TestDeployedSchemeServesLowercaseAddresses(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	id := seedAPIScheme(t, ctx, tx)

	got := decodeBody(t, get(t, srv, "/v1/schemes/"+id, nil))

	contracts, ok := got["contracts"].(map[string]any)
	if !ok {
		t.Fatalf("a deployed scheme must carry a contracts block, got %v", got["contracts"])
	}

	for _, key := range []string{"roles", "ballot", "scheme"} {
		addr, ok := contracts[key].(string)
		if !ok {
			t.Errorf("contracts.%s is missing", key)
			continue
		}
		if !strings.HasPrefix(addr, "0x") {
			t.Errorf("contracts.%s = %q, want a 0x prefix", key, addr)
		}
		if addr != strings.ToLower(addr) {
			t.Errorf("contracts.%s = %q, want lowercase so one address has one spelling", key, addr)
		}
		if len(addr) != 42 {
			t.Errorf("contracts.%s = %q, want 42 characters", key, addr)
		}
	}
	if int64(contracts["chainId"].(float64)) != 11155111 {
		t.Errorf("chainId = %v, want Sepolia's 11155111", contracts["chainId"])
	}
}

// TestUndeployedSchemeOmitsTheContractsBlock is why that field is a pointer.
//
// A block of empty addresses would read as a scheme deployed at address zero, which is a false statement.
func TestUndeployedSchemeOmitsTheContractsBlock(t *testing.T) {
	srv, ctx, tx := apiServer(t)

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO schemes (sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
			total_units, im_units, public_units)
		VALUES ($1, 'Undeployed', 50000000000, 100000000, 500, 25, 475) RETURNING id`,
		apiUniq("SEBI/SM-REIT/API-UNDEPLOYED")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	got := decodeBody(t, get(t, srv, "/v1/schemes/"+id, nil))

	if _, present := got["contracts"]; present {
		t.Errorf("an undeployed scheme must omit contracts entirely, got %v", got["contracts"])
	}
}

// TestUnknownSchemeIs404 checks the store sentinel reaches the right status.
//
// store.ErrNotFound is mapped in the classification table rather than translated per handler, so this also
// proves a handler that simply returns the store's error produces the correct response.
func TestUnknownSchemeIs404(t *testing.T) {
	srv, _, _ := apiServer(t)

	rec := get(t, srv, "/v1/schemes/00000000-0000-0000-0000-000000000000", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body: %s", rec.Code, rec.Body.String())
	}
	env := decodeError(t, rec.Result())
	if env.Error.Code != CodeNotFound {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeNotFound)
	}

	// The message must name what was missing. store.ErrNotFound reads "store: no such row", which is true
	// and useless: it names an internal layer and tells the caller nothing about their request.
	if !strings.Contains(env.Error.Message, "scheme") {
		t.Errorf("message = %q, want it to name the resource", env.Error.Message)
	}
	if strings.Contains(env.Error.Message, "store:") {
		t.Errorf("message = %q, which leaks an internal layer name", env.Error.Message)
	}
}

// TestNotFoundNamesTheResource covers each entity, so one handler forgetting is visible.
func TestNotFoundNamesTheResource(t *testing.T) {
	srv, _, _ := apiServer(t)
	const missing = "00000000-0000-0000-0000-000000000000"

	cases := []struct{ path, noun string }{
		{"/v1/schemes/" + missing, "scheme"},
		{"/v1/schemes/" + missing + "/offers", "scheme"},
		{"/v1/schemes/" + missing + "/periods", "scheme"},
		{"/v1/offers/" + missing, "offer"},
		{"/v1/periods/" + missing, "period"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := get(t, srv, tc.path, nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404. body: %s", rec.Code, rec.Body.String())
			}

			env := decodeError(t, rec.Result())
			if !strings.Contains(env.Error.Message, tc.noun) {
				t.Errorf("message = %q, want it to name %q", env.Error.Message, tc.noun)
			}
			if strings.Contains(env.Error.Message, "store:") {
				t.Errorf("message = %q leaks an internal layer name", env.Error.Message)
			}
		})
	}
}

// TestMalformedSchemeIDIs400 distinguishes a mistyped id from a missing one.
//
// A driver rejecting the uuid cast would be a 500, which tells a caller nothing about their own typo.
func TestMalformedSchemeIDIs400(t *testing.T) {
	srv, _, _ := apiServer(t)

	for _, bad := range []string{"not-a-uuid", "12345", "00000000-0000-0000-0000-00000000000", "zzzzzzzz-0000-0000-0000-000000000000"} {
		t.Run(bad, func(t *testing.T) {
			rec := get(t, srv, "/v1/schemes/"+bad, nil)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %q. body: %s", rec.Code, bad, rec.Body.String())
			}
			env := decodeError(t, rec.Result())
			if env.Error.Code != CodeValidationFailed {
				t.Errorf("code = %q, want %q", env.Error.Code, CodeValidationFailed)
			}
			if !strings.Contains(env.Error.Message, "schemeId") {
				t.Errorf("the message should name the parameter, got %q", env.Error.Message)
			}
		})
	}
}

// TestSchemeListIsAnObjectNotABareArray is a shape decision worth pinning.
//
// A top-level array cannot gain a field, so paging could never be added without breaking every client.
func TestSchemeListIsAnObjectNotABareArray(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	seedAPIScheme(t, ctx, tx)

	rec := get(t, srv, "/v1/schemes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	got := decodeBody(t, rec)
	items, ok := got["items"].([]any)
	if !ok {
		t.Fatalf("want an items array, got %T", got["items"])
	}
	if len(items) == 0 {
		t.Fatal("the seeded scheme is missing from the list")
	}
}

// TestEmptyListIsAnArrayNotNull stops a client faulting on null.
func TestEmptyListIsAnArrayNotNull(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	id := seedAPIScheme(t, ctx, tx)
	_ = ctx

	// A scheme with no offers.
	rec := get(t, srv, "/v1/schemes/"+id+"/offers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, `"items":null`) {
		t.Fatalf("an empty collection must be [] not null: %s", body)
	}

	got := decodeBody(t, rec)
	items, ok := got["items"].([]any)
	if !ok {
		t.Fatalf("items is %T, want an array", got["items"])
	}
	if len(items) != 0 {
		t.Fatalf("want an empty array, got %d entries", len(items))
	}
}

// TestOffersUnderAnUnknownSchemeIs404 is a distinction that matters.
//
// An empty list would assert "this scheme has no offers", which is a claim about a scheme that does not
// exist.
func TestOffersUnderAnUnknownSchemeIs404(t *testing.T) {
	srv, _, _ := apiServer(t)

	rec := get(t, srv, "/v1/schemes/00000000-0000-0000-0000-000000000000/offers", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, not an empty list. body: %s", rec.Code, rec.Body.String())
	}
}

// TestGetOfferServesTermsAndSubscription covers the offer detail view.
func TestGetOfferServesTermsAndSubscription(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)
	seedAPIBid(t, ctx, tx, offerID, 2, "BLOCKED")
	seedAPIBid(t, ctx, tx, offerID, 3, "REQUESTED")

	rec := get(t, srv, "/v1/offers/"+offerID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	got := decodeBody(t, rec)

	terms, ok := got["terms"].(map[string]any)
	if !ok {
		t.Fatalf("terms is required, got %v", got["terms"])
	}
	for _, field := range []string{
		"unitsOnOffer", "minBidUnits", "maxBidUnits", "minSubscriptionUnits",
		"minDistinctHolders", "priceBandLowerPaise", "priceBandUpperPaise",
		"opensAt", "closesAt", "allotmentDueAt",
	} {
		if _, present := terms[field]; !present {
			t.Errorf("terms.%s is required by the contract and absent", field)
		}
	}

	sub, ok := got["subscription"].(map[string]any)
	if !ok {
		t.Fatalf("an offer with bids must carry a subscription, got %v", got["subscription"])
	}
	if int64(sub["bidCount"].(float64)) != 2 {
		t.Errorf("bidCount = %v, want 2", sub["bidCount"])
	}
	if int64(sub["unitsBid"].(float64)) != 5 {
		t.Errorf("unitsBid = %v, want 5", sub["unitsBid"])
	}
	if int64(sub["fundsBlockedCount"].(float64)) != 1 {
		t.Errorf("fundsBlockedCount = %v, want 1; only the BLOCKED bid is funded", sub["fundsBlockedCount"])
	}
	// The ratio is exact, published as two integers.
	if int64(sub["oversubscriptionNumerator"].(float64)) != 5 ||
		int64(sub["oversubscriptionDenominator"].(float64)) != 475 {
		t.Errorf("oversubscription = %v/%v, want 5/475",
			sub["oversubscriptionNumerator"], sub["oversubscriptionDenominator"])
	}
}

// TestOfferWithNoBidsOmitsSubscription is the contract's "present once bidding has begun".
//
// A zeroed block would read as "nobody bid", which is a claim about demand rather than about the offer not
// having opened.
func TestOfferWithNoBidsOmitsSubscription(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)

	got := decodeBody(t, get(t, srv, "/v1/offers/"+offerID, nil))

	if _, present := got["subscription"]; present {
		t.Errorf("an offer with no bids must omit subscription, got %v", got["subscription"])
	}
}

// TestTimestampsAreRFC3339UTC pins the time format the contract requires.
func TestTimestampsAreRFC3339UTC(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)

	got := decodeBody(t, get(t, srv, "/v1/offers/"+offerID, nil))
	terms := got["terms"].(map[string]any)

	for _, field := range []string{"opensAt", "closesAt", "allotmentDueAt"} {
		v, ok := terms[field].(string)
		if !ok {
			t.Errorf("terms.%s is not a string", field)
			continue
		}
		if !strings.HasSuffix(v, "Z") {
			t.Errorf("terms.%s = %q, want UTC with a Z suffix; an offset is not what the contract permits", field, v)
		}
		if strings.Contains(v, "+") {
			t.Errorf("terms.%s = %q carries a timezone offset", field, v)
		}
	}

	if createdAt, ok := got["createdAt"].(string); ok && !strings.HasSuffix(createdAt, "Z") {
		t.Errorf("createdAt = %q, want a Z suffix", createdAt)
	}
}

// TestPeriodDatesAreDatesNotTimestamps is the record-date correctness point.
//
// A record date rendered as an instant invites a client to apply a timezone and land on the previous day,
// and the record date decides who is paid.
func TestPeriodDatesAreDatesNotTimestamps(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	periodID := seedAPIPeriod(t, ctx, tx, schemeID)

	rec := get(t, srv, "/v1/periods/"+periodID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	got := decodeBody(t, rec)

	for _, field := range []string{"periodStart", "periodEnd"} {
		v, ok := got[field].(string)
		if !ok {
			t.Errorf("%s is not a string", field)
			continue
		}
		if len(v) != len("2026-07-01") {
			t.Errorf("%s = %q, want a bare YYYY-MM-DD date", field, v)
		}
		if strings.Contains(v, "T") || strings.Contains(v, "Z") {
			t.Errorf("%s = %q, want a date with no time component", field, v)
		}
	}
}

// TestOpenPeriodHasNullRecordDateAndNoSnapshot covers the nullable path end to end.
func TestOpenPeriodHasNullRecordDateAndNoSnapshot(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	periodID := seedAPIPeriod(t, ctx, tx, schemeID)

	got := decodeBody(t, get(t, srv, "/v1/periods/"+periodID, nil))

	// recordDate is explicitly nullable in the contract, so it is present and null rather than absent.
	v, present := got["recordDate"]
	if !present {
		t.Error("recordDate should be present and null, since the contract types it as nullable")
	}
	if v != nil {
		t.Errorf("recordDate = %v, want null before it is declared", v)
	}

	if _, present := got["snapshot"]; present {
		t.Errorf("an open period has no snapshot, got %v", got["snapshot"])
	}
}

// TestETagAndConditionalRequest is what makes the contract's three-second poll cheap.
func TestETagAndConditionalRequest(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	id := seedAPIScheme(t, ctx, tx)

	first := get(t, srv, "/v1/schemes/"+id, nil)
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag was served, so a poller cannot avoid refetching")
	}

	second := get(t, srv, "/v1/schemes/"+id, map[string]string{"If-None-Match": tag})

	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 for an unchanged representation", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("a 304 must carry no body, got %d bytes", second.Body.Len())
	}
}

// TestETagMovesWhenTheDataChanges is the half that matters more.
//
// A tag that does not move on a real change means the poller never sees the update.
func TestETagMovesWhenTheDataChanges(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	id := seedAPIScheme(t, ctx, tx)

	before := get(t, srv, "/v1/schemes/"+id, nil).Header().Get("ETag")

	if _, err := tx.Exec(ctx, `UPDATE schemes SET status = 'OPERATIONAL' WHERE id = $1`, id); err != nil {
		t.Fatalf("updating the scheme: %v", err)
	}

	after := get(t, srv, "/v1/schemes/"+id, nil).Header().Get("ETag")

	if before == after {
		t.Fatalf("the status changed but the ETag did not move, both %q", before)
	}

	// And the stale tag must now get a body, not a 304.
	rec := get(t, srv, "/v1/schemes/"+id, map[string]string{"If-None-Match": before})
	if rec.Code != http.StatusOK {
		t.Fatalf("a stale ETag got %d, want 200 with the new representation", rec.Code)
	}
}

// TestLimitIsValidated stops a bad parameter being silently ignored.
func TestLimitIsValidated(t *testing.T) {
	srv, _, _ := apiServer(t)

	for _, bad := range []string{"abc", "0", "-1", "1.5"} {
		t.Run(bad, func(t *testing.T) {
			rec := get(t, srv, "/v1/schemes?limit="+bad, nil)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("limit=%s gave %d, want 400 rather than being silently defaulted", bad, rec.Code)
			}
		})
	}
}

// TestNoInvestorIdentityOnThePublicSurface is the boundary this file exists to hold.
//
// The public endpoints support the claim that anybody can verify the register. That is only safe because what
// they publish is wallet addresses and unit counts. This checks no encrypted blob, PAN, email or name column
// has leaked into a response through a wildcard select or an added field.
func TestNoInvestorIdentityOnThePublicSurface(t *testing.T) {
	srv, ctx, tx := apiServer(t)
	schemeID := seedAPIScheme(t, ctx, tx)
	offerID := seedAPIOffer(t, ctx, tx, schemeID)
	seedAPIBid(t, ctx, tx, offerID, 2, "BLOCKED")
	periodID := seedAPIPeriod(t, ctx, tx, schemeID)

	forbidden := []string{
		"full_name", "fullName", "pan", "email", "phone",
		"_enc", "account_number", "accountNumber", "ifsc",
		"investorId", "investor_id", "dp_id", "client_id",
	}

	paths := []string{
		"/v1/schemes",
		"/v1/schemes/" + schemeID,
		"/v1/schemes/" + schemeID + "/offers",
		"/v1/schemes/" + schemeID + "/periods",
		"/v1/offers/" + offerID,
		"/v1/periods/" + periodID,
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := get(t, srv, path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d for %s. body: %s", rec.Code, path, rec.Body.String())
			}

			body := strings.ToLower(rec.Body.String())
			for _, term := range forbidden {
				if strings.Contains(body, strings.ToLower(term)) {
					t.Errorf("%s leaked something identity-shaped: %q appears in the response.\nbody: %s",
						path, term, rec.Body.String())
				}
			}
		})
	}
}

// TestStoreFailuresBecome500WithoutLeaking covers the error path through a handler.
//
// A database failure must not put its text in the response: the message can name a host, a user or a column.
func TestStoreFailuresBecome500WithoutLeaking(t *testing.T) {
	srv := New(Deps{
		Env:     config.EnvLocal,
		Schemes: failingSchemes{},
	})

	rec := get(t, srv, "/v1/schemes", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "hunter2") || strings.Contains(body, "10.0.0.5") {
		t.Fatalf("the database error leaked into the response: %s", body)
	}

	env := decodeError(t, rec.Result())
	if env.Error.Code != CodeInternal {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeInternal)
	}
	if env.Error.RequestID == "" {
		t.Error("a 500 must still carry the request id; it is the only way to find the log line")
	}
}

type failingSchemes struct{}

func (failingSchemes) ByID(context.Context, string) (store.Scheme, error) {
	return store.Scheme{}, errors.New("dial tcp 10.0.0.5:5432: password=hunter2")
}

func (failingSchemes) List(context.Context, store.Pagination) ([]store.Scheme, error) {
	return nil, errors.New("dial tcp 10.0.0.5:5432: password=hunter2")
}

// TestRoutesAreAbsentWithoutTheirStores covers the nil-reader guard.
//
// A server built without stores must answer 404 rather than panic on a nil interface, because a panic at
// request time is a dropped connection that a client may read as worth retrying.
func TestRoutesAreAbsentWithoutTheirStores(t *testing.T) {
	srv := New(Deps{Env: config.EnvLocal})

	for _, path := range []string{"/v1/schemes", "/v1/offers/" + strings.Repeat("0", 8) + "-0000-0000-0000-000000000000"} {
		rec := get(t, srv, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s gave %d on a server with no stores, want 404", path, rec.Code)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
