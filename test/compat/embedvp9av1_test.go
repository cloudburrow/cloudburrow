//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/proto"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsVP9AndAV1ThroughThePredictionClient (#1731) embeds
// VP9 and AV1 video, the two codecs Google lists beside H.264 and H.265
// for gemini-embedding-2 ("Codec: AV1, H264, H265, VP9"), through the
// official aiplatform PredictionClient's EmbedContent. The clips are the
// first 3 seconds of the model card's sea turtle video at 320x180, encoded
// by ffmpeg 9 with libvpx-vp9 (two-pass, hidden alternate reference
// frames) and SVT-AV1, beside the H.264 encoding of the same frames
// (internal/embedding/testdata); both decoders are bit-exact against ffmpeg
// there. Since #1739 VP9 profiles 1 to 3 too: the same frames in 8-bit
// 4:4:4 (profile 1), 10-bit 4:2:0 (profile 2) and 12-bit 4:2:2 (profile
// 3), libvpx-vp9 two-pass from the H.264 clip, decoded by govpx's vp9hbd,
// bit-exact against ffmpeg (internal/embedding's TestVP9ProfilesMatchFFmpeg).
// Each is the H.264 clip's token count, a vector within cosine 0.98
// of the H.264 one (the encoders differ), and nearest the turtle query; the
// console's Embeddings API gives the SDK's vector.
func TestEmbedContentEmbedsVP9AndAV1ThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
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
	video := func(file string) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{
			MimeType: "video/mp4", Data: embeddingTestdata(t, file)}}}
	}
	doc := aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT
	ref := embed(doc, video("turtle-h264.mp4"))
	queries := []string{"cats sleeping on a couch", "a president's speech about serving your country", "a turtle swimming in the ocean"}
	qv := make([][]float32, len(queries))
	for i, q := range queries {
		qv[i] = embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY, &aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: q}}).GetEmbedding().GetValues()
	}
	for _, file := range []string{"turtle-vp9.mp4", "turtle-av1.mp4", "turtle-vp9-444.mp4", "turtle-vp9-10bit.mp4", "turtle-vp9-422-12bit.mp4"} {
		r := embed(doc, video(file))
		v := r.GetEmbedding().GetValues()
		if len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 {
			t.Fatalf("%s: %d values, norm %.6f", file, len(v), norm(v))
		}
		if n, want := r.GetUsageMetadata().GetPromptTokenCount(), ref.GetUsageMetadata().GetPromptTokenCount(); n != want {
			t.Errorf("%s: %d tokens, the H.264 clip's %d", file, n, want)
		}
		c := cosine(v, ref.GetEmbedding().GetValues())
		if c < 0.98 {
			t.Errorf("%s against the H.264 clip: cosine %.4f, want at least 0.98", file, c)
		}
		s := make([]float64, len(queries))
		for i := range queries {
			s[i] = cosine(qv[i], v)
		}
		if s[2] <= s[0]+0.1 || s[2] <= s[1]+0.1 {
			t.Errorf("%s against the queries %q: %.3f, want the turtle query far ahead", file, queries, s)
		}
		t.Logf("%s: %d tokens, cosine %.6f to the H.264 clip, %.3f against %q", file, r.GetUsageMetadata().GetPromptTokenCount(), c, s, queries)

		// The console's Embeddings API, which its Images picker sends.
		code, body := consoleDo(t, addr, http.MethodPost, "/api/ai/embeddings?method=embedContent&model="+embeddingModel2,
			`{"content":{"role":"user","parts":[{"inlineData":{"mimeType":"video/mp4","data":"`+
				base64.StdEncoding.EncodeToString(embeddingTestdata(t, file))+`"}}]},"embedContentConfig":{"taskType":"RETRIEVAL_DOCUMENT"}}`)
		var got struct {
			Embedding struct {
				Values []float32 `json:"values"`
			} `json:"embedding"`
		}
		if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil || cosine(got.Embedding.Values, v) < 0.99999 {
			t.Errorf("%s through the console = %d %.200s", file, code, body)
		}
	}
}
