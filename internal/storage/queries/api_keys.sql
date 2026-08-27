-- name: CreateAPIKey :one
INSERT INTO api_keys (tenant_id, key_hash, last_four, name, rate_limit_rpm)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetAPIKeyByHash :one
SELECT
    k.id            AS key_id,
    k.tenant_id     AS tenant_id,
    k.rate_limit_rpm AS rate_limit_rpm,
    k.revoked_at    AS revoked_at,
    t.name          AS tenant_name,
    t.prefix        AS tenant_prefix,
    t.budget_micros AS budget_micros,
    t.soft_limit_ratio AS soft_limit_ratio,
    t.disabled_at   AS tenant_disabled_at
FROM api_keys k
JOIN tenants t ON t.id = k.tenant_id
WHERE k.key_hash = $1;

-- name: ListAPIKeys :many
SELECT * FROM api_keys WHERE tenant_id = $1 ORDER BY created_at;

-- name: RevokeAPIKey :exec
UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1;
