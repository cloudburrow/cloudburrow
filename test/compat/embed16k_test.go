//go:build compat

package compat

import (
	"context"
	"math"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbeds16KVideoThroughThePredictionClient (#1651) embeds,
// through the official aiplatform PredictionClient's EmbedContent, videos
// whose frames are over 8K UHD, which were UNIMPLEMENTED: Google documents
// no video frame size, and its image limit for gemini-embedding-2 is
// 16384 x 16384. Each sampled frame is now patched as soon as it is
// decoded, its full-size picture released. The sea turtle clip's first
// frame (internal/embedding/testdata/turtle-h264.mp4) scaled to 16384x8704
// (lanczos) and encoded by ffmpeg 9 with libx264, libx265, libvpx-vp9 and
// libsvtav1 (testdata/t16k-*.mp4, one frame each) is within cosine 0.95 of
// the 320x180 clip's first second (the frames' aspect ratios differ, and
// so their patch grids) and nearest the turtle query; and the 8704x4352
// blue frames (testdata/over8k-*.mp4) that were UNIMPLEMENTED embed.
func TestEmbedContentEmbeds16KVideoThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2
	embed := func(what string, task aiplatformpb.EmbedContentRequest_EmbeddingTaskType, part *aiplatformpb.Part) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		r, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{part}},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{TaskType: &task},
		})
		if err != nil {
			t.Fatalf("%s: EmbedContent: %v", what, err)
		}
		if v := r.GetEmbedding().GetValues(); len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
			t.Fatalf("%s: %d values, norm %.6f", what, len(v), norm(v))
		}
		return r
	}
	video := func(file string) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{
			MimeType: "video/mp4", Data: embeddingTestdata(t, file)}}}
	}
	doc := aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT
	first := video("turtle-h264.mp4")
	first.Metadata = &aiplatformpb.Part_VideoMetadata{VideoMetadata: &aiplatformpb.VideoMetadata{EndOffset: durationpb.New(time.Second)}}
	ref := embed("turtle-h264.mp4's first second", doc, first)
	queries := []string{"cats sleeping on a couch", "a president's speech about serving your country", "a turtle swimming in the ocean"}
	qv := make([][]float32, len(queries))
	for i, q := range queries {
		qv[i] = embed(q, aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY, &aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: q}}).GetEmbedding().GetValues()
	}
	for _, c := range []string{"h264", "h265", "vp9", "av1"} {
		file := "t16k-" + c + ".mp4"
		start := time.Now()
		r := embed(file, doc, video(file))
		v := r.GetEmbedding().GetValues()
		cs := cosine(v, ref.GetEmbedding().GetValues())
		if cs < 0.95 {
			t.Errorf("%s against the 320x180 frame: cosine %.4f, want at least 0.95", file, cs)
		}
		s := make([]float64, len(queries))
		for i := range queries {
			s[i] = cosine(qv[i], v)
		}
		if s[2] <= s[0]+0.1 || s[2] <= s[1]+0.1 {
			t.Errorf("%s against the queries %q: %.3f, want the turtle query far ahead", file, queries, s)
		}
		t.Logf("%s (16384x8704): %d tokens (the 320x180 frame's %d), cosine %.6f to it, %.3f against %q, in %v", file,
			r.GetUsageMetadata().GetPromptTokenCount(), ref.GetUsageMetadata().GetPromptTokenCount(), cs, s, queries, time.Since(start).Round(time.Millisecond))

		over := "over8k-" + c + ".mp4"
		r = embed(over, doc, video(over))
		t.Logf("%s (8704x4352): %d tokens", over, r.GetUsageMetadata().GetPromptTokenCount())
	}
}
