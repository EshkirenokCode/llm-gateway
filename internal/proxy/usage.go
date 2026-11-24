package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
)

type tenantContextKey struct{}

// TenantTotals contains process-local counters, not billing records.
type TenantTotals struct {
	Requests         int64 `json:"requests"`
	RateLimited      int64 `json:"rate_limited"`
	Completions      int64 `json:"completions"`
	Failed           int64 `json:"failed"`
	Canceled         int64 `json:"canceled"`
	UsageReported    int64 `json:"usage_reported"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type TenantUsage struct {
	Tenant string       `json:"tenant"`
	Since  time.Time    `json:"since"`
	Totals TenantTotals `json:"totals"`
	Limits TenantLimits `json:"limits"`
}

type TenantLimits struct {
	RequestsPerMinute int       `json:"requests_per_minute"`
	RequestsRemaining int       `json:"requests_remaining"`
	WindowResetsAt    time.Time `json:"window_resets_at"`
	Concurrency       int       `json:"concurrency"`
	InFlight          int       `json:"in_flight"`
}

// usage authenticates without consuming quota, so blocked clients can inspect it.
func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	setRequestHeaders(w)
	a := h.access
	if a == nil {
		writeError(w, http.StatusUnauthorized, "authentication_error", "valid tenant bearer token required")
		return
	}
	a.mu.Lock()
	t := a.authenticate(r)
	if t == nil {
		a.mu.Unlock()
		writeError(w, http.StatusUnauthorized, "authentication_error", "valid tenant bearer token required")
		return
	}
	now := a.now()
	requests := t.requests
	resetsAt := t.window.Add(time.Minute)
	if t.window.IsZero() || !now.Before(resetsAt) {
		requests = 0
		resetsAt = now.Add(time.Minute)
	}
	snapshot := TenantUsage{Tenant: t.config.Name, Since: t.since, Totals: t.totals, Limits: TenantLimits{
		RequestsPerMinute: t.config.RPM, RequestsRemaining: t.config.RPM - requests, WindowResetsAt: resetsAt, Concurrency: t.config.Concurrency, InFlight: t.active,
	}}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, snapshot)
}

func (h *Handler) recordTenantCompletion(r *http.Request, completed bool, usage *providers.Usage) {
	t, _ := r.Context().Value(tenantContextKey{}).(*tenantState)
	if t == nil {
		return
	}
	a := h.access
	a.mu.Lock()
	defer a.mu.Unlock()
	if completed {
		t.totals.Completions++
	} else if r.Context().Err() == context.Canceled {
		t.totals.Canceled++
	} else {
		t.totals.Failed++
	}
	if usage != nil {
		t.totals.UsageReported++
		t.totals.PromptTokens += int64(usage.PromptTokens)
		t.totals.CompletionTokens += int64(usage.CompletionTokens)
	}
}
