package proxy

import (
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
	// Without a flusher the response is buffered and the client sees nothing
	// until the whole answer is ready, which defeats streaming.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "api_error", "streaming is not supported by this server")
		return
	}

	ctx := r.Context()

	stream, err := h.provider.ChatStream(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		h.metrics.ObserveStream(h.provider.Name(), "start_failed")
		h.logger.Error("upstream stream failed to start", "provider", h.provider.Name(), "error", err)
		writeError(w, http.StatusBadGateway, "api_error", "upstream provider failed")
		return
	}
	defer stream.Close()

	// Headers must go out before the first chunk. After this point the status
	// code is fixed at 200, so later failures can only end the stream.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	start := time.Now()
	first := true
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				h.logger.Info("client disconnected mid-stream", "provider", h.provider.Name())
				h.metrics.ObserveStream(h.provider.Name(), "client_gone")
				return
			}
			h.logger.Error("stream aborted", "provider", h.provider.Name(), "error", err)
			h.metrics.ObserveStream(h.provider.Name(), "error")
			return
		}

		event := streamChunk{
			ID:      stream.ID(),
			Object:  "chat.completion.chunk",
			Created: stream.Created(),
			Model:   stream.Model(),
			Choices: []streamPart{{Delta: streamDelta{Content: chunk.Delta}}},
		}

		// OpenAI announces the assistant role once, on the first chunk.
		if first {
			event.Choices[0].Delta.Role = "assistant"
			first = false
		}

		if chunk.FinishReason != "" {
			reason := chunk.FinishReason
			event.Choices[0].FinishReason = &reason
		}

		if chunk.Usage != nil {
			h.metrics.ObserveTokens(h.provider.Name(), stream.Model(), chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens)
			event.Usage = &streamUsage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
		}

		if err := writeEvent(w, event); err != nil {
			h.logger.Info("stopped writing to client", "error", err)
			h.metrics.ObserveStream(h.provider.Name(), "write_failed")
			return
		}
		flusher.Flush()
	}

	h.metrics.ObserveUpstream(h.provider.Name(), "success", time.Since(start))
	h.metrics.ObserveStream(h.provider.Name(), "completed")

	// The sentinel tells OpenAI clients the stream ended normally.
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	flusher.Flush()
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
