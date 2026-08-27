package providers

// Chunk is one incremental piece of a streamed completion.
type Chunk struct {
	// Delta is the text generated since the previous chunk. It is empty on the
	// final chunk, which only carries the finish reason.
	Delta string
	// FinishReason is set on the last chunk, e.g. "stop" or "length".
	FinishReason string
	// Usage is reported by providers that send token counts at the end of a
	// stream. It is nil for every other chunk.
	Usage *Usage
}

// ChatStream delivers a completion incrementally. Recv returns io.EOF when the
// provider has finished. Callers must always Close the stream.
type ChatStream interface {
	// ID is the completion identifier shared by every chunk.
	ID() string
	// Model is the model that produced the stream.
	Model() string
	// Created is the unix timestamp the completion started at.
	Created() int64
	// Recv blocks until the next chunk is available, the stream ends (io.EOF)
	// or the context passed to ChatStream is cancelled.
	Recv() (Chunk, error)
	// Close releases the underlying connection. It is safe to call more than
	// once.
	Close() error
}
