package store

import (
	"context"
	"time"

	"github.com/acresync/orchestrator/internal/outbox"
)

// The chain outbox, read for the operator console.
//
// # Why a query here rather than reusing outbox.PostgresStore
//
// That store is the relayer's. Its reads select the payload, the calldata and the idempotency key, because the
// relayer needs them to sign and to rebuild a transaction after a reorg. None of that belongs in an HTTP
// response: calldata and a payload are large, and an idempotency key is the value that makes a replay safe, so
// publishing it hands a caller the one input that would let them collide with a pending action.
//
// It also takes a *pgxpool.Pool, which would make this surface untestable inside a rolled-back transaction.
//
// So this is a narrower query over the same table, selecting only what the contract publishes.

// OutboxEntry is a chain queue entry as the operator console sees it.
type OutboxEntry struct {
	ID             string
	SchemeID       string
	TargetContract string
	FunctionName   string
	Status         outbox.Status

	// Nil until the transaction is signed and broadcast. A zero nonce is a legitimate value, so these are
	// pointers: reporting an unsigned entry as nonce zero would look like the first transaction ever sent.
	Nonce       *int64
	TxHash      string
	BlockNumber *int64

	Confirmations int32
	Attempts      int32
	LastError     string

	EnvironmentTag string
	CreatedAt      time.Time
}

// Outbox reads the chain outbox.
type Outbox struct {
	q Querier
}

// NewOutbox binds a store to a pool or transaction.
func NewOutbox(q Querier) Outbox { return Outbox{q: q} }

// OutboxFilter narrows a listing.
type OutboxFilter struct {
	// SchemeID limits to one scheme, empty for all.
	SchemeID string
	// Status limits to one status, empty for all.
	Status string
}

// List returns outbox entries, newest first.
//
// Newest first because the operational question is almost always "what is happening now", and an entry that has
// just failed is the one somebody is looking for. Ordered by created_at then id, so entries queued in the same
// transaction have a deterministic order rather than whatever the planner chose.
func (s Outbox) List(ctx context.Context, f OutboxFilter, p Pagination) ([]OutboxEntry, error) {
	p = p.normalise()

	const q = `
		SELECT id, scheme_id, target_contract, function_name, status::text,
		       nonce, coalesce(tx_hash, ''), block_number,
		       confirmations, attempt_count, coalesce(last_error, ''),
		       environment_tag::text, created_at
		FROM chain_outbox
		WHERE ($1 = '' OR scheme_id = $1::uuid)
		  AND ($2 = '' OR status::text = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3`

	rows, err := s.q.Query(ctx, q, f.SchemeID, f.Status, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutboxEntry
	for rows.Next() {
		var (
			e          OutboxEntry
			statusText string
		)
		if err := rows.Scan(
			&e.ID, &e.SchemeID, &e.TargetContract, &e.FunctionName, &statusText,
			&e.Nonce, &e.TxHash, &e.BlockNumber,
			&e.Confirmations, &e.Attempts, &e.LastError,
			&e.EnvironmentTag, &e.CreatedAt,
		); err != nil {
			return nil, err
		}

		e.Status = outbox.Status(statusText)
		if !outboxStatusValid(e.Status) {
			return nil, unknownEnum("chain_outbox.status", statusText)
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// outboxStatusValid checks a status against the relayer's own set.
//
// outbox.Status has no Valid method, so the set is listed here. That is a worse arrangement than asking the
// domain, and the test in this package compares this list against the SQL enum so the two cannot drift apart
// silently.
func outboxStatusValid(s outbox.Status) bool {
	for _, known := range AllOutboxStatuses() {
		if s == known {
			return true
		}
	}
	return false
}

// AllOutboxStatuses is the chain queue's lifecycle, in the order an entry passes through it.
//
// REORGED, FAILED, DEAD_LETTER and CANCELLED are the ways it leaves that path.
func AllOutboxStatuses() []outbox.Status {
	return []outbox.Status{
		outbox.StatusQueued,
		outbox.StatusSigned,
		outbox.StatusBroadcast,
		outbox.StatusPending,
		outbox.StatusConfirming,
		outbox.StatusConfirmed,
		outbox.StatusReorged,
		outbox.StatusFailed,
		outbox.StatusDeadLetter,
		outbox.StatusCancelled,
	}
}
