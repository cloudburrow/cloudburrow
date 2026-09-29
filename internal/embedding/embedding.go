// Package embedding runs the community ONNX int8 conversion of
// EmbeddingGemma (#41).
//
// The model is NOT an official Google artifact: it is onnx-community's
// conversion of google/embeddinggemma-300m, pinned in internal/localai. Every
// surface that reports it says so.
//
// ONNX Runtime is a C library, so the runtime is compiled only with the onnx
// build tag (`go build -tags onnx`), which also needs the Hugging Face
// tokenizers static library to link. The default build compiles the stub in
// stub.go, whose Open returns ErrNotCompiled; the HTTP layer turns that into
// 501 rather than pretending.
package embedding

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ErrNotCompiled means this binary was built without the onnx tag.
var ErrNotCompiled = errors.New("embedding runtime not compiled in: rebuild with -tags onnx (see docs/embeddings.md)")

// Dimensions is the model's native output size.
const Dimensions = 768

// MaxTokens is the model's context length. Longer input is truncated when the
// caller allows it and refused otherwise.
const MaxTokens = 2048

// ValidDimensions are the Matryoshka sizes the model card supports.
var ValidDimensions = []int{768, 512, 256, 128}

// Input is one text to embed.
type Input struct {
	Content  string
	TaskType string
	Title    string
}

// Output is one embedding.
type Output struct {
	Values     []float32
	TokenCount int
	Truncated  bool
}

// Embedder produces embeddings.
type Embedder interface {
	// Model is the identity reported to clients.
	Model() string
	// Embed embeds each input. dim is 0 for the native size.
	Embed(ctx context.Context, in []Input, dim int, autoTruncate bool) ([]Output, error)
	Close() error
}

// Prompt applies the task prefix EmbeddingGemma was trained with. The
// prefixes are the model card's; an unknown task type is refused rather than
// silently embedded without one.
func Prompt(in Input) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(in.TaskType)) {
	case "", "RETRIEVAL_QUERY", "TASK_TYPE_UNSPECIFIED":
		return "task: search result | query: " + in.Content, nil
	case "RETRIEVAL_DOCUMENT":
		title := strings.TrimSpace(in.Title)
		if title == "" {
			title = "none"
		}
		return "title: " + title + " | text: " + in.Content, nil
	case "QUESTION_ANSWERING":
		return "task: question answering | query: " + in.Content, nil
	case "FACT_VERIFICATION":
		return "task: fact checking | query: " + in.Content, nil
	case "CLASSIFICATION":
		return "task: classification | query: " + in.Content, nil
	case "CLUSTERING":
		return "task: clustering | query: " + in.Content, nil
	case "SEMANTIC_SIMILARITY":
		return "task: sentence similarity | query: " + in.Content, nil
	case "CODE_RETRIEVAL_QUERY":
		return "task: code retrieval | query: " + in.Content, nil
	default:
		return "", fmt.Errorf("task_type %q is not supported", in.TaskType)
	}
}

// ValidateDim checks a requested output dimensionality.
func ValidateDim(dim int) error {
	if dim == 0 {
		return nil
	}
	for _, d := range ValidDimensions {
		if d == dim {
			return nil
		}
	}
	return fmt.Errorf("outputDimensionality %d is not supported; use one of %v", dim, ValidDimensions)
}

// Truncate shortens a unit vector to dim and renormalises it, which is how
// Matryoshka embeddings are reduced.
func Truncate(v []float32, dim int) []float32 {
	if dim <= 0 || dim >= len(v) {
		return v
	}
	out := append([]float32(nil), v[:dim]...)
	var sum float64
	for _, x := range out {
		sum += float64(x) * float64(x)
	}
	if n := math.Sqrt(sum); n > 0 {
		for i := range out {
			out[i] = float32(float64(out[i]) / n)
		}
	}
	return out
}
