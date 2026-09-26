-- AcreSync M0 :: 0002 :: Domain 1, scheme and asset layer


-- ---------------------------------------------------------------------------
-- schemes
-- ---------------------------------------------------------------------------

CREATE TABLE schemes (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sebi_scheme_ref     TEXT        NOT NULL UNIQUE,
    name                TEXT        NOT NULL,
    is_leveraged        BOOLEAN     NOT NULL DEFAULT FALSE,

    asset_value_paise   BIGINT      NOT NULL,
    unit_price_paise    BIGINT      NOT NULL,
    total_units         INTEGER     NOT NULL,
    im_units            INTEGER     NOT NULL,
    public_units        INTEGER     NOT NULL,
    min_public_holders  INTEGER     NOT NULL DEFAULT 200,

    status              scheme_status NOT NULL DEFAULT 'DRAFT',

    chain_id            INTEGER,
    roles_address       TEXT,
    ballot_address      TEXT,
    scheme_address      TEXT,
    environment_tag     environment_tag NOT NULL DEFAULT 'LOCAL',

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Total value of a scheme's assets must sit inside the SM-REIT band of
    -- fifty crore to five hundred crore rupees.
    --   50 crore  = 50  * 10,000,000 rupees * 100 =      50,000,000,000 paise
    --   500 crore = 500 * 10,000,000 rupees * 100 =     500,000,000,000 paise
    CONSTRAINT schemes_asset_value_band CHECK (
        asset_value_paise BETWEEN 50000000000 AND 500000000000
    ),

    -- The minimum price of each unit of an SM-REIT scheme is ten lakh rupees,
    -- i.e. 1,000,000 rupees * 100 = 100,000,000 paise.
    CONSTRAINT schemes_min_unit_price CHECK (unit_price_paise >= 100000000),

    -- Units are indivisible. There is no fractional unit state that a Demat
    -- account could hold, which is also why the on-chain token has 0 decimals.
    CONSTRAINT schemes_units_positive CHECK (
        total_units > 0 AND im_units >= 0 AND public_units > 0
    ),

    -- The whole cap table must be accounted for. If this can drift, the
    -- settlement invariant (exactly total_units issued) has nothing to check
    -- itself against.
    CONSTRAINT schemes_units_balance CHECK (total_units = im_units + public_units),

    CONSTRAINT schemes_holder_floor CHECK (min_public_holders >= 200),

    -- A scheme priced at unit_price_paise for total_units must be consistent
    -- with its declared asset value.
    CONSTRAINT schemes_valuation_consistent CHECK (
        total_units::BIGINT * unit_price_paise = asset_value_paise
    ),

    -- Lowercase 0x hex only. Mixed-case EIP-55 addresses are rejected so that
    -- one address has exactly one spelling everywhere in the system.
    CONSTRAINT schemes_roles_address_fmt  CHECK (roles_address  IS NULL OR roles_address  ~ '^0x[0-9a-f]{40}$'),
    CONSTRAINT schemes_ballot_address_fmt CHECK (ballot_address IS NULL OR ballot_address ~ '^0x[0-9a-f]{40}$'),
    CONSTRAINT schemes_scheme_address_fmt CHECK (scheme_address IS NULL OR scheme_address ~ '^0x[0-9a-f]{40}$')
);

COMMENT ON COLUMN schemes.im_units IS
    'Investment manager holding. 5% of outstanding units for an unleveraged scheme, 15% if leveraged. Excluded from the 200-unitholder count.';

-- ---------------------------------------------------------------------------
-- spvs
-- ---------------------------------------------------------------------------

CREATE TABLE spvs (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id          UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    cin                TEXT        NOT NULL UNIQUE,
    name               TEXT        NOT NULL,
    ownership_pct      NUMERIC(5,2) NOT NULL DEFAULT 100.00,
    incorporation_date DATE        NOT NULL,
    is_active          BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- An SM-REIT scheme must hold its real estate through a wholly owned SPV.
    -- Anything other than 100% is not a permissible structure, so it is a
    -- constraint rather than a validation rule that application code might skip.
    CONSTRAINT spvs_wholly_owned CHECK (ownership_pct = 100.00)
);

CREATE INDEX spvs_scheme_idx ON spvs(scheme_id);

-- ---------------------------------------------------------------------------
-- properties
-- ---------------------------------------------------------------------------

-- Note what is absent: there is no scheme_id column.
--
-- The legal ownership chain is scheme -> SPV -> property, and a property that
-- appeared to hang directly off a scheme would misrepresent that chain. Omitting
-- the column makes the shortcut unrepresentable rather than merely discouraged;
-- reaching a scheme from a property requires the join through spvs, which is the
-- same path the legal structure takes.
CREATE TABLE properties (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    spv_id               UUID        NOT NULL REFERENCES spvs(id) ON DELETE RESTRICT,
    name                 TEXT        NOT NULL,
    address_text         TEXT        NOT NULL,
    city                 TEXT        NOT NULL,
    grade                TEXT        NOT NULL,
    carpet_area_sqft     INTEGER     NOT NULL,
    leasable_area_sqft   INTEGER     NOT NULL,
    occupancy_bps        INTEGER     NOT NULL DEFAULT 0,
    wale_months          INTEGER,
    acquisition_value_paise BIGINT   NOT NULL,
    status               property_status NOT NULL DEFAULT 'ACQUIRING',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT properties_areas_positive CHECK (carpet_area_sqft > 0 AND leasable_area_sqft > 0),
    CONSTRAINT properties_carpet_lte_leasable CHECK (carpet_area_sqft <= leasable_area_sqft),
    -- Occupancy as integer basis points, not a float percentage.
    CONSTRAINT properties_occupancy_range CHECK (occupancy_bps BETWEEN 0 AND 10000),
    CONSTRAINT properties_value_positive CHECK (acquisition_value_paise > 0)
);

CREATE INDEX properties_spv_idx ON properties(spv_id);

-- ---------------------------------------------------------------------------
-- leases
-- ---------------------------------------------------------------------------

CREATE TABLE leases (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id            UUID        NOT NULL REFERENCES properties(id) ON DELETE RESTRICT,
    tenant_name            TEXT        NOT NULL,
    monthly_rent_paise     BIGINT      NOT NULL,
    escalation_bps         INTEGER     NOT NULL DEFAULT 0,
    start_date             DATE        NOT NULL,
    end_date               DATE        NOT NULL,
    lock_in_end_date       DATE,
    security_deposit_paise BIGINT      NOT NULL DEFAULT 0,
    status                 lease_status NOT NULL DEFAULT 'DRAFT',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT leases_rent_positive CHECK (monthly_rent_paise > 0),
    CONSTRAINT leases_deposit_nonneg CHECK (security_deposit_paise >= 0),
    CONSTRAINT leases_dates_ordered CHECK (end_date > start_date),
    CONSTRAINT leases_lockin_within_term CHECK (
        lock_in_end_date IS NULL OR (lock_in_end_date >= start_date AND lock_in_end_date <= end_date)
    ),
    CONSTRAINT leases_escalation_sane CHECK (escalation_bps BETWEEN 0 AND 10000)
);

CREATE INDEX leases_property_idx ON leases(property_id);

-- ---------------------------------------------------------------------------
-- Document anchors
-- ---------------------------------------------------------------------------

CREATE TABLE scheme_documents (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id        UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    doc_type         doc_type    NOT NULL,
    version          INTEGER     NOT NULL DEFAULT 1,
    document_uri     TEXT        NOT NULL,
    document_sha256  BYTEA       NOT NULL,
    ipfs_cid         TEXT,
    filed_date       DATE,
    anchor_status    anchor_status NOT NULL DEFAULT 'NOT_ANCHORED',
    anchored_tx      TEXT,
    superseded_by    UUID REFERENCES scheme_documents(id),
    supersede_reason reversal_reason,
    idempotency_key  BYTEA       NOT NULL UNIQUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT scheme_documents_sha256_len CHECK (octet_length(document_sha256) = 32),
    CONSTRAINT scheme_documents_idem_len   CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT scheme_documents_version_pos CHECK (version >= 1),
    UNIQUE (scheme_id, doc_type, version)
);

CREATE TABLE valuation_reports (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id        UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    valuer_name      TEXT        NOT NULL,
    valuer_reg_no    TEXT        NOT NULL,
    valuation_date   DATE        NOT NULL,
    valued_amount_paise BIGINT   NOT NULL,
    document_uri     TEXT        NOT NULL,
    document_sha256  BYTEA       NOT NULL,
    anchor_status    anchor_status NOT NULL DEFAULT 'NOT_ANCHORED',
    anchored_tx      TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT valuation_reports_amount_positive CHECK (valued_amount_paise > 0),
    CONSTRAINT valuation_reports_sha256_len CHECK (octet_length(document_sha256) = 32)
);

CREATE TABLE trustee_attestations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id        UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    trustee_name     TEXT        NOT NULL,
    trustee_sebi_ref TEXT        NOT NULL,
    attestation_type attestation_type NOT NULL,
    effective_date   DATE        NOT NULL,
    document_uri     TEXT        NOT NULL,
    document_sha256  BYTEA       NOT NULL,
    anchor_status    anchor_status NOT NULL DEFAULT 'NOT_ANCHORED',
    anchored_tx      TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT trustee_attestations_sha256_len CHECK (octet_length(document_sha256) = 32)
);

CREATE INDEX scheme_documents_scheme_idx     ON scheme_documents(scheme_id);
CREATE INDEX valuation_reports_scheme_idx    ON valuation_reports(scheme_id);
CREATE INDEX trustee_attestations_scheme_idx ON trustee_attestations(scheme_id);

