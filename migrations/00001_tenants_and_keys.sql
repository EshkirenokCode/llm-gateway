-- +goose Up
CREATE TABLE tenants (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL,
    -- Short human readable marker embedded in every key of this tenant, so a
    -- leaked key can be traced without a database lookup.
    prefix      text        NOT NULL UNIQUE CHECK (prefix ~ '^[a-z0-9]{3,16}$'),
    -- Monthly hard cap in micro dollars. NULL means no limit.
    budget_micros    bigint CHECK (budget_micros IS NULL OR budget_micros >= 0),
    -- Fraction of the budget at which a warning header is returned.
    soft_limit_ratio numeric(4,3) NOT NULL DEFAULT 0.800
        CHECK (soft_limit_ratio > 0 AND soft_limit_ratio <= 1),
    disabled_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- sha256 of the plaintext key. The key itself is shown once at creation
    -- and never stored.
    key_hash    bytea       NOT NULL UNIQUE,
    -- Last four characters of the key, for identifying it in a list.
    last_four   text        NOT NULL CHECK (char_length(last_four) = 4),
    name        text        NOT NULL DEFAULT '',
    -- Requests per minute allowed for this key. NULL means unlimited.
    rate_limit_rpm integer CHECK (rate_limit_rpm IS NULL OR rate_limit_rpm > 0),
    last_used_at timestamptz,
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX api_keys_tenant_id_idx ON api_keys (tenant_id);

-- +goose Down
DROP TABLE api_keys;
DROP TABLE tenants;
