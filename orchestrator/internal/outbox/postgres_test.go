package outbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests run against a real Postgres.
//
// The behaviours that matter here are the ones a fake cannot reproduce: FOR UPDATE SKIP LOCKED
// under genuine concurrency, partial unique indexes, and CHECK constraints firing. Skipped rather
// than failed when no database is configured, so the rest of the suite stays runnable.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("ACRESYNC_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("ACRESYNC_DATABASE_URL")
	}
	if url == "" {
		t.Skip("no ACRESYNC_DATABASE_URL; skipping Postgres store tests")
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

// pgStore returns a store plus a scheme row to hang entries off.
//
// chain_outbox has a foreign key to schemes, so a real scheme is required. Each test gets its own
// so they cannot interfere, and the scheme is scoped by a unique SEBI reference rather than by
// truncating tables, which would fight with any concurrently running test.
// schemeSeq makes every scheme reference unique, including when one test creates two.
//
// sebi_scheme_ref is uniquely constrained, correctly: two schemes sharing a SEBI registration would
// be a data error worth rejecting. Deriving the reference from the test name alone therefore breaks
// as soon as a test needs a second scheme, which the isolation test does.
var schemeSeq atomic.Int64

func pgStore(t *testing.T) (*PostgresStore, string) {
	t.Helper()
	pool := pgPool(t)
	ctx := context.Background()

	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO schemes (
			sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
			total_units, im_units, public_units, environment_tag
		) VALUES ($1, 'Outbox Test Scheme', 50000000000, 100000000, 500, 25, 475, 'LOCAL')
		RETURNING id`,
		fmt.Sprintf("SEBI/TEST/%s/%d/%d", t.Name(), os.Getpid(), schemeSeq.Add(1)),
	).Scan(&id)
	if err != nil {
		t.Fatalf("creating a test scheme: %v", err)
	}

	t.Cleanup(func() {
		// Outbox rows first: they reference the scheme.
		_, _ = pool.Exec(ctx, `DELETE FROM chain_outbox WHERE scheme_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM schemes WHERE id = $1`, id)
	})

	return NewPostgresStore(pool), id
}

func pgEnqueue(t *testing.T, s *PostgresStore, schemeID string, n int) *Entry {
	t.Helper()
	e, err := s.Enqueue(context.Background(), NewEntry{
		SchemeID:       schemeID,
		TargetContract: "0x00000000000000000000000000000000000000c1",
		FunctionName:   "anchorPeriod",
		Payload:        []byte(fmt.Sprintf(`{"periodId":%d}`, n)),
		IdempotencyKey: key(n),
		EnvironmentTag: "LOCAL",
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ---------------------------------------------------------------------------
// Round trip
// ---------------------------------------------------------------------------

func TestPG_EnqueueAndGet(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	e := pgEnqueue(t, s, schemeID, 1)
	if e.Status != StatusQueued {
		t.Fatalf("status = %s, want QUEUED", e.Status)
	}
	if e.PayloadHash == [32]byte{} {
		t.Error("payload hash should be computed on enqueue")
	}

	got, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdempotencyKey != e.IdempotencyKey {
		t.Error("idempotency key did not round trip")
	}
	if string(got.Payload) == "" {
		t.Error("payload did not round trip")
	}

	byKey, err := s.GetByIdempotencyKey(ctx, key(1))
	if err != nil {
		t.Fatal(err)
	}
	if byKey.ID != e.ID {
		t.Error("lookup by idempotency key returned the wrong entry")
	}
}

func TestPG_DuplicateKeyRejected(t *testing.T) {
	s, schemeID := pgStore(t)
	pgEnqueue(t, s, schemeID, 1)

	_, err := s.Enqueue(context.Background(), NewEntry{
		SchemeID:       schemeID,
		TargetContract: "0x00000000000000000000000000000000000000c1",
		FunctionName:   "anchorPeriod",
		Payload:        []byte(`{"periodId":1}`),
		IdempotencyKey: key(1),
		EnvironmentTag: "LOCAL",
	})
	if !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey, got %v", err)
	}
}

func TestPG_NotFound(t *testing.T) {
	s, _ := pgStore(t)
	_, err := s.Get(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// SKIP LOCKED under genuine concurrency
// ---------------------------------------------------------------------------

// The reason the claim is written as a CTE with FOR UPDATE SKIP LOCKED.
//
// Ten goroutines on separate pool connections claim simultaneously. Every entry must go to exactly
// one worker: a row handed to two would mean two signed transactions for one anchor, one of which
// gets silently dropped while the orchestrator believes it made both.
func TestPG_ConcurrentClaimsDoNotOverlap(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	const entries = 10
	for i := range entries {
		pgEnqueue(t, s, schemeID, i)
	}

	var wg sync.WaitGroup
	claimed := make([]*Entry, entries)
	errs := make([]error, entries)

	for i := range entries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A distinct nonce each, so the unique index is not what provides the isolation.
			// SKIP LOCKED has to be doing the work.
			claimed[i], errs[i] = s.ClaimNextQueued(ctx, schemeID, uint64(1000+i))
		}(i)
	}
	wg.Wait()

	seenEntry := map[string]int{}
	got := 0
	for i := range entries {
		if errs[i] != nil {
			t.Errorf("worker %d: %v", i, errs[i])
			continue
		}
		if claimed[i] == nil {
			continue // every candidate was locked at that instant, which is legitimate
		}
		got++
		if prior, dup := seenEntry[claimed[i].ID]; dup {
			t.Errorf("entry %s claimed by both worker %d and worker %d",
				claimed[i].ID, prior, i)
		}
		seenEntry[claimed[i].ID] = i
	}

	t.Logf("%d of %d entries claimed without overlap", got, entries)
	if got == 0 {
		t.Fatal("no worker claimed anything; SKIP LOCKED may be skipping every row")
	}

	// Nothing may be left SIGNED twice, and every claim must hold a distinct nonce.
	signed, err := s.ListByStatus(ctx, schemeID, StatusSigned)
	if err != nil {
		t.Fatal(err)
	}
	nonces := map[uint64]string{}
	for _, e := range signed {
		if e.Nonce == nil {
			t.Errorf("entry %s is SIGNED with no nonce", e.ID)
			continue
		}
		if prior, dup := nonces[*e.Nonce]; dup {
			t.Errorf("nonce %d held by both %s and %s", *e.Nonce, prior, e.ID)
		}
		nonces[*e.Nonce] = e.ID
	}
}

// Two workers reaching for the same nonce must not both succeed. The partial unique index from
// migration 0007 is what stops them, and it surfaces as ErrNonceInUse rather than a deadlock.
func TestPG_SameNonceRejected(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	pgEnqueue(t, s, schemeID, 1)
	pgEnqueue(t, s, schemeID, 2)

	first, err := s.ClaimNextQueued(ctx, schemeID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("expected to claim an entry")
	}

	_, err = s.ClaimNextQueued(ctx, schemeID, 42)
	if !errors.Is(err, ErrNonceInUse) {
		t.Fatalf("want ErrNonceInUse for a reused nonce, got %v", err)
	}
}

func TestPG_ClaimIsFIFO(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	first := pgEnqueue(t, s, schemeID, 1)
	pgEnqueue(t, s, schemeID, 2)

	got, err := s.ClaimNextQueued(ctx, schemeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID {
		t.Errorf("claimed %s, want the oldest entry %s", got.ID, first.ID)
	}
	if got.AttemptCount != 1 {
		t.Errorf("attempt count = %d, want 1", got.AttemptCount)
	}
}

func TestPG_ClaimReturnsNilWhenEmpty(t *testing.T) {
	s, schemeID := pgStore(t)
	got, err := s.ClaimNextQueued(context.Background(), schemeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("expected nil when nothing is queued")
	}
}

// ---------------------------------------------------------------------------
// The transition graph, enforced in SQL
// ---------------------------------------------------------------------------

// The precondition lives in the UPDATE's WHERE clause, so it holds for a second process or a psql
// session, not only for code that remembers to check.
func TestPG_InvalidTransitionRejected(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	e := pgEnqueue(t, s, schemeID, 1)

	// Broadcasting something never signed has no nonce and no transaction.
	if err := s.MarkBroadcast(ctx, e.ID, "0x"+fmt.Sprintf("%064d", 1), "1000"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
}

func TestPG_FullHappyPath(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	e := pgEnqueue(t, s, schemeID, 1)

	claimed, err := s.ClaimNextQueued(ctx, schemeID, 7)
	if err != nil {
		t.Fatal(err)
	}
	txHash := "0x" + fmt.Sprintf("%064x", 0xabc)

	if err := s.MarkBroadcast(ctx, claimed.ID, txHash, "1500000000"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPending(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirming(ctx, claimed.ID, 500, 2); err != nil {
		t.Fatal(err)
	}

	// The database refuses a CONFIRMED row below five confirmations, which is the constraint the
	// fiat gate depends on.
	if err := s.MarkConfirmed(ctx, claimed.ID, 500, 3); err == nil {
		t.Error("Postgres should refuse a CONFIRMED row with only 3 confirmations")
	} else {
		t.Logf("below-depth confirmation correctly refused: %v", err)
	}

	if err := s.MarkConfirmed(ctx, claimed.ID, 500, 5); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusConfirmed {
		t.Fatalf("status = %s, want CONFIRMED", got.Status)
	}
	if !got.ReadyForFiat(5) {
		t.Fatal("a confirmed entry with five confirmations must satisfy the fiat gate")
	}
	if got.ConfirmedAt == nil || got.SubmittedAt == nil {
		t.Error("timestamps should be recorded")
	}
}

// A confirmed anchor may already have had fiat released against it, so nothing may move it.
func TestPG_ConfirmedIsTerminal(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	e := pgEnqueue(t, s, schemeID, 1)
	claimed, _ := s.ClaimNextQueued(ctx, schemeID, 0)
	txHash := "0x" + fmt.Sprintf("%064x", 0xdef)
	if err := s.MarkBroadcast(ctx, claimed.ID, txHash, "1000"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirming(ctx, claimed.ID, 100, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirmed(ctx, claimed.ID, 100, 6); err != nil {
		t.Fatal(err)
	}

	for _, attempt := range []struct {
		name string
		fn   func() error
	}{
		{"requeue", func() error { return s.Requeue(ctx, e.ID, "no") }},
		{"reorg", func() error { return s.MarkReorged(ctx, e.ID, "no") }},
		{"fail", func() error { return s.MarkFailed(ctx, e.ID, "no") }},
		{"dead letter", func() error { return s.DeadLetter(ctx, e.ID, "no") }},
		{"cancel", func() error { return s.Cancel(ctx, e.ID) }},
	} {
		if err := attempt.fn(); err == nil {
			t.Errorf("%s should be refused on a confirmed entry", attempt.name)
		}
	}
}

// ---------------------------------------------------------------------------
// Reorg
// ---------------------------------------------------------------------------

func TestPG_ReorgThenRequeue(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	e := pgEnqueue(t, s, schemeID, 1)
	claimed, _ := s.ClaimNextQueued(ctx, schemeID, 9)
	txHash := "0x" + fmt.Sprintf("%064x", 0x1234)
	if err := s.MarkBroadcast(ctx, claimed.ID, txHash, "1000"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirming(ctx, claimed.ID, 800, 2); err != nil {
		t.Fatal(err)
	}

	// Skipping the reorged state is refused, so the fact that this anchor was mined and then
	// unmined cannot be erased.
	if err := s.Requeue(ctx, e.ID, "skipping REORGED"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("CONFIRMING should not requeue directly, got %v", err)
	}

	if err := s.MarkReorged(ctx, e.ID, "block 800 is no longer canonical"); err != nil {
		t.Fatal(err)
	}

	mid, _ := s.Get(ctx, e.ID)
	// Migration 0011 also enforces this with a CHECK: a reorged row carrying a confirmation count
	// is the thing most likely to be misread as still valid.
	if mid.BlockNumber != nil || mid.Confirmations != 0 {
		t.Error("a reorged entry must not retain block evidence")
	}

	if err := s.Requeue(ctx, e.ID, "reorged"); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(ctx, e.ID)
	if got.Status != StatusQueued {
		t.Fatalf("status = %s, want QUEUED", got.Status)
	}
	if got.Nonce != nil {
		t.Error("the nonce must be released so the entry is re-signed rather than rebroadcast")
	}
	if got.TxHash != "" {
		t.Error("the tx hash must be cleared")
	}
	// The ceiling has to survive requeues or a permanently failing entry retries forever.
	if got.AttemptCount != 1 {
		t.Errorf("attempt count = %d, want the original 1 preserved", got.AttemptCount)
	}

	// Reclaimable with a fresh nonce.
	again, err := s.ClaimNextQueued(ctx, schemeID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if again == nil || *again.Nonce != 10 {
		t.Fatal("a requeued entry should be claimable with a new nonce")
	}
	if again.AttemptCount != 2 {
		t.Errorf("attempt count = %d, want 2", again.AttemptCount)
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func TestPG_CancelOnlyBeforeBroadcast(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	queued := pgEnqueue(t, s, schemeID, 1)
	if err := s.Cancel(ctx, queued.ID); err != nil {
		t.Fatalf("a queued entry should be cancellable: %v", err)
	}
	got, _ := s.Get(ctx, queued.ID)
	if got.Status != StatusCancelled || got.CancelledAt == nil {
		t.Fatal("cancellation not recorded")
	}
	// Migration 0011 refuses a cancelled row holding a nonce, because that would leave a gap in
	// the sequence and stall everything behind it.
	if got.Nonce != nil {
		t.Error("a cancelled entry must not hold a nonce")
	}

	signedEntry := pgEnqueue(t, s, schemeID, 2)
	if _, err := s.ClaimNextQueued(ctx, schemeID, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, signedEntry.ID); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("want ErrNotCancellable for a signed entry, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Nonce recovery
// ---------------------------------------------------------------------------

func TestPG_InFlightOrderedByNonce(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	for i := range 3 {
		pgEnqueue(t, s, schemeID, i)
	}
	for i := range 3 {
		e, err := s.ClaimNextQueued(ctx, schemeID, uint64(20+i))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MarkBroadcast(ctx, e.ID, "0x"+fmt.Sprintf("%064x", 0x100+i), "1000"); err != nil {
			t.Fatal(err)
		}
	}

	inFlight, err := s.InFlight(ctx, schemeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inFlight) != 3 {
		t.Fatalf("got %d in-flight entries, want 3", len(inFlight))
	}
	for i, e := range inFlight {
		if *e.Nonce != uint64(20+i) {
			t.Errorf("entry %d has nonce %d, want %d (must be ordered)", i, *e.Nonce, 20+i)
		}
	}
}

// A confirmed entry has released its nonce. If it still appeared as in-flight, nonce recovery
// after a restart would skip that slot forever and the sequence would drift.
func TestPG_ConfirmedNotInFlight(t *testing.T) {
	s, schemeID := pgStore(t)
	ctx := context.Background()

	pgEnqueue(t, s, schemeID, 1)
	e, _ := s.ClaimNextQueued(ctx, schemeID, 0)
	if err := s.MarkBroadcast(ctx, e.ID, "0x"+fmt.Sprintf("%064x", 0x999), "1000"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirming(ctx, e.ID, 300, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirmed(ctx, e.ID, 300, 7); err != nil {
		t.Fatal(err)
	}

	inFlight, _ := s.InFlight(ctx, schemeID)
	if len(inFlight) != 0 {
		t.Fatalf("a confirmed entry must not be in-flight, got %d", len(inFlight))
	}
}

// ---------------------------------------------------------------------------
// Isolation
// ---------------------------------------------------------------------------

// Nonces are a single sequence per relayer per scheme. A claim crossing schemes would corrupt both.
func TestPG_SchemesAreIsolated(t *testing.T) {
	s, schemeA := pgStore(t)
	_, schemeB := pgStore(t)
	ctx := context.Background()

	pgEnqueue(t, s, schemeA, 1)

	got, err := s.ClaimNextQueued(ctx, schemeB, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("a claim for one scheme must not take another scheme's work")
	}

	list, _ := s.ListByStatus(ctx, schemeA, StatusQueued)
	if len(list) != 1 {
		t.Fatalf("the first scheme should still have its queued entry, got %d", len(list))
	}
}

// ---------------------------------------------------------------------------
// Parity with the in-memory store
// ---------------------------------------------------------------------------

// The relayer is tested against the memory store and runs against Postgres, so any divergence in
// error semantics would only surface in production. This checks the cases the relayer actually
// branches on.
func TestPG_ErrorSemanticsMatchMemoryStore(t *testing.T) {
	pg, schemeID := pgStore(t)
	mem := NewMemoryStore()
	ctx := context.Background()

	type storeCase struct {
		name  string
		store Store
		sid   string
	}
	cases := []storeCase{
		{"postgres", pg, schemeID},
		{"memory", mem, schemeID},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := c.store.Enqueue(ctx, NewEntry{
				SchemeID:       c.sid,
				TargetContract: "0x00000000000000000000000000000000000000c1",
				FunctionName:   "anchorPeriod",
				Payload:        []byte(`{"periodId":99}`),
				IdempotencyKey: key(99),
				EnvironmentTag: "LOCAL",
			})
			if err != nil {
				t.Fatal(err)
			}

			// Duplicate key.
			if _, err := c.store.Enqueue(ctx, NewEntry{
				SchemeID:       c.sid,
				TargetContract: "0x00000000000000000000000000000000000000c1",
				FunctionName:   "anchorPeriod",
				Payload:        []byte(`{"periodId":99}`),
				IdempotencyKey: key(99),
				EnvironmentTag: "LOCAL",
			}); !errors.Is(err, ErrDuplicateKey) {
				t.Errorf("duplicate key: want ErrDuplicateKey, got %v", err)
			}

			// Invalid transition.
			if err := c.store.MarkBroadcast(ctx, e.ID, "0x"+fmt.Sprintf("%064d", 2), "1"); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("invalid transition: want ErrInvalidTransition, got %v", err)
			}

			// Nonce reuse.
			if _, err := c.store.ClaimNextQueued(ctx, c.sid, 500); err != nil {
				t.Fatal(err)
			}
			if _, err := c.store.Enqueue(ctx, NewEntry{
				SchemeID:       c.sid,
				TargetContract: "0x00000000000000000000000000000000000000c1",
				FunctionName:   "anchorPeriod",
				Payload:        []byte(`{"periodId":98}`),
				IdempotencyKey: key(98),
				EnvironmentTag: "LOCAL",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.store.ClaimNextQueued(ctx, c.sid, 500); !errors.Is(err, ErrNonceInUse) {
				t.Errorf("nonce reuse: want ErrNonceInUse, got %v", err)
			}
		})
	}
}
