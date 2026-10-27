package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMetricsRecordRejectedRequestsAndDurations(t *testing.T) {
	m := NewMetrics()
	h := m.Middleware("/v1/chat/completions")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	m.ObserveTokens("mock", "m", 2, 3)
	m.ObserveUpstream("mock", "success", time.Millisecond)
	m.ObserveOverhead(time.Millisecond, 2*time.Millisecond)
	m.ObserveStream("mock", "completed")
	families, err := m.Registry().Gather()
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
		metric := f.Metric[0]
		switch f.GetName() {
		case "gateway_http_requests_total":
			require.Equal(t, float64(1), metric.GetCounter().GetValue())
			require.Equal(t, "401", metric.Label[2].GetValue())
		case "gateway_http_requests_in_flight":
			require.Zero(t, metric.GetGauge().GetValue())
		case "gateway_http_request_duration_seconds":
			require.EqualValues(t, 1, metric.GetHistogram().GetSampleCount())
		case "gateway_overhead_seconds":
			require.Zero(t, metric.GetHistogram().GetSampleSum())
		}
	}
	for _, name := range []string{"gateway_tokens_total", "gateway_streams_total", "gateway_http_requests_total", "gateway_http_requests_in_flight", "gateway_upstream_duration_seconds"} {
		require.True(t, seen[name], name)
	}
}
func TestRecorderKeepsFirstStatusAndStreaming(t *testing.T) {
	underlying := httptest.NewRecorder()
	r := &statusRecorder{ResponseWriter: underlying, status: 200}
	r.WriteHeader(201)
	r.WriteHeader(503)
	_, err := r.Write([]byte("hello"))
	require.NoError(t, err)
	r.Flush()
	require.Equal(t, 201, r.status)
	require.Equal(t, 201, underlying.Code)
	require.True(t, underlying.Flushed)
	require.Equal(t, "hello", underlying.Body.String())
	require.Equal(t, underlying, r.Unwrap())
	implicit := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: 200}
	_, err = implicit.Write([]byte("ok"))
	require.NoError(t, err)
	require.True(t, implicit.wroteHeader)
}
