-- ---------------------------------------------------------------------------
-- 0015_document_uploads
-- ---------------------------------------------------------------------------
--
-- Upload intents for POST /documents/presign.
--
-- # Why bytes never pass through the API
--
-- The client is issued a URL, uploads straight to storage, and hands the
-- returned document id to whichever business call needs it. A multi-megabyte
-- body sharing a connection with a transaction submission adds a failure mode
-- with no upside, and a retried business call must not re-upload a file.
--
-- # What this row is
--
-- A promise about bytes that have not arrived yet: who may upload them, what
-- they claim to be, how large, and optionally what they hash to. When they
-- arrive they are hashed on receipt, and a declared hash that disagrees rejects
-- the upload rather than recording a digest of something else. The received
-- digest is the value any later anchor uses; the declared one is only a check.
--
-- # Scope
--
-- Investor uploads only: KYC evidence and OTHER. The operator document types in
-- the contract's purpose enum (valuation, offer document, trustee escrow, rent
-- evidence) are anchored on-chain through scheme_documents, and nothing in the
-- API consumes them yet, so accepting them here would be storage with no
-- lifecycle.

CREATE TABLE document_uploads (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    investor_id     UUID        NOT NULL REFERENCES investors(id) ON DELETE RESTRICT,
    purpose         TEXT        NOT NULL,
    filename        TEXT        NOT NULL,
    mime_type       TEXT        NOT NULL,
    declared_size   BIGINT,
    declared_sha256 BYTEA,

    status          TEXT        NOT NULL DEFAULT 'PENDING',
    received_sha256 BYTEA,
    received_size   BIGINT,
    received_at     TIMESTAMPTZ,

    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT doc_uploads_purpose CHECK (purpose IN ('KYC', 'OTHER')),
    -- A name, not a path. Separators and control characters are refused so the
    -- filename can never be used to address storage.
    CONSTRAINT doc_uploads_filename CHECK (
        length(filename) BETWEEN 1 AND 255 AND filename !~ '[/\\[:cntrl:]]'
    ),
    CONSTRAINT doc_uploads_mime CHECK (mime_type IN ('application/pdf', 'image/png', 'image/jpeg')),
    -- 25 MiB. Larger than any KYC scan needs to be.
    CONSTRAINT doc_uploads_declared_size CHECK (declared_size IS NULL OR declared_size BETWEEN 1 AND 26214400),
    CONSTRAINT doc_uploads_declared_sha CHECK (declared_sha256 IS NULL OR octet_length(declared_sha256) = 32),
    CONSTRAINT doc_uploads_status CHECK (status IN ('PENDING', 'RECEIVED')),

    -- Received means every received fact is present, and pending means none is.
    CONSTRAINT doc_uploads_received_complete CHECK (
        (status = 'RECEIVED') = (received_sha256 IS NOT NULL AND received_size IS NOT NULL AND received_at IS NOT NULL)
    ),
    CONSTRAINT doc_uploads_received_sha CHECK (received_sha256 IS NULL OR octet_length(received_sha256) = 32),
    -- The two checks that make the declaration worth making, held by the table
    -- as well as the handler.
    CONSTRAINT doc_uploads_hash_matches CHECK (
        declared_sha256 IS NULL OR received_sha256 IS NULL OR declared_sha256 = received_sha256
    ),
    CONSTRAINT doc_uploads_size_matches CHECK (
        declared_size IS NULL OR received_size IS NULL OR declared_size = received_size
    )
);

CREATE INDEX doc_uploads_investor_idx ON document_uploads(investor_id, created_at);

ALTER TABLE document_uploads ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_uploads FORCE ROW LEVEL SECURITY;
REVOKE ALL ON document_uploads FROM anon, authenticated;
