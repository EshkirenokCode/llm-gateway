package proxy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
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

type gatedProvider struct {
	providers.Provider
	started chan struct{}
	release chan struct{}
}

func (p *gatedProvider) Chat(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	p.started <- struct{}{}
	select {
	case <-p.release:
		return p.Provider.Chat(ctx, req)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestConcurrentTenantLimit(t *testing.T) {
	provider := &gatedProvider{Provider: providers.NewMock(providers.MockOptions{}), started: make(chan struct{}, 2), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := configured(t, proxy.Options{Tenants: []proxy.Tenant{{Name: "one", Key: tenantKey, RPM: 100, Concurrency: 2}}}, provider)
	const body = `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+tenantKey)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			results <- w.Code
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-provider.started:
		case <-time.After(time.Second):
			t.Fatal("requests did not occupy both slots")
		}
	}
	var wg sync.WaitGroup
	rejections := make(chan int, 18)
	for i := 0; i < 18; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); rejections <- request(h, "/v1/chat/completions", tenantKey, body).Code }()
	}
	wg.Wait()
	close(rejections)
	for code := range rejections {
		require.Equal(t, http.StatusTooManyRequests, code)
	}
	close(provider.release)
	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusOK, <-results)
	}
	require.Equal(t, http.StatusOK, request(h, "/v1/models", tenantKey, "").Code)
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

func TestRequestBodyLimitStatus(t *testing.T) {
	h := testHandler(t, providers.NewMock(providers.MockOptions{}))
	body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}]}`
	require.Equal(t, http.StatusRequestEntityTooLarge, request(h, "/v1/chat/completions", "", body).Code)
	valid := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	require.Equal(t, http.StatusRequestEntityTooLarge, request(h, "/v1/chat/completions", "", valid+strings.Repeat(" ", 1<<20)).Code)
}
func TestStreamDeadlineIsNotClientDisconnect(t *testing.T) {
	h := configured(t, proxy.Options{Timeout: 5 * time.Millisecond}, providers.NewMock(providers.MockOptions{Latency: time.Second}))
	w := request(h, "/v1/chat/completions", "", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	require.Contains(t, w.Body.String(), "timeout_error")
	require.NotContains(t, w.Body.String(), "[DONE]")
	metrics := request(h, "/metrics", "", "").Body.String()
	require.Contains(t, metrics, `gateway_streams_total{outcome="timeout",provider="mock"} 1`)
	require.NotContains(t, metrics, `outcome="client_gone"`)
}

func TestSlowRequestBodyHonorsRequestDeadline(t *testing.T) {
	for _, prefix := range []string{`{"model":`, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`} {
		t.Run(prefix, func(t *testing.T) {
			h := configured(t, proxy.Options{Timeout: 30 * time.Millisecond}, providers.NewMock(providers.MockOptions{}))
			server := httptest.NewServer(h)
			defer server.Close()
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
			_, err = conn.Write([]byte("POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n" + prefix))
			require.NoError(t, err)
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, http.StatusRequestTimeout, response.StatusCode)
		})
	}
}

func TestCumulativeStreamUsageIsAccountedOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"x","choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}

data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":8,"total_tokens":10}}

data: [DONE]

`))
	}))
	defer upstream.Close()
	h := configured(t, proxy.Options{}, providers.NewHTTP(upstream.URL, "", time.Second))
	w := request(h, "/v1/chat/completions", "", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	require.Contains(t, w.Body.String(), "[DONE]")
	metrics := request(h, "/metrics", "", "").Body.String()
	require.Contains(t, metrics, `gateway_tokens_total{kind="completion",model="m",provider="compatible"} 8`)
	require.Contains(t, metrics, `gateway_tokens_total{kind="prompt",model="m",provider="compatible"} 2`)
}
