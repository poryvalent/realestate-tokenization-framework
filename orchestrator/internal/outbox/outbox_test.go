package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/idempotency"
)

const schemeID = "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"

func key(n int) idempotency.Key {
	return idempotency.MustDerive(idempotency.Input{
		Action:   idempotency.ActionAnchorPeriod,
		SchemeID: schemeID,
		Payload:  map[string]any{"n": n},
	})
}

func newEntry(n int) NewEntry {
	return NewEntry{
		SchemeID:       schemeID,
		TargetContract: "0x00000000000000000000000000000000000000c1",
		FunctionName:   "anchorPeriod",
		Payload:        []byte(fmt.Sprintf(`{"periodId":%d}`, n)),
		IdempotencyKey: key(n),
		EnvironmentTag: "LOCAL",
	}
}

func mustEnqueue(t *testing.T, s *MemoryStore, n int) *Entry {
	t.Helper()
	e, err := s.Enqueue(context.Background(), newEntry(n))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ---------------------------------------------------------------------------
// The state graph
// ---------------------------------------------------------------------------

// The graph is enumerated rather than implicit because the failure it prevents is quiet: a row
// that reached CONFIRMED without passing through CONFIRMING would satisfy the fiat gate while
// having had its confirmation count checked by nobody.
func TestBroadcastCannotSkipToConfirmed(t *testing.T) {
	if CanTransition(StatusBroadcast, StatusConfirmed) {
		t.Fatal("BROADCAST must not reach CONFIRMED without passing through CONFIRMING")
	}
	if CanTransition(StatusQueued, StatusConfirmed) {
		t.Fatal("QUEUED must not reach CONFIRMED directly")
	}
	if !CanTransition(StatusConfirming, StatusConfirmed) {
		t.Fatal("CONFIRMING must be able to reach CONFIRMED")
	}
}

// An anchor that fiat has already been released against must never move again.
func TestConfirmedIsTerminal(t *testing.T) {
	for _, to := range AllStatuses {
		if CanTransition(StatusConfirmed, to) {
			t.Errorf("CONFIRMED must be terminal but allows a move to %s", to)
		}
	}
	if !StatusConfirmed.Terminal() {
		t.Error("StatusConfirmed.Terminal() should report true")
	}
	if !StatusDeadLetter.Terminal() || !StatusCancelled.Terminal() {
		t.Error("DEAD_LETTER and CANCELLED should be terminal")
	}
	if StatusQueued.Terminal() || StatusConfirming.Terminal() {
		t.Error("in-flight statuses must not be terminal")
	}
}

func TestInvalidTransitionRejected(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	e := mustEnqueue(t, s, 1)

	// Broadcasting something that was never signed has no nonce and no transaction.
	if err := s.MarkBroadcast(ctx, e.ID, "0xabc", "1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

func TestDuplicateKeyRejected(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	mustEnqueue(t, s, 1)
	if _, err := s.Enqueue(ctx, newEntry(1)); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey, got %v", err)
	}
}

func TestLookupByIdempotencyKey(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	e := mustEnqueue(t, s, 7)

	got, err := s.GetByIdempotencyKey(ctx, key(7))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != e.ID {
		t.Fatalf("got %s, want %s", got.ID, e.ID)
	}

	if _, err := s.GetByIdempotencyKey(ctx, key(999)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Nonce discipline
// ---------------------------------------------------------------------------

// The property that matters most under concurrency. If two workers both claimed the same nonce,
// both would sign a transaction for that slot, one would be mined and the other silently
// dropped, and the dropped one is an anchor the orchestrator believes it made.
func TestConcurrentClaimsCannotShareANonce(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	for i := range 20 {
		mustEnqueue(t, s, i)
	}

	const workers = 12
	var wg sync.WaitGroup
	results := make([]*Entry, workers)
	errs := make([]error, workers)

	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Every worker attempts the same nonce, which is the race a naive implementation
			// loses.
			results[i], errs[i] = s.ClaimNextQueued(ctx, schemeID, 42)
		}(i)
	}
	wg.Wait()

	claimed := 0
	for i := range workers {
		switch {
		case errs[i] == nil && results[i] != nil:
			claimed++
		case errors.Is(errs[i], ErrNonceInUse):
			// expected for the losers
		case errs[i] != nil:
			t.Errorf("worker %d unexpected error: %v", i, errs[i])
		}
	}
	if claimed != 1 {
		t.Fatalf("exactly one worker should claim nonce 42, %d did", claimed)
	}
}

func TestClaimIsFIFO(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	first := mustEnqueue(t, s, 1)
	mustEnqueue(t, s, 2)
	mustEnqueue(t, s, 3)

	got, err := s.ClaimNextQueued(ctx, schemeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID {
		t.Fatalf("claimed %s, want the oldest queued entry %s", got.ID, first.ID)
	}
	if got.Status != StatusSigned {
		t.Fatalf("claimed entry should be SIGNED, got %s", got.Status)
	}
	if got.Nonce == nil || *got.Nonce != 0 {
		t.Fatal("nonce not recorded on claim")
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt count = %d, want 1", got.AttemptCount)
	}
}

func TestClaimReturnsNilWhenEmpty(t *testing.T) {
	s := NewMemoryStore()
	got, err := s.ClaimNextQueued(context.Background(), schemeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("expected nil when nothing is queued")
	}
}

// After a restart the relayer has to work out which nonces are already consumed.
func TestInFlightReportsConsumedNonces(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	for i := range 3 {
		mustEnqueue(t, s, i)
	}
	for i := range 3 {
		e, err := s.ClaimNextQueued(ctx, schemeID, uint64(10+i))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MarkBroadcast(ctx, e.ID, fmt.Sprintf("0x%064x", i), "1"); err != nil {
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
		if *e.Nonce != uint64(10+i) {
			t.Errorf("entry %d has nonce %d, want %d (must be ordered)", i, *e.Nonce, 10+i)
		}
	}
}

// A confirmed entry has released its nonce and must not appear as in-flight, or nonce recovery
// after a restart would skip slots forever.
func TestConfirmedEntriesAreNotInFlight(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	mustEnqueue(t, s, 1)

	e, _ := s.ClaimNextQueued(ctx, schemeID, 0)
	if err := s.MarkBroadcast(ctx, e.ID, "0x"+fmt.Sprintf("%064d", 1), "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirming(ctx, e.ID, 100, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirmed(ctx, e.ID, 100, 5); err != nil {
		t.Fatal(err)
	}

	inFlight, _ := s.InFlight(ctx, schemeID)
	if len(inFlight) != 0 {
		t.Fatalf("a confirmed entry must not be in-flight, got %d", len(inFlight))
	}
}

// ---------------------------------------------------------------------------
// The fiat gate
// ---------------------------------------------------------------------------

// The single most dangerous link: reading chain state and then irreversibly moving money. Each
// condition here is one way that could go wrong.
func TestReadyForFiat(t *testing.T) {
	block := uint64(100)

	cases := []struct {
		name  string
		entry Entry
		want  bool
	}{
		{
			name:  "confirmed with enough confirmations",
			entry: Entry{Status: StatusConfirmed, Confirmations: 5, TxHash: "0xabc", BlockNumber: &block},
			want:  true,
		},
		{
			name:  "confirmed but one confirmation short",
			entry: Entry{Status: StatusConfirmed, Confirmations: 4, TxHash: "0xabc", BlockNumber: &block},
			want:  false,
		},
		{
			name:  "still confirming",
			entry: Entry{Status: StatusConfirming, Confirmations: 9, TxHash: "0xabc", BlockNumber: &block},
			want:  false,
		},
		{
			// A status check alone would pass this. Without a tx hash there is nothing to
			// point at if the payment is ever questioned.
			name:  "confirmed without a tx hash",
			entry: Entry{Status: StatusConfirmed, Confirmations: 9, BlockNumber: &block},
			want:  false,
		},
		{
			name:  "confirmed without a block number",
			entry: Entry{Status: StatusConfirmed, Confirmations: 9, TxHash: "0xabc"},
			want:  false,
		},
		{
			name:  "reorged",
			entry: Entry{Status: StatusReorged, Confirmations: 0, TxHash: "0xabc", BlockNumber: &block},
			want:  false,
		},
		{
			name:  "dead lettered",
			entry: Entry{Status: StatusDeadLetter, Confirmations: 9, TxHash: "0xabc", BlockNumber: &block},
			want:  false,
		},
	}

	for _, c := range cases {
		if got := c.entry.ReadyForFiat(5); got != c.want {
			t.Errorf("%s: ReadyForFiat = %v, want %v", c.name, got, c.want)
		}
	}
}

// A CONFIRMED row without evidence satisfies a status check while proving nothing, so the store
// refuses to create one.
func TestConfirmationRequiresEvidence(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	mustEnqueue(t, s, 1)

	e, _ := s.ClaimNextQueued(ctx, schemeID, 0)
	// Deliberately not broadcast, so there is no tx hash.
	if err := s.MarkConfirmed(ctx, e.ID, 100, 5); !errors.Is(err, ErrMissingEvidence) {
		t.Fatalf("want ErrMissingEvidence, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reorg handling
// ---------------------------------------------------------------------------

// A requeued entry must be re-signed from its recorded arguments rather than rebroadcast. After
// a reorg the old nonce may already have been consumed by a different transaction that survived,
// so replaying the old signed payload would either be rejected or replace unrelated work.
func TestRequeueClearsNonceAndTxHash(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	mustEnqueue(t, s, 1)

	e, _ := s.ClaimNextQueued(ctx, schemeID, 7)
	if err := s.MarkBroadcast(ctx, e.ID, "0x"+fmt.Sprintf("%064d", 1), "1000"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirming(ctx, e.ID, 500, 2); err != nil {
		t.Fatal(err)
	}

	// A reorg is recorded before the entry is requeued. Going straight from CONFIRMING to
	// QUEUED is refused by the graph on purpose: it would erase the fact that this anchor was
	// mined and then unmined, which is precisely the history an incident review needs.
	if err := s.Requeue(ctx, e.ID, "skipping the reorged state"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("CONFIRMING should not requeue directly, got %v", err)
	}

	if err := s.MarkReorged(ctx, e.ID, "reorg detected at block 500"); err != nil {
		t.Fatal(err)
	}
	mid, _ := s.Get(ctx, e.ID)
	if mid.Confirmations != 0 || mid.BlockNumber != nil {
		t.Error("a reorged entry must not retain a partial confirmation count")
	}

	if err := s.Requeue(ctx, e.ID, "reorg detected at block 500"); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(ctx, e.ID)
	if got.Status != StatusQueued {
		t.Fatalf("status = %s, want QUEUED", got.Status)
	}
	if got.Nonce != nil {
		t.Error("nonce must be cleared so the entry is re-signed rather than rebroadcast")
	}
	if got.TxHash != "" {
		t.Error("tx hash must be cleared")
	}
	if got.BlockNumber != nil || got.Confirmations != 0 {
		t.Error("block evidence must be cleared")
	}
	if got.LastError == "" {
		t.Error("the reason should be retained for diagnosis")
	}
	// The attempt count survives, so the retry ceiling still applies.
	if got.AttemptCount != 1 {
		t.Errorf("attempt count = %d, want the original 1 to be preserved", got.AttemptCount)
	}
}

func TestRequeuedEntryCanBeReclaimedWithANewNonce(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	mustEnqueue(t, s, 1)

	e, _ := s.ClaimNextQueued(ctx, schemeID, 7)
	if err := s.Requeue(ctx, e.ID, "stuck"); err != nil {
		t.Fatal(err)
	}

	again, err := s.ClaimNextQueued(ctx, schemeID, 8)
	if err != nil {
		t.Fatal(err)
	}
	if again == nil || again.ID != e.ID {
		t.Fatal("a requeued entry should be claimable again")
	}
	if *again.Nonce != 8 {
		t.Fatalf("nonce = %d, want the fresh 8", *again.Nonce)
	}
	if again.AttemptCount != 2 {
		t.Fatalf("attempt count = %d, want 2", again.AttemptCount)
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

// The one genuinely clean undo. Once a transaction is broadcast there is no taking it back, so
// the pre-broadcast window is the only place cancellation is honest.
func TestCancelOnlyBeforeBroadcast(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	queued := mustEnqueue(t, s, 1)
	if err := s.Cancel(ctx, queued.ID); err != nil {
		t.Fatalf("a queued entry should be cancellable: %v", err)
	}
	got, _ := s.Get(ctx, queued.ID)
	if got.Status != StatusCancelled || got.CancelledAt == nil {
		t.Fatal("cancellation not recorded")
	}

	// Once signed, the nonce is consumed and cancelling would leave a gap.
	signedEntry := mustEnqueue(t, s, 2)
	if _, err := s.ClaimNextQueued(ctx, schemeID, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, signedEntry.ID); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("want ErrNotCancellable for a signed entry, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Dead lettering
// ---------------------------------------------------------------------------

// A retry loop with no ceiling turns a permanent failure into an infinite gas burn and buries
// the real error under thousands of identical attempts.
func TestDeadLetterAfterFailure(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	mustEnqueue(t, s, 1)

	e, _ := s.ClaimNextQueued(ctx, schemeID, 0)
	if err := s.MarkFailed(ctx, e.ID, "execution reverted: PeriodAlreadyAnchored"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeadLetter(ctx, e.ID, "retries exhausted"); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(ctx, e.ID)
	if got.Status != StatusDeadLetter || got.DeadLetteredAt == nil {
		t.Fatal("dead lettering not recorded")
	}
	// A dead-lettered entry needs a human, so nothing may move it automatically.
	for _, to := range AllStatuses {
		if CanTransition(StatusDeadLetter, to) {
			t.Errorf("DEAD_LETTER must be terminal but allows %s", to)
		}
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the default config should be valid: %v", err)
	}

	cases := map[string]Config{
		"zero confirmation depth": {ConfirmationDepth: 0, MaxAttempts: 3, PollInterval: time.Second, ReceiptTimeout: time.Minute},
		"zero attempts":           {ConfirmationDepth: 5, MaxAttempts: 0, PollInterval: time.Second, ReceiptTimeout: time.Minute},
		"zero poll interval":      {ConfirmationDepth: 5, MaxAttempts: 3, PollInterval: 0, ReceiptTimeout: time.Minute},
		"zero receipt timeout":    {ConfirmationDepth: 5, MaxAttempts: 3, PollInterval: time.Second, ReceiptTimeout: 0},
	}
	for name, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestDefaultConfirmationDepthIsFive(t *testing.T) {
	// Matches the contract deployment and the database gate. If these three ever disagree, the
	// weakest one silently becomes the real policy.
	if got := DefaultConfig().ConfirmationDepth; got != 5 {
		t.Errorf("default confirmation depth = %d, want 5 to match the database gate", got)
	}
}

// ---------------------------------------------------------------------------
// Isolation
// ---------------------------------------------------------------------------

func TestSchemesAreIsolated(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	other := "0123abcd-4567-89ef-0123-456789abcdef"
	mustEnqueue(t, s, 1)

	e2 := newEntry(2)
	e2.SchemeID = other
	if _, err := s.Enqueue(ctx, e2); err != nil {
		t.Fatal(err)
	}

	// A claim for one scheme must not take another scheme's work: nonces are per relayer per
	// scheme and mixing them would corrupt both sequences.
	got, err := s.ClaimNextQueued(ctx, other, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.SchemeID != other {
		t.Fatal("claim crossed a scheme boundary")
	}

	list, _ := s.ListByStatus(ctx, schemeID, StatusQueued)
	if len(list) != 1 {
		t.Fatalf("the first scheme should still have its queued entry, got %d", len(list))
	}
}

// Returned entries must be copies. Handing out a pointer into the store would let a caller
// mutate persisted state without going through the transition graph.
func TestReturnedEntriesAreCopies(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	e := mustEnqueue(t, s, 1)

	e.Status = StatusConfirmed
	e.Payload[0] = 'X'

	got, _ := s.Get(ctx, e.ID)
	if got.Status != StatusQueued {
		t.Error("mutating a returned entry changed stored state")
	}
	if got.Payload[0] == 'X' {
		t.Error("payload is shared rather than copied")
	}
}
