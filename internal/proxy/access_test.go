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

const tenantKey = "test-tenant-key-at-least-24-characters"

func configured(t *testing.T, opts proxy.Options, p providers.Provider) http.Handler {
	h := proxy.NewHandler(p, discardLogger(t), nil, nil)
	h.Configure(opts)
	return h.Routes()
}
func request(h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	method := http.MethodGet
	if body != "" {
		method = http.MethodPost
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestTenantAuthenticationAndIsolation(t *testing.T) {
	opts := proxy.Options{Tenants: []proxy.Tenant{{Name: "one", Key: tenantKey, RPM: 1, Concurrency: 2}, {Name: "two", Key: tenantKey + "2", RPM: 1, Concurrency: 2}}, Models: []string{"m"}}
	h := configured(t, opts, providers.NewMock(providers.MockOptions{}))
	require.Equal(t, 401, request(h, "/v1/models", "", "").Code)
	require.Equal(t, 401, request(h, "/v1/models", "wrong", "").Code)
	first := request(h, "/v1/models", tenantKey, "")
	require.Equal(t, 200, first.Code)
	require.NotEmpty(t, first.Header().Get("X-Request-ID"))
	require.NotContains(t, first.Body.String(), tenantKey)
	limited := request(h, "/v1/models", tenantKey, "")
	require.Equal(t, 429, limited.Code)
	require.Equal(t, "60", limited.Header().Get("Retry-After"))
	require.Equal(t, 200, request(h, "/v1/models", tenantKey+"2", "").Code)
	require.Equal(t, 200, request(h, "/readyz", "", "").Code)
}
func TestConcurrentTenantLimit(t *testing.T) {
	h := configured(t, proxy.Options{Tenants: []proxy.Tenant{{Name: "one", Key: tenantKey, RPM: 100, Concurrency: 2}}}, providers.NewMock(providers.MockOptions{Latency: 30 * time.Millisecond}))
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- request(h, "/v1/chat/completions", tenantKey, `{"model":"m","messages":[{"role":"user","content":"hello"}]}`).Code
		}()
	}
	wg.Wait()
	close(codes)
	successes := 0
	for code := range codes {
		if code == 200 {
			successes++
		} else {
			require.Equal(t, 429, code)
		}
	}
	require.Equal(t, 2, successes)
	require.Equal(t, 200, request(h, "/v1/chat/completions", tenantKey, `{"model":"m","messages":[{"role":"user","content":"hello"}]}`).Code)
}
func TestModelsMetricsAndTimeout(t *testing.T) {
	h := configured(t, proxy.Options{Models: []string{"enabled"}, MetricsKey: "metrics-secret", Timeout: time.Millisecond}, providers.NewMock(providers.MockOptions{Latency: 10 * time.Millisecond}))
	require.Equal(t, 400, request(h, "/v1/chat/completions", "", `{"model":"disabled","messages":[{"role":"user","content":"hi"}]}`).Code)
	require.Equal(t, 504, request(h, "/v1/chat/completions", "", `{"model":"enabled","messages":[{"role":"user","content":"hi"}]}`).Code)
	require.Equal(t, 401, request(h, "/metrics", "", "").Code)
	require.Equal(t, 200, request(h, "/metrics", "metrics-secret", "").Code)
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(request(h, "/v1/models", "", "").Body.Bytes(), &models))
	require.Equal(t, "enabled", models.Data[0].ID)
	page := request(h, "/", "", "")
	require.Equal(t, 200, page.Code)
	require.Contains(t, page.Body.String(), "A clearer route to models.")
}
func TestStreamErrorIsNotSuccessful(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer upstream.Close()
	h := configured(t, proxy.Options{}, providers.NewHTTP(upstream.URL, "", time.Second))
	w := request(h, "/v1/chat/completions", "", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "upstream stream interrupted")
	require.NotContains(t, w.Body.String(), "[DONE]")
}
func TestCancelledRequestReleasesSlot(t *testing.T) {
	h := configured(t, proxy.Options{Tenants: []proxy.Tenant{{Name: "one", Key: tenantKey, RPM: 10, Concurrency: 1}}}, providers.NewMock(providers.MockOptions{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tenantKey)
	h.ServeHTTP(httptest.NewRecorder(), r)
	require.Equal(t, 200, request(h, "/v1/models", tenantKey, "").Code)
}
