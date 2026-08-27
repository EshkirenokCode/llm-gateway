-- name: InsertAuditEvent :exec
INSERT INTO audit_log (tenant_id, api_key_id, action, detail)
VALUES ($1, $2, $3, $4);

-- name: ListAuditEvents :many
SELECT * FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2;
