// Package clock separates business time from wall time.
//
// # Why this exists
//
// AcreSync demonstrates a thirty-day offer window and a T+3 settlement in a few
// minutes. That is only possible if every deadline in the system is evaluated
// against an injectable clock rather than against the host's wall clock. If one
// service calls time.Now for a business decision, the demo and a real deployment
// diverge in a way no unit test catches, because the test also calls time.Now and
// agrees with itself.
//
// # What it deliberately does not do
//
// Business time governs deadlines only. It has no influence over block
// timestamps, confirmation counts, or the 256-block window in which a target
// blockhash stays available. Those run on real chain time and cannot be
// compressed. A thirty-day offer collapses to a second; a five-confirmation wait
// does not collapse at all, and the ballot's ten-block seed delay does not
// either. Pretending otherwise would produce a demo that works and a system that
// cannot.
//
// Real wall time is still needed for audit metadata (when did an operator
// actually click this) and for transport timeouts. That is what Real provides,
// and its name is deliberately conspicuous so its use is a visible decision.
package clock

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotMonotonic is returned when an advance would move business time
	// backward. Rewinding would invalidate every simulated_clock_value already
	// recorded against a ledger entry, and those values are the audit trail's only
	// means of reconstructing the order in which things happened.
	ErrNotMonotonic = errors.New("clock: business time cannot move backward")

	// ErrFrozen is returned when an advance is attempted on a frozen clock.
	ErrFrozen = errors.New("clock: clock is frozen; unfreeze before advancing")

	// ErrConcurrentAdvance is returned when a compare-and-set loses, meaning
	// another actor advanced the clock first. The caller should re-read and decide,
	// never blindly retry: two advances that both "succeed" would double-apply
	// whatever due-event sweep each carried.
	ErrConcurrentAdvance = errors.New("clock: concurrent advance detected")

	ErrNotInitialised = errors.New("clock: no clock initialised for this scheme")
	ErrAlreadyExists  = errors.New("clock: clock already initialised for this scheme")
)

// State is the persisted condition of one scheme's business clock.
type State struct {
	SchemeID       string
	Current        time.Time
	Frozen         bool
	LastAdvancedBy string
	LastAdvancedAt time.Time
}

// Handle lets a hook write inside the same transaction that advances the clock.
type Handle interface {
	Exec(ctx context.Context, sql string, args ...any) error
}

// Hook runs inside the advance transaction.
//
// This is how the Phase 2 requirement that "advance and the due-event sweep are
// atomic" is honoured. Without it there is an observable window in which business
// time has passed a deadline but the handler for that deadline has not run, and a
// reader during that window sees an offer that is closed and open at once.
type Hook func(ctx context.Context, h Handle) error

// Store persists business time. Implementations must enforce monotonicity
// themselves rather than trusting the caller, because the caller is the thing most
// likely to have the bug.
type Store interface {
	Init(ctx context.Context, schemeID string, at time.Time) (State, error)
	Get(ctx context.Context, schemeID string) (State, error)

	// Advance moves business time to `to`, having observed `expectedFrom`.
	// It must fail with ErrConcurrentAdvance if the stored value is no longer
	// expectedFrom, ErrNotMonotonic if `to` is earlier than the stored value, and
	// ErrFrozen if the clock is frozen. Hooks run in the same transaction and any
	// hook error rolls the advance back.
	Advance(ctx context.Context, schemeID string, expectedFrom, to time.Time, actor string, hooks ...Hook) (State, error)

	SetFrozen(ctx context.Context, schemeID string, frozen bool, actor string) (State, error)
}

// Business reads business time. Most of the codebase should depend on this and
// nothing wider.
type Business interface {
	Now(ctx context.Context) (time.Time, error)
}

// Controller is the admin-facing surface. Only the simulation endpoints need it.
type Controller interface {
	Business
	State(ctx context.Context) (State, error)
	AdvanceTo(ctx context.Context, target time.Time, actor string, hooks ...Hook) (State, error)
	AdvanceBy(ctx context.Context, d time.Duration, actor string, hooks ...Hook) (State, error)
	Freeze(ctx context.Context, actor string) (State, error)
	Unfreeze(ctx context.Context, actor string) (State, error)
}

// scheme binds a Store to one scheme so callers cannot accidentally read another
// scheme's business time.
type scheme struct {
	store    Store
	schemeID string
}

// For returns a Controller scoped to one scheme.
func For(store Store, schemeID string) Controller {
	return &scheme{store: store, schemeID: schemeID}
}

func (s *scheme) Now(ctx context.Context) (time.Time, error) {
	st, err := s.store.Get(ctx, s.schemeID)
	if err != nil {
		return time.Time{}, err
	}
	return st.Current, nil
}

func (s *scheme) State(ctx context.Context) (State, error) {
	return s.store.Get(ctx, s.schemeID)
}

func (s *scheme) AdvanceTo(ctx context.Context, target time.Time, actor string, hooks ...Hook) (State, error) {
	current, err := s.store.Get(ctx, s.schemeID)
	if err != nil {
		return State{}, err
	}
	// Checked here for a clear error, and again in the store, which is the layer
	// that actually holds the line under concurrency.
	if target.Before(current.Current) {
		return State{}, fmt.Errorf("%w: %s -> %s",
			ErrNotMonotonic, current.Current.UTC().Format(time.RFC3339), target.UTC().Format(time.RFC3339))
	}
	return s.store.Advance(ctx, s.schemeID, current.Current, target, actor, hooks...)
}

func (s *scheme) AdvanceBy(ctx context.Context, d time.Duration, actor string, hooks ...Hook) (State, error) {
	if d < 0 {
		return State{}, fmt.Errorf("%w: negative duration %s", ErrNotMonotonic, d)
	}
	current, err := s.store.Get(ctx, s.schemeID)
	if err != nil {
		return State{}, err
	}
	return s.store.Advance(ctx, s.schemeID, current.Current, current.Current.Add(d), actor, hooks...)
}

func (s *scheme) Freeze(ctx context.Context, actor string) (State, error) {
	return s.store.SetFrozen(ctx, s.schemeID, true, actor)
}

func (s *scheme) Unfreeze(ctx context.Context, actor string) (State, error) {
	return s.store.SetFrozen(ctx, s.schemeID, false, actor)
}

// ---------------------------------------------------------------------------
// Real wall clock
// ---------------------------------------------------------------------------

// Real returns wall-clock time in UTC.
//
// Legitimate uses are audit metadata (created_at, "when did the operator actually
// do this") and transport timeouts. It must never decide whether a business
// deadline has passed. The wall-clock lint check in internal/lint enforces that
// by rejecting direct time.Now calls elsewhere, which is what makes this the only
// door.
func Real() Business { return realClock{} }

type realClock struct{}

//acresync:allow-wallclock this is the single sanctioned wall-clock reader
func (realClock) Now(context.Context) (time.Time, error) {
	return time.Now().UTC(), nil
}
