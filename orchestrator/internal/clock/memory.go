package clock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// MemoryStore is an in-process Store for tests and for the M1 milestone, which
// has no database.
//
// It holds the same guarantees as the Postgres store: monotonic, freeze-aware,
// compare-and-set, and hooks run atomically with the advance. Holding the mutex
// across the hooks is what makes that last property true here.
type MemoryStore struct {
	mu     sync.Mutex
	states map[string]State
	// execLog records statements hooks attempted, so a test can assert that a
	// sweep ran without needing a database.
	execLog []string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{states: make(map[string]State)}
}

type memHandle struct{ store *MemoryStore }

func (h memHandle) Exec(_ context.Context, sql string, _ ...any) error {
	h.store.execLog = append(h.store.execLog, sql)
	return nil
}

// ExecLog returns the statements hooks issued, in order.
func (m *MemoryStore) ExecLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.execLog))
	copy(out, m.execLog)
	return out
}

func (m *MemoryStore) Init(_ context.Context, schemeID string, at time.Time) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.states[schemeID]; exists {
		return State{}, fmt.Errorf("%w: %s", ErrAlreadyExists, schemeID)
	}
	st := State{
		SchemeID:       schemeID,
		Current:        at.UTC(),
		LastAdvancedBy: "init",
		LastAdvancedAt: at.UTC(),
	}
	m.states[schemeID] = st
	return st, nil
}

func (m *MemoryStore) Get(_ context.Context, schemeID string) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.states[schemeID]
	if !ok {
		return State{}, fmt.Errorf("%w: %s", ErrNotInitialised, schemeID)
	}
	return st, nil
}

func (m *MemoryStore) Advance(
	ctx context.Context,
	schemeID string,
	expectedFrom, to time.Time,
	actor string,
	hooks ...Hook,
) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.states[schemeID]
	if !ok {
		return State{}, fmt.Errorf("%w: %s", ErrNotInitialised, schemeID)
	}
	if !st.Current.Equal(expectedFrom.UTC()) {
		return State{}, fmt.Errorf("%w: expected %s, found %s",
			ErrConcurrentAdvance,
			expectedFrom.UTC().Format(time.RFC3339Nano),
			st.Current.Format(time.RFC3339Nano))
	}
	if st.Frozen && !to.Equal(st.Current) {
		return State{}, ErrFrozen
	}
	if to.UTC().Before(st.Current) {
		return State{}, fmt.Errorf("%w: %s -> %s",
			ErrNotMonotonic, st.Current.Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}

	// Hooks run before the value is committed to the map, and the mutex is still
	// held, so a failing hook leaves business time exactly where it was.
	snapshot := len(m.execLog)
	for i, h := range hooks {
		if err := h(ctx, memHandle{store: m}); err != nil {
			m.execLog = m.execLog[:snapshot]
			return State{}, fmt.Errorf("clock: advance hook %d failed, clock not advanced: %w", i, err)
		}
	}

	st.Current = to.UTC()
	st.LastAdvancedBy = actor
	st.LastAdvancedAt = to.UTC()
	m.states[schemeID] = st
	return st, nil
}

func (m *MemoryStore) SetFrozen(_ context.Context, schemeID string, frozen bool, actor string) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st, ok := m.states[schemeID]
	if !ok {
		return State{}, fmt.Errorf("%w: %s", ErrNotInitialised, schemeID)
	}
	st.Frozen = frozen
	st.LastAdvancedBy = actor
	m.states[schemeID] = st
	return st, nil
}

// Fixed returns a Business clock pinned to one instant, for tests that need a
// stable value and no controller.
func Fixed(at time.Time) Business { return fixedClock{at: at.UTC()} }

type fixedClock struct{ at time.Time }

func (f fixedClock) Now(context.Context) (time.Time, error) { return f.at, nil }

// Stopped is a Business clock that always errors, for tests asserting that a code
// path does not read time at all.
func Stopped() Business { return stoppedClock{} }

type stoppedClock struct{}

var errStopped = errors.New("clock: this code path must not read the clock")

func (stoppedClock) Now(context.Context) (time.Time, error) { return time.Time{}, errStopped }
