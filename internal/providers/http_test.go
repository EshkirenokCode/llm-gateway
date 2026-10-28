package providers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
	"github.com/stretchr/testify/require"
)

func TestHTTPChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer upstream-secret", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), `"max_tokens":12`)
		_, _ = io.WriteString(w, `{"id":"chat-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer srv.Close()
	p := providers.NewHTTP(srv.URL+"/v1/", "upstream-secret", time.Second)
	result, err := p.Chat(context.Background(), providers.ChatRequest{Model: "m", MaxTokens: 12})
	require.NoError(t, err)
	require.Equal(t, "hello", result.Choices[0].Message.Content)
}
func TestHTTPStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		require.Contains(t, string(body), `"include_usage":true`)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": heartbeat\r\n\r\ndata: {\"id\":\"s1\",\"model\":\"m\",\"created\":12,\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\r\n\r\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	s, err := providers.NewHTTP(srv.URL, "", time.Second).ChatStream(context.Background(), providers.ChatRequest{Model: "m"})
	require.NoError(t, err)
	defer s.Close()
	require.Equal(t, "s1", s.ID())
	require.EqualValues(t, 12, s.Created())
	c, err := s.Recv()
	require.NoError(t, err)
	require.Equal(t, "hello", c.Delta)
	c, err = s.Recv()
	require.NoError(t, err)
	require.Equal(t, 3, c.Usage.TotalTokens)
	_, err = s.Recv()
	require.ErrorIs(t, err, io.EOF)
}
func TestHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body, ct string
		stream   bool
	}{
		{"upstream rejected", 401, "secret-provider-diagnostic", "application/json", false},
		{"invalid JSON", 200, "not JSON", "application/json", false},
		{"missing choices", 200, `{"id":"x"}`, "application/json", false},
		{"wrong stream type", 200, "data: [DONE]\n\n", "application/json", true},
		{"broken event", 200, "data: nope\n\n", "text/event-stream", true},
		{"empty stream", 200, "", "text/event-stream", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := providers.NewHTTP(srv.URL, "", time.Second)
			var err error
			if tc.stream {
				_, err = p.ChatStream(context.Background(), providers.ChatRequest{})
			} else {
				_, err = p.Chat(context.Background(), providers.ChatRequest{})
			}
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-provider-diagnostic")
		})
	}
}
func TestHTTPStreamRejectsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"s1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer srv.Close()
	s, err := providers.NewHTTP(srv.URL, "", time.Second).ChatStream(context.Background(), providers.ChatRequest{})
	require.NoError(t, err)
	defer s.Close()
	_, err = s.Recv()
	require.NoError(t, err)
	_, err = s.Recv()
	require.ErrorContains(t, err, "without DONE")
}
func TestHTTPTimeoutAndRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "redirect") {
			http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer srv.Close()
	_, err := providers.NewHTTP(srv.URL, "", 20*time.Millisecond).Chat(context.Background(), providers.ChatRequest{})
	require.Error(t, err)
	_, err = providers.NewHTTP(srv.URL+"/redirect", "", time.Second).Chat(context.Background(), providers.ChatRequest{})
	var upstream *providers.UpstreamError
	require.ErrorAs(t, err, &upstream)
	require.Equal(t, 307, upstream.Status)
}

func TestMockHonorsOutputLimitAndPreservesStreamText(t *testing.T) {
	p := providers.NewMock(providers.MockOptions{Reply: "one  two\nthree four five six seven"})
	req := providers.ChatRequest{Model: "m", MaxTokens: 3}
	result, err := p.Chat(context.Background(), req)
	require.NoError(t, err)
	require.LessOrEqual(t, result.Usage.CompletionTokens, 3)
	require.Equal(t, "length", result.Choices[0].FinishReason)
	stream, err := p.ChatStream(context.Background(), req)
	require.NoError(t, err)
	defer stream.Close()
	var text strings.Builder
	var finish string
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		text.WriteString(c.Delta)
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}
	require.Equal(t, result.Choices[0].Message.Content, text.String())
	require.Equal(t, "length", finish)
}

func TestHTTPRejectsInvalidStreamEnvelopes(t *testing.T) {
	for _, tc := range []struct{ name, body, contentType string }{
		{"multiline event exceeds limit", strings.Repeat("data: "+strings.Repeat(" ", 1024)+"\n", 1025) + "\n", "text/event-stream"},
		{"null event", "data: null\n\n", "text/event-stream"},
		{"empty event", "data: {\"id\":\"x\"}\n\n", "text/event-stream"},
		{"negative total", "data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"total_tokens\":-1}}\n\n", "text/event-stream"},
		{"invalid MIME suffix", "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "text/event-stream-invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			stream, err := providers.NewHTTP(server.URL, "", time.Second).ChatStream(context.Background(), providers.ChatRequest{Model: "m"})
			if stream != nil {
				_ = stream.Close()
			}
			require.Error(t, err)
			if tc.name == "multiline event exceeds limit" {
				require.ErrorContains(t, err, "exceeds")
			}
		})
	}
}
func TestHTTPTimeoutRetainsClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer server.Close()
	_, err := providers.NewHTTP(server.URL, "", 20*time.Millisecond).Chat(context.Background(), providers.ChatRequest{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotContains(t, err.Error(), server.URL)
}
