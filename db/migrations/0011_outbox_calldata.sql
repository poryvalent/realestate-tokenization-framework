-- AcreSync M4 :: 0011 :: outbox support for the relayer worker
--
-- The chain_outbox table from 0007 was designed before the relayer existed. Two additions and a
-- pair of indexes are needed for it to be drained safely by a worker.

-- ---------------------------------------------------------------------------
-- Encoded calldata
-- ---------------------------------------------------------------------------

-- The ABI-encoded call, set when an entry is signed.
--
-- payload_json already holds the call arguments, and that remains the authoritative record. This
-- column is the encoding of those arguments at the moment of signing, kept for two reasons: a
-- transaction can be inspected against exactly what was signed, and a mismatch between the two
-- columns is detectable rather than invisible.
--
-- Deliberately nullable and deliberately not reused after a requeue. Following a reorg the old
-- nonce may already have been consumed by a different transaction that survived, so the entry is
-- re-encoded from payload_json rather than having these bytes replayed.
ALTER TABLE chain_outbox ADD COLUMN calldata BYTEA;

COMMENT ON COLUMN chain_outbox.calldata IS
    'ABI-encoded call as signed. Re-derived from payload_json on requeue, never replayed.';

-- ---------------------------------------------------------------------------
-- Claim path
-- ---------------------------------------------------------------------------

-- Supports the relayer claiming the oldest queued entry for a scheme.
--
-- The claim uses FOR UPDATE SKIP LOCKED, so concurrent workers take different rows instead of
-- queueing behind one another. Without SKIP LOCKED a second worker would block on the first
-- worker's row lock and the queue would serialise on lock contention rather than on the nonce
-- sequence, which is the thing that actually has to be serial.
CREATE INDEX chain_outbox_claim_idx
    ON chain_outbox (scheme_id, created_at, id)
    WHERE status = 'QUEUED';

-- Supports the confirmation-tracking sweep, which reads everything mid-flight each pass.
CREATE INDEX chain_outbox_inflight_idx
    ON chain_outbox (scheme_id, status)
    WHERE status IN ('BROADCAST', 'PENDING', 'CONFIRMING');

-- ---------------------------------------------------------------------------
-- Confirmation evidence
-- ---------------------------------------------------------------------------

-- 0007 already requires a CONFIRMED row to carry a tx hash, a block number, a timestamp and at
-- least five confirmations. That constraint is the database half of the fiat gate and is what
-- payout_instructions.gated_on_anchor_tx relies on.
--
-- This adds the matching rule for the state immediately before it: a row cannot claim to be
-- CONFIRMING without naming the block it was mined in. Otherwise a confirmation count could be
-- recorded against no block at all, and the reorg check has nothing to compare against.
ALTER TABLE chain_outbox
    ADD CONSTRAINT outbox_confirming_has_block CHECK (
        status <> 'CONFIRMING' OR (tx_hash IS NOT NULL AND block_number IS NOT NULL)
    );

-- A reorged row must have released its block evidence. Keeping a stale confirmation count on a
-- reorged entry is the single most likely way for one to be misread as still valid.
ALTER TABLE chain_outbox
    ADD CONSTRAINT outbox_reorged_has_no_evidence CHECK (
        status <> 'REORGED' OR (block_number IS NULL AND confirmations = 0)
    );

-- ---------------------------------------------------------------------------
-- Cancellation
-- ---------------------------------------------------------------------------

-- 0007 requires a CANCELLED row to have no tx hash. Strengthened here to also require no nonce:
-- cancelling after a nonce was assigned would leave a permanent gap in the sequence, and every
-- later transaction would stall behind it.
ALTER TABLE chain_outbox
    ADD CONSTRAINT outbox_cancelled_holds_no_nonce CHECK (
        status <> 'CANCELLED' OR nonce IS NULL
    );
