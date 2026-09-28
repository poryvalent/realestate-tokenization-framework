package devsim

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/outbox"
)

func testTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("ACRESYNC_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("no ACRESYNC_TEST_DATABASE_URL; skipping devsim integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return ctx, tx
}

var seq int

func scheme(t *testing.T, ctx context.Context, tx pgx.Tx, env string) string {
	t.Helper()
	seq++
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemes (sebi_scheme_ref, name, asset_value_paise, unit_price_paise, total_units, im_units,
		                     public_units, min_public_holders, chain_id, roles_address, ballot_address,
		                     scheme_address, environment_tag)
		VALUES ($1, 'devsim test', 50000000000, 100000000, 500, 25, 475, 200, 11155111,
		        '0x1111111111111111111111111111111111111111', '0x2222222222222222222222222222222222222222',
		        '0x3333333333333333333333333333333333333333', $2::environment_tag)
		RETURNING id`, fmt.Sprintf("DEVSIM/%d/%d", time.Now().UnixNano(), seq), env).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func enqueue(t *testing.T, ctx context.Context, tx pgx.Tx, schemeID, env, fn string) string {
	t.Helper()
	seq++
	var k idempotency.Key
	k = sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d/%d", schemeID, fn, seq, time.Now().UnixNano())))
	e, err := outbox.EnqueueWith(ctx, tx, outbox.NewEntry{
		SchemeID: schemeID, TargetContract: "0x2222222222222222222222222222222222222222", FunctionName: fn,
		Payload: []byte(`{}`), IdempotencyKey: k, EnvironmentTag: env,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

// TestConfirmOnlyTouchesLocalRows is the property that makes the faker safe to have in the repository.
func TestConfirmOnlyTouchesLocalRows(t *testing.T) {
	ctx, tx := testTx(t)
	local := scheme(t, ctx, tx, "LOCAL")
	real := scheme(t, ctx, tx, "SEPOLIA_SIM")
	enqueue(t, ctx, tx, local, "LOCAL", "anchorBidbook")
	enqueue(t, ctx, tx, local, "LOCAL", "commitSeed")
	sepolia := enqueue(t, ctx, tx, real, "SEPOLIA_SIM", "anchorBidbook")

	// Even when pointed straight at the Sepolia scheme, nothing is confirmed.
	got, err := Confirm(ctx, tx, Options{Head: 1000, SchemeID: real})
	if err != nil || len(got) != 0 {
		t.Fatalf("confirmed %v (%v) on a SEPOLIA_SIM scheme", got, err)
	}

	got, err = Confirm(ctx, tx, Options{Head: 1000, SchemeID: local})
	if err != nil {
		t.Fatal(err)
	}
	// Both rows were created in this one transaction, so created_at ties and their order is not asserted.
	names := map[string]bool{}
	for _, c := range got {
		names[c.FunctionName] = true
	}
	if len(got) != 2 || !names["anchorBidbook"] || !names["commitSeed"] {
		t.Fatalf("confirmed %v, want both LOCAL rows", got)
	}

	var status string
	if err := tx.QueryRow(ctx, `SELECT status::text FROM chain_outbox WHERE id = $1`, sepolia).Scan(&status); err != nil || status != "QUEUED" {
		t.Fatalf("the Sepolia row is %s (%v), want QUEUED", status, err)
	}

	// The rows satisfy the schema's confirmation evidence, on distinct nonces, backdated for the ceremony.
	var n, nonces int
	var block int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT nonce), max(block_number) FROM chain_outbox
		 WHERE scheme_id = $1 AND status = 'CONFIRMED' AND confirmations >= 5 AND tx_hash IS NOT NULL`, local).
		Scan(&n, &nonces, &block); err != nil {
		t.Fatal(err)
	}
	if n != 2 || nonces != 2 || block != 1000-Backdate {
		t.Fatalf("%d confirmed rows on %d nonces at block %d, want 2 on 2 at %d", n, nonces, block, 1000-Backdate)
	}

	// Nothing left to do.
	if again, err := Confirm(ctx, tx, Options{Head: 1000, SchemeID: local}); err != nil || len(again) != 0 {
		t.Fatalf("a second pass confirmed %v (%v)", again, err)
	}
}

// TestALostNonceRaceIsRetried is devseed and devconfirm -watch confirming at the same moment, forced.
//
// A second connection confirms row B on nonce 0 and holds its transaction open. Confirm then takes row A, whose
// statement cannot see the uncommitted nonce, so it also picks 0 and blocks on the unique index. When the other
// transaction commits, A's statement fails with a unique violation, and only the retry turns that into nonce 1.
//
// Committed rather than rolled back, because the race is between two connections. Deleted afterwards.
func TestALostNonceRaceIsRetried(t *testing.T) {
	url := os.Getenv("ACRESYNC_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("no ACRESYNC_TEST_DATABASE_URL; skipping devsim integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	commit := func(f func(pgx.Tx) string) string {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		v := f(tx)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return v
	}
	local := commit(func(tx pgx.Tx) string { return scheme(t, ctx, tx, "LOCAL") })
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM chain_outbox WHERE scheme_id = $1`, local)
		_, _ = pool.Exec(context.Background(), `DELETE FROM schemes WHERE id = $1`, local)
	})
	a := commit(func(tx pgx.Tx) string { return enqueue(t, ctx, tx, local, "LOCAL", "first") })
	time.Sleep(10 * time.Millisecond) // so A is strictly older and Confirm takes it first
	b := commit(func(tx pgx.Tx) string { return enqueue(t, ctx, tx, local, "LOCAL", "second") })

	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(ctx, `
		UPDATE chain_outbox SET status = 'CONFIRMED', tx_hash = '0x' || repeat('cd', 32), block_number = 1,
		       confirmations = 12, confirmed_at = now(), nonce = 0
		 WHERE id = $1`, b); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Confirm(ctx, pool, Options{Head: 1000, SchemeID: local})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond) // Confirm is now blocked on nonce 0
	if err := other.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Confirm failed on a lost nonce race instead of retrying: %v", err)
	}

	var nonce int64
	var status string
	if err := pool.QueryRow(ctx, `SELECT status::text, nonce FROM chain_outbox WHERE id = $1`, a).Scan(&status, &nonce); err != nil {
		t.Fatal(err)
	}
	if status != "CONFIRMED" || nonce != 1 {
		t.Fatalf("row A is %s on nonce %d, want CONFIRMED on nonce 1", status, nonce)
	}
}

// TestConfirmWaitsForMinAge lets a UI show its waiting state.
func TestConfirmWaitsForMinAge(t *testing.T) {
	ctx, tx := testTx(t)
	local := scheme(t, ctx, tx, "LOCAL")
	enqueue(t, ctx, tx, local, "LOCAL", "anchorBidbook")

	if got, err := Confirm(ctx, tx, Options{Head: 1000, SchemeID: local, MinAge: time.Hour}); err != nil || len(got) != 0 {
		t.Fatalf("confirmed %v (%v) before it was an hour old", got, err)
	}
}
