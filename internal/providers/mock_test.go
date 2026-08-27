package providers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/providers"
)

func TestMockEchoesLastUserMessage(t *testing.T) {
	t.Parallel()

	m := providers.NewMock(providers.MockOptions{})

	resp, err := m.Chat(context.Background(), providers.ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []providers.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "second"},
		},
	})

	require.NoError(t, err)
	require.Contains(t, resp.Choices[0].Message.Content, "second")
	require.Equal(t, "stop", resp.Choices[0].FinishReason)
	require.Equal(t, "mock", m.Name())
}

func TestMockReturnsConfiguredError(t *testing.T) {
	t.Parallel()

	want := errors.New("upstream is down")
	m := providers.NewMock(providers.MockOptions{Err: want})

	_, err := m.Chat(context.Background(), providers.ChatRequest{Model: "m"})

	require.ErrorIs(t, err, want)
}

func TestMockHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	m := providers.NewMock(providers.MockOptions{Latency: time.Minute})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := m.Chat(ctx, providers.ChatRequest{Model: "m"})

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), time.Second, "cancellation must not wait out the latency")
}

func TestMockCountsTokens(t *testing.T) {
	t.Parallel()

	m := providers.NewMock(providers.MockOptions{Reply: "four token reply here"})

	resp, err := m.Chat(context.Background(), providers.ChatRequest{
		Model:    "m",
		Messages: []providers.Message{{Role: "user", Content: "one two three"}},
	})

	require.NoError(t, err)
	require.Equal(t, 3, resp.Usage.PromptTokens)
	require.Equal(t, 5, resp.Usage.CompletionTokens)
	require.Equal(t, 8, resp.Usage.TotalTokens)
}
