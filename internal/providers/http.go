package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTP forwards the supported chat contract to an OpenAI-compatible endpoint.
// Each attempt has one deadline covering both headers and the response body.
type HTTP struct {
	base, key string
	client    *http.Client
}

func NewHTTP(base, key string, timeout time.Duration) *HTTP {
	return &HTTP{strings.TrimRight(base, "/"), key, &http.Client{Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (p *HTTP) Name() string { return "compatible" }

type UpstreamError struct{ Status int }

func (e *UpstreamError) Error() string { return fmt.Sprintf("upstream HTTP %d", e.Status) }

func (p *HTTP) request(ctx context.Context, req ChatRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	if p.key != "" {
		r.Header.Set("Authorization", "Bearer "+p.key)
	}
	resp, err := p.client.Do(r)
	if err != nil {
		return nil, fmt.Errorf("upstream transport failed")
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &UpstreamError{resp.StatusCode}
	}
	return resp, nil
}
func (p *HTTP) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	resp, err := p.request(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(payload) > 8<<20 {
		return nil, fmt.Errorf("upstream response could not be read within limits")
	}
	var result ChatResponse
	if err = json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("invalid upstream JSON")
	}
	if result.ID == "" || len(result.Choices) == 0 || result.Usage.PromptTokens < 0 || result.Usage.CompletionTokens < 0 {
		return nil, fmt.Errorf("invalid upstream completion")
	}
	return &result, nil
}
func (p *HTTP) ChatStream(ctx context.Context, req ChatRequest) (ChatStream, error) {
	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}
	resp, err := p.request(ctx, req)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("upstream did not return an event stream")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	s := &httpStream{body: resp.Body, scanner: scanner, model: req.Model, created: time.Now().Unix()}
	// Read the first event before the gateway commits its response headers.
	first, err := s.read()
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.id == "" {
		_ = s.Close()
		return nil, fmt.Errorf("upstream stream missing completion ID")
	}
	s.first = &first
	return s, nil
}

type httpStream struct {
	body      io.ReadCloser
	scanner   *bufio.Scanner
	id, model string
	created   int64
	first     *Chunk
	done      bool
}

func (s *httpStream) ID() string     { return s.id }
func (s *httpStream) Model() string  { return s.model }
func (s *httpStream) Created() int64 { return s.created }
func (s *httpStream) Close() error   { return s.body.Close() }
func (s *httpStream) Recv() (Chunk, error) {
	if s.first != nil {
		c := *s.first
		s.first = nil
		return c, nil
	}
	return s.read()
}
func (s *httpStream) read() (Chunk, error) {
	if s.done {
		return Chunk{}, io.EOF
	}
	var data []string
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			continue
		}
		if line != "" || len(data) == 0 {
			continue
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			s.done = true
			return Chunk{}, io.EOF
		}
		var event struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *Usage          `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil || len(event.Error) > 0 {
			return Chunk{}, fmt.Errorf("invalid upstream stream event")
		}
		if event.ID != "" {
			s.id = event.ID
		}
		if event.Model != "" {
			s.model = event.Model
		}
		if event.Created != 0 {
			s.created = event.Created
		}
		c := Chunk{Usage: event.Usage}
		if c.Usage != nil && (c.Usage.PromptTokens < 0 || c.Usage.CompletionTokens < 0) {
			return Chunk{}, fmt.Errorf("invalid upstream usage")
		}
		if len(event.Choices) > 0 {
			c.Delta = event.Choices[0].Delta.Content
			c.FinishReason = event.Choices[0].FinishReason
		}
		return c, nil
	}
	if err := s.scanner.Err(); err != nil {
		return Chunk{}, fmt.Errorf("upstream stream read failed")
	}
	return Chunk{}, fmt.Errorf("upstream stream ended without DONE")
}
