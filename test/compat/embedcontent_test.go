//go:build compat

package compat

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentThroughThePredictionClient (#1244) embeds through the
// official aiplatform PredictionClient's EmbedContent, over gRPC, with the
// same local model :predict uses: the vector equals Predict's for the same
// text and task type, output_dimensionality gives the renormalised
// Matryoshka prefix, task types change the vector, and what the model cannot
// do (a size it was not trained for, a non-text part, document OCR) is
// INVALID_ARGUMENT naming the field.
//
// The genai SDK is not used here: on its Vertex backend, Models.EmbedContent
// sends :embedContent only for a model whose name contains "gemini" or
// "maas", and :predict for any other, so it never sends EmbedContent for the
// one model served (TestEmbeddingGemmaThroughTheOfficialSDK covers that
// :predict path).
func TestEmbedContentThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel
	const text = "The quick brown fox jumps over the lazy dog."
	content := &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_Text{Text: text}}}}

	embedWith := func(task aiplatformpb.EmbedContentRequest_EmbeddingTaskType, dim int32) []float32 {
		t.Helper()
		cfg := &aiplatformpb.EmbedContentRequest_EmbedContentConfig{TaskType: &task}
		if dim > 0 {
			cfg.OutputDimensionality = &dim
		}
		resp, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: content, EmbedContentConfig: cfg,
		})
		if err != nil {
			t.Fatalf("EmbedContent (%v, dim %d): %v", task, dim, err)
		}
		if resp.GetUsageMetadata().GetPromptTokenCount() <= 0 {
			t.Errorf("usage metadata %v, want a positive prompt token count", resp.GetUsageMetadata())
		}
		return resp.GetEmbedding().GetValues()
	}
	cos := func(a []float32, b []float64) float64 {
		var dot, na, nb float64
		for i := range a {
			dot += float64(a[i]) * b[i]
			na += float64(a[i]) * float64(a[i])
			nb += b[i] * b[i]
		}
		return dot / math.Sqrt(na*nb)
	}

	full := embedWith(aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY, 0)
	if len(full) != 768 || math.Abs(norm(full)-1) > 1e-3 {
		t.Fatalf("full vector: %d values, norm %.6f", len(full), norm(full))
	}

	// The same model and prompt as :predict, so the same vector.
	in, _ := structpb.NewValue(map[string]any{"content": text, "task_type": "RETRIEVAL_QUERY"})
	pr, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: name, Instances: []*structpb.Value{in}})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	vals := pr.GetPredictions()[0].GetStructValue().GetFields()["embeddings"].GetStructValue().GetFields()["values"].GetListValue().GetValues()
	pv := make([]float64, len(vals))
	for i, v := range vals {
		pv[i] = v.GetNumberValue()
	}
	if c := cos(full, pv); c < 0.9999 {
		t.Errorf("EmbedContent and Predict disagree for the same input (cosine %.6f)", c)
	}

	for _, dim := range []int32{512, 256, 128} {
		v := embedWith(aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY, dim)
		if len(v) != int(dim) || math.Abs(norm(v)-1) > 1e-3 {
			t.Fatalf("output_dimensionality %d: %d values, norm %.6f", dim, len(v), norm(v))
		}
		if c := cosine(v, full[:dim]); c < 0.9999 {
			t.Errorf("output_dimensionality %d is not the renormalised prefix of 768 (cosine %.6f)", dim, c)
		}
	}

	doc := embedWith(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, 0)
	if c := cosine(full, doc); c > 0.9999 {
		t.Errorf("RETRIEVAL_QUERY and RETRIEVAL_DOCUMENT gave the same vector (cosine %.6f)", c)
	}
	if c := cosine(full, embedWith(aiplatformpb.EmbedContentRequest_UNSPECIFIED, 0)); c < 0.9999 {
		t.Errorf("UNSPECIFIED differs from RETRIEVAL_QUERY, the model's default (cosine %.6f)", c)
	}

	refused := map[string]*aiplatformpb.EmbedContentRequest{
		"output_dimensionality": {Model: proto.String(name), Content: content, OutputDimensionality: proto.Int32(300)},
		"content.parts[0]": {Model: proto.String(name), Content: &aiplatformpb.Content{Parts: []*aiplatformpb.Part{{
			Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: "image/png", Data: []byte{0}}},
		}}}},
		"document_ocr": {Model: proto.String(name), Content: content,
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{DocumentOcr: proto.Bool(true)}},
	}
	for field, req := range refused {
		if _, err := pc.EmbedContent(ctx, req); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v, want INVALID_ARGUMENT naming it", field, err)
		}
	}
	// No substitution.
	other := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/text-embedding-005"
	if _, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{Model: &other, Content: content}); status.Code(err) != codes.NotFound {
		t.Errorf("text-embedding-005: %v, want NOT_FOUND", err)
	}
}
