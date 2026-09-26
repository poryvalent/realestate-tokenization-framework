-- AcreSync M0 :: 0010 :: external provider vocabularies and IPFS pin tracking
--
-- Two additions.
--
-- The go-web3-bridge skill names Decentro and Setu as the KYC providers, so the
-- provider column becomes a closed enum rather than free text. A typo in a
-- provider name would otherwise route a KYC call to an adapter that does not
-- exist, and only at runtime.
--
-- MOCK is a first-class value for every external provider, not a testing
-- afterthought. RazorpayX sandbox access requires a business approval with real
-- lead time, so the pipeline has to be fully exercisable before any account
-- exists. Recording MOCK in the same column as the real provider keeps the
-- distinction auditable: a payout row states which system actually moved the
-- money.

-- ---------------------------------------------------------------------------
-- KYC providers
-- ---------------------------------------------------------------------------

CREATE TYPE kyc_provider AS ENUM ('MOCK', 'DECENTRO', 'SETU');

ALTER TABLE kyc_records
    ALTER COLUMN provider TYPE kyc_provider
    USING upper(trim(provider))::kyc_provider;

-- ---------------------------------------------------------------------------
-- Mock variants for the existing provider enums
-- ---------------------------------------------------------------------------

ALTER TYPE asba_provider   ADD VALUE IF NOT EXISTS 'MOCK';
ALTER TYPE payout_provider ADD VALUE IF NOT EXISTS 'MOCK';

-- ---------------------------------------------------------------------------
-- IPFS pins
-- ---------------------------------------------------------------------------

CREATE TYPE ipfs_provider AS ENUM ('MOCK', 'PINATA');

-- One row per document published to IPFS.
--
-- The on-chain anchor stores only the 32-byte SHA-256 digest, because a CIDv1 is
-- 36 bytes and does not fit a bytes32. This table is where the full CID and the
-- reconstruction inputs live, so a verifier can go from an on-chain digest to a
-- retrievable document.
--
-- pin_count exists because IPFS is content addressing, not storage. Unpinned
-- content is garbage collected, so "permanently verifiable" is a claim about how
-- many independent services hold a pin, and a single pinning account is an
-- availability dependency rather than permanence.
CREATE TABLE ipfs_pins (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scheme_id           UUID        NOT NULL REFERENCES schemes(id) ON DELETE RESTRICT,
    doc_type            doc_type    NOT NULL,

    -- SHA-256 of the canonical document bytes. This is the value anchored
    -- on-chain and the join key between the chain and this table.
    content_sha256      BYTEA       NOT NULL,

    cid                 TEXT        NOT NULL,
    cid_version         SMALLINT    NOT NULL DEFAULT 1,
    multicodec          TEXT        NOT NULL DEFAULT 'raw',
    byte_size           INTEGER     NOT NULL,

    provider            ipfs_provider NOT NULL,
    provider_ref        TEXT,
    pin_count           SMALLINT    NOT NULL DEFAULT 1,

    -- What this document describes, so an operator can get from a period or an
    -- offer to its published evidence without a scan.
    related_entity_type TEXT        NOT NULL,
    related_entity_id   UUID        NOT NULL,

    schema_version      INTEGER     NOT NULL DEFAULT 1,
    pinned_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    idempotency_key     BYTEA       NOT NULL UNIQUE,

    CONSTRAINT ipfs_pins_idem_len   CHECK (octet_length(idempotency_key) = 32),
    CONSTRAINT ipfs_pins_sha_len    CHECK (octet_length(content_sha256) = 32),
    CONSTRAINT ipfs_pins_size_pos   CHECK (byte_size > 0),
    CONSTRAINT ipfs_pins_cid_nonempty CHECK (length(cid) > 0),
    CONSTRAINT ipfs_pins_version    CHECK (cid_version IN (0, 1)),

    -- At least one pin, and a real deployment should carry two independent ones.
    CONSTRAINT ipfs_pins_pinned     CHECK (pin_count >= 1),

    -- Content addressing means one digest is one document. Two rows with the same
    -- digest and different CIDs would mean the reconstruction rule is wrong.
    UNIQUE (content_sha256)
);

CREATE INDEX ipfs_pins_scheme_idx  ON ipfs_pins(scheme_id, doc_type);
CREATE INDEX ipfs_pins_related_idx ON ipfs_pins(related_entity_type, related_entity_id);

COMMENT ON TABLE ipfs_pins IS
    'Published evidence documents. content_sha256 is what the contract anchors; cid is how to fetch it.';
COMMENT ON COLUMN ipfs_pins.pin_count IS
    'Number of independent pinning services holding this content. One is an availability dependency, not permanence.';

-- ---------------------------------------------------------------------------
-- RLS for the new table
-- ---------------------------------------------------------------------------

-- 0009 enabled RLS across every table that existed at the time. A table added
-- later would default to open, so it is handled explicitly here rather than
-- relying on anyone remembering to re-run 0009.
ALTER TABLE ipfs_pins ENABLE ROW LEVEL SECURITY;
ALTER TABLE ipfs_pins FORCE ROW LEVEL SECURITY;
REVOKE ALL ON ipfs_pins FROM anon, authenticated;

ALTER TABLE schema_migrations ENABLE ROW LEVEL SECURITY;
ALTER TABLE schema_migrations FORCE ROW LEVEL SECURITY;
REVOKE ALL ON schema_migrations FROM anon, authenticated;
