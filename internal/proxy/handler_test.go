package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/EshkirenokCode/llm-gateway/internal/proxy"
)

func discardLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testHandler(t *testing.T, p providers.Provider) http.Handler {
	t.Helper()
	return proxy.NewHandler(p, discardLogger(t), nil, nil).Routes()
}

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChatCompletionsSuccess(t *testing.T) {
	t.Parallel()

	h := testHandler(t, providers.NewMock(providers.MockOptions{Reply: "pong"}))

	rec := post(t, h, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var resp providers.ChatResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "chat.completion", resp.Object)
	require.Equal(t, "gpt-4o-mini", resp.Model)
	require.Len(t, resp.Choices, 1)
	require.Equal(t, "pong", resp.Choices[0].Message.Content)
	require.Equal(t, "assistant", resp.Choices[0].Message.Role)
	require.Positive(t, resp.Usage.TotalTokens)
	require.Equal(t, resp.Usage.PromptTokens+resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
}

func TestChatCompletionsRejectsBadRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing model",
			body: `{"messages":[{"role":"user","content":"hi"}]}`,
			want: "model is required",
		},
		{
			name: "no messages",
			body: `{"model":"gpt-4o-mini","messages":[]}`,
			want: "messages must not be empty",
		},
		{
			name: "unknown role",
			body: `{"model":"gpt-4o-mini","messages":[{"role":"root","content":"hi"}]}`,
			want: "role must be",
		},
		{
			name: "temperature out of range",
			body: `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"temperature":9}`,
			want: "temperature must be",
		},
		{
			name: "unknown field",
			body: `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"tempreature":1}`,
			want: "malformed request body",
		},
		{
			name: "not json",
			body: `hello`,
			want: "malformed request body",
		},
		{
			name: "trailing garbage",
			body: `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}{}`,
			want: "single JSON object",
		},
	}

	h := testHandler(t, providers.NewMock(providers.MockOptions{}))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := post(t, h, tt.body)

			require.Equal(t, http.StatusBadRequest, rec.Code)

			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Contains(t, body.Error.Message, tt.want)
			require.Equal(t, "invalid_request_error", body.Error.Type)
		})
	}
}

func TestChatCompletionsUpstreamFailure(t *testing.T) {
	t.Parallel()

	h := testHandler(t, providers.NewMock(providers.MockOptions{Err: errors.New("boom")}))

	rec := post(t, h, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.NotContains(t, rec.Body.String(), "boom", "upstream details must not leak to the client")
}

func TestChatCompletionsClientDisconnect(t *testing.T) {
	t.Parallel()

	h := testHandler(t, providers.NewMock(providers.MockOptions{Latency: time.Second}))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after the client disconnected")
	}

	require.Empty(t, rec.Body.String())
}

func TestUnknownRoute(t *testing.T) {
	t.Parallel()

	h := testHandler(t, providers.NewMock(providers.MockOptions{}))

	req := httptest.NewRequest(http.MethodGet, "/v1/missing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}
