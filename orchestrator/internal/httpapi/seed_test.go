package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/store"
)

// Seed helpers for the API integration tests.
//
// Duplicated from the store package's tests rather than exported from it, because test fixtures are not part
// of a package's contract and sharing them would make a change to one package's test setup able to break
// another package's tests.
//
// Every insert here satisfies the real CHECK constraints, which is deliberate: a fixture that bypassed them
// would let a handler be tested against data the system could never actually hold.

var apiSeq atomic.Int64

func apiUniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), apiSeq.Add(1))
}

// apiIdem builds a distinct 32-byte idempotency key, which several tables require and constrain.
func apiIdem(label string) []byte {
	sum := sha256.Sum256([]byte(apiUniq(label)))
	return sum[:]
}

// seedAPIScheme inserts a deployed scheme with the platform's real figures.
//
// 500 units at ₹10 lakh is ₹50 crore, the floor of the SM-REIT band, and 25 manager units plus 475 public
// units balances the cap table. The addresses are the live Sepolia deployment, lowercased as the schema
// requires.
func seedAPIScheme(t *testing.T, ctx context.Context, q store.Querier) string {
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
		apiUniq("SEBI/SM-REIT/API"), "AcreSync API Test Scheme").Scan(&id)
	if err != nil {
		t.Fatalf("seeding a scheme: %v", err)
	}
	return id
}

// seedAPIOffer inserts the scheme's INITIAL offer.
func seedAPIOffer(t *testing.T, ctx context.Context, q store.Querier, schemeID string) string {
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
			$1, 'INITIAL',
			100000000, 105000000,
			475, 1, 25,
			428, 200,
			$2, $3, $4,
			'OPEN', $5
		) RETURNING id`,
		schemeID, opens, opens.Add(72*time.Hour), opens.Add(96*time.Hour), apiIdem("offer")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding an offer: %v", err)
	}
	return id
}

// seedAPIInvestor inserts an investor with a demat and bank account.
//
// No plaintext identity is written. The columns are encrypted blobs in the schema, which is the same boundary
// the chain respects: there is nowhere in this system holding a name in the clear.
func seedAPIInvestor(t *testing.T, ctx context.Context, q store.Querier) (investorID, dematID, bankID string) {
	t.Helper()

	blob := []byte("enc:" + apiUniq("investor"))

	if err := q.QueryRow(ctx, `
		INSERT INTO investors (full_name_enc, pan_enc, email_enc, phone_enc, investor_class)
		VALUES ($1, $1, $1, $1, 'RESIDENT_IND') RETURNING id`, blob).Scan(&investorID); err != nil {
		t.Fatalf("seeding an investor: %v", err)
	}

	if err := q.QueryRow(ctx, `
		INSERT INTO demat_accounts (investor_id, depository, dp_id, client_id)
		VALUES ($1, 'NSDL', $2, $3) RETURNING id`,
		investorID, apiUniq("DP"), apiUniq("CL")).Scan(&dematID); err != nil {
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

// seedAPIBid inserts a bid and, when blockStatus is non-empty, its ASBA block.
func seedAPIBid(t *testing.T, ctx context.Context, q store.Querier, offerID string, units int32, blockStatus string) string {
	t.Helper()

	investorID, dematID, bankID := seedAPIInvestor(t, ctx, q)
	price := int64(100000000)

	// bids_reference_fmt requires exactly 32 lowercase hex characters.
	bidRef := hex.EncodeToString(apiIdem("bidref"))[:32]

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

	if blockStatus == "" {
		return bidID
	}

	amount := int64(units) * price
	var blockID string
	err = q.QueryRow(ctx, `
		INSERT INTO asba_blocks (
			bid_id, provider, requested_amount_paise, blocked_amount_paise,
			block_status, blocked_at, idempotency_key
		) VALUES ($1, 'RAZORPAYX_SANDBOX', $2, $2, $3::asba_block_status, now(), $4)
		RETURNING id`,
		bidID, amount, blockStatus, apiIdem("asba")).Scan(&blockID)
	if err != nil {
		t.Fatalf("seeding an asba block: %v", err)
	}
	return bidID
}

// seedAPIPeriod inserts an open distribution period.
//
// Only one non-terminal period may exist per scheme, so a test needing a second must close the first.
func seedAPIPeriod(t *testing.T, ctx context.Context, q store.Querier, schemeID string) string {
	t.Helper()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO distribution_periods (
			scheme_id, period_seq, period_label, period_start, period_end, status, idempotency_key
		) VALUES ($1, 1, 'FY27-Q1', $2, $3, 'OPEN', $4) RETURNING id`,
		schemeID, start, start.AddDate(0, 3, -1), apiIdem("period")).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a period: %v", err)
	}
	return id
}
