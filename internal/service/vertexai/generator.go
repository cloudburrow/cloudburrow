package vertexai

import (
	"context"
	"errors"
)

// Chunk is one increment of generated text.
type Chunk struct {
	Text string
	// Last marks the final chunk, carrying the finish reason.
	Last         bool
	FinishReason string
}

// Generator produces text for a prompt.
//
// It streams, because the runtime streams: tokens arrive from the model as they
// are decoded, and a generator that returned only the finished text would make
// streaming a presentation trick rather than a property of the system. A
// non-streaming response is then assembled from the stream, never the reverse.
type Generator interface {
	// Generate emits chunks on the returned channel until generation ends or
	// ctx is cancelled. The channel is closed when it ends. A non-nil error
	// returned from Err after the channel closes means generation failed.
	Generate(ctx context.Context, req Request) (<-chan Chunk, func() error, error)

	// Model is the identifier of what actually runs, reported to the caller
	// so a substitution cannot be silent.
	Model() string
}

// Request is what a generator is asked for.
//
// It carries only a prompt. Every generation option the Vertex API defines is
// refused at the edge, because the runtime honours none of them — so passing
// one down would be carrying a value nothing reads.
type Request struct {
	Prompt string
}

// ErrNoGenerator means generation is not configured.
//
// This is deliberately not a stub that returns plausible text. An endpoint
// that answers without a model is the most expensive kind of wrong.
var ErrNoGenerator = errors.New("no local generation runtime is configured")
