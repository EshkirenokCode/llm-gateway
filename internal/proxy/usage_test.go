package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/EshkirenokCode/llm-gateway/internal/proxy"
	"github.com/stretchr/testify/require"
)

func readUsage(t *testing.T, h http.Handler, key string) proxy.TenantUsage {
	t.Helper()
	w := request(h, "/v1/usage", key, "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.NotEmpty(t, w.Header().Get("X-Request-ID"))
	require.NotContains(t, w.Body.String(), key)
	var usage proxy.TenantUsage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &usage))
	return usage
}
func TestTenantUsageIsIsolatedAndRemainsAvailableAfterQuota(t *testing.T) {
	tenants := []proxy.Tenant{{Name: "one", Key: tenantKey, RPM: 2, Concurrency: 1}, {Name: "two", Key: tenantKey + "2", RPM: 2, Concurrency: 1}}
	h := configured(t, proxy.Options{Tenants: tenants}, providers.NewMock(providers.MockOptions{}))
	for _, body := range []string{`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`} {
		require.Equal(t, 200, request(h, "/v1/chat/completions", tenantKey, body).Code)
	}
	require.Equal(t, 429, request(h, "/v1/models", tenantKey, "").Code)
	for i := 0; i < 3; i++ {
		usage := readUsage(t, h, tenantKey)
		require.Equal(t, "one", usage.Tenant)
		require.EqualValues(t, 2, usage.Totals.Requests)
		require.EqualValues(t, 2, usage.Totals.Completions)
		require.EqualValues(t, 2, usage.Totals.UsageReported)
		require.EqualValues(t, 1, usage.Totals.RateLimited)
		require.Positive(t, usage.Totals.PromptTokens)
		require.Positive(t, usage.Totals.CompletionTokens)
		require.Zero(t, usage.Limits.RequestsRemaining)
		require.Zero(t, usage.Limits.InFlight)
	}
	other := readUsage(t, h, tenantKey+"2")
	require.Equal(t, "two", other.Tenant)
	require.Equal(t, proxy.TenantTotals{}, other.Totals)
	require.Equal(t, 2, other.Limits.RequestsRemaining)
	for _, key := range []string{"", "invalid"} {
		require.Equal(t, 401, request(h, "/v1/usage", key, "").Code)
	}
	demo := configured(t, proxy.Options{}, providers.NewMock(providers.MockOptions{}))
	require.Equal(t, 401, request(demo, "/v1/usage", "", "").Code)
}
func TestUsageRemainsAvailableWhileTenantIsBusy(t *testing.T) {
	provider := &gatedProvider{Provider: providers.NewMock(providers.MockOptions{}), started: make(chan struct{}, 1), release: make(chan struct{})}
	h := configured(t, proxy.Options{Tenants: []proxy.Tenant{{Name: "one", Key: tenantKey, RPM: 10, Concurrency: 1}}}, provider)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+tenantKey)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("provider was not called")
	}
	usage := readUsage(t, h, tenantKey)
	require.Equal(t, 1, usage.Limits.InFlight)
	blocked := request(h, "/v1/models", tenantKey, "")
	require.Equal(t, 429, blocked.Code)
	require.Equal(t, "1", blocked.Header().Get("Retry-After"))
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not finish request")
	}
	usage = readUsage(t, h, tenantKey)
	require.Zero(t, usage.Limits.InFlight)
	require.EqualValues(t, 1, usage.Totals.Canceled)
	require.Zero(t, usage.Totals.Failed)
}
func TestTenantUsageHandlesConcurrentCompletions(t *testing.T) {
	h := configured(t, proxy.Options{Tenants: []proxy.Tenant{{Name: "one", Key: tenantKey, RPM: 100, Concurrency: 100}}}, providers.NewMock(providers.MockOptions{}))
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request(h, "/v1/chat/completions", tenantKey, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		}()
	}
	wg.Wait()
	usage := readUsage(t, h, tenantKey)
	require.EqualValues(t, 30, usage.Totals.Completions)
	require.EqualValues(t, 30, usage.Totals.Requests)
	require.EqualValues(t, 30, usage.Totals.UsageReported)
	require.Zero(t, usage.Limits.InFlight)
}
func TestTenantUsageKeepsLastReportedTokensOnStreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"s1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n"))
	}))
	defer upstream.Close()
	h := configured(t, proxy.Options{Tenants: []proxy.Tenant{{Name: "one", Key: tenantKey, RPM: 10, Concurrency: 1}}}, providers.NewHTTP(upstream.URL, "", time.Second))
	response := request(h, "/v1/chat/completions", tenantKey, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	require.NotContains(t, response.Body.String(), "[DONE]")
	usage := readUsage(t, h, tenantKey)
	require.EqualValues(t, 1, usage.Totals.Failed)
	require.EqualValues(t, 1, usage.Totals.UsageReported)
	require.EqualValues(t, 3, usage.Totals.CompletionTokens)
	require.Zero(t, usage.Totals.Completions)
}
