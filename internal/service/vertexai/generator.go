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
// The options are set only for a generator that reports HonoursOptions; for
// any other the edge refuses them (api.go), so a value is never carried down
// to something that does not read it. Each is a pointer so that "absent" and
// "zero" differ: temperature 0 is greedy decoding, not "unset".
type Request struct {
	Prompt string

	MaxOutputTokens *int32
	Temperature     *float32
	TopP            *float32
	TopK            *int32
	Seed            *int32
}

// OptionsGenerator is a Generator whose runtime applies Request's sampling
// options and token limit (cloudburrow_generate, #1797). A Generator that
// does not implement it, or reports false, has them refused at the edge.
type OptionsGenerator interface {
	HonoursOptions() bool
}

// honoursOptions reports whether gen applies Request's options.
func honoursOptions(gen Generator) bool {
	o, ok := gen.(OptionsGenerator)
	return ok && o.HonoursOptions()
}

// ErrNoGenerator means generation is not configured.
//
// This is deliberately not a stub that returns plausible text. An endpoint
// that answers without a model is the most expensive kind of wrong.
var ErrNoGenerator = errors.New("no local generation runtime is configured")
