package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
)

// streamChunk is the wire format of a single server-sent event, matching the
// OpenAI streaming response.
type streamChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []streamPart `json:"choices"`
	Usage   *streamUsage `json:"usage,omitempty"`
}

type streamPart struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type streamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (h *Handler) streamCompletions(w http.ResponseWriter, r *http.Request, req providers.ChatRequest) {
	ctx := r.Context()
	start := time.Now()
	outcome := "start_failed"
	defer func() {
		h.metrics.ObserveStream(h.provider.Name(), outcome)
		upstreamOutcome := "error"
		if outcome == "completed" {
			upstreamOutcome = "success"
		}
		h.metrics.ObserveUpstream(h.provider.Name(), upstreamOutcome, time.Since(start))
	}()
	stream, err := h.provider.ChatStream(ctx, req)
	if err != nil {
		h.providerError(w, r, err)
		return
	}
	defer stream.Close()
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	outcome = "write_failed"
	if controller.Flush() != nil {
		return
	}
	send := func(event streamChunk) error {
		if err := writeEvent(w, event); err != nil {
			return err
		}
		return controller.Flush()
	}
	fail := func(kind, message string) {
		payload, _ := json.Marshal(errorEnvelope{Error: errorBody{Type: kind, Message: message}})
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err == nil {
			_ = controller.Flush()
		}
	}
	first := true
	var usage *providers.Usage
	defer func() {
		if usage != nil {
			h.metrics.ObserveTokens(h.provider.Name(), req.Model, usage.PromptTokens, usage.CompletionTokens)
		}
	}()
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				outcome = "client_gone"
				return
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
				outcome = "timeout"
				fail("timeout_error", "provider deadline exceeded")
				return
			}
			outcome = "error"
			h.logger.Error("stream aborted", "provider", h.provider.Name(), "request_id", w.Header().Get("X-Request-ID"), "error", err)
			fail("api_error", "upstream stream interrupted")
			return
		}
		// Providers may repeat cumulative usage; account for the final snapshot once.
		if chunk.Usage != nil {
			snapshot := *chunk.Usage
			usage = &snapshot
		}
		event := streamChunk{ID: stream.ID(), Object: "chat.completion.chunk", Created: stream.Created(), Model: stream.Model(), Choices: []streamPart{{Delta: streamDelta{Content: chunk.Delta}}}}
		usageOnly := chunk.Delta == "" && chunk.FinishReason == "" && chunk.Usage != nil
		if !usageOnly {
			if first {
				event.Choices[0].Delta.Role = "assistant"
				first = false
			}
			if chunk.FinishReason != "" {
				reason := chunk.FinishReason
				event.Choices[0].FinishReason = &reason
			}
			if send(event) != nil {
				return
			}
		}
		if chunk.Usage != nil && req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
			event.Choices = []streamPart{}
			event.Usage = &streamUsage{PromptTokens: chunk.Usage.PromptTokens, CompletionTokens: chunk.Usage.CompletionTokens, TotalTokens: chunk.Usage.TotalTokens}
			if send(event) != nil {
				return
			}
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	if controller.Flush() != nil {
		return
	}
	outcome = "completed"
}

func writeEvent(w io.Writer, event streamChunk) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode chunk: %w", err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return fmt.Errorf("write chunk: %w", err)
	}
	return nil
}
