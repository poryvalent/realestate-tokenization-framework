package store

import (
	"context"
	"time"

	"github.com/acresync/orchestrator/internal/ballotrun"
	"github.com/acresync/orchestrator/internal/bidbook"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/merkle"
)

// Writes for the ballot ceremony.
//
// Each one is a compare-and-set on ballot_runs.status, so two operators pressing the same ceremony button
// cannot both succeed, and ballot_runs_enforce_reveal_order still refuses anything out of order even if a
// caller skipped the Go checks.

// NewBallotRun is the row created when the bid book is frozen and its anchor queued.
type NewBallotRun struct {
	OfferID    string
	FrozenAt   time.Time
	Anchor     *bidbook.AnchorRequest
	OversubNum uint32
	OversubDen uint32
	Algo       uint32
	Key        []byte
}

// InsertBallotRun records the frozen book. The row starts at BIDBOOK_ANCHORED because its anchor is queued
// in the same transaction; whether that anchor has confirmed is read from the outbox, never from here.
func InsertBallotRun(ctx context.Context, w Writer, r NewBallotRun) (string, error) {
	var id string
	err := w.QueryRow(ctx, `
		INSERT INTO ballot_runs (
			offer_id, bidbook_snapshot_at, bidbook_merkle_root, bidbook_cid_digest,
			bid_leaf_count, total_units_bid, distinct_bidders,
			oversubscription_num, oversubscription_den, algo_version, status, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'BIDBOOK_ANCHORED', $11)
		RETURNING id`,
		r.OfferID, r.FrozenAt.UTC(), r.Anchor.Root[:], r.Anchor.CIDDigest[:],
		r.Anchor.LeafCount, int64(r.Anchor.TotalUnitsBid), r.Anchor.DistinctBidders,
		int64(r.OversubNum), int64(r.OversubDen), r.Algo, r.Key).Scan(&id)
	return id, err
}

// InsertPin records a pinned document against the entity it evidences.
//
// Content addressed, so the same bytes pinned twice are the same pin. A repeat is ignored rather than refused:
// pinning is idempotent by nature, and a second row for identical content would be two claims about one fact.
func InsertPin(ctx context.Context, w Writer, schemeID, docType, provider string, pin *ipfs.Pin, entityType, entityID string, key []byte) error {
	cid := pin.ProviderCID
	if cid == "" {
		cid = pin.DerivedCID
	}
	_, err := w.Exec(ctx, `
		INSERT INTO ipfs_pins (
			scheme_id, doc_type, content_sha256, cid, byte_size, provider,
			related_entity_type, related_entity_id, idempotency_key
		) VALUES ($1, $2::doc_type, $3, $4, $5, $6::ipfs_provider, $7, $8, $9)
		ON CONFLICT (content_sha256) DO NOTHING`,
		schemeID, docType, pin.Digest[:], cid, pin.ByteSize, provider, entityType, entityID, key)
	return err
}

// CommitSeed records the commitment and the attempt. The target block stays unset: the contract chooses it,
// and it is known only once commitSeed confirms.
func CommitSeed(ctx context.Context, w Writer, runID string, commitment merkle.Hash, attempt uint32) error {
	tag, err := w.Exec(ctx, `
		UPDATE ballot_runs SET seed_commitment = $2, attempt = $3, status = 'SEED_COMMITTED'
		 WHERE id = $1 AND status = 'BIDBOOK_ANCHORED'`, runID, commitment[:], attempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// Recommit records a fresh commit window. The commitment is untouched, and the trigger would refuse a change;
// only the attempt moves, and the target block is cleared until the recommit confirms and names a new one.
func Recommit(ctx context.Context, w Writer, runID string, fromAttempt, toAttempt uint32) error {
	tag, err := w.Exec(ctx, `
		UPDATE ballot_runs SET attempt = $3, target_block = NULL
		 WHERE id = $1 AND status = 'SEED_COMMITTED' AND attempt = $2`, runID, fromAttempt, toAttempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// Reveal records the secret, the target block and its hash, and the final seed, in one write.
//
// One UPDATE, because ballot_runs_enforce_reveal_order judges the new row as a whole: the plaintext is
// accepted only alongside the anchored commitment and the target block, and writing them separately would
// either fail or leave a window where the secret exists without the facts that make it verifiable.
func Reveal(ctx context.Context, w Writer, runID, commitTx string, targetBlock uint64, secret, targetHash, finalSeed merkle.Hash) error {
	tag, err := w.Exec(ctx, `
		UPDATE ballot_runs
		   SET commitment_anchored_tx = $2, target_block = $3, seed_plaintext = $4,
		       target_block_hash = $5, final_seed = $6, status = 'SEED_REVEALED'
		 WHERE id = $1 AND status = 'SEED_COMMITTED'`,
		runID, commitTx, int64(targetBlock), secret[:], targetHash[:], finalSeed[:])
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// RecordDraw writes one allocation per bid, every bid's outcome, and the result root.
//
// Losers are recorded too. A losing bidder needs a published rank to check their own result against, so an
// allocation for every bid is what makes the draw falsifiable for exactly the people it disappointed.
func RecordDraw(ctx context.Context, w Writer, runID string, run *ballotrun.Run, resultRoot, resultCID merkle.Hash, drawnAt time.Time) error {
	for _, l := range run.Lines {
		if _, err := w.Exec(ctx, `
			INSERT INTO allocations (
				ballot_run_id, bid_id, units_allotted, amount_payable_paise,
				refund_amount_paise, outcome, ballot_rank
			) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			runID, l.BidID, l.UnitsAllotted, int64(l.AmountPayable), int64(l.RefundAmount),
			string(l.Outcome), l.BallotRank); err != nil {
			return err
		}

		status, reason := bidOutcomeStatus(string(l.Outcome))
		if _, err := w.Exec(ctx, `
			UPDATE bids SET status = $2::bid_status, rejection_reason = $3::bid_rejection_reason, updated_at = now()
			 WHERE id = $1`, l.BidID, status, reason); err != nil {
			return err
		}
	}

	tag, err := w.Exec(ctx, `
		UPDATE ballot_runs
		   SET status = 'RESULT_ANCHORED', executed_at = $2, result_merkle_root = $3, result_cid_digest = $4
		 WHERE id = $1 AND status = 'SEED_REVEALED'`,
		runID, drawnAt.UTC(), resultRoot[:], resultCID[:])
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// bidOutcomeStatus maps a ballot outcome to the bid's status.
//
// bids_rejection_reason_present pairs every REJECTED_* status with a reason and forbids a reason on any
// other, so the two are always set together.
func bidOutcomeStatus(outcome string) (status string, reason any) {
	switch outcome {
	case "FULL":
		return "ALLOTTED_FULL", nil
	case "PARTIAL":
		return "ALLOTTED_PARTIAL", nil
	case "NIL_TECHNICAL":
		return "REJECTED_TECHNICAL", "BALLOT_NOT_DRAWN"
	default:
		return "REJECTED_BALLOT", "BALLOT_NOT_DRAWN"
	}
}
