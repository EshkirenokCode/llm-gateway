-- name: CreateTenant :one
INSERT INTO tenants (name, prefix, budget_micros, soft_limit_ratio)
VALUES ($1, $2, $3, COALESCE(sqlc.narg('soft_limit_ratio')::numeric, 0.800))
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = $1;

-- name: ListTenants :many
SELECT * FROM tenants ORDER BY created_at;

-- name: SetTenantBudget :one
UPDATE tenants
SET budget_micros = $2, updated_at = now()
WHERE id = $1
RETURNING *;
