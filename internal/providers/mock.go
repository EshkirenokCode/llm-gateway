package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
)

// MockOptions tunes the behaviour of a Mock provider in tests and local runs.
type MockOptions struct {
	// Latency delays every call, simulating a slow upstream.
	Latency time.Duration
	// Err, when set, is returned instead of a completion.
	Err error
	// Reply overrides the generated answer.
	Reply string
}

// Mock is a provider that answers locally, without network access or cost. It
// backs the local docker compose stack and every test that needs a predictable
// upstream.
type Mock struct {
	opts MockOptions
	now  func() time.Time
}

// NewMock returns a Mock provider.
func NewMock(opts MockOptions) *Mock {
	return &Mock{opts: opts, now: time.Now}
}

// Name implements Provider.
func (m *Mock) Name() string { return "mock" }

// Chat implements Provider. It echoes the last user message unless a reply is
// configured, and reports token counts estimated from the text length.
func (m *Mock) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if m.opts.Latency > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.opts.Latency):
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if m.opts.Err != nil {
		return nil, m.opts.Err
	}

	prompt := lastUserMessage(req.Messages)

	reply := m.opts.Reply
	if reply == "" {
		reply = "mock response to: " + prompt
	}

	promptTokens := 0
	for _, msg := range req.Messages {
		promptTokens += estimateTokens(msg.Content)
	}
	completionTokens := estimateTokens(reply)

	return &ChatResponse{
		ID:      "chatcmpl-" + randomID(),
		Object:  "chat.completion",
		Created: m.now().Unix(),
		Model:   req.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: reply},
			FinishReason: "stop",
		}},
		Usage: Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}, nil
}

func lastUserMessage(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// estimateTokens is a rough stand-in for a real tokenizer: roughly one token
// per four characters. Good enough for a fake upstream, and deliberately not
// used for billing real providers, which report their own counts.
func estimateTokens(s string) int {
	fields := len(strings.Fields(s))
	if fields == 0 {
		return 0
	}
	if n := len(s) / 4; n > fields {
		return n
	}
	return fields
}

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand only fails if the system entropy source is broken; a
		// timestamp keeps the response usable instead of failing the request.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// mockStream replays a prepared answer word by word.
type mockStream struct {
	id      string
	model   string
	created int64

	ctx    context.Context
	words  []string
	pos    int
	delay  time.Duration
	usage  *Usage
	closed bool
}

// ChatStream implements Provider. The reply is split into words so a client
// sees the same incremental behaviour as with a real model.
func (m *Mock) ChatStream(ctx context.Context, req ChatRequest) (ChatStream, error) {
	if m.opts.Err != nil {
		return nil, m.opts.Err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	reply := m.opts.Reply
	if reply == "" {
		reply = "mock response to: " + lastUserMessage(req.Messages)
	}

	promptTokens := 0
	for _, msg := range req.Messages {
		promptTokens += estimateTokens(msg.Content)
	}

	return &mockStream{
		id:      "chatcmpl-" + randomID(),
		model:   req.Model,
		created: m.now().Unix(),
		ctx:     ctx,
		words:   strings.Fields(reply),
		delay:   m.opts.Latency,
		usage: &Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: estimateTokens(reply),
			TotalTokens:      promptTokens + estimateTokens(reply),
		},
	}, nil
}

func (s *mockStream) ID() string     { return s.id }
func (s *mockStream) Model() string  { return s.model }
func (s *mockStream) Created() int64 { return s.created }

func (s *mockStream) Recv() (Chunk, error) {
	if s.closed {
		return Chunk{}, io.EOF
	}

	if s.delay > 0 {
		select {
		case <-s.ctx.Done():
			return Chunk{}, s.ctx.Err()
		case <-time.After(s.delay):
		}
	}

	if err := s.ctx.Err(); err != nil {
		return Chunk{}, err
	}

	// All words sent: emit the closing chunk once, then report EOF.
	if s.pos >= len(s.words) {
		if s.usage == nil {
			return Chunk{}, io.EOF
		}
		usage := s.usage
		s.usage = nil
		return Chunk{FinishReason: "stop", Usage: usage}, nil
	}

	word := s.words[s.pos]
	s.pos++
	if s.pos > 1 {
		word = " " + word
	}

	return Chunk{Delta: word}, nil
}

func (s *mockStream) Close() error {
	s.closed = true
	return nil
}
