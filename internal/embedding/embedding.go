// Package embedding runs the community ONNX int8 conversions of
// EmbeddingGemma (#41) and EmbeddingGemma 2 (#1454).
//
// Neither model is an official Google artifact: they are onnx-community's
// conversions of google/embeddinggemma-300m and google/embeddinggemma-2,
// pinned in internal/localai. Every surface that reports them says so.
// EmbeddingGemma embeds text; EmbeddingGemma 2 embeds text and images, and
// any mix of them, into one vector.
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
	"image"
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

// MaxTokens2 is EmbeddingGemma 2's context length, shared by text and the
// soft tokens of its images.
const MaxTokens2 = 8192

// MaxImages is the most images one input may hold: Google's documented limit
// for gemini-embedding-2's EmbedContent.
const MaxImages = 6

// ValidDimensions are the Matryoshka sizes the model card supports.
var ValidDimensions = []int{768, 512, 256, 128}

// Input is one text to embed, or with Parts one mix of text and images.
type Input struct {
	Content  string
	TaskType string
	Title    string
	// Parts, when set, are the input in order, and Content is unused. Only an
	// embedder that implements Multimodal accepts an image part.
	Parts []Part
}

// Part is one text or one image of a multimodal input.
type Part struct {
	Text  string
	Image image.Image
}

// Multimodal is implemented by an embedder that embeds images.
type Multimodal interface {
	EmbedsImages() bool
}

// EmbedsImages reports whether e embeds image parts.
func EmbedsImages(e Embedder) bool {
	m, ok := e.(Multimodal)
	return ok && m.EmbedsImages()
}

// Placeholder tokens of EmbeddingGemma 2's vocabulary. ImageToken marks an
// image's place; the processor expands it to the begin-image token, one
// ImageToken per soft token, and the end-image token.
const (
	ImageToken      = "<|image|>"
	BeginImageToken = "<|image>"
	EndImageToken   = "<image|>"
)

// reservedMarkers are the vocabulary's media markers, which a text part may
// not contain: the tokenizer would read them as placeholders.
var reservedMarkers = []string{"<|image|>", "<|video|>", "<|audio|>", "<|image>", "<image|>", "<|audio>", "<audio|>"}

// MultimodalPrompt is the text EmbeddingGemma 2 tokenizes for in.Parts, with
// an ImageToken where each image goes, and the number of images. Text parts
// are joined with newlines, as text-only content is, and each image's
// placeholder is placed between them in order, as the model's chat template
// does. The task prefix applies to the text: content with no text part is
// its images alone, with no prefix, as the model card says to pass media.
func MultimodalPrompt(in Input) (string, int, error) {
	var b strings.Builder
	images, texts := 0, 0
	prevText := false
	for i, p := range in.Parts {
		if p.Image != nil {
			if images > 0 && !prevText {
				b.WriteString(" ")
			}
			b.WriteString(ImageToken)
			images++
			prevText = false
			continue
		}
		for _, m := range reservedMarkers {
			if strings.Contains(p.Text, m) {
				return "", 0, fmt.Errorf("parts[%d]: the text contains %q, a media marker of the model's vocabulary", i, m)
			}
		}
		if prevText {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
		texts++
		prevText = true
	}
	if images > MaxImages {
		return "", 0, fmt.Errorf("%d images; at most %d are embedded together", images, MaxImages)
	}
	if texts == 0 {
		return b.String(), images, nil
	}
	text, err := Prompt(Input{Content: b.String(), TaskType: in.TaskType, Title: in.Title})
	return text, images, err
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
