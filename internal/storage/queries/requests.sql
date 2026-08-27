-- name: InsertRequest :one
INSERT INTO requests (
    tenant_id, api_key_id, provider, model, streamed, status_code,
    prompt_tokens, completion_tokens, cost_micros, duration_ms, error
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: SumTenantSpend :one
SELECT COALESCE(SUM(cost_micros), 0)::bigint AS spent_micros
FROM requests
WHERE tenant_id = $1 AND created_at >= $2;

-- name: ListTenantRequests :many
SELECT * FROM requests
WHERE tenant_id = $1 AND created_at >= $2
ORDER BY created_at DESC
LIMIT $3;
