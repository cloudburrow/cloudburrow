//go:build compat

package compat

import (
	"context"
	"fmt"
	"image/color"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsHEICAndAnimatedWebPThroughThePredictionClient
// (#1724) loads EmbeddingGemma 2 on the console's Local models page and
// embeds HEIC and HEIF images and an animated WebP through the official
// aiplatform PredictionClient's EmbedContent, as Google lists HEIC, HEIF
// and WebP for gemini-embedding-2. testdata holds shapeImage's shapes,
// written as PNG and encoded with Homebrew's libheif 1.23.7 (x265),
// libwebp 1.6.0 and libavif 1.4.2:
//
//	heif-enc -L triangle.png -o triangle-lossless.heic
//	heif-enc -q 80 circle.png -o circle-lossy.heic
//	img2webp -lossless square.png -lossless circle.png -o square-then-circle.webp
//	avifenc -q 80 circle.png circle.avif
//
// A lossless HEIC is the PNG's vector; a lossy one, sent as image/heic or
// image/heif, is within 0.98 of it and nearest its own description; the
// animated WebP is its first frame, the square's vector, nearest "a blue
// square" and not the circle of its second frame; each is 260 tokens; the
// HEIC uploaded with the official Cloud Storage client and sent as
// file_data with no mime_type is the inline vector; and an AV1-coded HEIF
// (an AVIF sent as image/heif) and a truncated HEIC are INVALID_ARGUMENT
// naming the part. AVIF itself is embedded (#1729, embedavif_test.go).
func TestEmbedContentEmbedsHEICAndAnimatedWebPThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	storageEndpoint := h.Endpoint(EnvStorage)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2

	read := func(file string) []byte {
		t.Helper()
		b, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	inline := func(mt string, data []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: mt, Data: data}}}
	}
	text := func(s string) *aiplatformpb.Part { return &aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: s}} }
	embed := func(task aiplatformpb.EmbedContentRequest_EmbeddingTaskType, parts ...*aiplatformpb.Part) (*aiplatformpb.EmbedContentResponse, error) {
		return pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: parts},
			EmbedContentConfig: &aiplatformpb.EmbedContentRequest_EmbedContentConfig{TaskType: &task},
		})
	}
	image := func(what string, part *aiplatformpb.Part) []float32 {
		t.Helper()
		r, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, part)
		if err != nil {
			t.Fatalf("%s: EmbedContent: %v", what, err)
		}
		v := r.GetEmbedding().GetValues()
		// 256 soft tokens, the image markers, BOS and EOS.
		if len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 || r.GetUsageMetadata().GetPromptTokenCount() != 260 {
			t.Errorf("%s: %d values, norm %.6f, %d tokens, want 768, 1 and 260", what, len(v), norm(v), r.GetUsageMetadata().GetPromptTokenCount())
		}
		return v
	}
	query := func(s string) []float32 {
		t.Helper()
		r, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY, text(s))
		if err != nil {
			t.Fatalf("query %q: %v", s, err)
		}
		return r.GetEmbedding().GetValues()
	}

	triangle := image("triangle PNG", inline("image/png", shapeImage(t, "triangle", color.RGBA{20, 180, 40, 255})))
	square := image("square PNG", inline("image/png", shapeImage(t, "square", color.RGBA{20, 40, 220, 255})))
	circle := image("circle PNG", inline("image/png", shapeImage(t, "circle", color.RGBA{220, 20, 30, 255})))
	circleHEIC := read("circle-lossy.heic")
	got := map[string][]float32{
		"lossless HEIC": image("lossless HEIC", inline("image/heic", read("triangle-lossless.heic"))),
		"lossy HEIC":    image("lossy HEIC", inline("image/heic", circleHEIC)),
		"lossy HEIF":    image("lossy HEIF", inline("image/heif", circleHEIC)),
		"animated WebP": image("animated WebP", inline("image/webp", read("square-then-circle.webp"))),
	}
	t.Logf("cosine with the PNG: lossless HEIC %.6f, lossy HEIC %.6f, animated WebP %.6f",
		cosine(got["lossless HEIC"], triangle), cosine(got["lossy HEIC"], circle), cosine(got["animated WebP"], square))
	if c := cosine(got["lossless HEIC"], triangle); c < 0.99999 {
		t.Errorf("lossless HEIC: cosine with the PNG %.6f, want the same vector", c)
	}
	if c := cosine(got["lossy HEIC"], circle); c < 0.98 {
		t.Errorf("lossy HEIC: cosine with the PNG %.6f, want at least 0.98", c)
	}
	if c := cosine(got["lossy HEIF"], got["lossy HEIC"]); c < 0.99999 {
		t.Errorf("image/heif: cosine with image/heic %.6f, want the same vector", c)
	}
	// The first frame is the square, drawn losslessly.
	if c := cosine(got["animated WebP"], square); c < 0.99999 {
		t.Errorf("animated WebP: cosine with its first frame's PNG %.6f, want the same vector", c)
	}
	queries := map[string][]float32{"a green triangle": query("a green triangle"), "a blue square": query("a blue square"), "a red circle": query("a red circle")}
	for what, want := range map[string]string{"lossless HEIC": "a green triangle", "lossy HEIC": "a red circle", "animated WebP": "a blue square"} {
		for q, qv := range queries {
			if q != want && cosine(qv, got[what]) >= cosine(queries[want], got[what]) {
				t.Errorf("%s: cosine with %q %.4f >= with %q %.4f", what, q, cosine(qv, got[what]), want, cosine(queries[want], got[what]))
			}
		}
	}
	// #1651's two-frame animated WebP, refused before #1724, is embedded too.
	if r, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, inline("image/webp", read("animated.webp"))); err != nil || len(r.GetEmbedding().GetValues()) != 768 {
		t.Errorf("animated.webp: %v", err)
	}
	// Text with a HEIC is one vector.
	if r, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, text("A drawing:"), inline("image/heic", circleHEIC)); err != nil ||
		len(r.GetEmbedding().GetValues()) != 768 || r.GetUsageMetadata().GetPromptTokenCount() <= 260 {
		t.Errorf("text and a HEIC: %v %v", r.GetUsageMetadata(), err)
	}

	// The lossy HEIC in this instance's Cloud Storage, as file_data with no
	// mime_type: the object's content type stands in.
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageEndpoint+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	bucket := fmt.Sprintf("embed-heic-%d", time.Now().UnixNano())
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	w := sc.Bucket(bucket).Object("circle.heic").NewWriter(ctx)
	w.ContentType = "image/heic"
	if _, err := w.Write(circleHEIC); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.Bucket(bucket).Object("circle.heic").Delete(context.Background())
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	fd := image("file_data HEIC", &aiplatformpb.Part{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{FileUri: "gs://" + bucket + "/circle.heic"}}})
	if c := cosine(fd, got["lossy HEIC"]); c < 0.99999 {
		t.Errorf("file_data HEIC: cosine with inline_data %.6f", c)
	}

	avif := read("circle.avif")
	for what, c := range map[string]struct {
		part *aiplatformpb.Part
		want string
	}{
		"AVIF as HEIF":   {inline("image/heif", avif), "content.parts[0].inline_data: the data is not a valid image/heif image"},
		"truncated HEIC": {inline("image/heic", circleHEIC[:len(circleHEIC)/2]), "content.parts[0].inline_data: the data is not a valid image/heic image"},
	} {
		_, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, c.part)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want INVALID_ARGUMENT naming %q", what, err, c.want)
		}
	}
}
