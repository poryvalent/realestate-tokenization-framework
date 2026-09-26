-- ---------------------------------------------------------------------------
-- 0012_asba_actions
-- ---------------------------------------------------------------------------
--
-- Adds the two ASBA fund-movement actions to the admin_action vocabulary.
--
-- Why a new migration rather than an edit to 0001:
--
-- 0001 has already been applied and its checksum is recorded. The migration
-- runner compares checksums on every run and refuses to continue when a file
-- that has already run has changed, which is the behaviour that stops a
-- developer silently rewriting history that a deployed database has acted on.
-- Editing 0001 to add two enum values would trip that check on every
-- environment, and the obvious workaround, updating the stored checksum, is
-- exactly the habit the check exists to prevent.
--
-- Why these two actions did not exist already:
--
-- The vocabulary was written when ASBA was modelled as a single step inside bid
-- submission. Building the block lifecycle made it clear that reserving funds
-- and settling them are separate operations with separate idempotency
-- requirements: a block is placed once per bid and must never be repeated, while
-- a settlement instruction carries the allotted amount and has to change its key
-- if that amount is corrected. One shared action could not express both.

-- ALTER TYPE ... ADD VALUE is transaction-safe on PostgreSQL 12 and later,
-- provided the new value is not used in the same transaction. Nothing below uses
-- them, so this runs inside the runner's transaction like every other migration.

-- REQUEST_ASBA_BLOCK: ask the investor's bank to reserve the full bid amount.
-- Keyed by bid, because one bid gets one reservation and a second block on the
-- same bid would freeze twice the money.
ALTER TYPE admin_action ADD VALUE IF NOT EXISTS 'REQUEST_ASBA_BLOCK';

-- SETTLE_ASBA_BLOCK: debit the allotted portion and release the remainder, as a
-- single instruction. Keyed by bid and amount, so a corrected allotment produces
-- a different key rather than being deduped onto the earlier instruction.
ALTER TYPE admin_action ADD VALUE IF NOT EXISTS 'SETTLE_ASBA_BLOCK';

-- RELEASE_ASBA_BLOCK: return the entire amount. Used for a bid that won nothing
-- and for every bid when an offer is abandoned. Separate from SETTLE because it
-- carries no amount: there is nothing to correct, so the key needs nothing
-- beyond the bid.
ALTER TYPE admin_action ADD VALUE IF NOT EXISTS 'RELEASE_ASBA_BLOCK';
