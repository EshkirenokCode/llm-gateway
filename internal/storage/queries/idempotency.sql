-- name: GetIdempotentResponse :one
SELECT * FROM idempotency_keys
WHERE tenant_id = $1 AND key = $2 AND expires_at > now();

-- name: SaveIdempotentResponse :exec
INSERT INTO idempotency_keys (tenant_id, key, request_hash, status_code, response_body, request_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, key) DO NOTHING;

-- name: DeleteExpiredIdempotencyKeys :execrows
DELETE FROM idempotency_keys WHERE expires_at <= now();
