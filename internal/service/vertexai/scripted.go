package vertexai

import (
	"context"
	"errors"
	"strings"
)

// ScriptedGenerator emits a fixed sequence of chunks.
//
// It exists so that protocol behaviour — routing, framing, finish reasons,
// error mapping, cancellation, the shape of an SSE event — can be tested
// deterministically, which is impossible against a language model. The issue
// asks for exactly this separation: what the service does is asserted here,
// and what the model says is not asserted anywhere.
type ScriptedGenerator struct {
	// ID is reported as the model that ran.
	ID string
	// Chunks are emitted in order.
	Chunks []string
	// Err, if set, fails generation after the chunks are emitted.
	Err error
	// Block, if non-nil, is waited on before the first chunk, so a test can
	// hold generation open and cancel it.
	Block <-chan struct{}
	// Options makes it an OptionsGenerator, so the edge passes the sampler
	// and the token limit down instead of refusing them; Seen, if set,
	// receives each Request so a test can check what arrived.
	Options bool
	Seen    func(Request)
	// Finish, if set, is the final chunk's finish reason instead of STOP.
	Finish string
	// Calls, if set, are function calls emitted after the chunks, as the
	// runtime parses them from the model's tool-call syntax (#1937).
	Calls []FunctionCall
	// Usage, if set, is the final chunk's token accounting.
	Usage *Usage
}

// Model implements Generator.
func (g *ScriptedGenerator) Model() string { return g.ID }

// HonoursOptions implements OptionsGenerator.
func (g *ScriptedGenerator) HonoursOptions() bool { return g.Options }

// Generate implements Generator.
func (g *ScriptedGenerator) Generate(ctx context.Context, req Request) (<-chan Chunk, func() error, error) {
	if g.Seen != nil {
		g.Seen(req)
	}
	finish := g.Finish
	if finish == "" {
		finish = FinishReasonStop
	}
	out := make(chan Chunk)
	var genErr error

	go func() {
		defer close(out)
		if g.Block != nil {
			select {
			case <-g.Block:
			case <-ctx.Done():
				return
			}
		}
		for _, c := range g.Chunks {
			select {
			case out <- Chunk{Text: c}:
			case <-ctx.Done():
				return
			}
		}
		if len(g.Calls) > 0 {
			select {
			case out <- Chunk{ToolCalls: g.Calls}:
			case <-ctx.Done():
				return
			}
		}
		if g.Err != nil {
			genErr = g.Err
			return
		}
		select {
		case out <- Chunk{Last: true, FinishReason: finish, Usage: g.Usage}:
		case <-ctx.Done():
		}
	}()

	return out, func() error { return genErr }, nil
}

// EchoGenerator returns the prompt back, uppercased, one word per chunk.
//
// Useful where a test needs to prove the prompt reached the generator intact
// without depending on a model to say anything in particular.
type EchoGenerator struct{ ID string }

// Model implements Generator.
func (g *EchoGenerator) Model() string { return g.ID }

// Generate implements Generator.
func (g *EchoGenerator) Generate(ctx context.Context, req Request) (<-chan Chunk, func() error, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, nil, errors.New("empty prompt reached the generator")
	}
	words := strings.Fields(req.Prompt)
	chunks := make([]string, 0, len(words))
	for i, w := range words {
		if i > 0 {
			w = " " + w
		}
		chunks = append(chunks, strings.ToUpper(w))
	}
	g2 := &ScriptedGenerator{ID: g.ID, Chunks: chunks}
	return g2.Generate(ctx, req)
}

// CountingGenerator is a ScriptedGenerator with a tokenizer, so a test can
// reach countTokens and computeTokens (#1937, #1962) without a model. Its
// count is Count's, or one token per whitespace-separated word of the
// conversation's text; computeTokens gives each word a token, numbered from
// one in order. Options must be set for the API to use it.
type CountingGenerator struct {
	*ScriptedGenerator
	Count func(Request) int
}

// CountTokens implements TokenCounter.
func (g *CountingGenerator) CountTokens(_ context.Context, req Request) (int, error) {
	if g.Count != nil {
		return g.Count(req), nil
	}
	n := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			n += len(strings.Fields(x))
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case []map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	if c := req.Conversation; c != nil {
		walk(c.Preface)
		walk(c.Message)
	}
	return n, nil
}

// ComputeTokens implements TokenCounter.
func (g *CountingGenerator) ComputeTokens(_ context.Context, texts []string) ([]TokenInfo, error) {
	out := make([]TokenInfo, len(texts))
	for i, t := range texts {
		for j, w := range strings.Fields(t) {
			out[i].IDs = append(out[i].IDs, int64(j+1))
			out[i].Tokens = append(out[i].Tokens, []byte(w))
		}
	}
	return out, nil
}
