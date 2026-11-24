package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/stretchr/testify/require"
)

func TestQuotaWindowAndRetryAfter(t *testing.T) {
	const key = "example-key-long-enough-for-tests"
	h := NewHandler(providers.NewMock(providers.MockOptions{}), nil, nil, nil)
	h.Configure(Options{Tenants: []Tenant{{Name: "one", Key: key, RPM: 1, Concurrency: 1}}})
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	h.access.now = func() time.Time { return now }
	routes := h.Routes()
	get := func(path, scheme string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", scheme+" "+key)
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, r)
		return w
	}
	require.Equal(t, 200, get("/v1/usage", "bearer").Code)
	now = now.Add(30 * time.Second)
	require.Equal(t, 200, get("/v1/models", "bEaReR").Code)
	now = now.Add(59*time.Second + 100*time.Millisecond)
	limited := get("/v1/models", "Bearer")
	require.Equal(t, 429, limited.Code)
	require.Equal(t, "1", limited.Header().Get("Retry-After"))
	now = now.Add(time.Second)
	var usage TenantUsage
	require.NoError(t, json.Unmarshal(get("/v1/usage", "Bearer").Body.Bytes(), &usage))
	require.Equal(t, 1, usage.Limits.RequestsRemaining)
	require.EqualValues(t, 1, usage.Totals.Requests)
	require.Equal(t, 200, get("/v1/models", "Bearer").Code)
}
