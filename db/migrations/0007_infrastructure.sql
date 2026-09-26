-- AcreSync M0 :: 0007 :: Domain 6, infrastructure


-- ---------------------------------------------------------------------------
-- simulated_clock
-- ---------------------------------------------------------------------------

-- Business time for a scheme. Nothing in the orchestrator reads the wall clock
-- for a business decision; every deadline is evaluated against this value.
--
-- The clock governs business deadlines only. It does not and cannot govern block
-- timestamps, confirmation counts, or the 256-block window in which a target
-- blockhash remains available. A thirty-day offer window compresses to a second;
-- a five-confirmation wait does not compress at all.
CREATE TABLE simulated_clock (
    scheme_id        UUID PRIMARY KEY REFERENCES schemes(id) ON DELETE RESTRICT,
    current_value    TIMESTAMPTZ NOT NULL,
    is_frozen        BOOLEAN     NOT NULL DEFAULT FALSE,
    last_advanced_by TEXT,
    last_advanced_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- chain_outbox
-- ---------------------------------------------------------------------------

CREATE TABLE chain_outbox (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id           UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    target_contract     TEXT        NOT NULL,
    function_name       TEXT        NOT NULL,
    payload_json        JSONB       NOT NULL,

    -- SHA-256 of the canonical encoding of payload_json. Lets a redelivered
    -- message be recognised as identical without comparing JSONB semantics.
    payload_hash        BYTEA       NOT NULL,

    idempotency_key     BYTEA       NOT NULL UNIQUE,

    nonce               BIGINT,
    tx_hash             TEXT,
    gas_price_wei       NUMERIC(39,0),
    status              outbox_status NOT NULL DEFAULT 'QUEUED',
    confirmations       INTEGER     NOT NULL DEFAULT 0,
    block_number        BIGINT,
    submitted_at        TIMESTAMPTZ,
    confirmed_at        TIMESTAMPTZ,
    attempt_count       INTEGER     NOT NULL DEFAULT 0,
    last_error          TEXT,
    dead_lettered_at    TIMESTAMPTZ,
    cancelled_at        TIMESTAMPTZ,
    related_entity_type TEXT,
    related_entity_id   UUID,
    environment_tag     environment_tag NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT outbox_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT outbox_payload_hash_len CHECK (octet_length(payload_hash) = 32),
    CONSTRAINT outbox_target_fmt CHECK (target_contract ~ '^0x[0-9a-f]{40}$'),
    CONSTRAINT outbox_tx_fmt CHECK (tx_hash IS NULL OR tx_hash ~ '^0x[0-9a-f]{64}$'),
    CONSTRAINT outbox_confirmations_nonneg CHECK (confirmations >= 0),
    CONSTRAINT outbox_attempts_nonneg CHECK (attempt_count >= 0),

    -- A confirmed transaction must carry the evidence of its confirmation. This
    -- is what makes the payout gate in 0008 checkable.
    CONSTRAINT outbox_confirmed_has_evidence CHECK (
        status <> 'CONFIRMED'
        OR (tx_hash IS NOT NULL AND block_number IS NOT NULL
            AND confirmed_at IS NOT NULL AND confirmations >= 5)
    ),
    CONSTRAINT outbox_broadcast_has_tx CHECK (
        status NOT IN ('BROADCAST', 'PENDING', 'CONFIRMING', 'CONFIRMED') OR tx_hash IS NOT NULL
    ),
    -- Cancellation is only available before broadcast. It is the one genuinely
    -- clean undo in the system.
    CONSTRAINT outbox_cancel_before_broadcast CHECK (
        status <> 'CANCELLED' OR (tx_hash IS NULL AND cancelled_at IS NOT NULL)
    )
);

CREATE INDEX outbox_status_idx  ON chain_outbox(status) WHERE status NOT IN ('CONFIRMED', 'CANCELLED');
CREATE INDEX outbox_scheme_idx  ON chain_outbox(scheme_id, created_at);
CREATE INDEX outbox_related_idx ON chain_outbox(related_entity_type, related_entity_id);

-- One in-flight nonce per scheme relayer. A duplicate nonce is a replaced
-- transaction, which is how a batch silently applies twice.
CREATE UNIQUE INDEX outbox_one_tx_per_nonce
    ON chain_outbox(scheme_id, nonce)
    WHERE nonce IS NOT NULL AND status NOT IN ('CANCELLED', 'FAILED', 'DEAD_LETTER', 'REORGED');

ALTER TABLE payout_instructions
    ADD CONSTRAINT payouts_anchor_fk
    FOREIGN KEY (gated_on_anchor_tx) REFERENCES chain_outbox(id) ON DELETE RESTRICT;

-- ---------------------------------------------------------------------------
-- reconciliation
-- ---------------------------------------------------------------------------

CREATE TABLE reconciliation_runs (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id              UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    run_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    simulated_clock_value  TIMESTAMPTZ NOT NULL,
    source                 TEXT        NOT NULL DEFAULT 'DEPOSITORY_SIM',
    depository_total_units   INTEGER   NOT NULL,
    chain_total_units        INTEGER   NOT NULL,
    depository_holder_count  INTEGER   NOT NULL,
    chain_holder_count       INTEGER   NOT NULL,
    divergence_count       INTEGER     NOT NULL,
    status                 reconciliation_status NOT NULL,
    blocks_payout          BOOLEAN     NOT NULL,
    resolved_at            TIMESTAMPTZ,
    notes                  TEXT,
    idempotency_key        BYTEA       NOT NULL UNIQUE,

    CONSTRAINT recon_idem_len CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT recon_counts_nonneg CHECK (
        depository_total_units >= 0 AND chain_total_units >= 0
        AND depository_holder_count >= 0 AND chain_holder_count >= 0
        AND divergence_count >= 0
    ),

    -- Divergence and payout blocking are the same fact expressed twice; they must
    -- not be able to disagree. A DIVERGED run that does not block payouts is
    -- precisely the failure this design exists to prevent.
    CONSTRAINT recon_divergence_blocks CHECK (
        (status = 'MATCHED'  AND divergence_count = 0 AND NOT blocks_payout)
        OR (status = 'DIVERGED' AND divergence_count > 0 AND blocks_payout)
        OR (status = 'RESOLVED' AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX recon_runs_scheme_idx ON reconciliation_runs(scheme_id, run_at DESC);

CREATE TABLE reconciliation_diffs (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reconciliation_run_id  UUID        NOT NULL REFERENCES reconciliation_runs(id) ON DELETE RESTRICT,
    investor_id            UUID REFERENCES investors(id),
    wallet_address         TEXT        NOT NULL,
    depository_units       INTEGER     NOT NULL,
    chain_units            INTEGER     NOT NULL,
    diff_units             INTEGER     NOT NULL,
    resolution             reconciliation_resolution NOT NULL DEFAULT 'PENDING',
    resolved_at            TIMESTAMPTZ,

    CONSTRAINT recon_diffs_wallet_fmt CHECK (wallet_address ~ '^0x[0-9a-f]{40}$'),
    CONSTRAINT recon_diffs_units_nonneg CHECK (depository_units >= 0 AND chain_units >= 0),
    CONSTRAINT recon_diffs_diff_consistent CHECK (diff_units = depository_units - chain_units),
    CONSTRAINT recon_diffs_is_a_diff CHECK (diff_units <> 0)
);

CREATE INDEX recon_diffs_run_idx ON reconciliation_diffs(reconciliation_run_id);

-- ---------------------------------------------------------------------------
-- depository_register :: simulated RTA
-- ---------------------------------------------------------------------------

-- The simulated legal register. Deliberately a separate table from
-- unit_holdings: an admin-simulated broker transfer writes here and not to the
-- mirror, which is what makes the next reconciliation run show real divergence
-- and drive the period into CHAIN_STALE.
CREATE TABLE depository_register (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id      UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    investor_id    UUID        NOT NULL REFERENCES investors(id) ON DELETE RESTRICT,
    wallet_address TEXT        NOT NULL,
    units          INTEGER     NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT dep_register_units_nonneg CHECK (units >= 0),
    CONSTRAINT dep_register_wallet_fmt CHECK (wallet_address ~ '^0x[0-9a-f]{40}$'),
    UNIQUE (scheme_id, investor_id)
);

-- ---------------------------------------------------------------------------
-- admin_actions
-- ---------------------------------------------------------------------------

CREATE TABLE admin_actions (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_user_id         TEXT        NOT NULL,
    action                admin_action NOT NULL,
    scheme_id             UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    target_entity_type    TEXT,
    target_entity_id      UUID,

    -- The business time at which the action was taken. Recorded alongside
    -- created_at (real time) so the audit trail is reconstructible after an
    -- arbitrary sequence of clock advances.
    simulated_clock_value TIMESTAMPTZ NOT NULL,

    params_json           JSONB       NOT NULL DEFAULT '{}'::JSONB,
    idempotency_key       BYTEA       NOT NULL UNIQUE,
    environment_tag       environment_tag NOT NULL,
    resulting_outbox_id   UUID REFERENCES chain_outbox(id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT admin_actions_idem_len CHECK (octet_length(idempotency_key) = 32)
);

CREATE INDEX admin_actions_scheme_idx ON admin_actions(scheme_id, created_at DESC);
CREATE INDEX admin_actions_action_idx ON admin_actions(action);

