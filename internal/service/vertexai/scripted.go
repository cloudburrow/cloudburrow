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
}

// Model implements Generator.
func (g *ScriptedGenerator) Model() string { return g.ID }

// Generate implements Generator.
func (g *ScriptedGenerator) Generate(ctx context.Context, req Request) (<-chan Chunk, func() error, error) {
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
		if g.Err != nil {
			genErr = g.Err
			return
		}
		select {
		case out <- Chunk{Last: true, FinishReason: FinishReasonStop}:
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
