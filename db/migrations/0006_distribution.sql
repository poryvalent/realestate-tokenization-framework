-- AcreSync M0 :: 0006 :: Domain 5, distribution


-- ---------------------------------------------------------------------------
-- distribution_periods
-- ---------------------------------------------------------------------------

CREATE TABLE distribution_periods (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id              UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,

    -- Monotonic per scheme, and the value passed to the contract as periodId.
    period_seq             INTEGER     NOT NULL,
    period_label           TEXT        NOT NULL,
    period_start           DATE        NOT NULL,
    period_end             DATE        NOT NULL,
    record_date            DATE,

    status                 period_status NOT NULL DEFAULT 'OPEN',

    ndcf_paise             BIGINT,
    distributed_paise      BIGINT,
    distribution_bps       INTEGER,

    ndcf_statement_uri     TEXT,
    ndcf_statement_sha256  BYTEA,
    ndcf_cid_digest        BYTEA,
    ndcf_ipfs_cid          TEXT,

    -- Four-eyes on the NDCF figures, mirrored by the contract's separate trustee
    -- approval for reversals. Two distinct actors, recorded.
    im_approved_by         TEXT,
    im_approved_at         TIMESTAMPTZ,
    trustee_approved_by    TEXT,
    trustee_approved_at    TIMESTAMPTZ,

    anchored_tx            TEXT,
    supersedes_period_id   UUID REFERENCES distribution_periods(id),
    reversal_reason        reversal_reason,
    reversal_narrative_sha256 BYTEA,
    reversed_at            TIMESTAMPTZ,

    closed_at              TIMESTAMPTZ,
    idempotency_key        BYTEA       NOT NULL UNIQUE,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT periods_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT periods_seq_positive CHECK (period_seq >= 1),
    CONSTRAINT periods_dates_ordered CHECK (period_end >= period_start),

    -- The record date must fall on or after the period it settles.
    CONSTRAINT periods_record_date_after_end CHECK (
        record_date IS NULL OR record_date >= period_end
    ),

    CONSTRAINT periods_ndcf_positive CHECK (ndcf_paise IS NULL OR ndcf_paise > 0),
    CONSTRAINT periods_distributed_nonneg CHECK (distributed_paise IS NULL OR distributed_paise >= 0),
    CONSTRAINT periods_distributed_within_ndcf CHECK (
        distributed_paise IS NULL OR ndcf_paise IS NULL OR distributed_paise <= ndcf_paise
    ),

    -- The statutory distribution floor, enforced in the database as well as in
    -- the contract.
    --
    -- Written as a cross-multiplication rather than as distributed * 10000 /
    -- ndcf >= 9500. Not because truncation would change the answer: for an
    -- integer threshold, floor(x) >= 9500 holds exactly when x >= 9500. The
    -- reason is overflow. BIGINT * 10000 overflows for NDCF values in the upper
    -- part of the range, and an overflow here does not error, it wraps, and a
    -- wrapped product passes a floor it should fail. The NUMERIC cast gives
    -- arbitrary precision and removes the failure mode rather than bounding it.
    CONSTRAINT periods_distribution_floor CHECK (
        distributed_paise IS NULL OR ndcf_paise IS NULL
        OR (distributed_paise::NUMERIC * 10000 >= ndcf_paise::NUMERIC * 9500)
    ),

    CONSTRAINT periods_bps_range CHECK (
        distribution_bps IS NULL OR distribution_bps BETWEEN 9500 AND 10000
    ),

    CONSTRAINT periods_statement_sha_len CHECK (
        ndcf_statement_sha256 IS NULL OR octet_length(ndcf_statement_sha256) = 32
    ),
    CONSTRAINT periods_ndcf_cid_len CHECK (
        ndcf_cid_digest IS NULL OR octet_length(ndcf_cid_digest) = 32
    ),

    -- A period cannot be anchored until both approvals are in place.
    CONSTRAINT periods_anchored_requires_dual_approval CHECK (
        status NOT IN ('ANCHORED', 'ENTITLEMENTS_ANCHORED', 'PAYOUT_INSTRUCTED', 'PAYOUTS_CONFIRMED', 'CLOSED')
        OR (im_approved_at IS NOT NULL AND trustee_approved_at IS NOT NULL)
    ),

    -- The two approvers must be different people. A single operator approving
    -- both halves is not four-eyes.
    CONSTRAINT periods_approvers_distinct CHECK (
        im_approved_by IS NULL OR trustee_approved_by IS NULL
        OR im_approved_by <> trustee_approved_by
    ),

    -- Anchoring requires the figures and the statement digest to be present.
    CONSTRAINT periods_anchored_requires_figures CHECK (
        status NOT IN ('ANCHORED', 'ENTITLEMENTS_ANCHORED', 'PAYOUT_INSTRUCTED', 'PAYOUTS_CONFIRMED', 'CLOSED')
        OR (ndcf_paise IS NOT NULL AND distributed_paise IS NOT NULL
            AND distribution_bps IS NOT NULL AND ndcf_statement_sha256 IS NOT NULL)
    ),

    -- A reversal must be justified, and a reversed period must record when.
    CONSTRAINT periods_reversal_justified CHECK (
        status <> 'REVERSED'
        OR (reversal_reason IS NOT NULL AND reversal_narrative_sha256 IS NOT NULL AND reversed_at IS NOT NULL)
    ),
    CONSTRAINT periods_reversal_narrative_len CHECK (
        reversal_narrative_sha256 IS NULL OR octet_length(reversal_narrative_sha256) = 32
    ),

    -- A period cannot supersede itself.
    CONSTRAINT periods_no_self_supersede CHECK (supersedes_period_id IS DISTINCT FROM id),

    UNIQUE (scheme_id, period_seq)
);

CREATE INDEX periods_scheme_status_idx ON distribution_periods(scheme_id, status);

-- Only one period at a time may be mid-flight for a scheme. Two concurrent
-- open periods would make "the" record date ambiguous.
CREATE UNIQUE INDEX periods_one_active_per_scheme
    ON distribution_periods(scheme_id)
    WHERE status NOT IN ('CLOSED', 'REVERSED');

ALTER TABLE register_snapshots
    ADD CONSTRAINT register_snapshots_period_fk
    FOREIGN KEY (distribution_period_id) REFERENCES distribution_periods(id) ON DELETE RESTRICT;

-- ---------------------------------------------------------------------------
-- ndcf_line_items
-- ---------------------------------------------------------------------------

-- NDCF is not rent. The 95% floor applies to net distributable cash flow after
-- property tax, insurance, maintenance, trustee and manager fees, and reserves.
-- A contract computing 95% of gross rent computes the wrong number, so the
-- deduction chain is stored line by line with an evidence digest per line and is
-- itself pinned and anchored.
CREATE TABLE ndcf_line_items (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    distribution_period_id UUID        NOT NULL REFERENCES distribution_periods(id) ON DELETE RESTRICT,
    line_type              ndcf_line_type NOT NULL,
    direction              ndcf_direction NOT NULL,
    amount_paise           BIGINT      NOT NULL,
    spv_id                 UUID REFERENCES spvs(id),
    property_id            UUID REFERENCES properties(id),
    description            TEXT,
    evidence_uri           TEXT,
    evidence_sha256        BYTEA       NOT NULL,
    idempotency_key        BYTEA       NOT NULL UNIQUE,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ndcf_items_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT ndcf_items_amount_positive CHECK (amount_paise > 0),
    CONSTRAINT ndcf_items_evidence_len CHECK (octet_length(evidence_sha256) = 32),

    -- Direction is determined by line type, not chosen freely. An INFLOW labelled
    -- PROPERTY_TAX would quietly inflate NDCF and therefore the distribution.
    CONSTRAINT ndcf_items_direction_matches_type CHECK (
        (line_type IN ('GROSS_RENT', 'CAM_RECOVERY', 'INTEREST_INCOME') AND direction = 'INFLOW')
        OR (line_type IN ('PROPERTY_TAX', 'INSURANCE', 'MAINTENANCE', 'TRUSTEE_FEE',
                          'IM_FEE', 'VALUER_FEE', 'AUDIT_FEE', 'STATUTORY_RESERVE',
                          'WORKING_CAPITAL_RESERVE', 'DEBT_SERVICE', 'OTHER')
            AND direction = 'OUTFLOW')
    )
);

CREATE INDEX ndcf_line_items_period_idx ON ndcf_line_items(distribution_period_id);

COMMENT ON COLUMN ndcf_line_items.description IS
    'Free text, retained in Postgres for audit. Never pinned to IPFS: only evidence_sha256 leaves this table.';

-- ---------------------------------------------------------------------------
-- rent_receipts
-- ---------------------------------------------------------------------------

CREATE TABLE rent_receipts (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    distribution_period_id UUID        NOT NULL REFERENCES distribution_periods(id) ON DELETE RESTRICT,
    lease_id               UUID        NOT NULL REFERENCES leases(id) ON DELETE RESTRICT,
    expected_amount_paise  BIGINT      NOT NULL,
    received_amount_paise  BIGINT      NOT NULL,
    received_at            TIMESTAMPTZ NOT NULL,
    escrow_bank_ref        TEXT        NOT NULL,
    variance_paise         BIGINT      NOT NULL,
    variance_reason        TEXT,
    provider_payload_hash  BYTEA,
    idempotency_key        BYTEA       NOT NULL UNIQUE,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT rent_receipts_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT rent_receipts_amounts_nonneg CHECK (
        expected_amount_paise >= 0 AND received_amount_paise >= 0
    ),
    CONSTRAINT rent_receipts_variance_consistent CHECK (
        variance_paise = received_amount_paise - expected_amount_paise
    ),
    -- A variance must be explained. Silent shortfalls are how a distribution
    -- quietly stops matching the lease schedule.
    CONSTRAINT rent_receipts_variance_explained CHECK (
        variance_paise = 0 OR variance_reason IS NOT NULL
    )
);

CREATE INDEX rent_receipts_period_idx ON rent_receipts(distribution_period_id);

-- ---------------------------------------------------------------------------
-- entitlements
-- ---------------------------------------------------------------------------

CREATE TABLE entitlements (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    distribution_period_id UUID        NOT NULL REFERENCES distribution_periods(id) ON DELETE RESTRICT,
    investor_id            UUID        NOT NULL REFERENCES investors(id) ON DELETE RESTRICT,
    snapshot_line_id       UUID        NOT NULL REFERENCES register_snapshot_lines(id) ON DELETE RESTRICT,

    units                  INTEGER     NOT NULL,
    snapshot_total_units   INTEGER     NOT NULL,

    -- floor(distributed_paise * units / snapshot_total_units)
    gross_entitlement_paise BIGINT     NOT NULL,

    -- Largest-remainder allocation of the truncation residue, at most
    -- snapshot_total_units - 1 paise in total across all holders. Recorded
    -- per holder so that the exact-sum invariant is provable from stored data
    -- rather than recomputed and hoped for.
    residue_paise_awarded  INTEGER     NOT NULL DEFAULT 0,

    -- The exact remainder from the floor division, kept as an integer so the
    -- largest-remainder ordering is reproducible. Never a float.
    remainder_numerator    NUMERIC(39,0) NOT NULL,

    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT entitlements_units_positive CHECK (units > 0),
    CONSTRAINT entitlements_total_positive CHECK (snapshot_total_units > 0),
    CONSTRAINT entitlements_units_within_total CHECK (units <= snapshot_total_units),
    CONSTRAINT entitlements_gross_nonneg CHECK (gross_entitlement_paise >= 0),
    CONSTRAINT entitlements_residue_bounded CHECK (residue_paise_awarded BETWEEN 0 AND 1),
    CONSTRAINT entitlements_remainder_nonneg CHECK (remainder_numerator >= 0),

    UNIQUE (distribution_period_id, investor_id),
    UNIQUE (snapshot_line_id)
);

CREATE INDEX entitlements_period_idx ON entitlements(distribution_period_id);

-- ---------------------------------------------------------------------------
-- tax_deductions
-- ---------------------------------------------------------------------------

-- Never anchored, never pinned, never derivable from chain data. TDS rates
-- differ by investor class, so publishing a net amount alongside a public unit
-- count would disclose an investor's tax classification.
CREATE TABLE tax_deductions (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entitlement_id            UUID        NOT NULL UNIQUE REFERENCES entitlements(id) ON DELETE RESTRICT,
    investor_class            investor_class NOT NULL,
    tds_section               TEXT        NOT NULL,
    tds_rate_bps              INTEGER     NOT NULL,
    tds_amount_paise          BIGINT      NOT NULL,
    net_payable_paise         BIGINT      NOT NULL,
    form_15g_h_on_file        BOOLEAN     NOT NULL DEFAULT FALSE,
    lower_deduction_cert_ref  TEXT,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT tax_rate_range CHECK (tds_rate_bps BETWEEN 0 AND 10000),
    CONSTRAINT tax_amounts_nonneg CHECK (tds_amount_paise >= 0 AND net_payable_paise >= 0)
);

-- ---------------------------------------------------------------------------
-- payout_instructions
-- ---------------------------------------------------------------------------

CREATE TABLE payout_instructions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entitlement_id    UUID        NOT NULL REFERENCES entitlements(id) ON DELETE RESTRICT,
    bank_account_id   UUID        NOT NULL REFERENCES bank_accounts(id) ON DELETE RESTRICT,
    amount_paise      BIGINT      NOT NULL,
    provider          payout_provider NOT NULL DEFAULT 'RAZORPAYX_SANDBOX',
    provider_ref      TEXT,
    status            payout_status NOT NULL DEFAULT 'QUEUED',
    utr               TEXT,
    submitted_at      TIMESTAMPTZ,
    settled_at        TIMESTAMPTZ,
    failure_code      TEXT,

    -- The confirmed anchor that authorises this payment. Not nullable: fiat
    -- cannot leave escrow without naming the on-chain attestation it rests on.
    -- Trigger payouts_require_confirmed_anchor in 0008 verifies the referenced
    -- outbox row actually reached CONFIRMED with sufficient confirmations.
    gated_on_anchor_tx UUID       NOT NULL,

    idempotency_key   BYTEA       NOT NULL UNIQUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT payouts_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT payouts_amount_positive CHECK (amount_paise > 0),
    CONSTRAINT payouts_settled_has_utr CHECK (
        status <> 'SETTLED' OR (utr IS NOT NULL AND settled_at IS NOT NULL)
    ),
    CONSTRAINT payouts_failed_has_code CHECK (status <> 'FAILED' OR failure_code IS NOT NULL),

    UNIQUE (entitlement_id)
);

CREATE INDEX payouts_status_idx ON payout_instructions(status);

-- ---------------------------------------------------------------------------
-- carry_forward_adjustments
-- ---------------------------------------------------------------------------

-- Class 3 remedy. Once fiat has settled there is no reversal, only recovery, and
-- netting against a later period is realistically the only recoverable path.
CREATE TABLE carry_forward_adjustments (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    investor_id       UUID        NOT NULL REFERENCES investors(id) ON DELETE RESTRICT,
    source_period_id  UUID        NOT NULL REFERENCES distribution_periods(id) ON DELETE RESTRICT,
    target_period_id  UUID REFERENCES distribution_periods(id) ON DELETE RESTRICT,
    amount_paise      BIGINT      NOT NULL,
    direction         adjustment_direction NOT NULL,
    reason_code       reversal_reason NOT NULL,
    narrative_sha256  BYTEA       NOT NULL,
    status            TEXT        NOT NULL DEFAULT 'PENDING',
    anchored_tx       TEXT,
    idempotency_key   BYTEA       NOT NULL UNIQUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT adjustments_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT adjustments_amount_positive CHECK (amount_paise > 0),
    CONSTRAINT adjustments_narrative_len CHECK (octet_length(narrative_sha256) = 32),
    CONSTRAINT adjustments_periods_distinct CHECK (
        target_period_id IS NULL OR target_period_id <> source_period_id
    ),
    CONSTRAINT adjustments_status_vocab CHECK (status IN ('PENDING', 'APPLIED', 'WRITTEN_OFF'))
);

CREATE INDEX adjustments_investor_idx ON carry_forward_adjustments(investor_id);
CREATE INDEX adjustments_target_idx   ON carry_forward_adjustments(target_period_id);

