package proxy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
)

// sseEvents reads a server-sent event body and returns the payload of every
// data: line.
func sseEvents(t *testing.T, body string) []string {
	t.Helper()

	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	require.NoError(t, sc.Err())
	return out
}

func TestStreamSendsChunksAndDone(t *testing.T) {
	t.Parallel()

	h := testHandler(t, providers.NewMock(providers.MockOptions{Reply: "one two three"}))

	rec := post(t, h, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))

	events := sseEvents(t, rec.Body.String())
	require.Equal(t, "[DONE]", events[len(events)-1])

	var text strings.Builder
	var finish string
	var usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}

	for i, raw := range events[:len(events)-1] {
		var chunk struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &chunk))
		require.Equal(t, "chat.completion.chunk", chunk.Object)
		require.Equal(t, "gpt-4o-mini", chunk.Model)
		require.NotEmpty(t, chunk.ID)
		if chunk.Usage != nil {
			usage = chunk.Usage
			require.Empty(t, chunk.Choices)
			continue
		}
		require.Len(t, chunk.Choices, 1)

		if i == 0 {
			require.Equal(t, "assistant", chunk.Choices[0].Delta.Role, "role is announced once, on the first chunk")
		} else {
			require.Empty(t, chunk.Choices[0].Delta.Role)
		}

		text.WriteString(chunk.Choices[0].Delta.Content)

		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}

	require.Equal(t, "one two three", text.String())
	require.Equal(t, "stop", finish)
	require.NotNil(t, usage, "the final chunk carries token counts")
	require.Equal(t, usage.PromptTokens+usage.CompletionTokens, usage.TotalTokens)
}

func TestStreamUpstreamFailsBeforeFirstChunk(t *testing.T) {
	t.Parallel()

	h := testHandler(t, providers.NewMock(providers.MockOptions{Err: errors.New("boom")}))

	rec := post(t, h, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.NotContains(t, rec.Body.String(), "boom")
}

func TestStreamStopsWhenClientDisconnects(t *testing.T) {
	t.Parallel()

	h := testHandler(t, providers.NewMock(providers.MockOptions{
		Reply:   "this answer is long enough to span several chunks",
		Latency: 50 * time.Millisecond,
	}))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Let a couple of chunks through, then hang up.
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler kept streaming after the client disconnected")
	}

	events := sseEvents(t, rec.Body.String())
	require.NotEmpty(t, events, "some chunks were delivered before the disconnect")
	require.NotContains(t, events, "[DONE]", "an aborted stream must not be terminated normally")
	require.Less(t, len(events), 9, "streaming must stop early, not run to completion")
}
