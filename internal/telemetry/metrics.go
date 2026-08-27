// Package telemetry wires logging and Prometheus metrics into the HTTP stack.
package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the collectors owned by the gateway. They are registered on a
// private registry so tests can build an isolated instance.
type Metrics struct {
	registry *prometheus.Registry

	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
	upstream *prometheus.HistogramVec
	overhead prometheus.Histogram
	tokens   *prometheus.CounterVec
	streamed *prometheus.CounterVec
}

// NewMetrics creates the collectors and registers them.
func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_http_requests_total",
			Help: "HTTP requests handled, by route, method and status class.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_http_request_duration_seconds",
			Help:    "End to end request duration as seen by the client.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"route", "method"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_http_requests_in_flight",
			Help: "Requests currently being served.",
		}),
		upstream: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_upstream_duration_seconds",
			Help:    "Time spent waiting for a provider.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"provider", "outcome"}),
		overhead: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "gateway_overhead_seconds",
			Help: "Request duration minus upstream duration: the cost of the gateway itself.",
			// Overhead should stay well under a millisecond, so the buckets are
			// much finer than for the end to end histogram.
			Buckets: []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.05, 0.1},
		}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tokens_total",
			Help: "Tokens accounted for, by provider, model and kind.",
		}, []string{"provider", "model", "kind"}),
		streamed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_streams_total",
			Help: "Streamed responses, by provider and how the stream ended.",
		}, []string{"provider", "outcome"}),
	}

	m.registry.MustRegister(m.requests, m.duration, m.inFlight, m.upstream, m.overhead, m.tokens, m.streamed)

	return m
}

// Registry exposes the collectors for the /metrics handler.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ObserveUpstream records how long a provider call took and whether it worked.
func (m *Metrics) ObserveUpstream(provider, outcome string, d time.Duration) {
	m.upstream.WithLabelValues(provider, outcome).Observe(d.Seconds())
}

// ObserveTokens records token counts for a completed request.
func (m *Metrics) ObserveTokens(provider, model string, prompt, completion int) {
	m.tokens.WithLabelValues(provider, model, "prompt").Add(float64(prompt))
	m.tokens.WithLabelValues(provider, model, "completion").Add(float64(completion))
}

// ObserveStream records the outcome of a streamed response.
func (m *Metrics) ObserveStream(provider, outcome string) {
	m.streamed.WithLabelValues(provider, outcome).Inc()
}

// ObserveOverhead records the share of a request that was spent inside the
// gateway rather than waiting for the provider.
func (m *Metrics) ObserveOverhead(total, upstream time.Duration) {
	d := total - upstream
	if d < 0 {
		d = 0
	}
	m.overhead.Observe(d.Seconds())
}

// statusRecorder captures the status code, which http.ResponseWriter does not
// expose after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer, keeping
// Flush available for streaming responses.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware records metrics for every request. The route label comes from the
// caller so that path parameters never explode the label cardinality.
func (m *Metrics) Middleware(route string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.inFlight.Inc()
			defer m.inFlight.Dec()

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()

			next.ServeHTTP(rec, r)

			elapsed := time.Since(start)
			m.requests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
			m.duration.WithLabelValues(route, r.Method).Observe(elapsed.Seconds())
		})
	}
}
