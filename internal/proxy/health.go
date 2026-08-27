package proxy

import (
	"context"
	"net/http"
	"sync/atomic"
)

// ReadinessCheck reports whether a dependency is usable. Checks must be cheap
// and must respect ctx: /readyz is polled continuously by orchestrators.
type ReadinessCheck func(ctx context.Context) error

// Health answers the liveness and readiness probes.
type Health struct {
	version string
	checks  map[string]ReadinessCheck
	// shuttingDown flips before the server stops accepting connections so load
	// balancers can drain this instance first.
	shuttingDown atomic.Bool
}

// NewHealth returns a Health with the given readiness checks.
func NewHealth(version string, checks map[string]ReadinessCheck) *Health {
	return &Health{version: version, checks: checks}
}

// BeginShutdown marks the instance as draining; readiness starts failing.
func (h *Health) BeginShutdown() { h.shuttingDown.Store(true) }

// Live reports that the process is running. It deliberately checks nothing
// else: a failing dependency must not cause the container to be restarted.
func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
}

// Ready reports whether the instance should receive traffic.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	if h.shuttingDown.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
		return
	}

	results := make(map[string]string, len(h.checks))
	ok := true

	for name, check := range h.checks {
		if err := check(r.Context()); err != nil {
			results[name] = err.Error()
			ok = false
			continue
		}
		results[name] = "ok"
	}

	status := http.StatusOK
	state := "ok"
	if !ok {
		status = http.StatusServiceUnavailable
		state = "degraded"
	}

	writeJSON(w, status, map[string]any{"status": state, "checks": results})
}
