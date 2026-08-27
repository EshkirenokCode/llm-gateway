// Package proxy exposes the OpenAI-compatible HTTP surface of the gateway.
package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
)

// maxRequestBody caps the request body. Prompts are text, so a megabyte is
// generous; without a cap a single client could exhaust the server's memory.
const maxRequestBody = 1 << 20

// Handler serves chat completion requests by forwarding them to a provider.
type Handler struct {
	provider providers.Provider
	logger   *slog.Logger
}

// NewHandler returns a Handler backed by the given provider.
func NewHandler(provider providers.Provider, logger *slog.Logger) *Handler {
	return &Handler{provider: provider, logger: logger}
}

// Routes returns the router with the gateway's public endpoints mounted.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/chat/completions", h.chatCompletions)
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

	resp, err := h.provider.Chat(r.Context(), req)
	if err != nil {
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
