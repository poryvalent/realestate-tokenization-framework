// Package outbox is the durable queue between AcreSync's business logic and the chain.
//
// # Why an outbox rather than direct calls
//
// The orchestrator is the sole authorised relayer. Every anchor the chain receives comes from
// one key, in order, and the orchestrator has to survive being killed halfway through sending
// one. Calling the chain directly from business code loses on all three counts: a crash between
// "we decided to anchor" and "we broadcast" leaves no record of the intent, two goroutines
// racing produce two transactions with the same nonce, and a retry after a timeout resubmits
// work that may already have landed.
//
// Writing the intent to a durable row first, then draining it, makes each of those recoverable.
// The row is the record of intent; the state machine is the record of how far it got.
//
// # The states, and why each exists
//
//	QUEUED      intent recorded, nothing sent. The only genuinely clean undo.
//	SIGNED      nonce assigned and transaction signed, not yet broadcast.
//	BROADCAST   handed to the node.
//	PENDING     accepted into the mempool, no receipt yet.
//	CONFIRMING  mined, fewer than the required confirmations.
//	CONFIRMED   mined with enough confirmations. The only state fiat may act on.
//	REORGED     was confirming, then the block disappeared. Returns to QUEUED.
//	FAILED      reverted on chain, or a permanent send error.
//	DEAD_LETTER retries exhausted. Needs a human.
//	CANCELLED   abandoned before broadcast.
//
// SIGNED exists as a distinct state because that is where the nonce is consumed. A crash
// between assigning a nonce and broadcasting must not silently reassign that nonce to different
// work, or two different anchors compete for one slot and one is lost.
//
// # What this package deliberately does not do
//
// It does not decide whether a payout may be created. That gate lives in the database, where
// payout_instructions.gated_on_anchor_tx must reference a row in CONFIRMED with at least the
// configured confirmations. The reason is that a reorg after money has left the escrow is
// unrecoverable, and the check belongs next to the money rather than next to the queue.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/acresync/orchestrator/internal/idempotency"
)

// Status is the lifecycle position of one outbox row. Values match the Postgres
// outbox_status enum.
type Status string

const (
	StatusQueued     Status = "QUEUED"
	StatusSigned     Status = "SIGNED"
	StatusBroadcast  Status = "BROADCAST"
	StatusPending    Status = "PENDING"
	StatusConfirming Status = "CONFIRMING"
	StatusConfirmed  Status = "CONFIRMED"
	StatusReorged    Status = "REORGED"
	StatusFailed     Status = "FAILED"
	StatusDeadLetter Status = "DEAD_LETTER"
	StatusCancelled  Status = "CANCELLED"
)

// Terminal reports whether no further transition is expected.
func (s Status) Terminal() bool {
	switch s {
	case StatusConfirmed, StatusDeadLetter, StatusCancelled:
		return true
	}
	return false
}

// AllStatuses is the authoritative list, kept in step with the Postgres enum.
var AllStatuses = []Status{
	StatusQueued, StatusSigned, StatusBroadcast, StatusPending, StatusConfirming,
	StatusConfirmed, StatusReorged, StatusFailed, StatusDeadLetter, StatusCancelled,
}

// transitions is the permitted state graph.
//
// Enumerated rather than left implicit because the failure it prevents is quiet: a row that
// slipped from BROADCAST straight to CONFIRMED without passing the confirmation count would
// satisfy the fiat gate while having had its confirmations checked by nobody.
var transitions = map[Status][]Status{
	StatusQueued:     {StatusSigned, StatusCancelled, StatusFailed},
	StatusSigned:     {StatusBroadcast, StatusFailed, StatusQueued},
	StatusBroadcast:  {StatusPending, StatusConfirming, StatusFailed},
	StatusPending:    {StatusConfirming, StatusFailed, StatusQueued},
	StatusConfirming: {StatusConfirmed, StatusReorged, StatusFailed},
	StatusReorged:    {StatusQueued, StatusDeadLetter},
	StatusFailed:     {StatusQueued, StatusDeadLetter},
	// Terminal states have no outbound edges. CONFIRMED in particular: an anchor that fiat
	// has already been released against must never move again.
	StatusConfirmed:  {},
	StatusDeadLetter: {},
	StatusCancelled:  {},
}

// CanTransition reports whether from -> to is permitted.
func CanTransition(from, to Status) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

var (
	ErrNotFound          = errors.New("outbox: entry not found")
	ErrDuplicateKey      = errors.New("outbox: idempotency key already enqueued")
	ErrInvalidTransition = errors.New("outbox: invalid state transition")
	ErrNotCancellable    = errors.New("outbox: only a queued entry can be cancelled")
	ErrNonceInUse        = errors.New("outbox: nonce already assigned to another in-flight entry")
	ErrRetriesExhausted  = errors.New("outbox: retry ceiling reached")
	ErrMissingEvidence   = errors.New("outbox: confirmation requires a tx hash and block number")
)

// Entry is one queued chain call.
type Entry struct {
	ID       string
	SchemeID string

	TargetContract string
	FunctionName   string

	// Payload is the canonical JSON of the call arguments, kept for audit and for rebuilding
	// calldata after a reorg. Storing the arguments rather than encoded calldata means a
	// requeued entry is re-encoded rather than replayed blind.
	Payload []byte

	// PayloadHash lets a redelivered message be recognised as identical without comparing
	// JSON semantics.
	PayloadHash [32]byte

	// Calldata is the ABI-encoded call, set when the entry is signed.
	Calldata []byte

	IdempotencyKey idempotency.Key

	Nonce       *uint64
	TxHash      string
	GasPriceWei string
	BlockNumber *uint64

	Status        Status
	Confirmations int
	AttemptCount  int
	LastError     string

	RelatedEntityType string
	RelatedEntityID   string
	EnvironmentTag    string

	CreatedAt      time.Time
	SubmittedAt    *time.Time
	ConfirmedAt    *time.Time
	DeadLetteredAt *time.Time
	CancelledAt    *time.Time
}

// ReadyForFiat reports whether this anchor may authorise a fiat instruction.
//
// The single most dangerous link in the architecture is the orchestrator reading chain state and
// then irreversibly moving money. A reorg after the transfer has cleared cannot be undone, so
// the bar is both CONFIRMED and a confirmation count at or above the configured depth, with the
// evidence present to prove it.
func (e *Entry) ReadyForFiat(requiredConfirmations int) bool {
	return e.Status == StatusConfirmed &&
		e.Confirmations >= requiredConfirmations &&
		e.TxHash != "" &&
		e.BlockNumber != nil
}

// NewEntry describes work to enqueue.
type NewEntry struct {
	SchemeID          string
	TargetContract    string
	FunctionName      string
	Payload           []byte
	IdempotencyKey    idempotency.Key
	RelatedEntityType string
	RelatedEntityID   string
	EnvironmentTag    string
}

// Store persists outbox entries.
//
// Implementations must enforce the transition graph and idempotency-key uniqueness themselves
// rather than trusting the caller, because the caller is the component most likely to have the
// bug and the consequence is a double anchor.
type Store interface {
	Enqueue(ctx context.Context, e NewEntry) (*Entry, error)
	Get(ctx context.Context, id string) (*Entry, error)
	GetByIdempotencyKey(ctx context.Context, key idempotency.Key) (*Entry, error)

	// ClaimNextQueued atomically takes the oldest queued entry for a scheme and moves it to
	// SIGNED with the given nonce. Atomicity is what stops two workers assigning one nonce to
	// two different calls.
	ClaimNextQueued(ctx context.Context, schemeID string, nonce uint64) (*Entry, error)

	MarkBroadcast(ctx context.Context, id, txHash, gasPriceWei string) error
	MarkPending(ctx context.Context, id string) error
	MarkConfirming(ctx context.Context, id string, blockNumber uint64, confirmations int) error
	MarkConfirmed(ctx context.Context, id string, blockNumber uint64, confirmations int) error
	MarkFailed(ctx context.Context, id, reason string) error

	// MarkReorged records that a confirming transaction's block disappeared.
	//
	// A distinct state rather than a direct return to QUEUED, because a reorg is a fact worth
	// keeping. "This anchor was mined, then unmined, then re-sent" is exactly the history
	// someone will want during an incident, and collapsing it into a requeue erases it.
	MarkReorged(ctx context.Context, id, reason string) error

	// Requeue returns an entry to QUEUED, clearing the nonce and tx hash so it is re-signed
	// rather than rebroadcast. Used after a reorg or a recoverable failure.
	Requeue(ctx context.Context, id, reason string) error

	DeadLetter(ctx context.Context, id, reason string) error
	Cancel(ctx context.Context, id string) error

	ListByStatus(ctx context.Context, schemeID string, statuses ...Status) ([]*Entry, error)

	// InFlight returns entries that have consumed a nonce and not yet reached a terminal
	// state, which is what nonce recovery after a restart needs.
	InFlight(ctx context.Context, schemeID string) ([]*Entry, error)
}

// Config tunes the relayer's behaviour.
type Config struct {
	// ConfirmationDepth is the number of confirmations before an anchor may authorise fiat.
	ConfirmationDepth int

	// MaxAttempts bounds retries before dead-lettering. A retry loop with no ceiling turns a
	// permanent failure into an infinite gas burn and buries the real error.
	MaxAttempts int

	// PollInterval is how often pending transactions are checked.
	PollInterval time.Duration

	// ReceiptTimeout bounds how long a broadcast transaction may sit without a receipt before
	// it is treated as stuck.
	ReceiptTimeout time.Duration
}

func DefaultConfig() Config {
	return Config{
		ConfirmationDepth: 5,
		MaxAttempts:       5,
		PollInterval:      4 * time.Second,
		ReceiptTimeout:    10 * time.Minute,
	}
}

func (c Config) Validate() error {
	if c.ConfirmationDepth < 1 {
		return fmt.Errorf("outbox: ConfirmationDepth must be at least 1")
	}
	if c.MaxAttempts < 1 {
		return fmt.Errorf("outbox: MaxAttempts must be at least 1")
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("outbox: PollInterval must be positive")
	}
	if c.ReceiptTimeout <= 0 {
		return fmt.Errorf("outbox: ReceiptTimeout must be positive")
	}
	return nil
}
