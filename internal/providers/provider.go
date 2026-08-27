// Package providers defines the upstream model provider contract and its
// implementations.
package providers

import (
	"context"
	"errors"
)

// ErrModelNotSupported is returned when a provider cannot serve the requested
// model. The caller may try the next provider.
var ErrModelNotSupported = errors.New("model not supported by provider")

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest mirrors the subset of the OpenAI chat completion request that the
// gateway understands. Unknown fields are rejected so a silently dropped
// parameter never changes the meaning of a request.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

// Usage reports the token counts of a completed request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice is one generated alternative.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ChatResponse mirrors the OpenAI chat completion response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Provider is an upstream that can answer chat completion requests.
type Provider interface {
	// Name identifies the provider in logs, metrics and audit records.
	Name() string
	// Chat performs a single non-streaming completion. Implementations must
	// honour ctx cancellation and must not retry internally; retry and
	// failover are the caller's responsibility.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}
