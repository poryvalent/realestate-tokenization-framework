package outbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/acresync/orchestrator/internal/idempotency"
)

// MemoryStore is an in-process Store for tests and for the Anvil integration runs.
//
// It holds the same guarantees as the Postgres store: the transition graph, idempotency-key
// uniqueness, and atomic claim-with-nonce. Holding the mutex across the claim is what makes the
// last one true here, and it is the property the concurrency tests exercise.
type MemoryStore struct {
	mu      sync.Mutex
	byID    map[string]*Entry
	byKey   map[idempotency.Key]string
	order   []string // insertion order, so claims are FIFO
	nextID  int
	nowFunc func() time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byID:  make(map[string]*Entry),
		byKey: make(map[idempotency.Key]string),
	}
}

// SetClock replaces the timestamp source. Timestamps here are audit metadata rather than
// business deadlines, so wall time is correct, but tests need determinism.
func (m *MemoryStore) SetClock(f func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nowFunc = f
}

func (m *MemoryStore) now() time.Time {
	if m.nowFunc != nil {
		return m.nowFunc()
	}
	//acresync:allow-wallclock outbox timestamps are audit metadata, not business deadlines
	return time.Now().UTC()
}

func (m *MemoryStore) Enqueue(_ context.Context, n NewEntry) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, dup := m.byKey[n.IdempotencyKey]; dup {
		return nil, fmt.Errorf("%w: %s already enqueued as %s",
			ErrDuplicateKey, n.IdempotencyKey.Hex(), existing)
	}

	m.nextID++
	id := fmt.Sprintf("entry-%04d", m.nextID)

	e := &Entry{
		ID:                id,
		SchemeID:          n.SchemeID,
		TargetContract:    n.TargetContract,
		FunctionName:      n.FunctionName,
		Payload:           append([]byte(nil), n.Payload...),
		PayloadHash:       sha256.Sum256(n.Payload),
		IdempotencyKey:    n.IdempotencyKey,
		Status:            StatusQueued,
		RelatedEntityType: n.RelatedEntityType,
		RelatedEntityID:   n.RelatedEntityID,
		EnvironmentTag:    n.EnvironmentTag,
		CreatedAt:         m.now(),
	}

	m.byID[id] = e
	m.byKey[n.IdempotencyKey] = id
	m.order = append(m.order, id)

	return cloneEntry(e), nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return cloneEntry(e), nil
}

func (m *MemoryStore) GetByIdempotencyKey(_ context.Context, key idempotency.Key) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byKey[key]
	if !ok {
		return nil, fmt.Errorf("%w: key %s", ErrNotFound, key.Hex())
	}
	return cloneEntry(m.byID[id]), nil
}

// ClaimNextQueued takes the oldest queued entry and assigns it a nonce, atomically.
//
// Two workers must never both succeed with the same nonce. If they did, both would sign a
// transaction for that slot, one would be mined and the other silently dropped, and the dropped
// one is an anchor the orchestrator believes it made.
func (m *MemoryStore) ClaimNextQueued(_ context.Context, schemeID string, nonce uint64) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// A nonce already held by an in-flight entry must not be handed out again.
	for _, e := range m.byID {
		if e.SchemeID != schemeID || e.Nonce == nil {
			continue
		}
		if *e.Nonce == nonce && !e.Status.Terminal() && e.Status != StatusQueued {
			return nil, fmt.Errorf("%w: nonce %d held by %s in %s",
				ErrNonceInUse, nonce, e.ID, e.Status)
		}
	}

	for _, id := range m.order {
		e := m.byID[id]
		if e.SchemeID != schemeID || e.Status != StatusQueued {
			continue
		}
		if err := m.transition(e, StatusSigned); err != nil {
			return nil, err
		}
		n := nonce
		e.Nonce = &n
		e.AttemptCount++
		return cloneEntry(e), nil
	}
	return nil, nil // nothing queued
}

func (m *MemoryStore) MarkBroadcast(_ context.Context, id, txHash, gasPriceWei string) error {
	return m.update(id, StatusBroadcast, func(e *Entry) {
		e.TxHash = txHash
		e.GasPriceWei = gasPriceWei
		t := m.now()
		e.SubmittedAt = &t
	})
}

func (m *MemoryStore) MarkPending(_ context.Context, id string) error {
	return m.update(id, StatusPending, nil)
}

func (m *MemoryStore) MarkConfirming(_ context.Context, id string, blockNumber uint64, confirmations int) error {
	return m.update(id, StatusConfirming, func(e *Entry) {
		b := blockNumber
		e.BlockNumber = &b
		e.Confirmations = confirmations
	})
}

// MarkConfirmed requires the evidence that makes the fiat gate checkable. A CONFIRMED row
// without a tx hash and block number satisfies a status check while proving nothing.
func (m *MemoryStore) MarkConfirmed(_ context.Context, id string, blockNumber uint64, confirmations int) error {
	m.mu.Lock()
	e, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if e.TxHash == "" || blockNumber == 0 {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrMissingEvidence, id)
	}
	m.mu.Unlock()

	return m.update(id, StatusConfirmed, func(e *Entry) {
		b := blockNumber
		e.BlockNumber = &b
		e.Confirmations = confirmations
		t := m.now()
		e.ConfirmedAt = &t
	})
}

func (m *MemoryStore) MarkFailed(_ context.Context, id, reason string) error {
	return m.update(id, StatusFailed, func(e *Entry) { e.LastError = reason })
}

// MarkReorged records that a confirming transaction's block was reorganised away.
//
// The confirmation count is reset here rather than at requeue, because a partially-confirmed
// count is the thing most likely to be misread as still valid.
func (m *MemoryStore) MarkReorged(_ context.Context, id, reason string) error {
	return m.update(id, StatusReorged, func(e *Entry) {
		e.LastError = reason
		e.Confirmations = 0
		e.BlockNumber = nil
	})
}

// Requeue returns an entry to QUEUED, clearing the nonce and tx hash.
//
// Clearing rather than reusing matters. After a reorg the previous nonce may already have been
// consumed by a different transaction that survived, so rebroadcasting the old signed payload
// would either be rejected or replace unrelated work. Re-signing from the recorded arguments is
// the only safe path.
func (m *MemoryStore) Requeue(_ context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := m.transition(e, StatusQueued); err != nil {
		return err
	}
	e.Nonce = nil
	e.TxHash = ""
	e.BlockNumber = nil
	e.Confirmations = 0
	e.GasPriceWei = ""
	e.LastError = reason
	return nil
}

func (m *MemoryStore) DeadLetter(_ context.Context, id, reason string) error {
	return m.update(id, StatusDeadLetter, func(e *Entry) {
		e.LastError = reason
		t := m.now()
		e.DeadLetteredAt = &t
	})
}

// Cancel abandons an entry before broadcast, the one genuinely clean undo in the system.
func (m *MemoryStore) Cancel(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if e.Status != StatusQueued {
		return fmt.Errorf("%w: %s is %s", ErrNotCancellable, id, e.Status)
	}
	if e.TxHash != "" {
		return fmt.Errorf("%w: %s has been broadcast", ErrNotCancellable, id)
	}
	if err := m.transition(e, StatusCancelled); err != nil {
		return err
	}
	t := m.now()
	e.CancelledAt = &t
	return nil
}

func (m *MemoryStore) ListByStatus(_ context.Context, schemeID string, statuses ...Status) ([]*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	want := make(map[Status]bool, len(statuses))
	for _, s := range statuses {
		want[s] = true
	}

	var out []*Entry
	for _, id := range m.order {
		e := m.byID[id]
		if e.SchemeID != schemeID {
			continue
		}
		if len(want) == 0 || want[e.Status] {
			out = append(out, cloneEntry(e))
		}
	}
	return out, nil
}

func (m *MemoryStore) InFlight(_ context.Context, schemeID string) ([]*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []*Entry
	for _, id := range m.order {
		e := m.byID[id]
		if e.SchemeID != schemeID || e.Nonce == nil || e.Status.Terminal() {
			continue
		}
		out = append(out, cloneEntry(e))
	}
	sort.Slice(out, func(i, j int) bool { return *out[i].Nonce < *out[j].Nonce })
	return out, nil
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (m *MemoryStore) update(id string, to Status, mutate func(*Entry)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := m.transition(e, to); err != nil {
		return err
	}
	if mutate != nil {
		mutate(e)
	}
	return nil
}

// transition enforces the state graph. Called with the mutex held.
func (m *MemoryStore) transition(e *Entry, to Status) error {
	if !CanTransition(e.Status, to) {
		return fmt.Errorf("%w: %s cannot move from %s to %s", ErrInvalidTransition, e.ID, e.Status, to)
	}
	e.Status = to
	return nil
}

func cloneEntry(e *Entry) *Entry {
	c := *e
	c.Payload = append([]byte(nil), e.Payload...)
	c.Calldata = append([]byte(nil), e.Calldata...)
	if e.Nonce != nil {
		n := *e.Nonce
		c.Nonce = &n
	}
	if e.BlockNumber != nil {
		b := *e.BlockNumber
		c.BlockNumber = &b
	}
	if e.SubmittedAt != nil {
		t := *e.SubmittedAt
		c.SubmittedAt = &t
	}
	if e.ConfirmedAt != nil {
		t := *e.ConfirmedAt
		c.ConfirmedAt = &t
	}
	if e.DeadLetteredAt != nil {
		t := *e.DeadLetteredAt
		c.DeadLetteredAt = &t
	}
	if e.CancelledAt != nil {
		t := *e.CancelledAt
		c.CancelledAt = &t
	}
	return &c
}
