-- AcreSync M0 :: 0005 :: Domain 4, unitholder register
--
-- Three ledgers exist in AcreSync and their authority is ranked:
--
--   1. The depository / RTA is the legal register of unit ownership.
--   2. unit_holdings below is the operational mirror.
--   3. The smart contract is the audit mirror.
--
-- When they disagree, the depository wins and distribution halts. Nothing in
-- this file is authoritative in a legal sense, which is exactly why holding
-- changes are recorded as an append-only ledger rather than as mutable state:
-- the mirror's history is the only thing that makes a later divergence
-- diagnosable.


-- ---------------------------------------------------------------------------
-- unit_holdings :: live mirror
-- ---------------------------------------------------------------------------

CREATE TABLE unit_holdings (
    id                           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id                    UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    investor_id                  UUID        NOT NULL REFERENCES investors(id) ON DELETE RESTRICT,
    wallet_address               TEXT        NOT NULL,
    units                        INTEGER     NOT NULL DEFAULT 0,
    is_excluded_from_holder_count BOOLEAN    NOT NULL DEFAULT FALSE,

    -- Null until the scheme reaches LISTED. The investment manager's lock-in runs
    -- for two years from listing, not from subscription, so the expiry cannot be
    -- known at the time the IM's units are credited before the public ballot.
    lock_in_expires_at           TIMESTAMPTZ,

    first_credited_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT unit_holdings_units_nonneg CHECK (units >= 0),
    CONSTRAINT unit_holdings_wallet_fmt CHECK (wallet_address ~ '^0x[0-9a-f]{40}$'),

    UNIQUE (scheme_id, investor_id)
);

CREATE INDEX unit_holdings_scheme_idx ON unit_holdings(scheme_id);
CREATE INDEX unit_holdings_wallet_idx ON unit_holdings(wallet_address);

-- Counting holders toward the statutory floor is a hot path at settlement.
CREATE INDEX unit_holdings_countable_idx
    ON unit_holdings(scheme_id)
    WHERE units > 0 AND NOT is_excluded_from_holder_count;

-- ---------------------------------------------------------------------------
-- holding_ledger :: append-only
-- ---------------------------------------------------------------------------

CREATE TABLE holding_ledger (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id               UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    investor_id             UUID        NOT NULL REFERENCES investors(id) ON DELETE RESTRICT,
    entry_type              holding_entry_type NOT NULL,

    -- Signed. A transfer out is negative. balance_after is the running total and
    -- is verified against the sum of deltas by trigger, which is invariant 4.
    units_delta             INTEGER     NOT NULL,
    balance_after           INTEGER     NOT NULL,

    depository_ref          TEXT,
    counterparty_investor_id UUID REFERENCES investors(id),
    source                  holding_source NOT NULL,

    -- A correction is a new entry that points at the one it corrects. Nothing is
    -- ever updated or deleted, so an operational error stays permanently visible
    -- alongside its remedy.
    reversal_of_entry_id    UUID REFERENCES holding_ledger(id),
    reason_code             reversal_reason,
    narrative_sha256        BYTEA,

    occurred_at             TIMESTAMPTZ NOT NULL,
    simulated_clock_value   TIMESTAMPTZ NOT NULL,
    anchor_status           anchor_status NOT NULL DEFAULT 'NOT_ANCHORED',
    anchored_tx             TEXT,
    idempotency_key         BYTEA       NOT NULL UNIQUE,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT holding_ledger_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT holding_ledger_delta_nonzero CHECK (units_delta <> 0),
    CONSTRAINT holding_ledger_balance_nonneg CHECK (balance_after >= 0),
    CONSTRAINT holding_ledger_narrative_len CHECK (
        narrative_sha256 IS NULL OR octet_length(narrative_sha256) = 32
    ),

    -- A correction or reversal must say why, and a narrative hash must accompany
    -- it. The text itself stays in Postgres under access control; only its digest
    -- is ever anchored.
    CONSTRAINT holding_ledger_correction_justified CHECK (
        entry_type NOT IN ('CORRECTION', 'REVERSAL')
        OR (reason_code IS NOT NULL AND narrative_sha256 IS NOT NULL)
    ),

    -- A reversal must name what it reverses.
    CONSTRAINT holding_ledger_reversal_targets CHECK (
        entry_type <> 'REVERSAL' OR reversal_of_entry_id IS NOT NULL
    ),

    -- Transfers and transmissions come from the depository and must carry its
    -- reference, otherwise the mirror cannot be reconciled back to the legal
    -- register.
    CONSTRAINT holding_ledger_depository_ref_present CHECK (
        entry_type NOT IN ('TRANSFER_IN', 'TRANSFER_OUT', 'TRANSMISSION_IN', 'TRANSMISSION_OUT')
        OR depository_ref IS NOT NULL
    ),

    -- Direction must agree with the entry type.
    CONSTRAINT holding_ledger_direction_consistent CHECK (
        (entry_type IN ('ALLOTMENT', 'IM_SUBSCRIPTION', 'TRANSFER_IN', 'TRANSMISSION_IN') AND units_delta > 0)
        OR (entry_type IN ('TRANSFER_OUT', 'TRANSMISSION_OUT') AND units_delta < 0)
        OR entry_type IN ('CORRECTION', 'REVERSAL')
    )
);

CREATE INDEX holding_ledger_scheme_investor_idx ON holding_ledger(scheme_id, investor_id, created_at);
CREATE INDEX holding_ledger_anchor_idx ON holding_ledger(anchor_status) WHERE anchor_status <> 'CONFIRMED';

-- A reversal may be reversed exactly zero times. Chaining forward with a fresh
-- compensating entry is the supported path; reversing a reversal produces an
-- audit trail nobody can read.
CREATE UNIQUE INDEX holding_ledger_one_reversal_per_entry
    ON holding_ledger(reversal_of_entry_id)
    WHERE reversal_of_entry_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- register_snapshots :: record-date freeze
-- ---------------------------------------------------------------------------

-- Entitlement is computed on holdings as of a declared record date, never on
-- live balances. The mirror lags the depository by up to T+3, so a transfer
-- reconciled late would otherwise retroactively change who was owed money for a
-- period that has already been anchored.
CREATE TABLE register_snapshots (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id             UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    distribution_period_id UUID       NOT NULL,
    record_date           DATE        NOT NULL,
    taken_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    simulated_clock_value TIMESTAMPTZ NOT NULL,
    total_units           INTEGER     NOT NULL,
    distinct_holders      INTEGER     NOT NULL,
    snapshot_merkle_root  BYTEA       NOT NULL,
    snapshot_cid_digest   BYTEA       NOT NULL,
    ipfs_cid              TEXT,
    algo_version          INTEGER     NOT NULL DEFAULT 1,
    anchor_status         anchor_status NOT NULL DEFAULT 'NOT_ANCHORED',
    anchored_tx           TEXT,
    idempotency_key       BYTEA       NOT NULL UNIQUE,

    CONSTRAINT register_snapshots_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT register_snapshots_root_len CHECK (octet_length(snapshot_merkle_root) = 32),
    CONSTRAINT register_snapshots_cid_len  CHECK (octet_length(snapshot_cid_digest) = 32),
    CONSTRAINT register_snapshots_counts_positive CHECK (total_units > 0 AND distinct_holders > 0),

    UNIQUE (distribution_period_id)
);

CREATE TABLE register_snapshot_lines (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    snapshot_id    UUID        NOT NULL REFERENCES register_snapshots(id) ON DELETE RESTRICT,
    leaf_index     INTEGER     NOT NULL,
    investor_id    UUID        NOT NULL REFERENCES investors(id) ON DELETE RESTRICT,
    investor_anchor_hash BYTEA NOT NULL,
    wallet_address TEXT        NOT NULL,
    units          INTEGER     NOT NULL,
    is_excluded    BOOLEAN     NOT NULL,

    CONSTRAINT snapshot_lines_units_positive CHECK (units > 0),
    CONSTRAINT snapshot_lines_leaf_nonneg CHECK (leaf_index >= 0),
    CONSTRAINT snapshot_lines_anchor_len CHECK (octet_length(investor_anchor_hash) = 32),
    CONSTRAINT snapshot_lines_wallet_fmt CHECK (wallet_address ~ '^0x[0-9a-f]{40}$'),

    -- Leaf ordering is part of the Merkle root. A duplicate or missing index
    -- would make the published proof unverifiable.
    UNIQUE (snapshot_id, leaf_index),
    UNIQUE (snapshot_id, investor_id)
);

CREATE INDEX register_snapshot_lines_snapshot_idx ON register_snapshot_lines(snapshot_id);

