// Package relayer drains the outbox onto the chain.
//
// # The loop
//
// One worker per scheme, deliberately. Nonces are a single sequence per address, so concurrency
// within a scheme buys nothing and costs the guarantee that transaction N is broadcast before
// N+1. A gap in the sequence stalls everything behind it until the gap is filled, which is worse
// than serialising in the first place.
//
// Each pass does two things: advance in-flight transactions toward confirmation, then claim and
// broadcast one queued entry. Advancing first matters, because a confirmed transaction releases
// its nonce and a reorged one has to be requeued before the next claim picks a nonce.
//
// # Nonce recovery after a restart
//
// The node's pending nonce is used only to seed the sequence. From then on the outbox is
// authoritative, because the node's view lags a transaction that has been signed but not yet
// accepted. Trusting the node after a crash between signing and broadcasting would hand the same
// nonce to different work, and one of the two anchors would vanish while the orchestrator
// believed it had made both.
package relayer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/acresync/orchestrator/internal/chain"
	"github.com/acresync/orchestrator/internal/outbox"
)

// Encoder turns a queued entry's recorded arguments into calldata.
//
// An interface rather than a concrete encoder because the entry stores arguments, not calldata.
// A requeued entry is therefore re-encoded from the recorded intent rather than replaying bytes
// signed against a nonce that may no longer be available.
type Encoder interface {
	Encode(e *outbox.Entry) (chain.Call, error)
}

// Relayer drains one scheme's outbox.
type Relayer struct {
	store   outbox.Store
	client  *chain.Client
	encoder Encoder
	cfg     outbox.Config
	log     *slog.Logger

	schemeID string

	// nextNonce is the relayer's own view of the sequence, seeded from the node and then owned
	// here.
	nextNonce uint64
	seeded    bool

	// maxFeeCapWei stalls the queue rather than draining the relayer during a fee spike.
	maxFeeCapWei *big.Int
}

type Options struct {
	SchemeID     string
	Config       outbox.Config
	Logger       *slog.Logger
	MaxFeeCapWei *big.Int
}

func New(store outbox.Store, client *chain.Client, encoder Encoder, opts Options) (*Relayer, error) {
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	if opts.SchemeID == "" {
		return nil, errors.New("relayer: SchemeID is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Relayer{
		store:        store,
		client:       client,
		encoder:      encoder,
		cfg:          opts.Config,
		log:          log.With("component", "relayer", "scheme", opts.SchemeID),
		schemeID:     opts.SchemeID,
		maxFeeCapWei: opts.MaxFeeCapWei,
	}, nil
}

// Run drains the outbox until the context is cancelled.
func (r *Relayer) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		if err := r.Step(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A failing pass is logged and retried. Returning would stop the relayer on a
			// transient RPC error, and a stopped relayer is indistinguishable from a stuck one
			// to everything downstream.
			r.log.Error("relayer pass failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Step performs one pass. Exported so tests can drive the loop deterministically instead of
// waiting on a ticker.
func (r *Relayer) Step(ctx context.Context) error {
	if err := r.advanceInFlight(ctx); err != nil {
		return err
	}
	return r.broadcastNext(ctx)
}

// SeedNonce establishes the starting nonce.
//
// Takes the higher of the node's pending nonce and one past the highest nonce the outbox has
// already handed out. The outbox wins when it is ahead, which is exactly the case after a crash
// between signing and broadcasting: the node has not seen that transaction, so its pending nonce
// would reissue a slot the outbox considers spent.
func (r *Relayer) SeedNonce(ctx context.Context) error {
	nodeNonce, err := r.client.PendingNonce(ctx)
	if err != nil {
		return fmt.Errorf("relayer: reading the pending nonce: %w", err)
	}

	inFlight, err := r.store.InFlight(ctx, r.schemeID)
	if err != nil {
		return fmt.Errorf("relayer: reading in-flight entries: %w", err)
	}

	next := nodeNonce
	for _, e := range inFlight {
		if e.Nonce != nil && *e.Nonce+1 > next {
			next = *e.Nonce + 1
		}
	}

	r.nextNonce = next
	r.seeded = true
	r.log.Info("nonce seeded",
		"node_pending", nodeNonce, "in_flight", len(inFlight), "next", next)
	return nil
}

// advanceInFlight moves broadcast transactions toward confirmation.
func (r *Relayer) advanceInFlight(ctx context.Context) error {
	entries, err := r.store.ListByStatus(ctx, r.schemeID,
		outbox.StatusBroadcast, outbox.StatusPending, outbox.StatusConfirming)
	if err != nil {
		return fmt.Errorf("relayer: listing in-flight entries: %w", err)
	}

	for _, e := range entries {
		if err := r.advanceOne(ctx, e); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.log.Error("advancing an entry failed", "id", e.ID, "error", err)
		}
	}
	return nil
}

func (r *Relayer) advanceOne(ctx context.Context, e *outbox.Entry) error {
	if e.TxHash == "" {
		return fmt.Errorf("entry %s is %s with no tx hash", e.ID, e.Status)
	}
	hash := common.HexToHash(e.TxHash)

	confirmations, receipt, err := r.client.ConfirmationsFor(ctx, hash)

	switch {
	case errors.Is(err, chain.ErrNoReceipt):
		// A receipt that has disappeared is a reorg, not a pending transaction.
		//
		// This case is easy to miss. The obvious reorg is a receipt still naming a block that is
		// no longer canonical, which ConfirmationsFor reports as ErrReorged. But a deeper reorg
		// drops the transaction from the chain altogether, and then the receipt is simply gone.
		// Treating that as "still in the mempool" leaves an already-mined entry stuck in
		// CONFIRMING forever, waiting for confirmations that will never arrive, and because
		// CONFIRMING is not terminal nothing else ever notices.
		//
		// An entry that has recorded a block number was mined. If its receipt is now absent, the
		// chain has changed under us.
		if e.BlockNumber != nil {
			r.log.Warn("receipt disappeared for a previously mined transaction, treating as a reorg",
				"id", e.ID, "tx", e.TxHash, "was_block", *e.BlockNumber)
			if err := r.store.MarkReorged(ctx, e.ID, "receipt disappeared after the transaction was mined"); err != nil {
				return err
			}
			return r.requeueOrDeadLetter(ctx, e, "reorged: receipt disappeared")
		}

		// Genuinely still in the mempool. Moved to PENDING once so the state reflects reality,
		// then left alone until the receipt timeout.
		if e.Status == outbox.StatusBroadcast {
			if err := r.store.MarkPending(ctx, e.ID); err != nil {
				return err
			}
		}
		return r.checkStuck(ctx, e)

	case errors.Is(err, chain.ErrReorged):
		// The block the receipt named is no longer canonical. Recorded as reorged before being
		// requeued, so the history shows this anchor was mined and then unmined rather than
		// simply being re-sent.
		r.log.Warn("reorg detected", "id", e.ID, "tx", e.TxHash, "error", err)
		if err := r.store.MarkReorged(ctx, e.ID, err.Error()); err != nil {
			return err
		}
		return r.requeueOrDeadLetter(ctx, e, "reorged")

	case err != nil:
		return fmt.Errorf("checking confirmations for %s: %w", e.ID, err)
	}

	if !receipt.Success {
		// Reverted on chain. Gas was spent, so this is not retryable without understanding why,
		// and a silent retry would spend it again on the same failure.
		r.log.Error("transaction reverted", "id", e.ID, "tx", e.TxHash, "block", receipt.BlockNumber)
		if err := r.store.MarkFailed(ctx, e.ID, "execution reverted on chain"); err != nil {
			return err
		}
		return r.store.DeadLetter(ctx, e.ID, "reverted on chain; needs investigation before retry")
	}

	if confirmations >= r.cfg.ConfirmationDepth {
		if e.Status != outbox.StatusConfirming {
			if err := r.store.MarkConfirming(ctx, e.ID, receipt.BlockNumber, confirmations); err != nil {
				return err
			}
		}
		if err := r.store.MarkConfirmed(ctx, e.ID, receipt.BlockNumber, confirmations); err != nil {
			return err
		}
		r.log.Info("confirmed",
			"id", e.ID, "tx", e.TxHash, "block", receipt.BlockNumber, "confirmations", confirmations)
		return nil
	}

	if e.Status != outbox.StatusConfirming || e.Confirmations != confirmations {
		return r.store.MarkConfirming(ctx, e.ID, receipt.BlockNumber, confirmations)
	}
	return nil
}

// checkStuck requeues a transaction that has sat without a receipt past the timeout.
//
// Requeued rather than rebroadcast, so it is re-signed with a fresh nonce and current fees. A
// transaction stuck because the fee is now too low will stay stuck no matter how often the same
// bytes are resent, and every later nonce is blocked behind it.
func (r *Relayer) checkStuck(ctx context.Context, e *outbox.Entry) error {
	if e.SubmittedAt == nil {
		return nil
	}
	//acresync:allow-wallclock receipt timeout is transport behaviour, not a business deadline
	if time.Since(*e.SubmittedAt) < r.cfg.ReceiptTimeout {
		return nil
	}

	r.log.Warn("no receipt within the timeout, requeueing",
		"id", e.ID, "tx", e.TxHash, "submitted_at", e.SubmittedAt)
	return r.requeueOrDeadLetter(ctx, e, "no receipt within the timeout")
}

// requeueOrDeadLetter applies the retry ceiling.
//
// Without a ceiling a permanent failure becomes an infinite gas burn, and the real error is
// buried under thousands of identical attempts.
func (r *Relayer) requeueOrDeadLetter(ctx context.Context, e *outbox.Entry, reason string) error {
	if e.AttemptCount < r.cfg.MaxAttempts {
		return r.store.Requeue(ctx, e.ID, reason)
	}

	r.log.Error("retry ceiling reached, dead lettering",
		"id", e.ID, "attempts", e.AttemptCount, "reason", reason)

	detail := fmt.Sprintf("%s after %d attempts", reason, e.AttemptCount)

	// DEAD_LETTER is only reachable from FAILED or REORGED, so the failure is recorded before
	// the entry is parked. That ordering is enforced by the state graph rather than left to
	// convention, and it is worth keeping: a dead-lettered entry is one a human has to pick up,
	// and arriving at it without a recorded reason wastes the first hour of that investigation.
	current, err := r.store.Get(ctx, e.ID)
	if err != nil {
		return err
	}
	if current.Status != outbox.StatusFailed && current.Status != outbox.StatusReorged {
		if err := r.store.MarkFailed(ctx, e.ID, detail); err != nil {
			return err
		}
	}
	return r.store.DeadLetter(ctx, e.ID, detail)
}

// broadcastNext claims one queued entry and sends it.
func (r *Relayer) broadcastNext(ctx context.Context) error {
	if !r.seeded {
		if err := r.SeedNonce(ctx); err != nil {
			return err
		}
	}

	nonce := r.nextNonce

	entry, err := r.store.ClaimNextQueued(ctx, r.schemeID, nonce)
	if err != nil {
		if errors.Is(err, outbox.ErrNonceInUse) {
			// Another holder of this nonce means the seed was stale. Re-seeding is correct;
			// guessing the next value would risk a gap.
			r.log.Warn("nonce already in use, re-seeding", "nonce", nonce)
			r.seeded = false
			return nil
		}
		return fmt.Errorf("relayer: claiming the next queued entry: %w", err)
	}
	if entry == nil {
		return nil // nothing to do
	}

	call, err := r.encoder.Encode(entry)
	if err != nil {
		// A call that cannot be encoded will never encode, so retrying is pointless.
		r.log.Error("encoding failed", "id", entry.ID, "function", entry.FunctionName, "error", err)
		if ferr := r.store.MarkFailed(ctx, entry.ID, "encoding failed: "+err.Error()); ferr != nil {
			return ferr
		}
		return r.store.DeadLetter(ctx, entry.ID, "calldata could not be encoded")
	}

	result, err := r.client.Send(ctx, call, chain.SendOpts{
		Nonce:        nonce,
		MaxFeeCapWei: r.maxFeeCapWei,
	})
	if err != nil {
		r.log.Error("broadcast failed", "id", entry.ID, "nonce", nonce, "error", err)

		// The nonce was not consumed, so the entry returns to the queue and the sequence does
		// not advance. Advancing here would leave a permanent gap that stalls everything behind
		// it.
		if rerr := r.requeueOrDeadLetter(ctx, entry, "broadcast failed: "+err.Error()); rerr != nil {
			return rerr
		}
		return nil
	}

	if err := r.store.MarkBroadcast(ctx, entry.ID, result.TxHash.Hex(), result.GasFeeCap.String()); err != nil {
		// The transaction is on the network but unrecorded, which is the one genuinely dangerous
		// outcome here: a later pass will not know to watch it. Logged loudly with the hash so
		// it can be reconciled by hand.
		r.log.Error("BROADCAST BUT NOT RECORDED, reconcile manually",
			"id", entry.ID, "tx", result.TxHash.Hex(), "nonce", nonce, "error", err)
		return err
	}

	r.nextNonce = nonce + 1
	r.log.Info("broadcast",
		"id", entry.ID, "function", entry.FunctionName,
		"tx", result.TxHash.Hex(), "nonce", nonce, "gas_limit", result.GasLimit)
	return nil
}

// NextNonce exposes the relayer's view of the sequence, for diagnostics.
func (r *Relayer) NextNonce() uint64 { return r.nextNonce }
