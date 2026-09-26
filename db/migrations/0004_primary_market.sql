-- AcreSync M0 :: 0004 :: Domain 3, primary market


-- ---------------------------------------------------------------------------
-- offers
-- ---------------------------------------------------------------------------

CREATE TABLE offers (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id             UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    offer_type            offer_type  NOT NULL DEFAULT 'INITIAL',

    price_band_lower_paise BIGINT     NOT NULL,
    price_band_upper_paise BIGINT     NOT NULL,

    units_on_offer        INTEGER     NOT NULL,
    min_bid_units         INTEGER     NOT NULL DEFAULT 1,
    max_bid_units         INTEGER     NOT NULL,
    min_subscription_units INTEGER    NOT NULL,
    min_distinct_holders  INTEGER     NOT NULL DEFAULT 200,

    opens_at              TIMESTAMPTZ NOT NULL,
    closes_at             TIMESTAMPTZ NOT NULL,
    allotment_due_at      TIMESTAMPTZ NOT NULL,

    status                offer_status NOT NULL DEFAULT 'CONFIGURED',
    idempotency_key       BYTEA       NOT NULL UNIQUE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT offers_idem_len CHECK (octet_length(idempotency_key) = 32),

    -- The lower bound of the price band cannot go below the statutory minimum
    -- unit price of ten lakh rupees.
    CONSTRAINT offers_band_above_floor CHECK (price_band_lower_paise >= 100000000),
    CONSTRAINT offers_band_ordered CHECK (price_band_upper_paise >= price_band_lower_paise),

    CONSTRAINT offers_units_positive CHECK (units_on_offer > 0),
    CONSTRAINT offers_min_bid_positive CHECK (min_bid_units >= 1),
    CONSTRAINT offers_bid_bounds_ordered CHECK (max_bid_units >= min_bid_units),
    CONSTRAINT offers_max_bid_within_offer CHECK (max_bid_units <= units_on_offer),

    -- Feasibility arithmetic: reaching min_distinct_holders requires at least
    -- one unit each, so a single bidder can never be allotted more than
    -- units_on_offer - (min_distinct_holders - 1). A max_bid_units above that
    -- makes the holder floor unreachable by construction.
    CONSTRAINT offers_cap_preserves_holder_floor CHECK (
        max_bid_units <= units_on_offer - (min_distinct_holders - 1)
    ),

    CONSTRAINT offers_min_subscription_sane CHECK (
        min_subscription_units > 0 AND min_subscription_units <= units_on_offer
    ),

    -- The holder floor cannot exceed the number of units available, since every
    -- holder needs at least one whole unit.
    CONSTRAINT offers_holder_floor_feasible CHECK (min_distinct_holders <= units_on_offer),
    CONSTRAINT offers_holder_floor_statutory CHECK (min_distinct_holders >= 200),

    CONSTRAINT offers_window_ordered CHECK (closes_at > opens_at),
    CONSTRAINT offers_allotment_after_close CHECK (allotment_due_at >= closes_at)
);

CREATE INDEX offers_scheme_idx ON offers(scheme_id);

-- Exactly one initial offer per scheme.
CREATE UNIQUE INDEX offers_one_initial_per_scheme
    ON offers(scheme_id) WHERE offer_type = 'INITIAL';

-- ---------------------------------------------------------------------------
-- bids
-- ---------------------------------------------------------------------------

CREATE TABLE bids (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    offer_id              UUID        NOT NULL REFERENCES offers(id) ON DELETE RESTRICT,
    investor_id           UUID        NOT NULL REFERENCES investors(id) ON DELETE RESTRICT,

    -- Denormalised from investor_anchors so the Merkle leaf is stable even if
    -- the anchor row is later shredded.
    investor_anchor_hash  BYTEA       NOT NULL,

    demat_account_id      UUID        NOT NULL REFERENCES demat_accounts(id) ON DELETE RESTRICT,
    bank_account_id       UUID        NOT NULL REFERENCES bank_accounts(id) ON DELETE RESTRICT,

    units_bid             INTEGER     NOT NULL,
    price_per_unit_paise  BIGINT      NOT NULL,
    total_amount_paise    BIGINT      NOT NULL,

    -- 16 random bytes rendered as lowercase hex. Deliberately opaque: a
    -- human-readable bid reference would be a personal-data vector in the bid
    -- book document, which is pinned publicly to IPFS.
    bid_reference         TEXT        NOT NULL UNIQUE,

    status                bid_status  NOT NULL DEFAULT 'SUBMITTED',
    rejection_reason      bid_rejection_reason,
    is_synthetic          BOOLEAN     NOT NULL DEFAULT FALSE,
    idempotency_key       BYTEA       NOT NULL UNIQUE,
    submitted_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT bids_anchor_len CHECK (octet_length(investor_anchor_hash) = 32),
    CONSTRAINT bids_idem_len   CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT bids_reference_fmt CHECK (bid_reference ~ '^[0-9a-f]{32}$'),

    -- Whole units only, at least one. The offer-specific min and max are checked
    -- by trigger in 0008 because they are a cross-row constraint.
    CONSTRAINT bids_units_positive CHECK (units_bid >= 1),
    CONSTRAINT bids_price_above_floor CHECK (price_per_unit_paise >= 100000000),

    -- Derived, but stored and constrained so that an inconsistent total cannot
    -- be written by a buggy caller.
    CONSTRAINT bids_total_consistent CHECK (
        total_amount_paise = units_bid::BIGINT * price_per_unit_paise
    ),

    CONSTRAINT bids_rejection_reason_present CHECK (
        (status NOT IN ('REJECTED_BALLOT', 'REJECTED_TECHNICAL') AND rejection_reason IS NULL)
        OR (status IN ('REJECTED_BALLOT', 'REJECTED_TECHNICAL') AND rejection_reason IS NOT NULL)
    ),

    -- One bid per investor per offer. A second bid would let a single investor
    -- circumvent max_bid_units and would double-count them toward the holder
    -- floor.
    UNIQUE (offer_id, investor_id)
);

CREATE INDEX bids_offer_status_idx ON bids(offer_id, status);
CREATE INDEX bids_investor_idx     ON bids(investor_id);

-- ---------------------------------------------------------------------------
-- asba_blocks
-- ---------------------------------------------------------------------------

CREATE TABLE asba_blocks (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bid_id                 UUID        NOT NULL UNIQUE REFERENCES bids(id) ON DELETE RESTRICT,
    provider               asba_provider NOT NULL DEFAULT 'RAZORPAYX_SANDBOX',
    bank_ref               TEXT,
    requested_amount_paise BIGINT      NOT NULL,
    blocked_amount_paise   BIGINT,
    debited_amount_paise   BIGINT,
    block_status           asba_block_status NOT NULL DEFAULT 'REQUESTED',
    requested_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    blocked_at             TIMESTAMPTZ,
    debited_at             TIMESTAMPTZ,
    unblocked_at           TIMESTAMPTZ,
    failure_code           TEXT,
    provider_payload_hash  BYTEA,
    idempotency_key        BYTEA       NOT NULL UNIQUE,

    CONSTRAINT asba_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT asba_requested_positive CHECK (requested_amount_paise > 0),
    CONSTRAINT asba_blocked_matches_request CHECK (
        blocked_amount_paise IS NULL OR blocked_amount_paise = requested_amount_paise
    ),
    -- You can never debit more than was blocked. A partial allotment debits less.
    CONSTRAINT asba_debit_within_block CHECK (
        debited_amount_paise IS NULL
        OR (blocked_amount_paise IS NOT NULL AND debited_amount_paise <= blocked_amount_paise)
    ),
    CONSTRAINT asba_blocked_has_timestamp CHECK (
        block_status <> 'BLOCKED' OR (blocked_at IS NOT NULL AND blocked_amount_paise IS NOT NULL)
    )
);

-- ---------------------------------------------------------------------------
-- ballot_runs
-- ---------------------------------------------------------------------------

-- The commit-reveal ballot. Column order below mirrors the order in which the
-- fields become known, and the ordering itself is the security property: the bid
-- book root is anchored before the seed is committed, and the seed plaintext is
-- not written until the commitment is confirmed on-chain. Trigger
-- ballot_runs_enforce_reveal_order in 0008 enforces that.
CREATE TABLE ballot_runs (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    offer_id              UUID        NOT NULL REFERENCES offers(id) ON DELETE RESTRICT,

    bidbook_snapshot_at   TIMESTAMPTZ NOT NULL,
    bidbook_merkle_root   BYTEA       NOT NULL,
    bidbook_cid_digest    BYTEA       NOT NULL,
    bid_leaf_count        INTEGER     NOT NULL,
    total_units_bid       BIGINT      NOT NULL,
    distinct_bidders      INTEGER     NOT NULL,

    -- Exact rational, never a float: units bid over units on offer.
    oversubscription_num  BIGINT      NOT NULL,
    oversubscription_den  BIGINT      NOT NULL,

    seed_commitment       BYTEA,
    target_block          BIGINT,
    attempt               SMALLINT    NOT NULL DEFAULT 0,
    seed_plaintext        BYTEA,
    target_block_hash     BYTEA,
    final_seed            BYTEA,

    commitment_anchored_tx TEXT,
    reveal_anchored_tx     TEXT,
    result_merkle_root     BYTEA,
    result_cid_digest      BYTEA,

    algo_version          INTEGER     NOT NULL DEFAULT 1,
    status                ballot_status NOT NULL DEFAULT 'PENDING',
    executed_at           TIMESTAMPTZ,
    idempotency_key       BYTEA       NOT NULL UNIQUE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ballot_idem_len          CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT ballot_bidbook_root_len  CHECK (octet_length(bidbook_merkle_root) = 32),
    CONSTRAINT ballot_bidbook_cid_len   CHECK (octet_length(bidbook_cid_digest) = 32),
    CONSTRAINT ballot_commitment_len    CHECK (seed_commitment   IS NULL OR octet_length(seed_commitment) = 32),
    CONSTRAINT ballot_seed_len          CHECK (seed_plaintext    IS NULL OR octet_length(seed_plaintext) = 32),
    CONSTRAINT ballot_blockhash_len     CHECK (target_block_hash IS NULL OR octet_length(target_block_hash) = 32),
    CONSTRAINT ballot_final_seed_len    CHECK (final_seed        IS NULL OR octet_length(final_seed) = 32),
    CONSTRAINT ballot_result_root_len   CHECK (result_merkle_root IS NULL OR octet_length(result_merkle_root) = 32),

    CONSTRAINT ballot_counts_nonneg CHECK (
        bid_leaf_count >= 0 AND total_units_bid >= 0 AND distinct_bidders >= 0
    ),
    CONSTRAINT ballot_bidders_lte_leaves CHECK (distinct_bidders <= bid_leaf_count),
    CONSTRAINT ballot_oversub_den_positive CHECK (oversubscription_den > 0),

    -- Three attempts, then the path locks and the draw requires trustee
    -- escalation. Each expiry is publicly counted on-chain.
    CONSTRAINT ballot_attempt_bounded CHECK (attempt BETWEEN 0 AND 3),

    -- Exactly one ballot run per offer in v1.
    UNIQUE (offer_id)
);

-- ---------------------------------------------------------------------------
-- allocations
-- ---------------------------------------------------------------------------

CREATE TABLE allocations (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ballot_run_id        UUID        NOT NULL REFERENCES ballot_runs(id) ON DELETE RESTRICT,
    bid_id               UUID        NOT NULL REFERENCES bids(id) ON DELETE RESTRICT,
    units_allotted       INTEGER     NOT NULL,
    amount_payable_paise BIGINT      NOT NULL,
    refund_amount_paise  BIGINT      NOT NULL,
    outcome              allocation_outcome NOT NULL,
    ballot_rank          INTEGER     NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT allocations_units_nonneg CHECK (units_allotted >= 0),
    CONSTRAINT allocations_amounts_nonneg CHECK (
        amount_payable_paise >= 0 AND refund_amount_paise >= 0
    ),

    -- Outcome and units must agree. A FULL or PARTIAL allotment of zero units,
    -- or a NIL outcome with units, is a bug in the allocation engine and this is
    -- where it gets caught.
    CONSTRAINT allocations_outcome_consistent CHECK (
        (outcome IN ('FULL', 'PARTIAL') AND units_allotted >= 1)
        OR (outcome IN ('NIL_BALLOT', 'NIL_TECHNICAL') AND units_allotted = 0)
    ),

    UNIQUE (ballot_run_id, bid_id)
);

CREATE INDEX allocations_ballot_idx ON allocations(ballot_run_id);

