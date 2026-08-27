package storage_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/EshkirenokCode/llm-gateway/internal/auth"
	"github.com/EshkirenokCode/llm-gateway/internal/billing"
	"github.com/EshkirenokCode/llm-gateway/internal/storage"
	"github.com/EshkirenokCode/llm-gateway/internal/storage/db"
)

// newStore starts a throwaway Postgres, applies the migrations and returns a
// connected store. The container is shared by every test in the package: it
// takes a few seconds to start and the tests clean up after themselves.
func newStore(t *testing.T) *storage.Store {
	t.Helper()

	if testing.Short() {
		t.Skip("integration test needs docker")
	}

	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("gateway"),
		postgres.WithUsername("gateway"),
		postgres.WithPassword("gateway"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("could not terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	store, err := storage.Open(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(store.Close)

	require.NoError(t, storage.Migrate(ctx, store.Pool))

	return store
}

func createTenant(t *testing.T, store *storage.Store, prefix string, budgetUSD float64) db.Tenant {
	t.Helper()

	params := db.CreateTenantParams{Name: "Test " + prefix, Prefix: prefix}
	if budgetUSD > 0 {
		micros := int64(budgetUSD * 1_000_000)
		params.BudgetMicros = &micros
	}
	require.NoError(t, params.SoftLimitRatio.Scan("0.800"))

	tenant, err := store.CreateTenant(context.Background(), params)
	require.NoError(t, err)

	return tenant
}

func createKey(t *testing.T, store *storage.Store, tenant db.Tenant, rpm int) (auth.Key, db.ApiKey) {
	t.Helper()

	key, err := auth.Generate(tenant.Prefix)
	require.NoError(t, err)

	params := db.CreateAPIKeyParams{
		TenantID: tenant.ID,
		KeyHash:  key.Hash,
		LastFour: key.LastFour,
		Name:     "test",
	}
	if rpm > 0 {
		limit := int32(rpm)
		params.RateLimitRpm = &limit
	}

	row, err := store.CreateAPIKey(context.Background(), params)
	require.NoError(t, err)

	return key, row
}

func TestMigrationsAreIdempotent(t *testing.T) {
	store := newStore(t)

	require.NoError(t, storage.Migrate(context.Background(), store.Pool),
		"running migrations twice must be a no-op")
}

func TestIdentityLookup(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	tenant := createTenant(t, store, "acme", 12.5)
	key, row := createKey(t, store, tenant, 120)

	identity, err := store.IdentityByKeyHash(ctx, key.Hash)
	require.NoError(t, err)

	require.Equal(t, tenant.ID, identity.TenantID)
	require.Equal(t, "acme", identity.TenantPrefix)
	require.Equal(t, row.ID, identity.KeyID)
	require.Equal(t, 120, identity.RateLimitRPM)
	require.True(t, identity.HasBudget)
	require.Equal(t, int64(12_500_000), identity.BudgetMicros)
	require.InDelta(t, 0.8, identity.SoftLimitRatio, 0.001)
}

func TestUnknownKeyIsRejected(t *testing.T) {
	store := newStore(t)

	_, err := store.IdentityByKeyHash(context.Background(), auth.HashKey("sk_acme_nope"))

	require.ErrorIs(t, err, auth.ErrUnknownKey)
}

func TestRevokedKeyIsRejected(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	tenant := createTenant(t, store, "acme", 0)
	key, row := createKey(t, store, tenant, 0)

	require.NoError(t, store.RevokeAPIKey(ctx, row.ID))

	_, err := store.IdentityByKeyHash(ctx, key.Hash)
	require.ErrorIs(t, err, auth.ErrRevoked)
}

func TestPlaintextKeyIsNeverStored(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	tenant := createTenant(t, store, "acme", 0)
	key, _ := createKey(t, store, tenant, 0)

	var found int
	err := store.Pool.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE encode(key_hash, 'escape') LIKE '%' || $1 || '%'`,
		key.Plaintext).Scan(&found)
	require.NoError(t, err)
	require.Zero(t, found, "the database must not contain the plaintext key")
}

func TestSpendAccumulates(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	tenant := createTenant(t, store, "acme", 100)
	_, key := createKey(t, store, tenant, 0)

	for i := 0; i < 3; i++ {
		_, err := store.RecordRequest(ctx, storage.RequestRecord{
			TenantID:         tenant.ID,
			APIKeyID:         key.ID,
			Provider:         "mock",
			Model:            "gpt-4o-mini",
			StatusCode:       200,
			PromptTokens:     100,
			CompletionTokens: 200,
			Cost:             billing.Micros(1500),
			Duration:         250 * time.Millisecond,
		})
		require.NoError(t, err)
	}

	spent, err := store.SpentSince(ctx, tenant.ID, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, billing.Micros(4500), spent)

	// A different window must not see those rows.
	future, err := store.SpentSince(ctx, tenant.ID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, future)
}

func TestSpendIsPerTenant(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	acme := createTenant(t, store, "acme", 0)
	beta := createTenant(t, store, "beta", 0)

	_, err := store.RecordRequest(ctx, storage.RequestRecord{
		TenantID: acme.ID, Provider: "mock", Model: "m", StatusCode: 200, Cost: 5000,
	})
	require.NoError(t, err)

	spent, err := store.SpentSince(ctx, beta.ID, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Zero(t, spent)
}

func TestIdempotentResponseRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	tenant := createTenant(t, store, "acme", 0)
	hash := auth.HashKey("request-body")
	body := []byte(`{"id":"chatcmpl-1"}`)

	missing, err := store.FindIdempotent(ctx, tenant.ID, "key-1")
	require.NoError(t, err)
	require.Nil(t, missing)

	require.NoError(t, store.SaveIdempotent(ctx, tenant.ID, "key-1", hash, 200, body, uuid.Nil, time.Hour))

	stored, err := store.FindIdempotent(ctx, tenant.ID, "key-1")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, 200, stored.StatusCode)
	require.JSONEq(t, string(body), string(stored.Body))
	require.True(t, auth.EqualHash(hash, stored.RequestHash))
}

func TestIdempotentResponseExpires(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	tenant := createTenant(t, store, "acme", 0)

	require.NoError(t, store.SaveIdempotent(ctx, tenant.ID, "old", auth.HashKey("x"), 200,
		[]byte(`{}`), uuid.Nil, -time.Minute))

	stored, err := store.FindIdempotent(ctx, tenant.ID, "old")
	require.NoError(t, err)
	require.Nil(t, stored, "an expired entry must not be replayed")

	removed, err := store.DeleteExpiredIdempotencyKeys(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), removed)
}

func TestIdempotentSaveIgnoresConflicts(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	tenant := createTenant(t, store, "acme", 0)
	first := []byte(`{"n":1}`)
	second := []byte(`{"n":2}`)

	require.NoError(t, store.SaveIdempotent(ctx, tenant.ID, "k", auth.HashKey("a"), 200, first, uuid.Nil, time.Hour))
	require.NoError(t, store.SaveIdempotent(ctx, tenant.ID, "k", auth.HashKey("a"), 200, second, uuid.Nil, time.Hour))

	stored, err := store.FindIdempotent(ctx, tenant.ID, "k")
	require.NoError(t, err)
	require.JSONEq(t, string(first), string(stored.Body), "the first response wins")
}

func TestAuditLog(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	tenant := createTenant(t, store, "acme", 0)
	_, key := createKey(t, store, tenant, 0)

	require.NoError(t, store.Audit(ctx, tenant.ID, key.ID, "chat.completion",
		map[string]any{"model": "gpt-4o-mini", "prompt_sha256": "abc"}))

	events, err := store.ListAuditEvents(ctx, db.ListAuditEventsParams{TenantID: &tenant.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "chat.completion", events[0].Action)

	var detail map[string]any
	require.NoError(t, json.Unmarshal(events[0].Detail, &detail))
	require.Equal(t, "gpt-4o-mini", detail["model"])
}

func TestTenantPrefixMustBeUnique(t *testing.T) {
	store := newStore(t)

	createTenant(t, store, "acme", 0)

	_, err := store.CreateTenant(context.Background(), db.CreateTenantParams{
		Name: "Another", Prefix: "acme", SoftLimitRatio: pgtype.Numeric{},
	})
	require.Error(t, err, "two tenants must not share a prefix")
}

func TestRequestsAreDeletedWithTheirTenant(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	tenant := createTenant(t, store, "acme", 0)
	_, err := store.RecordRequest(ctx, storage.RequestRecord{
		TenantID: tenant.ID, Provider: "mock", Model: "m", StatusCode: 200,
	})
	require.NoError(t, err)

	_, err = store.Pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenant.ID)
	require.NoError(t, err)

	var count int
	require.NoError(t, store.Pool.QueryRow(ctx,
		`SELECT count(*) FROM requests WHERE tenant_id = $1`, tenant.ID).Scan(&count))
	require.Zero(t, count)
}
