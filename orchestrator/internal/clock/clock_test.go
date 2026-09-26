package clock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const schemeID = "aaaaaaaa-0000-0000-0000-000000000001"

func mustInit(t *testing.T, at time.Time) (*MemoryStore, Controller) {
	t.Helper()
	store := NewMemoryStore()
	if _, err := store.Init(context.Background(), schemeID, at); err != nil {
		t.Fatal(err)
	}
	return store, For(store, schemeID)
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestAdvanceForward(t *testing.T) {
	_, c := mustInit(t, ts("2026-08-01T00:00:00Z"))
	ctx := context.Background()

	// Thirty days of offer window, compressed.
	st, err := c.AdvanceTo(ctx, ts("2026-08-31T00:00:00Z"), "admin@acresync")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Current.Equal(ts("2026-08-31T00:00:00Z")) {
		t.Fatalf("current = %s", st.Current)
	}
	if st.LastAdvancedBy != "admin@acresync" {
		t.Fatalf("actor not recorded: %q", st.LastAdvancedBy)
	}

	now, err := c.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !now.Equal(ts("2026-08-31T00:00:00Z")) {
		t.Fatalf("Now = %s", now)
	}
}

func TestAdvanceBy(t *testing.T) {
	_, c := mustInit(t, ts("2026-08-31T00:00:00Z"))
	ctx := context.Background()

	// T+3 settlement.
	st, err := c.AdvanceBy(ctx, 72*time.Hour, "admin@acresync")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Current.Equal(ts("2026-09-03T00:00:00Z")) {
		t.Fatalf("current = %s, want 2026-09-03T00:00:00Z", st.Current)
	}
}

// Rewinding would invalidate every simulated_clock_value already stamped on a
// ledger entry, which is the audit trail's only record of ordering.
func TestRewindRejected(t *testing.T) {
	_, c := mustInit(t, ts("2026-09-01T00:00:00Z"))
	ctx := context.Background()

	if _, err := c.AdvanceTo(ctx, ts("2026-08-01T00:00:00Z"), "admin"); !errors.Is(err, ErrNotMonotonic) {
		t.Fatalf("want ErrNotMonotonic, got %v", err)
	}
	if _, err := c.AdvanceBy(ctx, -time.Hour, "admin"); !errors.Is(err, ErrNotMonotonic) {
		t.Fatalf("negative duration: want ErrNotMonotonic, got %v", err)
	}

	now, _ := c.Now(ctx)
	if !now.Equal(ts("2026-09-01T00:00:00Z")) {
		t.Fatalf("a rejected rewind must leave the clock untouched, got %s", now)
	}
}

func TestIdempotentAdvanceToSameInstant(t *testing.T) {
	_, c := mustInit(t, ts("2026-09-01T00:00:00Z"))
	ctx := context.Background()

	// Advancing to the instant already reached is a no-op rather than an error,
	// which is what makes the ADVANCE_CLOCK idempotency key behave: the same
	// target derives the same key and re-submitting changes nothing.
	if _, err := c.AdvanceTo(ctx, ts("2026-09-01T00:00:00Z"), "admin"); err != nil {
		t.Fatalf("advancing to the current instant should be permitted: %v", err)
	}
}

func TestFreeze(t *testing.T) {
	_, c := mustInit(t, ts("2026-09-01T00:00:00Z"))
	ctx := context.Background()

	if _, err := c.Freeze(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AdvanceTo(ctx, ts("2026-09-02T00:00:00Z"), "admin"); !errors.Is(err, ErrFrozen) {
		t.Fatalf("want ErrFrozen, got %v", err)
	}
	if _, err := c.Unfreeze(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AdvanceTo(ctx, ts("2026-09-02T00:00:00Z"), "admin"); err != nil {
		t.Fatalf("advance after unfreeze: %v", err)
	}
}

// Hooks are how the Phase 2 requirement that advance and the due-event sweep are
// atomic gets honoured. A failing hook must leave business time where it was,
// otherwise there is an observable state where a deadline has passed but nothing
// has reacted to it.
func TestHookFailureAbortsAdvance(t *testing.T) {
	store, c := mustInit(t, ts("2026-09-01T00:00:00Z"))
	ctx := context.Background()

	boom := errors.New("sweep failed")
	_, err := c.AdvanceTo(ctx, ts("2026-09-02T00:00:00Z"), "admin",
		func(ctx context.Context, h Handle) error {
			return h.Exec(ctx, "UPDATE offers SET status = 'CLOSED'")
		},
		func(ctx context.Context, h Handle) error {
			return boom
		},
	)
	if !errors.Is(err, boom) {
		t.Fatalf("want the hook error, got %v", err)
	}

	now, _ := c.Now(ctx)
	if !now.Equal(ts("2026-09-01T00:00:00Z")) {
		t.Fatalf("clock advanced despite a failed hook: %s", now)
	}
	if got := store.ExecLog(); len(got) != 0 {
		t.Fatalf("the first hook's writes should have been rolled back, got %v", got)
	}
}

func TestHookSuccessRunsInOrder(t *testing.T) {
	store, c := mustInit(t, ts("2026-09-01T00:00:00Z"))
	ctx := context.Background()

	_, err := c.AdvanceTo(ctx, ts("2026-09-02T00:00:00Z"), "admin",
		func(ctx context.Context, h Handle) error { return h.Exec(ctx, "first") },
		func(ctx context.Context, h Handle) error { return h.Exec(ctx, "second") },
	)
	if err != nil {
		t.Fatal(err)
	}
	log := store.ExecLog()
	if len(log) != 2 || log[0] != "first" || log[1] != "second" {
		t.Fatalf("hooks did not run in order: %v", log)
	}
}

// Two concurrent advances must not both succeed. If they did, each would run its
// own due-event sweep and whatever those sweeps do would happen twice.
func TestConcurrentAdvanceLosesCleanly(t *testing.T) {
	store, _ := mustInit(t, ts("2026-09-01T00:00:00Z"))
	ctx := context.Background()
	from := ts("2026-09-01T00:00:00Z")

	const goroutines = 8
	var wg sync.WaitGroup
	results := make([]error, goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Advance(ctx, schemeID, from, from.Add(time.Duration(i+1)*time.Hour), "admin")
			results[i] = err
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrConcurrentAdvance):
			// expected for the losers
		default:
			t.Errorf("goroutine %d got an unexpected error: %v", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("exactly one advance should win, %d did", succeeded)
	}
}

func TestNotInitialised(t *testing.T) {
	store := NewMemoryStore()
	if _, err := store.Get(context.Background(), schemeID); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("want ErrNotInitialised, got %v", err)
	}
}

func TestDoubleInit(t *testing.T) {
	store, _ := mustInit(t, ts("2026-09-01T00:00:00Z"))
	if _, err := store.Init(context.Background(), schemeID, ts("2026-09-01T00:00:00Z")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestStoredTimesAreUTC(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	store := NewMemoryStore()
	st, err := store.Init(context.Background(), schemeID, time.Date(2026, 9, 1, 12, 0, 0, 0, ist))
	if err != nil {
		t.Fatal(err)
	}
	// Normalising to UTC on the way in means a stored instant has one
	// representation, which keeps canonical payloads and therefore idempotency
	// keys stable regardless of the caller's location.
	if st.Current.Location() != time.UTC {
		t.Fatalf("stored time is in %s, want UTC", st.Current.Location())
	}
	if !st.Current.Equal(ts("2026-09-01T06:30:00Z")) {
		t.Fatalf("got %s", st.Current)
	}
}

func TestFixedAndStopped(t *testing.T) {
	ctx := context.Background()

	at := ts("2026-09-01T00:00:00Z")
	got, err := Fixed(at).Now(ctx)
	if err != nil || !got.Equal(at) {
		t.Fatalf("Fixed: %s, %v", got, err)
	}

	// Stopped exists so a test can assert that a code path reads no clock at all.
	if _, err := Stopped().Now(ctx); err == nil {
		t.Fatal("Stopped must return an error")
	}
}

func TestRealClockIsUTC(t *testing.T) {
	got, err := Real().Now(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Location() != time.UTC {
		t.Fatalf("Real() returned %s, want UTC", got.Location())
	}
}
