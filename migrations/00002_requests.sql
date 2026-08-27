-- +goose Up
CREATE TABLE requests (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    api_key_id    uuid        REFERENCES api_keys(id) ON DELETE SET NULL,
    provider      text        NOT NULL,
    model         text        NOT NULL,
    streamed      boolean     NOT NULL DEFAULT false,
    status_code   integer     NOT NULL,
    prompt_tokens     integer NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens integer NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    -- Cost in micro dollars: integers avoid the rounding surprises of floats
    -- and stay exact when summed over millions of rows.
    cost_micros   bigint      NOT NULL DEFAULT 0 CHECK (cost_micros >= 0),
    duration_ms   integer     NOT NULL DEFAULT 0,
    error         text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Budget checks sum a tenant's spend since the start of the month, which is
-- the only query on the hot path that touches this table.
CREATE INDEX requests_tenant_created_idx ON requests (tenant_id, created_at DESC);

CREATE TABLE idempotency_keys (
    tenant_id     uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key           text        NOT NULL,
    -- Hash of the request body: replaying a key with a different payload is a
    -- client bug and must not silently return the wrong answer.
    request_hash  bytea       NOT NULL,
    status_code   integer     NOT NULL,
    response_body jsonb       NOT NULL,
    request_id    uuid        REFERENCES requests(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, key)
);

CREATE INDEX idempotency_keys_expires_idx ON idempotency_keys (expires_at);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE requests;
