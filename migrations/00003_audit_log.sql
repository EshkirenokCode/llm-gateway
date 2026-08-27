-- +goose Up
CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    tenant_id  uuid REFERENCES tenants(id) ON DELETE SET NULL,
    api_key_id uuid REFERENCES api_keys(id) ON DELETE SET NULL,
    action     text        NOT NULL,
    -- Prompt content is redacted, hashed or stored depending on configuration;
    -- see the audit section of the README.
    detail     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_tenant_created_idx ON audit_log (tenant_id, created_at DESC);

-- +goose Down
DROP TABLE audit_log;
