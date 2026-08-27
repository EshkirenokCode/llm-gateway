package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/auth"
	"github.com/EshkirenokCode/llm-gateway/internal/billing"
	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/EshkirenokCode/llm-gateway/internal/proxy"
	"github.com/EshkirenokCode/llm-gateway/internal/storage"
	"github.com/EshkirenokCode/llm-gateway/internal/telemetry"
)

// memStore is an in-memory stand-in for Postgres. The database itself is
// covered by the integration tests; this keeps the HTTP tests fast.
type memStore struct {
	mu sync.Mutex

	identity auth.Identity
	spent    billing.Micros

	records []storage.RequestRecord
	stored  map[string]*storage.StoredResponse
	audits  []string
}

func newMemStore(identity auth.Identity) *memStore {
	return &memStore{identity: identity, stored: make(map[string]*storage.StoredResponse)}
}

func (m *memStore) IdentityByKeyHash(context.Context, []byte) (auth.Identity, error) {
	return m.identity, nil
}

func (m *memStore) MarkKeyUsed(context.Context, uuid.UUID) {}

func (m *memStore) SpentSince(context.Context, uuid.UUID, time.Time) (billing.Micros, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spent, nil
}

func (m *memStore) RecordRequest(_ context.Context, rec storage.RequestRecord) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, rec)
	return uuid.New(), nil
}

func (m *memStore) FindIdempotent(_ context.Context, tenantID uuid.UUID, key string) (*storage.StoredResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stored[tenantID.String()+key], nil
}

func (m *memStore) SaveIdempotent(_ context.Context, tenantID uuid.UUID, key string, requestHash []byte, status int, body []byte, _ uuid.UUID, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stored[tenantID.String()+key] = &storage.StoredResponse{StatusCode: status, Body: body, RequestHash: requestHash}
	return nil
}

func (m *memStore) Audit(_ context.Context, _, _ uuid.UUID, action string, _ map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audits = append(m.audits, action)
	return nil
}

func (m *memStore) recorded() []storage.RequestRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]storage.RequestRecord, len(m.records))
	copy(out, m.records)
	return out
}

func testIdentity() auth.Identity {
	return auth.Identity{
		TenantID:       uuid.New(),
		TenantName:     "Acme",
		TenantPrefix:   "acme",
		KeyID:          uuid.New(),
		RateLimitRPM:   60,
		BudgetMicros:   1_000_000,
		HasBudget:      true,
		SoftLimitRatio: 0.8,
	}
}

type gatedOptions struct {
	provider providers.Provider
	store    *memStore
	metrics  *telemetry.Metrics
}

func gatedHandler(t *testing.T, opts gatedOptions) http.Handler {
	t.Helper()

	if opts.provider == nil {
		opts.provider = providers.NewMock(providers.MockOptions{Reply: "two words"})
	}
	if opts.metrics == nil {
		opts.metrics = telemetry.NewMetrics()
	}

	return proxy.NewHandler(proxy.Options{
		Provider: opts.provider,
		Logger:   discardLogger(t),
		Metrics:  opts.metrics,
		Auth:     auth.NewAuthenticator(opts.store, 0),
		Budget:   billing.NewBudget(opts.store, time.Minute),
		Limiter:  billing.NewRateLimiter(0.25),
		Pricing:  billing.DefaultPricing(),
		Store:    opts.store,
	}).Routes()
}

func authorized(t *testing.T, h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk_acme_secret")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const chatBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`

func TestRequestWithoutKeyIsRejected(t *testing.T) {
	t.Parallel()

	h := gatedHandler(t, gatedOptions{store: newMemStore(testIdentity())})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "missing api key")
}

func TestAuthorizedRequestIsRecorded(t *testing.T) {
	t.Parallel()

	store := newMemStore(testIdentity())
	h := gatedHandler(t, gatedOptions{store: store})

	rec := authorized(t, h, chatBody, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "mock", rec.Header().Get("X-Gateway-Provider"))
	require.NotEmpty(t, rec.Header().Get("X-Gateway-Cost-Usd"))
	require.NotEmpty(t, rec.Header().Get("X-Gateway-Budget-Remaining-Usd"))

	records := store.recorded()
	require.Len(t, records, 1)
	require.Equal(t, "gpt-4o-mini", records[0].Model)
	require.Equal(t, http.StatusOK, records[0].StatusCode)
	require.Positive(t, records[0].PromptTokens)
	require.Positive(t, records[0].CompletionTokens)
	require.Positive(t, int64(records[0].Cost))
	require.False(t, records[0].Streamed)
}

func TestSoftLimitAddsWarningHeader(t *testing.T) {
	t.Parallel()

	store := newMemStore(testIdentity())
	store.spent = 900_000 // budget is 1_000_000 with a 0.8 soft limit

	h := gatedHandler(t, gatedOptions{store: store})

	rec := authorized(t, h, chatBody, nil)

	require.Equal(t, http.StatusOK, rec.Code, "the soft limit warns, it does not block")
	require.Contains(t, rec.Header().Get("X-Gateway-Budget-Warning"), "spent")
}

func TestHardLimitReturnsPaymentRequired(t *testing.T) {
	t.Parallel()

	store := newMemStore(testIdentity())
	store.spent = 1_000_000

	h := gatedHandler(t, gatedOptions{store: store})

	rec := authorized(t, h, chatBody, nil)

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	require.Contains(t, rec.Body.String(), "budget")
	require.Empty(t, store.recorded(), "a blocked request never reaches the provider")
}

func TestRateLimitReturnsTooManyRequests(t *testing.T) {
	t.Parallel()

	identity := testIdentity()
	identity.RateLimitRPM = 4 // burst ratio 0.25 gives a single token
	store := newMemStore(identity)

	h := gatedHandler(t, gatedOptions{store: store})

	require.Equal(t, http.StatusOK, authorized(t, h, chatBody, nil).Code)

	rec := authorized(t, h, chatBody, nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
}

func TestIdempotencyReplaysStoredResponse(t *testing.T) {
	t.Parallel()

	store := newMemStore(testIdentity())
	h := gatedHandler(t, gatedOptions{store: store})

	first := authorized(t, h, chatBody, map[string]string{"Idempotency-Key": "abc-123"})
	require.Equal(t, http.StatusOK, first.Code)
	require.Empty(t, first.Header().Get("Idempotent-Replay"))

	second := authorized(t, h, chatBody, map[string]string{"Idempotency-Key": "abc-123"})
	require.Equal(t, http.StatusOK, second.Code)
	require.Equal(t, "true", second.Header().Get("Idempotent-Replay"))
	require.JSONEq(t, first.Body.String(), second.Body.String())

	require.Len(t, store.recorded(), 1, "a replay must not call the provider again")
}

func TestIdempotencyKeyReuseWithDifferentBodyConflicts(t *testing.T) {
	t.Parallel()

	store := newMemStore(testIdentity())
	h := gatedHandler(t, gatedOptions{store: store})

	require.Equal(t, http.StatusOK, authorized(t, h, chatBody, map[string]string{"Idempotency-Key": "abc-123"}).Code)

	other := `{"model":"gpt-4o","messages":[{"role":"user","content":"different"}]}`
	rec := authorized(t, h, other, map[string]string{"Idempotency-Key": "abc-123"})

	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "different request body")
}

func TestStreamedRequestIsAccounted(t *testing.T) {
	t.Parallel()

	store := newMemStore(testIdentity())
	h := gatedHandler(t, gatedOptions{store: store})

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := authorized(t, h, body, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "[DONE]")

	records := store.recorded()
	require.Len(t, records, 1)
	require.True(t, records[0].Streamed)
	require.Positive(t, records[0].CompletionTokens, "usage arrives with the final chunk")
	require.Positive(t, int64(records[0].Cost))
}

func TestBlockedRequestsAreCounted(t *testing.T) {
	t.Parallel()

	metrics := telemetry.NewMetrics()
	store := newMemStore(testIdentity())
	store.spent = 2_000_000

	h := gatedHandler(t, gatedOptions{store: store, metrics: metrics})

	require.Equal(t, http.StatusPaymentRequired, authorized(t, h, chatBody, nil).Code)

	body := get(t, h, "/metrics").Body.String()
	require.Contains(t, body, `gateway_blocked_total{reason="budget",tenant="acme"} 1`)
}

func TestUsageHeadersMatchResponseBody(t *testing.T) {
	t.Parallel()

	store := newMemStore(testIdentity())
	h := gatedHandler(t, gatedOptions{store: store})

	rec := authorized(t, h, chatBody, nil)

	var resp providers.ChatResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	require.Equal(t, strconv.Itoa(resp.Usage.TotalTokens), rec.Header().Get("X-Gateway-Tokens-Total"))
	require.Positive(t, resp.Usage.TotalTokens)
}
