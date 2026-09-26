package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The primary market fixture is the mirror image of the distribution fixture.
//
// newFixture seeds a scheme whose register is already complete, because a distribution period starts
// from a settled cap table. A primary market test must start from nothing: no unit_holdings, no
// depository_register, no holdings ledger. The whole point is that the offer creates them.
//
// Bid shape: 490 bidders for two units each is 980 units bid against 475 on offer.
//
// The count is above the units on offer on purpose, and the first version of this fixture got it wrong.
// With 240 bidders the book was oversubscribed in units but not in bidders, and the allocator's
// water-filling pass gave every single bidder at least one unit. Nothing was rejected, so the nil-outcome
// branch never ran and neither did the bids_rejection_reason_present constraint that pairs a rejected bid
// with a reason. The test passed while leaving a whole path unexercised.
//
// With 490 bidders for 475 units the allocator cannot satisfy everyone even at one unit each, so the
// draw ranks and cuts. The result exercises three outcomes at once: partial fills for those who wanted
// two units and received one, nil outcomes for those below the cut, and the constraint that a rejected
// bid must record why.
const (
	primaryBidders  = 490
	unitsPerBid     = 2
	bidPricePaise   = 100_000_000 // ₹10 lakh, the band floor
	primaryIMUnits  = 25
	primaryOnOffer  = 475
	primaryTotal    = 500
	primaryMinBid   = 1
	primaryMaxBid   = 25
	primaryMinSub   = 428
	primaryMinHold  = 200
	primaryBandHigh = 105_000_000
)

// primaryFixture is a scheme with investors and an offer, and no register at all.
type primaryFixture struct {
	pool     *pgxpool.Pool
	schemeID string
	sebiRef  string

	schemeRef [32]byte

	offerID string

	// Bidders. Index i is one investor with one bid.
	investorIDs []string
	anchors     [][32]byte
	wallets     []string
	dematIDs    []string
	bankIDs     []string

	// The manager, which never bids.
	imInvestorID string
	imWallet     string
}

var primaryPepper = []byte("e2e-primary-pepper-never-used-in-production")

// Offer dates, fixed so nothing depends on when the test runs.
var (
	offerOpens    = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	offerCloses   = time.Date(2026, 5, 8, 17, 0, 0, 0, time.UTC)
	allotmentDue  = time.Date(2026, 5, 12, 17, 0, 0, 0, time.UTC)
	bookFrozenAt  = time.Date(2026, 5, 9, 11, 0, 0, 0, time.UTC)
	ballotDrawnAt = time.Date(2026, 5, 11, 11, 0, 0, 0, time.UTC)
	settledAt     = time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
)

func newPrimaryFixture(t *testing.T) *primaryFixture {
	t.Helper()
	pool := pgPool(t)
	ctx := context.Background()

	seq := schemeSeq.Add(1)
	sebiRef := fmt.Sprintf("SEBI/E2E-PM/%s/%d/%d", t.Name(), os.Getpid(), seq)

	f := &primaryFixture{pool: pool, sebiRef: sebiRef}
	f.schemeRef = sha256.Sum256([]byte(sebiRef))

	if err := pool.QueryRow(ctx, `
		INSERT INTO schemes (
			sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
			total_units, im_units, public_units, environment_tag
		) VALUES ($1, 'AcreSync Primary Market E2E', 50000000000, 100000000, $2, $3, $4, 'LOCAL')
		RETURNING id`,
		sebiRef, primaryTotal, primaryIMUnits, primaryOnOffer,
	).Scan(&f.schemeID); err != nil {
		t.Fatalf("creating the scheme: %v", err)
	}

	t.Cleanup(func() { f.teardown(ctx) })

	// The manager first, mirroring the real order: recordImSubscription runs before the public ballot so
	// the allocation engine starts from a clean 475-unit pool.
	f.imInvestorID = f.newInvestor(ctx, t, -1, true)

	// Deliberately outside the bidder range. The first version of this fixture used 0xaa, which is 170
	// in decimal and therefore the same address as bidder 169, and the settlement guard caught it: a
	// public allottee credited to the manager's wallet would be credited to an address excluded from the
	// holder count, vanishing from the statutory tally while still being owed a distribution.
	f.imWallet = fmt.Sprintf("0x%040x", 0xdeadbeef)

	for i := 0; i < primaryBidders; i++ {
		id := f.newInvestor(ctx, t, i, false)

		f.investorIDs = append(f.investorIDs, id)
		f.anchors = append(f.anchors, anchorFor(primaryPepper, id, sebiRef))
		f.wallets = append(f.wallets, fmt.Sprintf("0x%040x", i+1))
		f.dematIDs = append(f.dematIDs, f.newDemat(ctx, t, id, i))
		f.bankIDs = append(f.bankIDs, f.newBank(ctx, t, id, i))

		if _, err := pool.Exec(ctx, `
			INSERT INTO investor_anchors (investor_id, scheme_id, anchor_hash, pepper_key_id, algo_version)
			VALUES ($1, $2, $3, 'kms://test/pepper/v1', 1)`,
			id, f.schemeID, f.anchors[i][:],
		); err != nil {
			t.Fatalf("anchoring investor %d: %v", i, err)
		}
	}

	return f
}

func (f *primaryFixture) newInvestor(ctx context.Context, t *testing.T, i int, isManager bool) string {
	t.Helper()

	class := "RESIDENT_IND"
	if isManager {
		class = "BODY_CORPORATE"
	}

	var id string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO investors (
			full_name_enc, pan_enc, email_enc, phone_enc,
			investor_class, is_im_related, is_synthetic
		) VALUES ($1, $2, $3, $4, $5, $6, TRUE)
		RETURNING id`,
		[]byte(fmt.Sprintf("enc-name-pm-%d", i)),
		[]byte(fmt.Sprintf("enc-pan-pm-%d", i)),
		[]byte(fmt.Sprintf("enc-email-pm-%d", i)),
		[]byte(fmt.Sprintf("enc-phone-pm-%d", i)),
		class, isManager,
	).Scan(&id); err != nil {
		t.Fatalf("creating investor %d: %v", i, err)
	}
	return id
}

// newDemat creates the depository account a bid is required to name.
//
// bids.demat_account_id is NOT NULL, which is the schema insisting that units have somewhere to be
// credited before money is blocked for them.
func (f *primaryFixture) newDemat(ctx context.Context, t *testing.T, investorID string, i int) string {
	t.Helper()

	var id string
	// dp_id plus client_id is uniquely constrained, so it varies by process as well as by index or a
	// rerun collides with rows a previous run failed to clean up.
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO demat_accounts (investor_id, depository, dp_id, client_id, verified_at)
		VALUES ($1, 'CDSL', $2, $3, now())
		RETURNING id`,
		investorID,
		fmt.Sprintf("IN%06d", os.Getpid()%1000000),
		fmt.Sprintf("%08d%04d", i, os.Getpid()%10000),
	).Scan(&id); err != nil {
		t.Fatalf("creating a demat account for investor %d: %v", i, err)
	}
	return id
}

func (f *primaryFixture) newBank(ctx context.Context, t *testing.T, investorID string, i int) string {
	t.Helper()

	var id string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO bank_accounts (investor_id, account_number_enc, ifsc, account_name_enc, verified_at)
		VALUES ($1, $2, 'HDFC0001234', $3, now())
		RETURNING id`,
		investorID,
		[]byte(fmt.Sprintf("enc-acct-pm-%d", i)),
		[]byte(fmt.Sprintf("enc-acctname-pm-%d", i)),
	).Scan(&id); err != nil {
		t.Fatalf("creating a bank account for investor %d: %v", i, err)
	}
	return id
}

// allInvestorIDs includes the manager, which the bidder slice deliberately does not.
func (f *primaryFixture) allInvestorIDs() []string {
	return append(append([]string{}, f.investorIDs...), f.imInvestorID)
}

// teardown removes the fixture in dependency order.
//
// Same shape and the same reasoning as the distribution fixture's: deletion rather than truncation so a
// concurrent test elsewhere is unaffected, statements grouped by parameter count so a bind-count error
// cannot hide behind discarded errors, and triggers suppressed because holding_ledger is append-only and
// refuses DELETE by design.
func (f *primaryFixture) teardown(ctx context.Context) {
	schemeScoped := []string{
		`DELETE FROM allocations WHERE ballot_run_id IN (
			SELECT br.id FROM ballot_runs br JOIN offers o ON o.id = br.offer_id
			WHERE o.scheme_id = $1)`,
		`DELETE FROM ballot_runs WHERE offer_id IN (SELECT id FROM offers WHERE scheme_id = $1)`,
		`DELETE FROM asba_blocks WHERE bid_id IN (
			SELECT b.id FROM bids b JOIN offers o ON o.id = b.offer_id WHERE o.scheme_id = $1)`,
		`DELETE FROM bids WHERE offer_id IN (SELECT id FROM offers WHERE scheme_id = $1)`,
		`DELETE FROM offers WHERE scheme_id = $1`,
		`DELETE FROM holding_ledger WHERE scheme_id = $1`,
		`DELETE FROM unit_holdings WHERE scheme_id = $1`,
		`DELETE FROM depository_register WHERE scheme_id = $1`,
		`DELETE FROM investor_anchors WHERE scheme_id = $1`,
		`DELETE FROM chain_outbox WHERE scheme_id = $1`,
		`DELETE FROM ipfs_pins WHERE scheme_id = $1`,
	}

	investorScoped := []string{
		`DELETE FROM demat_accounts WHERE investor_id = ANY($1)`,
		`DELETE FROM bank_accounts WHERE investor_id = ANY($1)`,
		`DELETE FROM investors WHERE id = ANY($1)`,
	}

	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "primary e2e teardown: acquiring a connection: %v\n", err)
		return
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		fmt.Fprintf(os.Stderr, "primary e2e teardown: could not suppress triggers: %v\n", err)
	}

	for _, s := range schemeScoped {
		if _, err := conn.Exec(ctx, s, f.schemeID); err != nil {
			fmt.Fprintf(os.Stderr, "primary e2e teardown: %v\n  statement: %s\n", err, s)
		}
	}
	for _, s := range investorScoped {
		if _, err := conn.Exec(ctx, s, f.allInvestorIDs()); err != nil {
			fmt.Fprintf(os.Stderr, "primary e2e teardown: %v\n  statement: %s\n", err, s)
		}
	}
	if _, err := conn.Exec(ctx, `DELETE FROM schemes WHERE id = $1`, f.schemeID); err != nil {
		fmt.Fprintf(os.Stderr, "primary e2e teardown: %v\n", err)
	}

	if _, err := conn.Exec(ctx, `SET session_replication_role = origin`); err != nil {
		fmt.Fprintf(os.Stderr, "primary e2e teardown: could not restore triggers: %v\n", err)
	}
}

// fixedBusinessClock is business time frozen at an instant.
//
// The clock package deliberately exposes no such constructor: business time is stored, advanced through
// a controller, and read back, so a fixed implementation belongs to tests rather than to the package. The
// ASBA provider only needs to stamp the instant a block was placed, and pinning it keeps the recorded
// timestamps independent of when the suite runs.
type fixedBusinessClock struct{ at time.Time }

func (c fixedBusinessClock) Now(context.Context) (time.Time, error) { return c.at, nil }
