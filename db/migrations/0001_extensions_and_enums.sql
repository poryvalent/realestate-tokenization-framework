-- AcreSync M0 :: 0001 :: extensions and enumerated types
--
-- Conventions that apply across every migration in this directory:
--
--   * Money is always BIGINT paise. There is no NUMERIC-with-scale and no
--     DOUBLE PRECISION anywhere in the money path. The distribution invariant
--     (sum of per-holder entitlements equals distributed_paise exactly) cannot
--     be guaranteed under binary floating point.
--
--   * Ratios are stored as exact scaled integers (NUMERIC(39,0)) or, preferably,
--     as a numerator and denominator pair, never as a float.
--
--   * Personal data lives only in columns suffixed _enc, typed BYTEA, holding
--     application-layer envelope-encrypted ciphertext. Encryption happens in the
--     orchestrator against a KMS key so that Postgres never sees plaintext and
--     never sees a key. pgcrypto is deliberately not used for this.
--
--   * Hashes and on-chain references are BYTEA, not TEXT, so that a mis-cased
--     hex string cannot be stored as a distinct value from its lowercase form.
--
--   * Every table that can be driven by an external retry carries a UNIQUE
--     idempotency_key. That is the database half of the two-layer replay
--     defence; the contract's usedKeys mapping is the other half.


CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid only

-- ---------------------------------------------------------------------------
-- Scheme and asset lifecycle
-- ---------------------------------------------------------------------------

CREATE TYPE scheme_status AS ENUM (
    'DRAFT',
    'SPV_FORMED',
    'VALUED',
    'FILED',
    'OFFER_APPROVED',
    'OFFER_OPEN',
    'OFFER_CLOSED',
    'ALLOCATED',
    'SETTLED',
    'LISTED',
    'OPERATIONAL',
    -- Abort branch. An SM-REIT scheme that fails its minimum subscription or
    -- its 200-unitholder floor is a normal outcome, not an error state.
    'UNDERSUBSCRIBED',
    'REFUNDING',
    'ABORTED',
    -- Terminal
    'WIND_DOWN',
    'DISSOLVED'
);

CREATE TYPE doc_type AS ENUM (
    'OFFER_DOCUMENT',
    'KIS',
    'TRUST_DEED',
    'IM_AGREEMENT',
    'VALUATION',
    'TRUSTEE_ESCROW',
    'SPV_CONTROL',
    'ALLOTMENT_FILE',
    'NDCF_STATEMENT',
    'SNAPSHOT',
    'BIDBOOK'
);

CREATE TYPE attestation_type AS ENUM ('TITLE_ESCROW', 'SPV_CONTROL');

CREATE TYPE property_status AS ENUM ('ACQUIRING', 'HELD', 'DISPOSED');

CREATE TYPE lease_status AS ENUM ('DRAFT', 'ACTIVE', 'EXPIRED', 'TERMINATED');

-- ---------------------------------------------------------------------------
-- Identity
-- ---------------------------------------------------------------------------

-- Drives TDS treatment. It does not drive transfer rights, which is why the
-- reference DELTA framework's ISO country-code gate was dropped: in an SM-REIT
-- every holder is 356 and the useful discriminator is tax class.
CREATE TYPE investor_class AS ENUM ('RESIDENT_IND', 'NRI', 'BODY_CORPORATE', 'HUF');

CREATE TYPE kyc_status AS ENUM ('PENDING', 'VERIFIED', 'REJECTED', 'EXPIRED');

CREATE TYPE depository AS ENUM ('NSDL', 'CDSL');

CREATE TYPE wallet_provider AS ENUM ('WEB3AUTH');

-- ---------------------------------------------------------------------------
-- Primary market
-- ---------------------------------------------------------------------------

CREATE TYPE offer_type AS ENUM ('INITIAL', 'FOLLOW_ON');

CREATE TYPE offer_status AS ENUM (
    'CONFIGURED',
    'OPEN',
    'CLOSED',
    'BOOK_FROZEN',
    'FEASIBILITY_CHECKED',
    'BIDBOOK_ANCHORED',
    'SEED_COMMITTED',
    'SEED_REVEALED',
    'BALLOT_DRAWN',
    'ALLOTMENT_FINALISED',
    'SETTLED',
    'ABORTED'
);

CREATE TYPE bid_status AS ENUM (
    'SUBMITTED',
    'BLOCK_REQUESTED',
    'FUNDS_BLOCKED',
    'VALIDATED',
    'IN_BOOK',
    'ALLOTTED_FULL',
    'ALLOTTED_PARTIAL',
    'REJECTED_BALLOT',
    'REJECTED_TECHNICAL',
    'DEBIT_INSTRUCTED',
    'FUNDS_DEBITED',
    'UNITS_CREDITED',
    'ANCHORED',
    'UNBLOCK_REQUESTED',
    'FUNDS_UNBLOCKED'
);

CREATE TYPE bid_rejection_reason AS ENUM (
    'BALLOT_NOT_DRAWN',
    'KYC_INVALID',
    'KYC_EXPIRED',
    'DEMAT_UNVERIFIED',
    'ASBA_BLOCK_FAILED',
    'BELOW_MIN_BID',
    'ABOVE_MAX_BID',
    'DUPLICATE_BID',
    'OFFER_ABORTED'
);

CREATE TYPE asba_block_status AS ENUM (
    'REQUESTED', 'BLOCKED', 'DEBITED', 'UNBLOCKED', 'FAILED'
);

CREATE TYPE asba_provider AS ENUM ('RAZORPAYX_SANDBOX');

CREATE TYPE ballot_status AS ENUM (
    'PENDING',
    'BIDBOOK_ANCHORED',
    'SEED_COMMITTED',
    'SEED_REVEALED',
    'DRAWN',
    'RESULT_ANCHORED',
    'ESCALATED',
    'ABANDONED'
);

CREATE TYPE allocation_outcome AS ENUM ('FULL', 'PARTIAL', 'NIL_BALLOT', 'NIL_TECHNICAL');

-- ---------------------------------------------------------------------------
-- Register
-- ---------------------------------------------------------------------------

CREATE TYPE holding_entry_type AS ENUM (
    'ALLOTMENT',
    'IM_SUBSCRIPTION',
    'TRANSFER_IN',
    'TRANSFER_OUT',
    'TRANSMISSION_IN',
    'TRANSMISSION_OUT',
    'CORRECTION',
    'REVERSAL'
);

CREATE TYPE holding_source AS ENUM ('BALLOT', 'DEPOSITORY_SYNC', 'ADMIN_CORRECTION');

-- ---------------------------------------------------------------------------
-- Distribution
-- ---------------------------------------------------------------------------

CREATE TYPE period_status AS ENUM (
    'OPEN',
    'RENT_COLLECTED',
    'NDCF_DRAFTED',
    'NDCF_APPROVED',
    'RECORD_DATE_DECLARED',
    'SNAPSHOT_TAKEN',
    'RECONCILED',
    'CHAIN_STALE',
    'ANCHORED',
    'ENTITLEMENTS_ANCHORED',
    'PAYOUT_INSTRUCTED',
    'PAYOUTS_CONFIRMED',
    'CLOSED',
    'REVERSED'
);

-- DEBT_SERVICE is reserved. A v1 unleveraged scheme has no debt, so the
-- orchestrator never emits it, but keeping the slot avoids a type migration when
-- a leveraged scheme is modelled.
CREATE TYPE ndcf_line_type AS ENUM (
    'GROSS_RENT',
    'CAM_RECOVERY',
    'INTEREST_INCOME',
    'PROPERTY_TAX',
    'INSURANCE',
    'MAINTENANCE',
    'TRUSTEE_FEE',
    'IM_FEE',
    'VALUER_FEE',
    'AUDIT_FEE',
    'STATUTORY_RESERVE',
    'WORKING_CAPITAL_RESERVE',
    'DEBT_SERVICE',
    'OTHER'
);

CREATE TYPE ndcf_direction AS ENUM ('INFLOW', 'OUTFLOW');

CREATE TYPE payout_status AS ENUM (
    'QUEUED', 'SUBMITTED', 'PROCESSING', 'SETTLED', 'FAILED', 'REVERSED'
);

CREATE TYPE payout_provider AS ENUM ('RAZORPAYX_SANDBOX');

CREATE TYPE adjustment_direction AS ENUM ('RECOVER_FROM_HOLDER', 'PAY_TO_HOLDER');

CREATE TYPE reversal_reason AS ENUM (
    'DATA_ENTRY_ERROR',
    'DUPLICATE_INJECTION',
    'VALUATION_RESTATEMENT',
    'BANK_FAILURE',
    'REGULATORY_DIRECTION',
    'OTHER'
);

-- ---------------------------------------------------------------------------
-- Infrastructure
-- ---------------------------------------------------------------------------

CREATE TYPE anchor_status AS ENUM ('NOT_ANCHORED', 'QUEUED', 'CONFIRMED', 'FAILED');

CREATE TYPE outbox_status AS ENUM (
    'QUEUED',
    'SIGNED',
    'BROADCAST',
    'PENDING',
    'CONFIRMING',
    'CONFIRMED',
    'REORGED',
    'FAILED',
    'DEAD_LETTER',
    'CANCELLED'
);

CREATE TYPE reconciliation_status AS ENUM ('MATCHED', 'DIVERGED', 'RESOLVED');

CREATE TYPE reconciliation_resolution AS ENUM (
    'PENDING', 'CHAIN_UPDATED', 'DEPOSITORY_QUERIED', 'MANUAL'
);

CREATE TYPE environment_tag AS ENUM ('LOCAL', 'SEPOLIA_SIM');

-- admin_action is the closed vocabulary of mutating operations.
--
-- INVARIANT: this list must remain byte-identical to idempotency.AllActions in
-- orchestrator/internal/idempotency/action.go. An action that exists in Go but
-- not here derives a key successfully and then fails on INSERT at runtime;
-- TestActionsMatchDDL parses this file and fails the build instead.
CREATE TYPE admin_action AS ENUM (
    'ADVANCE_CLOCK',
    'FREEZE_CLOCK',
    'UNFREEZE_CLOCK',

    'CREATE_SCHEME',
    'CREATE_SPV',
    'CREATE_PROPERTY',
    'CREATE_LEASE',
    'UPLOAD_DOCUMENT',
    'ANCHOR_DOCUMENT',
    'SUPERSEDE_DOCUMENT',
    'DEPLOY_CONTRACT',
    'ADVANCE_SCHEME_STATUS',
    'IM_SUBSCRIPTION',
    'SET_LOCK_IN',

    'CREATE_OFFER',
    'OPEN_OFFER',
    'SEED_BIDS',
    'SUBMIT_BID',
    'CLOSE_OFFER',
    'FREEZE_BOOK',
    'FEASIBILITY_CHECK',
    'ANCHOR_BIDBOOK',
    'COMMIT_SEED',
    'REVEAL_SEED',
    'RECOMMIT_SEED',
    'ESCALATE_BALLOT',
    'RUN_BALLOT',
    'FINALISE_ALLOTMENT',
    'ABORT_OFFER',
    'SIMULATE_ASBA_RESULT',

    'BEGIN_SETTLEMENT',
    'SETTLE_BATCH',
    'FINALISE_SETTLEMENT',

    'CREATE_PERIOD',
    'INJECT_RENT',
    'ADD_NDCF_LINE_ITEM',
    'DRAFT_NDCF',
    'APPROVE_NDCF_IM',
    'APPROVE_NDCF_TRUSTEE',
    'DECLARE_RECORD_DATE',
    'TAKE_SNAPSHOT',
    'ANCHOR_PERIOD',
    'ANCHOR_ENTITLEMENTS_BATCH',
    'FINALISE_ENTITLEMENTS',
    'INSTRUCT_PAYOUT',
    'SIMULATE_PAYOUT_RESULT',
    'CONFIRM_PAYOUTS',
    'CLOSE_PERIOD',
    'APPROVE_REVERSAL',
    'REVERSE_PERIOD',
    'RECORD_PAYOUT_ADJUSTMENT',

    'DEPOSITORY_TRANSFER',
    'DEPOSITORY_TRANSMISSION',
    'RUN_RECONCILIATION',
    'PUSH_RECONCILIATION',
    'CORRECT_HOLDING',

    'RETRY_OUTBOX',
    'CANCEL_OUTBOX',
    'DEAD_LETTER_OUTBOX',

    'PROPOSE_ROLE_CHANGE',
    'EXECUTE_ROLE_CHANGE',
    'CANCEL_ROLE_CHANGE',
    'REVOKE_RELAYER',
    'PAUSE',
    'UNPAUSE'
);

