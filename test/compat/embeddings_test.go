//go:build compat

package compat

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"google.golang.org/genai"
)

// EnvEmbeddings points at the local AI endpoint of an instance started with
// -local-ai-embeddings from a -tags onnx build (make build-onnx), with
// ONNXRUNTIME_LIB set. It is separate from EnvLocalAI because an instance can
// serve generation with no embedding model, and these tests need the real
// model: the pinned community ONNX int8 conversion of EmbeddingGemma (#41).
const EnvEmbeddings = "CLOUDBURROW_TEST_EMBEDDINGS"

// embeddingModel is the only model the endpoint serves for :predict. It is a
// community conversion, NOT an official Google artifact.
const embeddingModel = "embeddinggemma-300m-onnx-community"

func embed(t *testing.T, ctx context.Context, cl *genai.Client, cfg *genai.EmbedContentConfig, texts ...string) [][]float32 {
	t.Helper()
	contents := make([]*genai.Content, len(texts))
	for i, s := range texts {
		contents[i] = genai.NewContentFromText(s, genai.RoleUser)
	}
	resp, err := cl.Models.EmbedContent(ctx, embeddingModel, contents, cfg)
	if err != nil {
		t.Fatalf("EmbedContent: %v", err)
	}
	if len(resp.Embeddings) != len(texts) {
		t.Fatalf("got %d embeddings for %d texts", len(resp.Embeddings), len(texts))
	}
	out := make([][]float32, len(texts))
	for i, e := range resp.Embeddings {
		if e.Statistics == nil || e.Statistics.TokenCount <= 0 {
			t.Errorf("embedding %d: statistics %+v, want a positive token count", i, e.Statistics)
		}
		out[i] = e.Values
	}
	return out
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / math.Sqrt(na*nb)
}

func norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

// TestEmbeddingGemmaThroughTheOfficialSDK runs the real model through Google's
// genai SDK on its Vertex AI backend, which sends text-embedding :predict for
// a non-Gemini model: every Matryoshka size, finite unit vectors, task types
// that change the vector, and a retrieval sanity check.
func TestEmbeddingGemmaThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClientAt(t, h, EnvEmbeddings)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const text = "The quick brown fox jumps over the lazy dog."
	var full []float32
	for _, dim := range []int32{768, 512, 256, 128} {
		d := dim
		v := embed(t, ctx, cl, &genai.EmbedContentConfig{OutputDimensionality: &d}, text)[0]
		if len(v) != int(dim) {
			t.Fatalf("outputDimensionality %d: got %d values", dim, len(v))
		}
		for i, x := range v {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				t.Fatalf("outputDimensionality %d: value %d is %v", dim, i, x)
			}
		}
		if n := norm(v); math.Abs(n-1) > 1e-3 {
			t.Errorf("outputDimensionality %d: norm %.6f, want 1 (truncated and renormalised)", dim, n)
		}
		if dim == 768 {
			full = v
			continue
		}
		// Matryoshka: a smaller size is the leading values of the full vector, renormalised.
		if c := cosine(v, full[:dim]); c < 0.9999 {
			t.Errorf("outputDimensionality %d is not the renormalised prefix of 768 (cosine %.6f)", dim, c)
		}
	}

	// Task types select the model card's prompt prefixes, so the same text
	// embeds differently as a query and as a document.
	q := embed(t, ctx, cl, &genai.EmbedContentConfig{TaskType: "RETRIEVAL_QUERY"}, text)[0]
	doc := embed(t, ctx, cl, &genai.EmbedContentConfig{TaskType: "RETRIEVAL_DOCUMENT", Title: "Typing"}, text)[0]
	sim := embed(t, ctx, cl, &genai.EmbedContentConfig{TaskType: "SEMANTIC_SIMILARITY"}, text)[0]
	if c := cosine(q, doc); c > 0.9999 {
		t.Errorf("RETRIEVAL_QUERY and RETRIEVAL_DOCUMENT gave the same vector (cosine %.6f)", c)
	}
	if c := cosine(q, sim); c > 0.9999 {
		t.Errorf("RETRIEVAL_QUERY and SEMANTIC_SIMILARITY gave the same vector (cosine %.6f)", c)
	}
	// No task type is RETRIEVAL_QUERY's prefix.
	if c := cosine(q, embed(t, ctx, cl, nil, text)[0]); c < 0.9999 {
		t.Errorf("no task type differs from RETRIEVAL_QUERY (cosine %.6f)", c)
	}
	// An unknown task type is refused, never embedded without a prefix.
	_, err := cl.Models.EmbedContent(ctx, embeddingModel, genai.Text(text), &genai.EmbedContentConfig{TaskType: "NOT_A_TASK"})
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 400 {
		t.Errorf("unknown task type: err %v, want a 400", err)
	}

	// Retrieval: a query scores its answer above an unrelated document.
	query := embed(t, ctx, cl, &genai.EmbedContentConfig{TaskType: "RETRIEVAL_QUERY"},
		"How do I bake a loaf of sourdough bread?")[0]
	docs := embed(t, ctx, cl, &genai.EmbedContentConfig{TaskType: "RETRIEVAL_DOCUMENT"},
		"Mix flour, water, salt and a sourdough starter, let the dough rise overnight, then bake it at 230 degrees for 40 minutes.",
		"The central bank raised interest rates by a quarter point to slow inflation.")
	related, unrelated := cosine(query, docs[0]), cosine(query, docs[1])
	t.Logf("cosine(query, related) = %.4f, cosine(query, unrelated) = %.4f", related, unrelated)
	if related <= unrelated {
		t.Errorf("related pair %.4f does not score above unrelated pair %.4f", related, unrelated)
	}
}
