package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/EshkirenokCode/llm-gateway/internal/proxy"
	"github.com/EshkirenokCode/llm-gateway/internal/telemetry"
)

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestLivenessAlwaysOK(t *testing.T) {
	t.Parallel()

	failing := map[string]proxy.ReadinessCheck{
		"database": func(context.Context) error { return errors.New("connection refused") },
	}
	h := proxy.NewHandler(providers.NewMock(providers.MockOptions{}), discardLogger(t),
		telemetry.NewMetrics(), proxy.NewHealth("test", failing)).Routes()

	rec := get(t, h, "/healthz")

	require.Equal(t, http.StatusOK, rec.Code, "liveness must not depend on external systems")
}

func TestReadinessReflectsChecks(t *testing.T) {
	t.Parallel()

	checks := map[string]proxy.ReadinessCheck{
		"database": func(context.Context) error { return errors.New("connection refused") },
		"cache":    func(context.Context) error { return nil },
	}
	h := proxy.NewHandler(providers.NewMock(providers.MockOptions{}), discardLogger(t),
		telemetry.NewMetrics(), proxy.NewHealth("test", checks)).Routes()

	rec := get(t, h, "/readyz")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "degraded", body.Status)
	require.Equal(t, "ok", body.Checks["cache"])
	require.Contains(t, body.Checks["database"], "connection refused")
}

func TestReadinessFailsWhileShuttingDown(t *testing.T) {
	t.Parallel()

	health := proxy.NewHealth("test", nil)
	h := proxy.NewHandler(providers.NewMock(providers.MockOptions{}), discardLogger(t),
		telemetry.NewMetrics(), health).Routes()

	require.Equal(t, http.StatusOK, get(t, h, "/readyz").Code)

	health.BeginShutdown()

	rec := get(t, h, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "shutting_down")
}

func TestMetricsExposeGatewayCollectors(t *testing.T) {
	t.Parallel()

	metrics := telemetry.NewMetrics()
	h := proxy.NewHandler(providers.NewMock(providers.MockOptions{Reply: "hi there"}), discardLogger(t),
		metrics, proxy.NewHealth("test", nil)).Routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := get(t, h, "/metrics").Body.String()

	require.Contains(t, body, `gateway_http_requests_total{method="POST",route="/v1/chat/completions",status="200"} 1`)
	require.Contains(t, body, "gateway_upstream_duration_seconds")
	require.Contains(t, body, `gateway_tokens_total{kind="completion",model="gpt-4o-mini",provider="mock"}`)
	require.Contains(t, body, "gateway_overhead_seconds")
	require.NotContains(t, body, `route="/metrics"`, "probes and metrics must not be instrumented")
}
