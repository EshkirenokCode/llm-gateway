// Package proxy exposes the OpenAI-compatible HTTP surface of the gateway.
package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/EshkirenokCode/llm-gateway/internal/telemetry"
)

// maxRequestBody caps the request body. Prompts are text, so a megabyte is
// generous; without a cap a single client could exhaust the server's memory.
const maxRequestBody = 1 << 20

// Handler serves chat completion requests by forwarding them to a provider.
type Handler struct {
	provider providers.Provider
	logger   *slog.Logger
	metrics  *telemetry.Metrics
	health   *Health
}

// NewHandler returns a Handler backed by the given provider.
func NewHandler(provider providers.Provider, logger *slog.Logger, metrics *telemetry.Metrics, health *Health) *Handler {
	if metrics == nil {
		metrics = telemetry.NewMetrics()
	}
	if health == nil {
		health = NewHealth("dev", nil)
	}
	return &Handler{provider: provider, logger: logger, metrics: metrics, health: health}
}

// Routes returns the router with the gateway's endpoints mounted. Probes and
// metrics are not instrumented: they would drown the real traffic in noise.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()

	r.Group(func(r chi.Router) {
		r.Use(h.metrics.Middleware("/v1/chat/completions"))
		r.Post("/v1/chat/completions", h.chatCompletions)
	})

	r.Get("/healthz", h.health.Live)
	r.Get("/readyz", h.health.Ready)
	r.Handle("/metrics", promhttp.HandlerFor(h.metrics.Registry(), promhttp.HandlerOpts{}))

	return r
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	req, err := decodeChatRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if req.Stream {
		h.streamCompletions(w, r, req)
		return
	}

	start := time.Now()
	resp, err := h.provider.Chat(r.Context(), req)
	upstream := time.Since(start)
	if err != nil {
		h.metrics.ObserveUpstream(h.provider.Name(), "error", upstream)
		// A cancelled context means the client hung up; nobody is left to read
		// a response, so only record it.
		if errors.Is(err, r.Context().Err()) && r.Context().Err() != nil {
			h.logger.Info("client disconnected", "path", r.URL.Path)
			return
		}

		h.logger.Error("upstream call failed", "provider", h.provider.Name(), "error", err)
		writeError(w, http.StatusBadGateway, "api_error", "upstream provider failed")
		return
	}

	h.metrics.ObserveUpstream(h.provider.Name(), "success", upstream)
	h.metrics.ObserveTokens(h.provider.Name(), resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	h.metrics.ObserveOverhead(time.Since(start), upstream)

	writeJSON(w, http.StatusOK, resp)
}

func decodeChatRequest(r *http.Request) (providers.ChatRequest, error) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !isJSON(ct) {
		return providers.ChatRequest{}, fmt.Errorf("content-type must be application/json")
	}

	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()

	var req providers.ChatRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return providers.ChatRequest{}, fmt.Errorf("request body exceeds %d bytes", maxRequestBody)
		}
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
