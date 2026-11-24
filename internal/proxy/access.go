package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EshkirenokCode/llm-gateway/internal/tenant"
)

// Tenant limits apply to this process and reset when it restarts.
type Tenant = tenant.Policy

type Options struct {
	Tenants    []Tenant
	Models     []string
	Timeout    time.Duration
	MetricsKey string
}
type tenantState struct {
	config           Tenant
	window           time.Time
	requests, active int
	since            time.Time
	totals           TenantTotals
}
type access struct {
	mu         sync.Mutex
	tenants    map[[32]byte]*tenantState
	models     []string
	timeout    time.Duration
	metricsKey string
	now        func() time.Time
}

func (h *Handler) Configure(opts Options) {
	a := &access{tenants: make(map[[32]byte]*tenantState), models: append([]string(nil), opts.Models...), timeout: opts.Timeout, metricsKey: opts.MetricsKey, now: time.Now}
	for _, t := range opts.Tenants {
		hash := sha256.Sum256([]byte(t.Key))
		t.Key = ""
		a.tenants[hash] = &tenantState{config: t, since: a.now().UTC()}
	}
	h.access = a
}
func (h *Handler) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setRequestHeaders(w)
		a := h.access
		if a == nil {
			next.ServeHTTP(w, r)
			return
		}
		if len(a.tenants) > 0 {
			a.mu.Lock()
			t := a.authenticate(r)
			if t == nil {
				a.mu.Unlock()
				writeError(w, 401, "authentication_error", "valid bearer token required")
				return
			}
			now := a.now()
			t.resetWindow(now)
			retryAfter := 0
			if t.requests >= t.config.RPM {
				retryAfter = max(1, int(math.Ceil(t.window.Add(time.Minute).Sub(now).Seconds())))
			} else if t.active >= t.config.Concurrency {
				retryAfter = 1
			}
			if retryAfter > 0 {
				t.totals.RateLimited++
				a.mu.Unlock()
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				writeError(w, http.StatusTooManyRequests, "rate_limit_error", "tenant request limit reached")
				return
			}
			t.requests++
			t.totals.Requests++
			t.active++
			a.mu.Unlock()
			defer func() { a.mu.Lock(); t.active--; a.mu.Unlock() }()
			r = r.WithContext(context.WithValue(r.Context(), tenantContextKey{}, t))
		}
		if a.timeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), a.timeout)
			defer cancel()
			// A context deadline alone cannot interrupt a blocked HTTP body read.
			readDeadline := time.Now().Add(15 * time.Second)
			if deadline, ok := ctx.Deadline(); ok && deadline.Before(readDeadline) {
				readDeadline = deadline
			}
			controller := http.NewResponseController(w)
			if controller.SetReadDeadline(readDeadline) == nil {
				defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
			}
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}
func (h *Handler) models(w http.ResponseWriter, _ *http.Request) {
	data := []map[string]any{}
	if h.access != nil {
		for _, model := range h.access.models {
			data = append(data, map[string]any{"id": model, "object": "model", "created": 0, "owned_by": h.provider.Name()})
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}
func (h *Handler) providerError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == context.Canceled {
		return
	}
	if errors.Is(r.Context().Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		writeError(w, 504, "timeout_error", "provider deadline exceeded")
		return
	}
	h.logger.Error("upstream failed", "provider", h.provider.Name(), "request_id", w.Header().Get("X-Request-ID"), "error", err)
	writeError(w, 502, "api_error", "upstream provider failed")
}
func (h *Handler) validateModel(model string) error {
	if h.access == nil || len(h.access.models) == 0 {
		return nil
	}
	for _, m := range h.access.models {
		if m == model {
			return nil
		}
	}
	return fmt.Errorf("model is not enabled on this gateway")
}

func (t *tenantState) resetWindow(now time.Time) {
	if t.window.IsZero() || !now.Before(t.window.Add(time.Minute)) {
		t.window = now
		t.requests = 0
	}
}

// The caller holds access.mu when reading tenant state.
func (a *access) authenticate(r *http.Request) *tenantState {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return nil
	}
	return a.tenants[sha256.Sum256([]byte(fields[1]))]
}

func setRequestHeaders(w http.ResponseWriter) {
	var id [12]byte
	_, _ = rand.Read(id[:])
	w.Header().Set("X-Request-ID", hex.EncodeToString(id[:]))
	w.Header().Set("Cache-Control", "no-store")
}
