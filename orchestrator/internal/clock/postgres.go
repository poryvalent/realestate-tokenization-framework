package clock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore persists business time in the simulated_clock table.
//
// The database already enforces monotonicity and the freeze rule through the
// simulated_clock_monotonic trigger. The checks here are not redundant: they run
// under a row lock, so they turn a race into a clean ErrConcurrentAdvance instead
// of a trigger error that a caller cannot distinguish from a logic bug. The
// trigger remains the backstop for anything that reaches the table by another
// path, including a psql session.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

type txHandle struct{ tx pgx.Tx }

func (h txHandle) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := h.tx.Exec(ctx, sql, args...)
	return err
}

func (p *PostgresStore) Init(ctx context.Context, schemeID string, at time.Time) (State, error) {
	const q = `
		INSERT INTO simulated_clock (scheme_id, current_value, last_advanced_by)
		VALUES ($1, $2, 'init')
		RETURNING scheme_id, current_value, is_frozen,
		          coalesce(last_advanced_by, ''), last_advanced_at`

	var st State
	err := p.pool.QueryRow(ctx, q, schemeID, at.UTC()).Scan(
		&st.SchemeID, &st.Current, &st.Frozen, &st.LastAdvancedBy, &st.LastAdvancedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return State{}, fmt.Errorf("%w: %s", ErrAlreadyExists, schemeID)
		}
		return State{}, fmt.Errorf("clock: init: %w", err)
	}
	st.Current = st.Current.UTC()
	return st, nil
}

func (p *PostgresStore) Get(ctx context.Context, schemeID string) (State, error) {
	const q = `
		SELECT scheme_id, current_value, is_frozen,
		       coalesce(last_advanced_by, ''), last_advanced_at
		  FROM simulated_clock WHERE scheme_id = $1`

	var st State
	err := p.pool.QueryRow(ctx, q, schemeID).Scan(
		&st.SchemeID, &st.Current, &st.Frozen, &st.LastAdvancedBy, &st.LastAdvancedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, fmt.Errorf("%w: %s", ErrNotInitialised, schemeID)
	}
	if err != nil {
		return State{}, fmt.Errorf("clock: get: %w", err)
	}
	st.Current = st.Current.UTC()
	return st, nil
}

func (p *PostgresStore) Advance(
	ctx context.Context,
	schemeID string,
	expectedFrom, to time.Time,
	actor string,
	hooks ...Hook,
) (State, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return State{}, fmt.Errorf("clock: begin: %w", err)
	}
	// Rollback is a no-op once Commit has succeeded.
	defer func() { _ = tx.Rollback(ctx) }()

	var current time.Time
	var frozen bool
	err = tx.QueryRow(ctx,
		`SELECT current_value, is_frozen FROM simulated_clock WHERE scheme_id = $1 FOR UPDATE`,
		schemeID).Scan(&current, &frozen)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, fmt.Errorf("%w: %s", ErrNotInitialised, schemeID)
	}
	if err != nil {
		return State{}, fmt.Errorf("clock: lock row: %w", err)
	}
	current = current.UTC()

	if !current.Equal(expectedFrom.UTC()) {
		return State{}, fmt.Errorf("%w: expected %s, found %s",
			ErrConcurrentAdvance,
			expectedFrom.UTC().Format(time.RFC3339Nano),
			current.Format(time.RFC3339Nano))
	}
	if frozen && !to.UTC().Equal(current) {
		return State{}, ErrFrozen
	}
	if to.UTC().Before(current) {
		return State{}, fmt.Errorf("%w: %s -> %s",
			ErrNotMonotonic, current.Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}

	// Hooks join this transaction. A failing hook aborts the advance, which is the
	// property that keeps business time and the due-event sweep from diverging.
	for i, h := range hooks {
		if err := h(ctx, txHandle{tx: tx}); err != nil {
			return State{}, fmt.Errorf("clock: advance hook %d failed, clock not advanced: %w", i, err)
		}
	}

	var st State
	err = tx.QueryRow(ctx, `
		UPDATE simulated_clock
		   SET current_value = $2, last_advanced_by = $3
		 WHERE scheme_id = $1
		RETURNING scheme_id, current_value, is_frozen,
		          coalesce(last_advanced_by, ''), last_advanced_at`,
		schemeID, to.UTC(), actor).Scan(
		&st.SchemeID, &st.Current, &st.Frozen, &st.LastAdvancedBy, &st.LastAdvancedAt)
	if err != nil {
		return State{}, fmt.Errorf("clock: update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return State{}, fmt.Errorf("clock: commit: %w", err)
	}
	st.Current = st.Current.UTC()
	return st, nil
}

func (p *PostgresStore) SetFrozen(ctx context.Context, schemeID string, frozen bool, actor string) (State, error) {
	var st State
	err := p.pool.QueryRow(ctx, `
		UPDATE simulated_clock
		   SET is_frozen = $2, last_advanced_by = $3
		 WHERE scheme_id = $1
		RETURNING scheme_id, current_value, is_frozen,
		          coalesce(last_advanced_by, ''), last_advanced_at`,
		schemeID, frozen, actor).Scan(
		&st.SchemeID, &st.Current, &st.Frozen, &st.LastAdvancedBy, &st.LastAdvancedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, fmt.Errorf("%w: %s", ErrNotInitialised, schemeID)
	}
	if err != nil {
		return State{}, fmt.Errorf("clock: set frozen: %w", err)
	}
	st.Current = st.Current.UTC()
	return st, nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
