package vertexai

import (
	"context"
	"errors"
)

// Chunk is one increment of generated text.
type Chunk struct {
	Text string
	// Thought is text of the model's thought channel (thinkingConfig,
	// #2107).
	Thought string
	// ToolCalls are function calls the model made, parsed by the runtime
	// from the model's native tool-call syntax (#1937).
	ToolCalls []FunctionCall
	// Logprobs are decode steps' log-probabilities (responseLogprobs,
	// #2107): from the runtime, one step per chunk with nothing else in
	// it; downstream of applyStops, the steps of the chunk's own tokens.
	Logprobs []LogprobStep
	// Last marks the final chunk, carrying the finish reason.
	Last         bool
	FinishReason string
	// Usage is the measured token accounting: on the final chunk, and on an
	// earlier chunk of its own carrying only the prompt's count, sent before
	// decoding starts so a generation cut short still reports it.
	Usage *Usage
}

// Usage is what the runtime measured. A count of -1 was not measured.
type Usage struct {
	PromptTokens    int
	CandidateTokens int
	// Images marks a prompt with image tokens, whose per-modality split the
	// runtime does not report.
	Images bool
	// ThoughtTokens are the decoded tokens of the model's thoughts, part of
	// CandidateTokens as the engine counts them (#2107).
	ThoughtTokens int
	// CachedTokens are the cached content's tokens, part of PromptTokens
	// (cachedContent, #2107); CachedImages marks a cache holding images.
	CachedTokens int
	CachedImages bool
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
	// JSONSchema, when set, constrains the output to JSON valid against it
	// (responseMimeType application/json, #1779).
	JSONSchema string
	// PresencePenalty and FrequencyPenalty are LiteRT-LM's
	// RepetitionPenaltyConfig (#1937).
	PresencePenalty  *float32
	FrequencyPenalty *float32
	// Thinking, when set, is the model's thinking mode on or off, with
	// ThinkingBudget its token budget (-1 unlimited) (#2107).
	Thinking       *bool
	ThinkingBudget int32
	// ResponseLogprobs reports each chosen token's log-probability
	// (Chunk.Logprobs, #2107), and TopLogprobs (logprobs, 0 to 20) that
	// many of each step's most probable tokens.
	ResponseLogprobs bool
	TopLogprobs      int32

	// Conversation, when set, is the whole request in LiteRT-LM's message
	// format (#1937, conversation.go), sent to cloudburrow_generate on
	// standard input; Prompt then holds only the last turn's text, for
	// generators that read nothing else. Vision loads the model's vision
	// encoder for image parts.
	Conversation *RunnerRequest
	Vision       bool
}

// TokenInfo is one content's tokens, as computeTokens reports them.
type TokenInfo struct {
	IDs    []int64
	Tokens [][]byte
}

// TokenCounter is a generator whose runtime exposes the model's tokenizer:
// cloudburrow_generate's count_only and compute modes (#1937).
type TokenCounter interface {
	// CountTokens is the prompt tokens req.Conversation renders to.
	CountTokens(ctx context.Context, req Request) (int, error)
	// ComputeTokens tokenizes each text with the model's tokenizer.
	ComputeTokens(ctx context.Context, texts []string) ([]TokenInfo, error)
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
