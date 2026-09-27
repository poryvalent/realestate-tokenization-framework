package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acresync/orchestrator/internal/offer"
	"github.com/acresync/orchestrator/internal/period"
)

// These tests run against a real Postgres.
//
// A fake would prove nothing here. The whole risk in this package is that a column name is wrong, a cast is
// missing, or a nullable column is scanned into a non-pointer. Every one of those compiles and only fails
// against the real schema.
//
// # Isolation
//
// Each test runs inside a transaction that is rolled back, which is why Querier is an interface. Nothing has
// to be cleaned up, tests cannot interfere with each other, and a failing test leaves no residue for the
// next run to trip over.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("ACRESYNC_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("ACRESYNC_DATABASE_URL")
	}
	if url == "" {
		t.Skip("no ACRESYNC_DATABASE_URL; skipping store tests")
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

// pgTx opens a transaction that is always rolled back.
func pgTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()

	ctx := context.Background()
	tx, err := pgPool(t).Begin(ctx)
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	t.Cleanup(func() {
		// Rollback on a committed or already-closed transaction returns an error that means nothing here.
		_ = tx.Rollback(context.Background())
	})
	return ctx, tx
}

// seq keeps every seeded scheme's SEBI reference unique, including within one test.
var seq atomic.Int64

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), seq.Add(1))
}

// idem builds a distinct 32-byte idempotency key.
//
// Several tables require one and constrain it to exactly 32 bytes and unique, so a shared constant would
// make the second insert in any test fail for a reason unrelated to what it is testing.
func idem(label string) []byte {
	sum := sha256.Sum256([]byte(uniq(label)))
	return sum[:]
}

// seedScheme inserts a scheme that satisfies every statutory CHECK.
//
// The figures are the platform's real ones: 500 units at ₹10 lakh is ₹50 crore, which is the floor of the
// SM-REIT band, and 25 manager units plus 475 public units balances the cap table.
func seedScheme(t *testing.T, ctx context.Context, q Querier) string {
	t.Helper()

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO schemes (
			sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
			total_units, im_units, public_units, min_public_holders,
			status, chain_id, roles_address, ballot_address, scheme_address, environment_tag
		) VALUES (
			$1, $2, 50000000000, 100000000,
			500, 25, 475, 200,
			'DRAFT', 11155111,
			'0x21d409cb5470fd3bcda344731d945c34cb13b53f',
			'0x63615652d8ff9190454229c2add1f173d28b32d5',
			'0xa656a42974b40cf64f32e758abb0689a2a178391',
			'SEPOLIA_SIM'
		) RETURNING id`,
		uniq("SEBI/SM-REIT/TEST"), "Store Test Scheme").Scan(&id)
	if err != nil {
		t.Fatalf("seeding a scheme: %v", err)
	}
	return id
}

// seedOffer inserts an offer whose terms satisfy the holder-floor cap.
//
// max_bid_units is 25, well inside `units_on_offer - (min_distinct_holders - 1)` which is 276 here. A larger
// cap would let the offer be fully subscribed in rupees by too few holders to be listable.
// Only one INITIAL offer may exist per scheme, enforced by the partial unique index
// offers_one_initial_per_scheme. A scheme's first issuance is a singular event, so a test needing several
// offers on one scheme must mark the rest FOLLOW_ON.
func seedOffer(t *testing.T, ctx context.Context, q Querier, schemeID string, status offer.Status) string {
	t.Helper()
	return seedOfferOfType(t, ctx, q, schemeID, status, "INITIAL")
}

func seedOfferOfType(t *testing.T, ctx context.Context, q Querier, schemeID string, status offer.Status, offerType string) string {
	t.Helper()

	opens := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO offers (
			scheme_id, offer_type,
			price_band_lower_paise, price_band_upper_paise,
			units_on_offer, min_bid_units, max_bid_units,
			min_subscription_units, min_distinct_holders,
			opens_at, closes_at, allotment_due_at,
			status, idempotency_key
		) VALUES (
			$1, $2::offer_type,
			100000000, 105000000,
			475, 1, 25,
			428, 200,
			$3, $4, $5,
			$6, $7
		) RETURNING id`,
		schemeID, offerType, opens, opens.Add(72*time.Hour), opens.Add(96*time.Hour),
		string(status), idem("offer")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a %s offer: %v", offerType, err)
	}
	return id
}

// seedInvestor inserts an investor with its demat and bank accounts.
//
// The personal columns are encrypted blobs in the schema and no plaintext is written here, which is the
// same reason no PII reaches the chain: there is nowhere in this system that holds a name in the clear.
func seedInvestor(t *testing.T, ctx context.Context, q Querier) (investorID, dematID, bankID string) {
	t.Helper()

	blob := []byte("enc:" + uniq("investor"))

	if err := q.QueryRow(ctx, `
		INSERT INTO investors (full_name_enc, pan_enc, email_enc, phone_enc, investor_class)
		VALUES ($1, $1, $1, $1, 'RESIDENT_IND') RETURNING id`, blob).Scan(&investorID); err != nil {
		t.Fatalf("seeding an investor: %v", err)
	}

	if err := q.QueryRow(ctx, `
		INSERT INTO demat_accounts (investor_id, depository, dp_id, client_id)
		VALUES ($1, 'NSDL', $2, $3) RETURNING id`,
		investorID, uniq("DP"), uniq("CL")).Scan(&dematID); err != nil {
		t.Fatalf("seeding a demat account: %v", err)
	}

	if err := q.QueryRow(ctx, `
		INSERT INTO bank_accounts (investor_id, account_number_enc, ifsc, account_name_enc)
		VALUES ($1, $2, 'HDFC0000001', $2) RETURNING id`,
		investorID, blob).Scan(&bankID); err != nil {
		t.Fatalf("seeding a bank account: %v", err)
	}

	return investorID, dematID, bankID
}

// seedBid inserts a bid and, when blockStatus is non-empty, its ASBA block.
func seedBid(t *testing.T, ctx context.Context, q Querier, offerID string, units int32, blockStatus string) string {
	t.Helper()

	investorID, dematID, bankID := seedInvestor(t, ctx, q)
	price := int64(100000000)

	// bids_reference_fmt constrains the reference to exactly 32 lowercase hex characters, which is what an
	// investor quotes as their own receipt and what the bid book is ordered by.
	bidRef := hex.EncodeToString(idem("bidref"))[:32]

	var bidID string
	err := q.QueryRow(ctx, `
		INSERT INTO bids (
			offer_id, investor_id, investor_anchor_hash, demat_account_id, bank_account_id,
			units_bid, price_per_unit_paise, total_amount_paise, bid_reference, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		offerID, investorID, idem("anchor"), dematID, bankID,
		units, price, int64(units)*price, bidRef, idem("bid")).Scan(&bidID)
	if err != nil {
		t.Fatalf("seeding a bid: %v", err)
	}

	if blockStatus == "" {
		return bidID
	}

	amount := int64(units) * price
	// blocked_at and blocked_amount_paise are required once the status is BLOCKED, and debited implies
	// blocked, so both are supplied for either terminal funded state.
	//
	// RETURNING plus Scan rather than Query, because a pgx Rows left unclosed holds the connection and the
	// next statement fails with "conn busy". Querier deliberately has no Exec: the stores only read, and
	// adding one would invite a write to be smuggled through the read path.
	var blockID string
	err = q.QueryRow(ctx, `
		INSERT INTO asba_blocks (
			bid_id, provider, requested_amount_paise, blocked_amount_paise,
			block_status, blocked_at, idempotency_key
		) VALUES ($1, 'RAZORPAYX_SANDBOX', $2, $2, $3::asba_block_status, now(), $4)
		RETURNING id`,
		bidID, amount, blockStatus, idem("asba")).Scan(&blockID)
	if err != nil {
		t.Fatalf("seeding an asba block: %v", err)
	}
	return bidID
}

// TestSchemeRoundTrip proves the column list and scan order agree with the schema.
func TestSchemeRoundTrip(t *testing.T) {
	ctx, tx := pgTx(t)
	id := seedScheme(t, ctx, tx)

	got, err := NewSchemes(tx).ByID(ctx, id)
	if err != nil {
		t.Fatalf("reading the scheme back: %v", err)
	}

	if got.ID != id {
		t.Errorf("id = %q, want %q", got.ID, id)
	}
	if got.AssetValue != 50000000000 {
		t.Errorf("assetValue = %d paise, want the ₹50 crore floor", got.AssetValue)
	}
	if got.UnitPrice != 100000000 {
		t.Errorf("unitPrice = %d paise, want ₹10 lakh", got.UnitPrice)
	}
	if got.TotalUnits != 500 || got.IMUnits != 25 || got.PublicUnits != 475 {
		t.Errorf("cap table = %d total, %d im, %d public; want 500/25/475",
			got.TotalUnits, got.IMUnits, got.PublicUnits)
	}
	if got.MinPublicHolders != 200 {
		t.Errorf("minPublicHolders = %d, want the statutory 200", got.MinPublicHolders)
	}
	if got.EnvironmentTag != "SEPOLIA_SIM" {
		t.Errorf("environmentTag = %q, want SEPOLIA_SIM", got.EnvironmentTag)
	}
	if !got.Deployed() {
		t.Error("a scheme with all three addresses should read as deployed")
	}
	if got.SchemeAddress != "0xa656a42974b40cf64f32e758abb0689a2a178391" {
		t.Errorf("schemeAddress = %q", got.SchemeAddress)
	}
}

// TestSchemeNotFoundIsASentinel is what the 404 depends on.
func TestSchemeNotFoundIsASentinel(t *testing.T) {
	ctx, tx := pgTx(t)

	_, err := NewSchemes(tx).ByID(ctx, "00000000-0000-0000-0000-000000000000")

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound so the HTTP layer can answer 404 without importing pgx", err)
	}
}

// TestUndeployedSchemeReadsAsNotDeployed covers the nullable chain columns.
//
// These are the columns most likely to be scanned wrongly, because a NULL into a non-pointer string is a
// runtime error that only the real schema produces.
func TestUndeployedSchemeReadsAsNotDeployed(t *testing.T) {
	ctx, tx := pgTx(t)

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO schemes (
			sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
			total_units, im_units, public_units
		) VALUES ($1, 'Undeployed', 50000000000, 100000000, 500, 25, 475)
		RETURNING id`, uniq("SEBI/SM-REIT/UNDEPLOYED")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding an undeployed scheme: %v", err)
	}

	got, err := NewSchemes(tx).ByID(ctx, id)
	if err != nil {
		t.Fatalf("reading an undeployed scheme: %v", err)
	}

	if got.Deployed() {
		t.Error("a scheme with no addresses must not read as deployed")
	}
	if got.ChainID != 0 || got.SchemeAddress != "" {
		t.Errorf("want the coalesced zero values, got chainId=%d address=%q", got.ChainID, got.SchemeAddress)
	}
}

// TestSchemeListIsOrderedAndBounded covers paging.
func TestSchemeListIsOrderedAndBounded(t *testing.T) {
	ctx, tx := pgTx(t)
	for i := 0; i < 3; i++ {
		seedScheme(t, ctx, tx)
	}

	got, err := NewSchemes(tx).List(ctx, Pagination{Limit: 2})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d schemes, want the limit of 2", len(got))
	}
	if got[0].SebiSchemeRef >= got[1].SebiSchemeRef {
		t.Errorf("not ordered by sebi_scheme_ref: %q then %q", got[0].SebiSchemeRef, got[1].SebiSchemeRef)
	}
}

// TestListLimitIsCapped stops a caller asking for everything.
func TestListLimitIsCapped(t *testing.T) {
	if got := (Pagination{Limit: 100000}).normalise().Limit; got != MaxLimit {
		t.Errorf("limit %d was not capped to %d", got, MaxLimit)
	}
	if got := (Pagination{Limit: 0}).normalise().Limit; got != DefaultLimit {
		t.Errorf("an unset limit became %d, want the default %d", got, DefaultLimit)
	}
	if got := (Pagination{Limit: -5}).normalise().Limit; got != DefaultLimit {
		t.Errorf("a negative limit became %d, want the default %d", got, DefaultLimit)
	}
}

// TestOfferRoundTripCarriesDomainTypes is the point of returning offer.Terms.
func TestOfferRoundTripCarriesDomainTypes(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)
	offerID := seedOffer(t, ctx, tx, schemeID, offer.StatusOpen)

	got, err := NewOffers(tx).ByID(ctx, offerID)
	if err != nil {
		t.Fatalf("reading the offer back: %v", err)
	}

	if got.Status != offer.StatusOpen {
		t.Errorf("status = %q, want %q", got.Status, offer.StatusOpen)
	}
	if !got.Status.Valid() {
		t.Error("the status did not survive as a valid domain value")
	}
	if got.Terms.UnitsOnOffer != 475 {
		t.Errorf("unitsOnOffer = %d, want 475", got.Terms.UnitsOnOffer)
	}
	if got.Terms.MaxBidUnits != 25 {
		t.Errorf("maxBidUnits = %d, want 25", got.Terms.MaxBidUnits)
	}
	if got.Terms.MinDistinctHolders != 200 {
		t.Errorf("minDistinctHolders = %d, want the statutory 200", got.Terms.MinDistinctHolders)
	}
	if got.Terms.PriceBandLowerPaise != 100000000 {
		t.Errorf("priceBandLower = %d, want ₹10 lakh", got.Terms.PriceBandLowerPaise)
	}

	// UTC is not cosmetic: the contract requires RFC3339 with a Z, and a time carrying a local offset
	// serialises to something it forbids.
	if got.Terms.OpensAt.Location() != time.UTC {
		t.Errorf("opensAt is in %v, want UTC", got.Terms.OpensAt.Location())
	}
	if !got.Terms.ClosesAt.After(got.Terms.OpensAt) {
		t.Error("closesAt should be after opensAt")
	}
}

// TestEveryOfferStatusSurvivesARoundTrip is the guard against the Go enum and the SQL enum drifting.
//
// A value in one and not the other is a mismatch that no unit test can see, because each side is
// self-consistent. Writing every Go value into the column is the only thing that proves they agree.
func TestEveryOfferStatusSurvivesARoundTrip(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)
	store := NewOffers(tx)

	for i, status := range offer.AllStatuses() {
		t.Run(string(status), func(t *testing.T) {
			// The first is the scheme's INITIAL offer; the rest are FOLLOW_ON, because only one INITIAL
			// offer may exist per scheme.
			offerType := "FOLLOW_ON"
			if i == 0 {
				offerType = "INITIAL"
			}
			id := seedOfferOfType(t, ctx, tx, schemeID, status, offerType)

			got, err := store.ByID(ctx, id)
			if err != nil {
				t.Fatalf("status %q does not round-trip: %v", status, err)
			}
			if got.Status != status {
				t.Fatalf("status came back as %q, want %q", got.Status, status)
			}
		})
	}
}

// fakeRow feeds scanOffer a value the database could hold but the Go enum does not define.
//
// Postgres refuses to use an enum value added by ALTER TYPE in the same transaction that added it, so the
// realistic scenario — a migration adds a status and the Go side is not updated — cannot be staged against a
// live enum inside a rolled-back transaction. Driving the scan function directly tests exactly the branch
// that matters without needing to.
type fakeRow struct {
	statusText string
}

func (f fakeRow) Scan(dest ...any) error {
	// Mirrors scanOffer's scan order. Only the status is interesting; the rest need plausible values so the
	// function reaches its validation.
	for i, d := range dest {
		switch target := d.(type) {
		case *string:
			switch i {
			case 3:
				*target = f.statusText
			default:
				*target = "00000000-0000-0000-0000-000000000000"
			}
		case *int32:
			*target = 1
		case *int64:
			*target = 100000000
		case *time.Time:
			*target = time.Unix(0, 0).UTC()
		}
	}
	return nil
}

// TestUnknownOfferStatusIsRefused covers the validation on read.
//
// A status the Go enum does not define must not be served. Every caller would take its default branch, and
// for a lifecycle status that means showing an offer as something it is not.
func TestUnknownOfferStatusIsRefused(t *testing.T) {
	_, err := scanOffer(fakeRow{statusText: "INVENTED_BY_A_MIGRATION"})

	if !errors.Is(err, ErrUnknownEnumValue) {
		t.Fatalf("err = %v, want ErrUnknownEnumValue; an undefined status must not be served", err)
	}
	if !strings.Contains(err.Error(), "INVENTED_BY_A_MIGRATION") {
		t.Errorf("the error should name the offending value, got %v", err)
	}
	if !strings.Contains(err.Error(), "offers.status") {
		t.Errorf("the error should name the column, got %v", err)
	}
}

// TestKnownOfferStatusPassesValidation proves the guard is not rejecting everything.
//
// Without this, a scanOffer that always failed would satisfy the test above and look correct.
func TestKnownOfferStatusPassesValidation(t *testing.T) {
	got, err := scanOffer(fakeRow{statusText: string(offer.StatusOpen)})
	if err != nil {
		t.Fatalf("a valid status was rejected: %v", err)
	}
	if got.Status != offer.StatusOpen {
		t.Errorf("status = %q, want %q", got.Status, offer.StatusOpen)
	}
}

// TestSubscriptionCountsOnlyFundedBlocks is the join that decides whether a bid is real.
func TestSubscriptionCountsOnlyFundedBlocks(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)
	offerID := seedOffer(t, ctx, tx, schemeID, offer.StatusOpen)

	seedBid(t, ctx, tx, offerID, 2, "BLOCKED")   // funded
	seedBid(t, ctx, tx, offerID, 3, "DEBITED")   // funded, money already moved
	seedBid(t, ctx, tx, offerID, 4, "REQUESTED") // not yet funded
	seedBid(t, ctx, tx, offerID, 5, "FAILED")    // never funded
	seedBid(t, ctx, tx, offerID, 6, "")          // no block at all

	got, err := NewOffers(tx).SubscriptionFor(ctx, offerID)
	if err != nil {
		t.Fatalf("aggregating the subscription: %v", err)
	}

	if got.BidCount != 5 {
		t.Errorf("bidCount = %d, want 5", got.BidCount)
	}
	if got.DistinctBidders != 5 {
		t.Errorf("distinctBidders = %d, want 5", got.DistinctBidders)
	}
	if got.UnitsBid != 20 {
		t.Errorf("unitsBid = %d, want 2+3+4+5+6 = 20", got.UnitsBid)
	}
	if got.FundsBlockedCount != 2 {
		t.Errorf("fundsBlockedCount = %d, want 2; only BLOCKED and DEBITED are funded", got.FundsBlockedCount)
	}
}

// TestSubscriptionOnAnOfferWithNoBidsIsZero covers the aggregate over an empty set.
//
// Without the coalesce, sum() over no rows is NULL and scanning it into an int64 fails, which would turn the
// most common state of a fresh offer into a 500.
func TestSubscriptionOnAnOfferWithNoBidsIsZero(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)
	offerID := seedOffer(t, ctx, tx, schemeID, offer.StatusConfigured)

	got, err := NewOffers(tx).SubscriptionFor(ctx, offerID)
	if err != nil {
		t.Fatalf("aggregating an empty subscription: %v", err)
	}

	if got.BidCount != 0 || got.UnitsBid != 0 || got.FundsBlockedCount != 0 {
		t.Errorf("want all zero, got %+v", got)
	}
}

// TestOversubscribed is arithmetic, checked at the boundary.
func TestOversubscribed(t *testing.T) {
	cases := []struct {
		unitsBid, onOffer int64
		want              bool
	}{
		{0, 475, false},
		{474, 475, false},
		{475, 475, false}, // exactly subscribed is not over
		{476, 475, true},
		{980, 475, true},
	}

	for _, tc := range cases {
		got := Subscription{UnitsBid: tc.unitsBid}.Oversubscribed(tc.onOffer)
		if got != tc.want {
			t.Errorf("%d bid against %d on offer: got %v, want %v", tc.unitsBid, tc.onOffer, got, tc.want)
		}
	}
}

// TestOffersForScheme covers the list path.
func TestOffersForScheme(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)
	seedOffer(t, ctx, tx, schemeID, offer.StatusConfigured)
	seedOfferOfType(t, ctx, tx, schemeID, offer.StatusOpen, "FOLLOW_ON")

	// A second scheme's offer must not appear.
	otherScheme := seedScheme(t, ctx, tx)
	seedOffer(t, ctx, tx, otherScheme, offer.StatusOpen)

	got, err := NewOffers(tx).ForScheme(ctx, schemeID, Pagination{})
	if err != nil {
		t.Fatalf("listing offers: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d offers, want 2 for this scheme only", len(got))
	}
	for _, o := range got {
		if o.SchemeID != schemeID {
			t.Errorf("an offer from scheme %q leaked into scheme %q's list", o.SchemeID, schemeID)
		}
	}
}

// seedPeriod inserts a distribution period.
//
// Only ONE non-terminal period may exist per scheme, enforced by periods_one_active_per_scheme, so a test
// needing a second must close or reverse the first.
func seedPeriod(t *testing.T, ctx context.Context, q Querier, schemeID string, seq int32, status period.Status) string {
	t.Helper()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO distribution_periods (
			scheme_id, period_seq, period_label, period_start, period_end,
			status, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		schemeID, seq, fmt.Sprintf("FY27-Q%d", seq), start, start.AddDate(0, 3, -1),
		string(status), idem("period")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a period: %v", err)
	}
	return id
}

// TestPeriodRoundTripWithNullFigures is the nullable-column case.
//
// An open period has no NDCF, no approvals, no record date and no anchor. Every one of those is a NULL that
// must survive into a nil rather than a zero, because a zero would read as "₹0 was distributed" which is a
// statement the system has not made.
func TestPeriodRoundTripWithNullFigures(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)
	id := seedPeriod(t, ctx, tx, schemeID, 1, period.StatusOpen)

	got, err := NewPeriods(tx).ByID(ctx, id)
	if err != nil {
		t.Fatalf("reading the period back: %v", err)
	}

	if got.Status != period.StatusOpen {
		t.Errorf("status = %q, want %q", got.Status, period.StatusOpen)
	}
	if got.NdcfPaise != nil {
		t.Errorf("ndcfPaise = %v, want nil; an open period has not drafted one", *got.NdcfPaise)
	}
	if got.DistributedPaise != nil {
		t.Error("distributedPaise should be nil, not zero; ₹0 distributed is a different claim")
	}
	if got.RecordDate != nil {
		t.Error("recordDate should be nil before it is declared")
	}
	if got.IMApprovedAt != nil || got.TrusteeApprovedAt != nil {
		t.Error("approvals should be nil on an open period")
	}
	if got.ReversedAt != nil || got.ReversalReason != "" {
		t.Error("a live period carries no reversal record")
	}
	if got.PeriodStart.Location() != time.UTC {
		t.Errorf("periodStart is in %v, want UTC", got.PeriodStart.Location())
	}
}

// TestPeriodRoundTripWithFigures covers a fully anchored period.
//
// The figures sit exactly on the 95% floor: 27,550,000,000 of 29,000,000,000 is 9500 bps to the paise. That
// is deliberate, because a comfortable margin would pass whether the floor check used >= or >.
func TestPeriodRoundTripWithFigures(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)

	const (
		ndcf        = int64(29000000000)
		distributed = int64(27550000000)
	)

	digest := sha256.Sum256([]byte("ndcf statement"))
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO distribution_periods (
			scheme_id, period_seq, period_label, period_start, period_end, record_date,
			status, ndcf_paise, distributed_paise, distribution_bps, ndcf_statement_sha256,
			im_approved_by, im_approved_at, trustee_approved_by, trustee_approved_at,
			anchored_tx, idempotency_key
		) VALUES (
			$1, 1, 'FY27-Q1', $2, $3, $3,
			'ANCHORED', $4, $5, 9500, $6,
			'im@acresync.test', now(), 'trustee@acresync.test', now(),
			'0xabc', $7
		) RETURNING id`,
		schemeID, start, start.AddDate(0, 3, -1),
		ndcf, distributed, digest[:], idem("period")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding an anchored period: %v", err)
	}

	got, err := NewPeriods(tx).ByID(ctx, id)
	if err != nil {
		t.Fatalf("reading the anchored period: %v", err)
	}

	if got.NdcfPaise == nil || int64(*got.NdcfPaise) != ndcf {
		t.Fatalf("ndcfPaise = %v, want %d", got.NdcfPaise, ndcf)
	}
	if got.DistributedPaise == nil || int64(*got.DistributedPaise) != distributed {
		t.Fatalf("distributedPaise = %v, want %d", got.DistributedPaise, distributed)
	}
	if got.DistributionBps == nil || *got.DistributionBps != 9500 {
		t.Fatalf("distributionBps = %v, want exactly the 9500 floor", got.DistributionBps)
	}
	if got.IMApprovedBy == "" || got.TrusteeApprovedBy == "" {
		t.Error("an anchored period must carry both approvals")
	}
	if got.IMApprovedBy == got.TrusteeApprovedBy {
		t.Error("four eyes means two different people")
	}
	if len(got.StatementSHA256) != 32 {
		t.Errorf("statement digest is %d bytes, want 32", len(got.StatementSHA256))
	}
	if got.RecordDate == nil {
		t.Error("an anchored period has a declared record date")
	}
}

// TestEveryPeriodStatusSurvivesARoundTrip guards the Go and SQL enums against drift.
//
// Each status goes into its own scheme, because only one non-terminal period may exist per scheme.
func TestEveryPeriodStatusSurvivesARoundTrip(t *testing.T) {
	ctx, tx := pgTx(t)
	store := NewPeriods(tx)

	for _, status := range period.AllStatuses() {
		t.Run(string(status), func(t *testing.T) {
			schemeID := seedScheme(t, ctx, tx)

			// The CHECK constraints demand figures, approvals and a reversal record for the later
			// statuses, so anything beyond the early lifecycle is seeded through the full insert.
			var id string
			switch status {
			case period.StatusOpen, period.StatusRentCollected, period.StatusNDCFDrafted,
				period.StatusNDCFApproved, period.StatusRecordDateDeclared,
				period.StatusSnapshotTaken, period.StatusReconciled, period.StatusChainStale:
				id = seedPeriod(t, ctx, tx, schemeID, 1, status)
			default:
				id = seedFullPeriod(t, ctx, tx, schemeID, status)
			}

			got, err := store.ByID(ctx, id)
			if err != nil {
				t.Fatalf("status %q does not round-trip: %v", status, err)
			}
			if got.Status != status {
				t.Fatalf("status came back as %q, want %q", got.Status, status)
			}
		})
	}
}

// seedFullPeriod inserts a period satisfying the dual-approval, figures and reversal constraints.
func seedFullPeriod(t *testing.T, ctx context.Context, q Querier, schemeID string, status period.Status) string {
	t.Helper()

	digest := sha256.Sum256([]byte("statement"))
	narrative := sha256.Sum256([]byte("why this was reversed"))
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	reversalReason := "OTHER"
	if status != period.StatusReversed {
		reversalReason = ""
	}

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO distribution_periods (
			scheme_id, period_seq, period_label, period_start, period_end, record_date,
			status, ndcf_paise, distributed_paise, distribution_bps, ndcf_statement_sha256,
			im_approved_by, im_approved_at, trustee_approved_by, trustee_approved_at,
			reversal_reason, reversal_narrative_sha256, reversed_at,
			idempotency_key
		) VALUES (
			$1, 1, 'FY27-Q1', $2, $3, $3,
			$4, 29000000000, 27550000000, 9500, $5,
			'im@acresync.test', now(), 'trustee@acresync.test', now(),
			nullif($6, '')::reversal_reason,
			CASE WHEN $6 = '' THEN NULL ELSE $7::bytea END,
			CASE WHEN $6 = '' THEN NULL ELSE now() END,
			$8
		) RETURNING id`,
		schemeID, start, start.AddDate(0, 3, -1),
		string(status), digest[:], reversalReason, narrative[:], idem("period")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a %q period: %v", status, err)
	}
	return id
}

// TestPeriodNotFoundIsASentinel mirrors the scheme case.
func TestPeriodNotFoundIsASentinel(t *testing.T) {
	ctx, tx := pgTx(t)

	_, err := NewPeriods(tx).ByID(ctx, "00000000-0000-0000-0000-000000000000")

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestSnapshotAbsentBeforeTheRegisterIsFrozen is a normal state, not a fault.
func TestSnapshotAbsentBeforeTheRegisterIsFrozen(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)
	periodID := seedPeriod(t, ctx, tx, schemeID, 1, period.StatusOpen)

	_, err := NewPeriods(tx).SnapshotFor(ctx, periodID)

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound; an open period has no snapshot yet", err)
	}
}

// TestSnapshotRoundTripCountsLines covers the subquery.
//
// lineCount must exceed distinctHolders by the excluded manager. Counting the lines rather than storing a
// count means the two cannot disagree.
func TestSnapshotRoundTripCountsLines(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)
	periodID := seedPeriod(t, ctx, tx, schemeID, 1, period.StatusSnapshotTaken)

	root := sha256.Sum256([]byte("merkle root"))
	cid := sha256.Sum256([]byte("cid digest"))
	recordDate := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	var snapshotID string
	err := tx.QueryRow(ctx, `
		INSERT INTO register_snapshots (
			scheme_id, distribution_period_id, record_date, simulated_clock_value,
			total_units, distinct_holders, snapshot_merkle_root, snapshot_cid_digest,
			anchor_status, idempotency_key
		) VALUES ($1, $2, $3, now(), 500, 2, $4, $5, 'CONFIRMED', $6)
		RETURNING id`,
		schemeID, periodID, recordDate, root[:], cid[:], idem("snapshot")).Scan(&snapshotID)
	if err != nil {
		t.Fatalf("seeding a snapshot: %v", err)
	}

	// Three lines for two countable holders: the manager's line is excluded from the statutory count.
	for i, excluded := range []bool{false, false, true} {
		investorID, _, _ := seedInvestor(t, ctx, tx)
		_, err := tx.Exec(ctx, `
			INSERT INTO register_snapshot_lines (
				snapshot_id, leaf_index, investor_id, investor_anchor_hash,
				wallet_address, units, is_excluded
			) VALUES ($1, $2, $3, $4, $5, 10, $6)`,
			snapshotID, i, investorID, idem("anchor"),
			fmt.Sprintf("0x%040x", i+1), excluded)
		if err != nil {
			t.Fatalf("seeding snapshot line %d: %v", i, err)
		}
	}

	got, err := NewPeriods(tx).SnapshotFor(ctx, periodID)
	if err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}

	if got.TotalUnits != 500 {
		t.Errorf("totalUnits = %d, want 500 including the manager", got.TotalUnits)
	}
	if got.DistinctHolders != 2 {
		t.Errorf("distinctHolders = %d, want 2 excluding the manager", got.DistinctHolders)
	}
	if got.LineCount != 3 {
		t.Errorf("lineCount = %d, want 3; it exceeds distinctHolders by the excluded manager", got.LineCount)
	}
	if got.LineCount <= int64(got.DistinctHolders) {
		t.Error("lineCount must exceed distinctHolders whenever a holder is excluded")
	}
	if len(got.MerkleRoot) != 32 {
		t.Errorf("merkleRoot is %d bytes, want 32", len(got.MerkleRoot))
	}
	if got.TakenAt.Location() != time.UTC {
		t.Errorf("takenAt is in %v, want UTC", got.TakenAt.Location())
	}
}

// TestPeriodsForScheme covers the list path and scheme isolation.
func TestPeriodsForScheme(t *testing.T) {
	ctx, tx := pgTx(t)
	schemeID := seedScheme(t, ctx, tx)

	// Only one non-terminal period per scheme, so the first is CLOSED before the second is opened.
	seedFullPeriod(t, ctx, tx, schemeID, period.StatusClosed)
	seedPeriod(t, ctx, tx, schemeID, 2, period.StatusOpen)

	other := seedScheme(t, ctx, tx)
	seedPeriod(t, ctx, tx, other, 1, period.StatusOpen)

	got, err := NewPeriods(tx).ForScheme(ctx, schemeID, Pagination{})
	if err != nil {
		t.Fatalf("listing periods: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d periods, want 2", len(got))
	}
	// Most recent first.
	if got[0].PeriodSeq != 2 || got[1].PeriodSeq != 1 {
		t.Errorf("order = %d then %d, want 2 then 1", got[0].PeriodSeq, got[1].PeriodSeq)
	}
	for _, p := range got {
		if p.SchemeID != schemeID {
			t.Errorf("a period from scheme %q leaked into scheme %q's list", p.SchemeID, schemeID)
		}
	}
}

// TestStoresWorkAgainstAPoolAsWellAsATransaction proves the Querier abstraction is real.
//
// The tests all run in a transaction, so a store that only worked against a transaction would look fine
// here and fail in production, where it is handed a pool.
func TestStoresWorkAgainstAPoolAsWellAsATransaction(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	// A read that returns nothing is enough: it exercises Query and QueryRow through the pool.
	if _, err := NewSchemes(pool).List(ctx, Pagination{Limit: 1}); err != nil {
		t.Fatalf("listing schemes through a pool: %v", err)
	}
	_, err := NewSchemes(pool).ByID(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound through a pool", err)
	}
}
