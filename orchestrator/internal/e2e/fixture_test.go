// Package e2e drives one full distribution period against a real Postgres.
//
// # Why this exists as its own package
//
// Every other test in the repository checks one component against its own contract. None of them can
// catch the failures that only appear when the pieces are joined: a figure computed in one package and
// rejected by a constraint in another, a document that validates in isolation and fails the allowlist
// once real data fills it, or a status the orchestrator believes in that the schema has no value for.
//
// Those are the failures that would surface during a live rehearsal, so they are worth a test that
// costs a database.
package e2e

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acresync/orchestrator/internal/entitlement"
	"github.com/acresync/orchestrator/internal/snapshot"
)

// Scheme shape, matching the v1 SM-REIT: ₹50 crore across 500 units at ₹10 lakh each.
const (
	totalUnits  = 500
	imUnits     = 25
	publicUnits = 475

	// publicHolders is the number of public register lines. With the manager that gives 202 lines and
	// 201 countable holders, which clears the statutory minimum of 200 by one.
	publicHolders = 201
)

var schemeSeq atomic.Int64

// fixture is a seeded scheme with a full register.
type fixture struct {
	pool     *pgxpool.Pool
	schemeID string

	spvID      string
	propertyID string
	leaseID    string

	// investorIDs is indexed the same way as the register: index 0 is the manager.
	investorIDs []string
	wallets     []string
	anchors     [][32]byte
	units       []uint32
	excluded    []bool
	classes     []entitlement.InvestorClass
	bankIDs     []string

	schemeRef [32]byte
}

func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("ACRESYNC_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("ACRESYNC_DATABASE_URL")
	}
	if url == "" {
		t.Skip("no ACRESYNC_DATABASE_URL; skipping the end-to-end period test")
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

// anchorFor derives an investor anchor the way the real system does.
//
// HMAC-SHA256 over the internal UUID and the scheme reference, under a pepper. Not a plain hash of a
// PAN: the PAN space is small enough to enumerate, so a digest of one is reversible by brute force. The
// pepper here is a test constant; in production it never leaves KMS, and destroying it severs every
// linkage between on-chain anchors and identified investors at once, which is the only way a DPDP
// erasure request can be honoured against data already written to an immutable ledger.
func anchorFor(pepper []byte, investorID, schemeRef string) [32]byte {
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(investorID))
	m.Write([]byte("|"))
	m.Write([]byte(schemeRef))
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// newFixture seeds a scheme with a complete, reconciled register.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgPool(t)
	ctx := context.Background()

	seq := schemeSeq.Add(1)
	sebiRef := fmt.Sprintf("SEBI/E2E/%s/%d/%d", t.Name(), os.Getpid(), seq)

	f := &fixture{pool: pool}

	err := pool.QueryRow(ctx, `
		INSERT INTO schemes (
			sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
			total_units, im_units, public_units, environment_tag
		) VALUES ($1, 'AcreSync E2E Scheme', 50000000000, 100000000, $2, $3, $4, 'LOCAL')
		RETURNING id`,
		sebiRef, totalUnits, imUnits, publicUnits,
	).Scan(&f.schemeID)
	if err != nil {
		t.Fatalf("creating the scheme: %v", err)
	}

	t.Cleanup(func() { f.teardown(ctx) })

	// The asset chain. Properties hang off SPVs rather than schemes, which the schema enforces, so
	// reaching a scheme from a property takes the same join the legal structure does.
	if err := pool.QueryRow(ctx, `
		INSERT INTO spvs (scheme_id, cin, name, incorporation_date)
		VALUES ($1, $2, 'AcreSync E2E SPV One', '2024-04-01')
		RETURNING id`,
		// CIN is uniquely constrained, so it has to vary by process as well as by sequence, or a rerun
		// collides with a row a previous run failed to clean up.
		f.schemeID, fmt.Sprintf("U70100KA24PTC%03d%03d", os.Getpid()%1000, seq%1000),
	).Scan(&f.spvID); err != nil {
		t.Fatalf("creating the SPV: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO properties (
			spv_id, name, address_text, city, grade,
			carpet_area_sqft, leasable_area_sqft, acquisition_value_paise
		) VALUES ($1, 'Whitefield Tech Park', '1 Test Road', 'Bengaluru', 'A',
		          80000, 100000, 50000000000)
		RETURNING id`,
		f.spvID,
	).Scan(&f.propertyID); err != nil {
		t.Fatalf("creating the property: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO leases (property_id, tenant_name, monthly_rent_paise, start_date, end_date)
		VALUES ($1, 'Anchor Tenant Pvt Ltd', 11000000000, '2025-04-01', '2030-03-31')
		RETURNING id`,
		f.propertyID,
	).Scan(&f.leaseID); err != nil {
		t.Fatalf("creating the lease: %v", err)
	}

	f.schemeRef = sha256.Sum256([]byte(sebiRef))
	pepper := []byte("e2e-test-pepper-never-used-in-production")

	// Unit split: the manager holds 25, then 199 holders with 2 units each and one with 77, which
	// sums to 475 public units across 200 public lines. Plus the manager, 201 lines.
	//
	// Deliberately not a clean division. If every holder held the same number of units the
	// largest-remainder pass would never award a residue paise, and the exactness invariant would be
	// satisfied by accident rather than by the allocator working.
	unitPlan := make([]uint32, 0, publicHolders)
	unitPlan = append(unitPlan, imUnits)
	for i := 0; i < publicHolders-2; i++ {
		unitPlan = append(unitPlan, 2)
	}
	unitPlan = append(unitPlan, uint32(publicUnits-2*(publicHolders-2)))

	var sum uint32
	for _, u := range unitPlan {
		sum += u
	}
	if sum != totalUnits {
		t.Fatalf("fixture unit plan sums to %d, want %d", sum, totalUnits)
	}

	for i, u := range unitPlan {
		isManager := i == 0

		class := entitlement.ClassResidentIndividual
		switch {
		case isManager:
			class = entitlement.ClassBodyCorporate
		case i%37 == 0:
			// A handful of non-residents, so the withholding path is exercised with more than one rate
			// and the gross figures can be shown not to depend on tax class.
			class = entitlement.ClassNRI
		case i%23 == 0:
			class = entitlement.ClassHUF
		}

		var investorID string
		err := pool.QueryRow(ctx, `
			INSERT INTO investors (
				full_name_enc, pan_enc, email_enc, phone_enc,
				investor_class, is_im_related, is_synthetic
			) VALUES ($1, $2, $3, $4, $5, $6, TRUE)
			RETURNING id`,
			[]byte(fmt.Sprintf("enc-name-%d", i)),
			[]byte(fmt.Sprintf("enc-pan-%d", i)),
			[]byte(fmt.Sprintf("enc-email-%d", i)),
			[]byte(fmt.Sprintf("enc-phone-%d", i)),
			string(class), isManager,
		).Scan(&investorID)
		if err != nil {
			t.Fatalf("creating investor %d: %v", i, err)
		}

		wallet := fmt.Sprintf("0x%040x", i+1)
		anchor := anchorFor(pepper, investorID, sebiRef)

		if _, err := pool.Exec(ctx, `
			INSERT INTO investor_anchors (investor_id, scheme_id, anchor_hash, pepper_key_id, algo_version)
			VALUES ($1, $2, $3, 'kms://test/pepper/v1', 1)`,
			investorID, f.schemeID, anchor[:],
		); err != nil {
			t.Fatalf("anchoring investor %d: %v", i, err)
		}

		// The mirror.
		if _, err := pool.Exec(ctx, `
			INSERT INTO unit_holdings (
				scheme_id, investor_id, wallet_address, units, is_excluded_from_holder_count
			) VALUES ($1, $2, $3, $4, $5)`,
			f.schemeID, investorID, wallet, u, isManager,
		); err != nil {
			t.Fatalf("crediting investor %d: %v", i, err)
		}

		// The legal register. Seeded to agree with the mirror, so the period starts reconciled and a
		// divergence has to be introduced deliberately.
		if _, err := pool.Exec(ctx, `
			INSERT INTO depository_register (scheme_id, investor_id, wallet_address, units)
			VALUES ($1, $2, $3, $4)`,
			f.schemeID, investorID, wallet, u,
		); err != nil {
			t.Fatalf("seeding the depository for investor %d: %v", i, err)
		}

		var bankID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO bank_accounts (investor_id, account_number_enc, ifsc, account_name_enc, verified_at)
			VALUES ($1, $2, 'HDFC0001234', $3, now())
			RETURNING id`,
			investorID,
			[]byte(fmt.Sprintf("enc-acct-%d", i)),
			[]byte(fmt.Sprintf("enc-acctname-%d", i)),
		).Scan(&bankID); err != nil {
			t.Fatalf("creating a bank account for investor %d: %v", i, err)
		}

		f.investorIDs = append(f.investorIDs, investorID)
		f.wallets = append(f.wallets, wallet)
		f.anchors = append(f.anchors, anchor)
		f.units = append(f.units, u)
		f.excluded = append(f.excluded, isManager)
		f.classes = append(f.classes, class)
		f.bankIDs = append(f.bankIDs, bankID)
	}

	return f
}

// teardown removes the fixture in dependency order.
//
// Deletion rather than truncation, so a concurrently running test in another package is unaffected.
// Every foreign key in the schema is ON DELETE RESTRICT, which means the order here is not a
// convenience: getting it wrong fails rather than cascading, which is the behaviour a financial schema
// should have.
func (f *fixture) teardown(ctx context.Context) {
	// Statements are grouped by how many parameters they take.
	//
	// This was a bug worth recording: the first version passed both the scheme id and the investor
	// list to every statement, so Postgres rejected each one that used only the first parameter with a
	// bind-count error. Because teardown discards errors, the failure was invisible and showed up a run
	// later as a unique-constraint violation on leftover rows. Discarding errors during cleanup is
	// reasonable, and it means cleanup has to be correct by construction rather than by observation.
	schemeScoped := []string{
		`DELETE FROM payout_instructions WHERE entitlement_id IN (
			SELECT e.id FROM entitlements e
			JOIN distribution_periods p ON p.id = e.distribution_period_id
			WHERE p.scheme_id = $1)`,
		`DELETE FROM tax_deductions WHERE entitlement_id IN (
			SELECT e.id FROM entitlements e
			JOIN distribution_periods p ON p.id = e.distribution_period_id
			WHERE p.scheme_id = $1)`,
		// Before distribution_periods, which it references twice.
		//
		// Worth noting why this cannot be left out: teardown runs with session_replication_role set to
		// replica, so foreign keys are not enforced and omitting this does not fail loudly. The periods
		// get deleted anyway and the adjustment is silently orphaned, pointing at rows that no longer
		// exist. That was the observed behaviour before this line existed.
		`DELETE FROM carry_forward_adjustments WHERE source_period_id IN (
			SELECT id FROM distribution_periods WHERE scheme_id = $1)
		   OR target_period_id IN (
			SELECT id FROM distribution_periods WHERE scheme_id = $1)`,
		`DELETE FROM entitlements WHERE distribution_period_id IN (
			SELECT id FROM distribution_periods WHERE scheme_id = $1)`,
		`DELETE FROM register_snapshot_lines WHERE snapshot_id IN (
			SELECT id FROM register_snapshots WHERE scheme_id = $1)`,
		`DELETE FROM register_snapshots WHERE scheme_id = $1`,
		`DELETE FROM ndcf_line_items WHERE distribution_period_id IN (
			SELECT id FROM distribution_periods WHERE scheme_id = $1)`,
		`DELETE FROM rent_receipts WHERE distribution_period_id IN (
			SELECT id FROM distribution_periods WHERE scheme_id = $1)`,
		`DELETE FROM reconciliation_diffs WHERE reconciliation_run_id IN (
			SELECT id FROM reconciliation_runs WHERE scheme_id = $1)`,
		`DELETE FROM reconciliation_runs WHERE scheme_id = $1`,
		`DELETE FROM distribution_periods WHERE scheme_id = $1`,
		`DELETE FROM chain_outbox WHERE scheme_id = $1`,
		`DELETE FROM leases WHERE property_id IN (
			SELECT p.id FROM properties p JOIN spvs s ON s.id = p.spv_id WHERE s.scheme_id = $1)`,
		`DELETE FROM properties WHERE spv_id IN (SELECT id FROM spvs WHERE scheme_id = $1)`,
		`DELETE FROM spvs WHERE scheme_id = $1`,
		`DELETE FROM depository_register WHERE scheme_id = $1`,
		`DELETE FROM unit_holdings WHERE scheme_id = $1`,
		`DELETE FROM investor_anchors WHERE scheme_id = $1`,
	}

	investorScoped := []string{
		`DELETE FROM bank_accounts WHERE investor_id = ANY($1)`,
		`DELETE FROM investors WHERE id = ANY($1)`,
	}

	// Cleanup runs with triggers suppressed, which is necessary rather than convenient.
	//
	// register_snapshot_lines and holding_ledger are append-only, enforced by triggers that raise on
	// DELETE and tell the caller to record a compensating entry instead. That is the correct behaviour
	// for a register whose history is the only thing making a later divergence diagnosable, and it
	// means a test fixture cannot be removed through the normal path at all.
	//
	// session_replication_role = replica disables user triggers and foreign-key checks for this session
	// only. Confined to teardown: nothing in the test body runs with the guarantees relaxed, so the
	// append-only rules and every constraint are fully in force for everything being tested.
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: acquiring a connection: %v\n", err)
		return
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: could not suppress triggers: %v\n", err)
	}

	for _, s := range schemeScoped {
		if _, err := conn.Exec(ctx, s, f.schemeID); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: %v\n  statement: %s\n", err, s)
		}
	}
	for _, s := range investorScoped {
		if _, err := conn.Exec(ctx, s, f.investorIDs); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: %v\n  statement: %s\n", err, s)
		}
	}
	if _, err := conn.Exec(ctx, `DELETE FROM schemes WHERE id = $1`, f.schemeID); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: %v\n", err)
	}

	// Restored explicitly so a pooled connection is never handed back with triggers off.
	if _, err := conn.Exec(ctx, `SET session_replication_role = origin`); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: could not restore trigger enforcement: %v\n", err)
	}
}

// holdings builds the snapshot input from the seeded mirror.
func (f *fixture) holdings() []snapshot.Holding {
	out := make([]snapshot.Holding, 0, len(f.investorIDs))
	for i := range f.investorIDs {
		out = append(out, snapshot.Holding{
			InvestorID:              f.investorIDs[i],
			WalletAddress:           f.wallets[i],
			InvestorAnchor:          f.anchors[i],
			Units:                   f.units[i],
			ExcludedFromHolderCount: f.excluded[i],
		})
	}
	return out
}

// details builds the per-holder payment and tax data.
func (f *fixture) details() map[string]entitlement.HolderDetail {
	out := make(map[string]entitlement.HolderDetail, len(f.investorIDs))
	for i, id := range f.investorIDs {
		out[id] = entitlement.HolderDetail{
			InvestorID:    id,
			Class:         f.classes[i],
			BankAccountID: f.bankIDs[i],
			FundAccountID: fmt.Sprintf("fa_%014d", i+1),
		}
	}
	return out
}

// readMirror returns the register as the reconciler sees it.
func (f *fixture) readMirror(ctx context.Context, t *testing.T) []position {
	t.Helper()
	return f.readPositions(ctx, t, `
		SELECT investor_id, wallet_address, units FROM unit_holdings
		WHERE scheme_id = $1 ORDER BY wallet_address`)
}

// readDepository returns the legal register.
func (f *fixture) readDepository(ctx context.Context, t *testing.T) []position {
	t.Helper()
	return f.readPositions(ctx, t, `
		SELECT investor_id, wallet_address, units FROM depository_register
		WHERE scheme_id = $1 ORDER BY wallet_address`)
}

type position struct {
	investorID string
	wallet     string
	units      uint32
}

func (f *fixture) readPositions(ctx context.Context, t *testing.T, q string) []position {
	t.Helper()
	rows, err := f.pool.Query(ctx, q, f.schemeID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []position
	for rows.Next() {
		var p position
		var units int32
		if err := rows.Scan(&p.investorID, &p.wallet, &units); err != nil {
			t.Fatal(err)
		}
		p.units = uint32(units)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Period dates, fixed so nothing in this test depends on when it runs.
var (
	periodStart = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	recordDate  = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	takenAt     = time.Date(2026, 9, 30, 18, 30, 0, 0, time.UTC)
)
