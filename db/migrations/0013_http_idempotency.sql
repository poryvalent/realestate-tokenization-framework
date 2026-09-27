-- ---------------------------------------------------------------------------
-- 0013_http_idempotency
-- ---------------------------------------------------------------------------
--
-- Stores the first response to each HTTP mutation so a retry receives exactly
-- that response again.
--
-- # Why this is a second idempotency mechanism and not a duplicate of the first
--
-- Every business table already carries a UNIQUE idempotency_key derived from the
-- content of the action, and the contract's usedKeys mapping carries the same
-- key on-chain. Those are what stop an action happening twice, and they work
-- without the client's cooperation: a double-clicked button produces the same
-- derived key from the same content.
--
-- What they cannot do is answer the second request the way the first was
-- answered. A client whose connection dropped after the server committed needs
-- to learn what happened, and "this already exists" is a worse answer than the
-- original 201 with the resource in it. This table holds that answer.
--
-- The client's Idempotency-Key is therefore a handle on a response, never the
-- thing trusted to prevent a double execution.
--
-- # Scope
--
-- A key is scoped to the caller and the route. Two investors who happen to send
-- the same key are different requests, and one caller reusing a key on a
-- different endpoint is a client bug that should not silently return a response
-- belonging to another operation.
--
-- # What is recorded
--
-- Only successful responses. A refused request changed nothing, because the
-- write and this row share one transaction and both roll back. Recording the
-- refusal would pin a client to a 409 that the state of the world may since have
-- resolved, and the client's only way out would be to invent a new key for what
-- is logically the same request.

CREATE TABLE http_idempotency (
    -- Who sent it: 'INVESTOR:<investor id>' or 'OPERATOR:<subject>'.
    principal       TEXT        NOT NULL,
    -- The route pattern, e.g. 'POST /v1/offers/{offerId}/bids', plus the
    -- concrete path, so a key reused against a different offer is a conflict
    -- rather than a replay of another offer's bid.
    route           TEXT        NOT NULL,
    path            TEXT        NOT NULL,
    idempotency_key TEXT        NOT NULL,
    -- sha256 of the request body as received. A different body under the same
    -- key is a conflict: the client has reused a key for a different request.
    request_sha256  BYTEA       NOT NULL,
    status_code     INTEGER     NOT NULL,
    response_body   BYTEA       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT http_idem_key_len     CHECK (length(idempotency_key) BETWEEN 16 AND 128),
    CONSTRAINT http_idem_sha_len     CHECK (octet_length(request_sha256) = 32),
    CONSTRAINT http_idem_success     CHECK (status_code BETWEEN 200 AND 299),
    CONSTRAINT http_idem_principal   CHECK (principal ~ '^(INVESTOR|OPERATOR):.+$'),

    PRIMARY KEY (principal, route, idempotency_key)
);

-- The same deny-by-default posture 0009 applied to every table that existed
-- then. 0009's loop does not reach a table created afterwards, so it is stated
-- again here rather than assumed.
ALTER TABLE http_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE http_idempotency FORCE ROW LEVEL SECURITY;
REVOKE ALL ON http_idempotency FROM anon, authenticated;
