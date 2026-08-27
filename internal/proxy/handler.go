// Package proxy exposes the OpenAI-compatible HTTP surface of the gateway.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/EshkirenokCode/llm-gateway/internal/auth"
	"github.com/EshkirenokCode/llm-gateway/internal/billing"
	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/EshkirenokCode/llm-gateway/internal/storage"
	"github.com/EshkirenokCode/llm-gateway/internal/telemetry"
)

// maxRequestBody caps the request body. Prompts are text, so a megabyte is
// generous; without a cap a single client could exhaust the server's memory.
const maxRequestBody = 1 << 20

// Store is the persistence the handler needs. It is an interface so the HTTP
// layer can be tested without Postgres, and so the gateway can run without a
// database at all.
type Store interface {
	RecordRequest(ctx context.Context, rec storage.RequestRecord) (uuid.UUID, error)
	FindIdempotent(ctx context.Context, tenantID uuid.UUID, key string) (*storage.StoredResponse, error)
	SaveIdempotent(ctx context.Context, tenantID uuid.UUID, key string, requestHash []byte, status int, body []byte, requestID uuid.UUID, ttl time.Duration) error
	Audit(ctx context.Context, tenantID, keyID uuid.UUID, action string, detail map[string]any) error
}

// Options carries the handler dependencies. Everything except Provider and
// Logger is optional: with no store and no authenticator the gateway runs as
// the open, in-memory proxy of phase one.
type Options struct {
	Provider       providers.Provider
	Logger         *slog.Logger
	Metrics        *telemetry.Metrics
	Health         *Health
	Auth           *auth.Authenticator
	Limiter        *billing.RateLimiter
	Budget         *billing.Budget
	Pricing        *billing.Pricing
	Store          Store
	IdempotencyTTL time.Duration
}

// Handler serves chat completion requests by forwarding them to a provider.
type Handler struct {
	provider       providers.Provider
	logger         *slog.Logger
	metrics        *telemetry.Metrics
	health         *Health
	auth           *auth.Authenticator
	limiter        *billing.RateLimiter
	budget         *billing.Budget
	pricing        *billing.Pricing
	store          Store
	idempotencyTTL time.Duration
}

// NewHandler returns a Handler with the given dependencies.
func NewHandler(opts Options) *Handler {
	if opts.Metrics == nil {
		opts.Metrics = telemetry.NewMetrics()
	}
	if opts.Health == nil {
		opts.Health = NewHealth("dev", nil)
	}
	if opts.Pricing == nil {
		opts.Pricing = billing.DefaultPricing()
	}
	if opts.IdempotencyTTL <= 0 {
		opts.IdempotencyTTL = 24 * time.Hour
	}

	return &Handler{
		provider:       opts.Provider,
		logger:         opts.Logger,
		metrics:        opts.Metrics,
		health:         opts.Health,
		auth:           opts.Auth,
		limiter:        opts.Limiter,
		budget:         opts.Budget,
		pricing:        opts.Pricing,
		store:          opts.Store,
		idempotencyTTL: opts.IdempotencyTTL,
	}
}

// Routes returns the router with the gateway endpoints mounted. Probes and
// metrics are not instrumented: they would drown the real traffic in noise.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()

	r.Group(func(r chi.Router) {
		r.Use(h.metrics.Middleware("/v1/chat/completions"))
		if h.auth != nil {
			r.Use(auth.Middleware(h.auth, h.logger, writeError))
		}
		r.Use(h.rateLimit)
		r.Post("/v1/chat/completions", h.chatCompletions)
	})

	r.Get("/healthz", h.health.Live)
	r.Get("/readyz", h.health.Ready)
	r.Handle("/metrics", promhttp.HandlerFor(h.metrics.Registry(), promhttp.HandlerOpts{}))

	return r
}

// rateLimit rejects requests that exceed the allowance of the calling key.
func (h *Handler) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok || h.limiter == nil {
			next.ServeHTTP(w, r)
			return
		}

		allowed, retryAfter := h.limiter.Allow(identity.KeyID, identity.RateLimitRPM)
		if !allowed {
			seconds := int(retryAfter.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			h.metrics.CountRateLimited(identity.TenantPrefix)
			writeError(w, http.StatusTooManyRequests, "rate_limit_error",
				fmt.Sprintf("rate limit of %d requests per minute exceeded", identity.RateLimitRPM))
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	req, err := decodeChatRequest(body, r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	identity, hasIdentity := auth.FromContext(r.Context())

	status, err := h.checkBudget(r.Context(), w, identity, hasIdentity)
	if err != nil {
		return
	}

	if req.Stream {
		h.streamCompletions(w, r, req, identity, hasIdentity)
		return
	}

	idempotencyKey := r.Header.Get("Idempotency-Key")
	requestHash := sha256.Sum256(body)

	if replayed := h.replay(r.Context(), w, identity, hasIdentity, idempotencyKey, requestHash[:]); replayed {
		return
	}

	start := time.Now()
	resp, upstreamErr := h.provider.Chat(r.Context(), req)
	upstream := time.Since(start)

	if upstreamErr != nil {
		// A cancelled context means the client hung up; nobody is left to read
		// a response, so only record it.
		if r.Context().Err() != nil {
			h.logger.Info("client disconnected", "path", r.URL.Path)
			return
		}

		h.metrics.ObserveUpstream(h.provider.Name(), "error", upstream)
		h.logger.Error("upstream call failed", "provider", h.provider.Name(), "error", upstreamErr)
		h.record(r.Context(), identity, hasIdentity, req.Model, false, http.StatusBadGateway,
			providers.Usage{}, 0, upstream, upstreamErr.Error())
		writeError(w, http.StatusBadGateway, "api_error", "upstream provider failed")
		return
	}

	cost := h.pricing.Cost(resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

	h.metrics.ObserveUpstream(h.provider.Name(), "success", upstream)
	h.metrics.ObserveTokens(h.provider.Name(), resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	h.metrics.ObserveCost(h.provider.Name(), resp.Model, cost.Dollars())
	h.metrics.ObserveOverhead(time.Since(start), upstream)

	payload, err := json.Marshal(resp)
	if err != nil {
		h.logger.Error("encode response", "error", err)
		writeError(w, http.StatusInternalServerError, "api_error", "could not encode response")
		return
	}

	requestID := h.record(r.Context(), identity, hasIdentity, resp.Model, false, http.StatusOK,
		resp.Usage, cost, upstream, "")

	if hasIdentity && h.budget != nil {
		h.budget.Record(identity.TenantID, cost)
	}

	if idempotencyKey != "" && hasIdentity && h.store != nil {
		if err := h.store.SaveIdempotent(r.Context(), identity.TenantID, idempotencyKey, requestHash[:],
			http.StatusOK, payload, requestID, h.idempotencyTTL); err != nil {
			// The answer is already correct; failing to remember it only means
			// a retry would be charged twice.
			h.logger.Warn("could not store idempotent response", "error", err)
		}
	}

	h.writeUsageHeaders(w, resp.Usage, cost, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(payload); err != nil {
		h.logger.Info("response not fully written", "error", err)
	}
}

// checkBudget stops the request before any money is spent. It returns the
// status so the caller can add warning headers.
func (h *Handler) checkBudget(ctx context.Context, w http.ResponseWriter, identity auth.Identity, hasIdentity bool) (billing.Status, error) {
	if !hasIdentity || h.budget == nil {
		return billing.Status{}, nil
	}

	status, err := h.budget.Check(ctx, identity.TenantID, billing.Micros(identity.BudgetMicros),
		identity.HasBudget, identity.SoftLimitRatio)
	if err == nil {
		return status, nil
	}

	if errors.Is(err, billing.ErrBudgetExceeded) {
		h.metrics.CountBudgetBlocked(identity.TenantPrefix)
		h.logger.Info("request blocked by budget", "tenant", identity.TenantPrefix,
			"spent_micros", int64(status.Spent), "limit_micros", identity.BudgetMicros)
		w.Header().Set("X-Gateway-Budget-Limit-Usd", formatUSD(status.Limit))
		w.Header().Set("X-Gateway-Budget-Spent-Usd", formatUSD(status.Spent))
		writeError(w, http.StatusPaymentRequired, "insufficient_quota",
			"monthly budget exhausted for this tenant")
		return status, err
	}

	h.logger.Error("budget check failed", "error", err)
	writeError(w, http.StatusServiceUnavailable, "api_error", "cannot verify budget right now")

	return status, err
}

// replay answers from a stored response when the same Idempotency-Key is seen
// again. It reports whether the request has been answered.
func (h *Handler) replay(ctx context.Context, w http.ResponseWriter, identity auth.Identity, hasIdentity bool, key string, requestHash []byte) bool {
	if key == "" || !hasIdentity || h.store == nil {
		return false
	}

	stored, err := h.store.FindIdempotent(ctx, identity.TenantID, key)
	if err != nil {
		// Treat a lookup failure as a cache miss: at worst the upstream call
		// happens twice, which beats failing a valid request.
		h.logger.Warn("idempotency lookup failed", "error", err)
		return false
	}
	if stored == nil {
		return false
	}

	if !auth.EqualHash(stored.RequestHash, requestHash) {
		writeError(w, http.StatusConflict, "invalid_request_error",
			"this Idempotency-Key was used with a different request body")
		return true
	}

	h.metrics.CountIdempotentReplay(identity.TenantPrefix)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(stored.StatusCode)
	if _, err := w.Write(stored.Body); err != nil {
		h.logger.Info("replayed response not fully written", "error", err)
	}

	return true
}

func (h *Handler) record(ctx context.Context, identity auth.Identity, hasIdentity bool, model string, streamed bool, status int, usage providers.Usage, cost billing.Micros, upstream time.Duration, errMsg string) uuid.UUID {
	if !hasIdentity || h.store == nil {
		return uuid.Nil
	}

	// The client context may already be cancelled, and the record still has to
	// be written, so this deliberately does not inherit cancellation.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	id, err := h.store.RecordRequest(writeCtx, storage.RequestRecord{
		TenantID:         identity.TenantID,
		APIKeyID:         identity.KeyID,
		Provider:         h.provider.Name(),
		Model:            model,
		Streamed:         streamed,
		StatusCode:       status,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		Cost:             cost,
		Duration:         upstream,
		Err:              errMsg,
	})
	if err != nil {
		// Losing an accounting row is bad, but failing the request because of
		// it is worse. It is logged loudly instead.
		h.logger.Error("could not record request", "error", err, "tenant", identity.TenantPrefix)
		return uuid.Nil
	}

	return id
}

func (h *Handler) writeUsageHeaders(w http.ResponseWriter, usage providers.Usage, cost billing.Micros, status billing.Status) {
	w.Header().Set("X-Gateway-Provider", h.provider.Name())
	w.Header().Set("X-Gateway-Tokens-Total", strconv.Itoa(usage.TotalTokens))
	w.Header().Set("X-Gateway-Cost-Usd", formatUSD(cost))

	if status.HasLimit {
		w.Header().Set("X-Gateway-Budget-Remaining-Usd", formatUSD(status.Remaining()))
		if status.OverSoftLimit {
			w.Header().Set("X-Gateway-Budget-Warning",
				fmt.Sprintf("spent %s of %s", formatUSD(status.Spent), formatUSD(status.Limit)))
		}
	}
}

func formatUSD(m billing.Micros) string {
	return strconv.FormatFloat(m.Dollars(), 'f', 6, 64)
}

func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxRequestBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("request body exceeds %d bytes", maxRequestBody)
		}
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return body, nil
}

func decodeChatRequest(body []byte, contentType string) (providers.ChatRequest, error) {
	if contentType != "" && !isJSON(contentType) {
		return providers.ChatRequest{}, fmt.Errorf("content-type must be application/json")
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var req providers.ChatRequest
	if err := dec.Decode(&req); err != nil {
		return providers.ChatRequest{}, fmt.Errorf("malformed request body: %w", err)
	}

	// A second value means the client sent more than one JSON document.
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return providers.ChatRequest{}, fmt.Errorf("request body must contain a single JSON object")
	}

	return req, validate(req)
}

func validate(req providers.ChatRequest) error {
	if req.Model == "" {
		return fmt.Errorf("model is required")
	}
	if len(req.Messages) == 0 {
		return fmt.Errorf("messages must not be empty")
	}
	for i, msg := range req.Messages {
		switch msg.Role {
		case "system", "user", "assistant":
		default:
			return fmt.Errorf("messages[%d].role must be system, user or assistant", i)
		}
	}
	if req.MaxTokens < 0 {
		return fmt.Errorf("max_tokens must not be negative")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	return nil
}

func isJSON(contentType string) bool {
	for i := 0; i < len(contentType); i++ {
		if contentType[i] == ';' {
			contentType = contentType[:i]
			break
		}
	}
	return contentType == "application/json"
}
