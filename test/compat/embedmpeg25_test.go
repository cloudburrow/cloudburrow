//go:build compat

package compat

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/proto"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsMPEG25MP3ThroughThePredictionClient (#1734) embeds
// MPEG-2.5 MP3s (the 8, 11.025 and 12 kHz rates LAME picks for low bit
// rates) through the official aiplatform PredictionClient's EmbedContent,
// as mime_type audio/mpeg inline_data, the shape Google's
// gemini-embedding-2 samples use. testdata/jfk-mpeg25-8k.mp3 (mono,
// 24 kbit/s) and testdata/jfk-mpeg25-11k.mp3 (joint stereo, 32 kbit/s) are
// the model card's jfk.wav (fetched and checked by SHA-256 as the WAV test
// does) encoded by ffmpeg 9's libmp3lame. Each is a 768-value unit vector
// of the WAV's 279 tokens, close to the WAV's vector (the 8 kHz copy keeps
// only the speech band below 4 kHz) and nearest the speech query.
func TestEmbedContentEmbedsMPEG25MP3ThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	wav := fetchJFK(t, ctx)
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	embed := func(task aiplatformpb.EmbedContentRequest_EmbeddingTaskType, part *aiplatformpb.Part) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		r, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{part}},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{TaskType: &task},
		})
		if err != nil {
			t.Fatalf("EmbedContent: %v", err)
		}
		return r
	}
	inline := func(mt string, data []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}
	}
	doc := aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT
	fromWAV := embed(doc, inline("audio/wav", wav)).GetEmbedding().GetValues()
	queries := []string{"a president's speech about serving your country", "cats sleeping on a couch", "a turtle swimming in the ocean"}
	queryVecs := make([][]float32, len(queries))
	for i, q := range queries {
		queryVecs[i] = embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY, &aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: q}}).GetEmbedding().GetValues()
	}
	for _, file := range []string{"jfk-mpeg25-8k.mp3", "jfk-mpeg25-11k.mp3"} {
		mp3, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		r := embed(doc, inline("audio/mpeg", mp3))
		v := r.GetEmbedding().GetValues()
		if len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
			t.Fatalf("%s: %d values, norm %.6f", file, len(v), norm(v))
		}
		if n := r.GetUsageMetadata().GetPromptTokenCount(); n != 279 {
			t.Errorf("%s: prompt token count %d, want the WAV's 279", file, n)
		}
		c := cosine(v, fromWAV)
		if c < 0.95 {
			t.Errorf("%s against the WAV it was made from: cosine %.4f, want at least 0.95", file, c)
		}
		scores := make([]float64, len(queries))
		for i := range queries {
			scores[i] = cosine(queryVecs[i], v)
		}
		if scores[0] < 0.7 || scores[0] <= scores[1]+0.1 || scores[0] <= scores[2]+0.1 {
			t.Errorf("%s against the model card's queries: %.3f, want the speech query far ahead", file, scores)
		}
		t.Logf("%s: cosine %.6f to the WAV, %.3f against %q", file, c, scores, queries)
	}
}
