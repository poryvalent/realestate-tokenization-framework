package outbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acresync/orchestrator/internal/idempotency"
)

// PostgresStore is the durable Store backing the relayer.
//
// # Why the claim uses FOR UPDATE SKIP LOCKED
//
// Claiming work is the one place this store has to be genuinely concurrent-safe, and the obvious
// implementations are both wrong.
//
// A plain SELECT then UPDATE races: two workers read the same row, both update it, and both
// believe they own it. A SELECT ... FOR UPDATE without SKIP LOCKED is safe but pathological: the
// second worker blocks on the first worker's row lock, so the queue serialises on lock contention
// and a slow first worker stalls everyone. SKIP LOCKED gives the second worker the next unlocked
// row instead, which is what a work queue actually wants.
//
// Serialisation still happens, but on the thing that genuinely has to be serial: the nonce. That
// is enforced by a unique index rather than by locking, so a nonce collision surfaces as a clean
// ErrNonceInUse instead of a deadlock.
//
// # Where the transition graph is enforced
//
// In the UPDATE's WHERE clause, as a status precondition. An update that matches no row means the
// entry was not in the state the caller assumed, which is either a bug or a concurrent change, and
// both deserve an error rather than a silent no-op. Enforcing it in SQL rather than in Go means it
// holds even for a second process, or a psql session.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// selectColumns is shared by every read so the scan order cannot drift between queries.
const selectColumns = `
	id, scheme_id, target_contract, function_name,
	payload_json::text, payload_hash, calldata, idempotency_key,
	nonce, coalesce(tx_hash, ''), coalesce(gas_price_wei::text, ''), block_number,
	status::text, confirmations, attempt_count, coalesce(last_error, ''),
	coalesce(related_entity_type, ''), related_entity_id, environment_tag::text,
	created_at, submitted_at, confirmed_at, dead_lettered_at, cancelled_at`

func scanEntry(row pgx.Row) (*Entry, error) {
	var (
		e            Entry
		payloadJSON  string
		payloadHash  []byte
		calldata     []byte
		idemKey      []byte
		relatedID    *string
		gasPrice     string
		statusText   string
		envTag       string
	)

	err := row.Scan(
		&e.ID, &e.SchemeID, &e.TargetContract, &e.FunctionName,
		&payloadJSON, &payloadHash, &calldata, &idemKey,
		&e.Nonce, &e.TxHash, &gasPrice, &e.BlockNumber,
		&statusText, &e.Confirmations, &e.AttemptCount, &e.LastError,
		&e.RelatedEntityType, &relatedID, &envTag,
		&e.CreatedAt, &e.SubmittedAt, &e.ConfirmedAt, &e.DeadLetteredAt, &e.CancelledAt,
	)
	if err != nil {
		return nil, err
	}

	e.Payload = []byte(payloadJSON)
	copy(e.PayloadHash[:], payloadHash)
	e.Calldata = calldata
	copy(e.IdempotencyKey[:], idemKey)
	e.GasPriceWei = gasPrice
	e.Status = Status(statusText)
	e.EnvironmentTag = envTag
	if relatedID != nil {
		e.RelatedEntityID = *relatedID
	}
	return &e, nil
}

func (p *PostgresStore) Enqueue(ctx context.Context, n NewEntry) (*Entry, error) {
	hash := sha256.Sum256(n.Payload)

	var relatedID *string
	if n.RelatedEntityID != "" {
		relatedID = &n.RelatedEntityID
	}
	var relatedType *string
	if n.RelatedEntityType != "" {
		relatedType = &n.RelatedEntityType
	}

	q := `
		INSERT INTO chain_outbox (
			scheme_id, target_contract, function_name, payload_json, payload_hash,
			idempotency_key, status, environment_tag, related_entity_type, related_entity_id
		) VALUES ($1, $2, $3, $4::jsonb, $5, $6, 'QUEUED', $7::environment_tag, $8, $9)
		RETURNING ` + selectColumns

	e, err := scanEntry(p.pool.QueryRow(ctx, q,
		n.SchemeID, n.TargetContract, n.FunctionName, string(n.Payload), hash[:],
		n.IdempotencyKey.Bytes(), n.EnvironmentTag, relatedType, relatedID))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateKey, n.IdempotencyKey.Hex())
		}
		return nil, fmt.Errorf("outbox: enqueue: %w", err)
	}
	return e, nil
}

func (p *PostgresStore) Get(ctx context.Context, id string) (*Entry, error) {
	e, err := scanEntry(p.pool.QueryRow(ctx,
		`SELECT `+selectColumns+` FROM chain_outbox WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("outbox: get: %w", err)
	}
	return e, nil
}

func (p *PostgresStore) GetByIdempotencyKey(ctx context.Context, key idempotency.Key) (*Entry, error) {
	e, err := scanEntry(p.pool.QueryRow(ctx,
		`SELECT `+selectColumns+` FROM chain_outbox WHERE idempotency_key = $1`, key.Bytes()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: key %s", ErrNotFound, key.Hex())
	}
	if err != nil {
		return nil, fmt.Errorf("outbox: get by key: %w", err)
	}
	return e, nil
}

// ClaimNextQueued takes the oldest queued entry for a scheme and assigns it a nonce.
//
// The CTE locks exactly one candidate row with SKIP LOCKED, so a concurrent worker takes a
// different row rather than blocking. The UPDATE then re-asserts status = 'QUEUED', which closes
// the window between the select and the write.
//
// A nonce already held by another in-flight entry trips the partial unique index from migration
// 0007 and surfaces as ErrNonceInUse. That is the correct failure: the relayer's view of the
// sequence is stale and it must re-seed rather than guess.
func (p *PostgresStore) ClaimNextQueued(ctx context.Context, schemeID string, nonce uint64) (*Entry, error) {
	// The CTE column is aliased to candidate_id rather than left as id.
	//
	// UPDATE ... FROM brings both relations into scope for RETURNING, so an unqualified `id`
	// there is ambiguous and Postgres rejects the statement outright. Renaming the CTE column
	// leaves exactly one `id` in scope and keeps the shared column list usable unqualified.
	q := `
		WITH candidate AS (
			SELECT id AS candidate_id
			  FROM chain_outbox
			 WHERE scheme_id = $1 AND status = 'QUEUED'
			 ORDER BY created_at, id
			   FOR UPDATE SKIP LOCKED
			 LIMIT 1
		)
		UPDATE chain_outbox o
		   SET status        = 'SIGNED',
		       nonce         = $2,
		       attempt_count = o.attempt_count + 1,
		       updated_at    = now()
		  FROM candidate c
		 WHERE o.id = c.candidate_id AND o.status = 'QUEUED'
		RETURNING ` + selectColumns

	e, err := scanEntry(p.pool.QueryRow(ctx, q, schemeID, int64(nonce)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // nothing queued, or every candidate is locked by another worker
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: nonce %d", ErrNonceInUse, nonce)
		}
		return nil, fmt.Errorf("outbox: claim: %w", err)
	}
	return e, nil
}

func (p *PostgresStore) MarkBroadcast(ctx context.Context, id, txHash, gasPriceWei string) error {
	return p.transition(ctx, id, StatusBroadcast, `
		UPDATE chain_outbox
		   SET status = 'BROADCAST', tx_hash = $2, gas_price_wei = $3::numeric,
		       submitted_at = now(), updated_at = now()
		 WHERE id = $1 AND status = 'SIGNED'`,
		id, txHash, gasPriceWei)
}

func (p *PostgresStore) MarkPending(ctx context.Context, id string) error {
	return p.transition(ctx, id, StatusPending, `
		UPDATE chain_outbox SET status = 'PENDING', updated_at = now()
		 WHERE id = $1 AND status = 'BROADCAST'`, id)
}

func (p *PostgresStore) MarkConfirming(ctx context.Context, id string, blockNumber uint64, confirmations int) error {
	return p.transition(ctx, id, StatusConfirming, `
		UPDATE chain_outbox
		   SET status = 'CONFIRMING', block_number = $2, confirmations = $3, updated_at = now()
		 WHERE id = $1 AND status IN ('BROADCAST', 'PENDING', 'CONFIRMING')`,
		id, int64(blockNumber), confirmations)
}

// MarkConfirmed requires the evidence the fiat gate checks.
//
// The database already refuses a CONFIRMED row without a tx hash, block number, timestamp and at
// least five confirmations, via the constraint in migration 0007. Checking here too gives a clear
// error instead of a constraint violation, and the constraint remains the backstop for anything
// reaching the table by another path.
func (p *PostgresStore) MarkConfirmed(ctx context.Context, id string, blockNumber uint64, confirmations int) error {
	if blockNumber == 0 {
		return fmt.Errorf("%w: %s has no block number", ErrMissingEvidence, id)
	}

	current, err := p.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.TxHash == "" {
		return fmt.Errorf("%w: %s has no tx hash", ErrMissingEvidence, id)
	}

	return p.transition(ctx, id, StatusConfirmed, `
		UPDATE chain_outbox
		   SET status = 'CONFIRMED', block_number = $2, confirmations = $3,
		       confirmed_at = now(), updated_at = now()
		 WHERE id = $1 AND status = 'CONFIRMING'`,
		id, int64(blockNumber), confirmations)
}

func (p *PostgresStore) MarkFailed(ctx context.Context, id, reason string) error {
	return p.transition(ctx, id, StatusFailed, `
		UPDATE chain_outbox
		   SET status = 'FAILED', last_error = $2, updated_at = now()
		 WHERE id = $1 AND status IN ('QUEUED', 'SIGNED', 'BROADCAST', 'PENDING', 'CONFIRMING')`,
		id, reason)
}

// MarkReorged records that a mined transaction's block is gone.
//
// The block number and confirmation count are cleared, which migration 0011 also enforces with a
// constraint. A reorged row still carrying a confirmation count is the single most likely thing to
// be misread as still valid.
func (p *PostgresStore) MarkReorged(ctx context.Context, id, reason string) error {
	return p.transition(ctx, id, StatusReorged, `
		UPDATE chain_outbox
		   SET status = 'REORGED', last_error = $2,
		       block_number = NULL, confirmations = 0, updated_at = now()
		 WHERE id = $1 AND status = 'CONFIRMING'`,
		id, reason)
}

// Requeue returns an entry to QUEUED, releasing its nonce.
//
// The nonce is cleared rather than reused, because after a reorg the previous nonce may already
// have been consumed by a different transaction that survived. Re-signing from payload_json is the
// only safe path; replaying the old calldata against a stale nonce would either be rejected or
// replace unrelated work.
//
// attempt_count is deliberately preserved, so the retry ceiling still applies across requeues.
func (p *PostgresStore) Requeue(ctx context.Context, id, reason string) error {
	return p.transition(ctx, id, StatusQueued, `
		UPDATE chain_outbox
		   SET status = 'QUEUED', nonce = NULL, tx_hash = NULL, calldata = NULL,
		       block_number = NULL, confirmations = 0, gas_price_wei = NULL,
		       submitted_at = NULL, last_error = $2, updated_at = now()
		 WHERE id = $1 AND status IN ('SIGNED', 'PENDING', 'REORGED', 'FAILED')`,
		id, reason)
}

func (p *PostgresStore) DeadLetter(ctx context.Context, id, reason string) error {
	return p.transition(ctx, id, StatusDeadLetter, `
		UPDATE chain_outbox
		   SET status = 'DEAD_LETTER', last_error = $2,
		       dead_lettered_at = now(), updated_at = now()
		 WHERE id = $1 AND status IN ('FAILED', 'REORGED')`,
		id, reason)
}

// Cancel abandons an entry before broadcast.
//
// The nonce is nulled as part of the same statement. Migration 0011 refuses a CANCELLED row that
// still holds one, because a cancelled nonce would leave a permanent gap in the sequence and stall
// every later transaction behind it.
func (p *PostgresStore) Cancel(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE chain_outbox
		   SET status = 'CANCELLED', nonce = NULL, cancelled_at = now(), updated_at = now()
		 WHERE id = $1 AND status = 'QUEUED' AND tx_hash IS NULL`, id)
	if err != nil {
		return fmt.Errorf("outbox: cancel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		current, getErr := p.Get(ctx, id)
		if getErr != nil {
			return getErr
		}
		return fmt.Errorf("%w: %s is %s", ErrNotCancellable, id, current.Status)
	}
	return nil
}

func (p *PostgresStore) ListByStatus(ctx context.Context, schemeID string, statuses ...Status) ([]*Entry, error) {
	q := `SELECT ` + selectColumns + ` FROM chain_outbox WHERE scheme_id = $1`
	args := []any{schemeID}

	if len(statuses) > 0 {
		texts := make([]string, len(statuses))
		for i, s := range statuses {
			texts[i] = string(s)
		}
		q += ` AND status::text = ANY($2)`
		args = append(args, texts)
	}
	q += ` ORDER BY created_at, id`

	return p.query(ctx, q, args...)
}

// InFlight returns entries holding a nonce that have not reached a terminal state, ordered by
// nonce. This is what nonce recovery after a restart reads.
func (p *PostgresStore) InFlight(ctx context.Context, schemeID string) ([]*Entry, error) {
	return p.query(ctx, `
		SELECT `+selectColumns+`
		  FROM chain_outbox
		 WHERE scheme_id = $1
		   AND nonce IS NOT NULL
		   AND status NOT IN ('CONFIRMED', 'DEAD_LETTER', 'CANCELLED')
		 ORDER BY nonce`, schemeID)
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// transition applies an UPDATE whose WHERE clause carries the status precondition.
//
// Zero rows affected means the entry was not in an expected state. Distinguishing "not found" from
// "wrong state" costs one extra read and is worth it: the two have entirely different causes, and
// conflating them sends an operator looking for a missing row that is sitting right there.
func (p *PostgresStore) transition(ctx context.Context, id string, to Status, sql string, args ...any) error {
	tag, err := p.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("outbox: transition to %s: %w", to, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	current, getErr := p.Get(ctx, id)
	if getErr != nil {
		return getErr
	}
	return fmt.Errorf("%w: %s cannot move from %s to %s", ErrInvalidTransition, id, current.Status, to)
}

func (p *PostgresStore) query(ctx context.Context, sql string, args ...any) ([]*Entry, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("outbox: query: %w", err)
	}
	defer rows.Close()

	var out []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("outbox: scanning: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// SetCalldata records the encoded call at signing time, for audit.
func (p *PostgresStore) SetCalldata(ctx context.Context, id string, calldata []byte) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE chain_outbox SET calldata = $2, updated_at = now() WHERE id = $1`, id, calldata)
	if err != nil {
		return fmt.Errorf("outbox: setting calldata: %w", err)
	}
	return nil
}

// Compile-time assertion that the durable store satisfies the same contract as the in-memory one.
// Without it, a drift between the two would only surface when the relayer was pointed at Postgres.
var (
	_ Store = (*PostgresStore)(nil)
	_ Store = (*MemoryStore)(nil)
)
