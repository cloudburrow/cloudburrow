//go:build compat

package compat

import (
	"bytes"
	"context"
	"fmt"
	"image/color"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/storage"
	"golang.org/x/image/bmp"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsWebPAndBMPThroughThePredictionClient (#1651) loads
// EmbeddingGemma 2 on the console's Local models page and embeds WebP and
// BMP images through the official aiplatform PredictionClient's
// EmbedContent, as Google lists them for gemini-embedding-2. testdata holds
// shapeImage's shapes made by cwebp 1.6 (triangle-lossless.webp with
// -lossless -exact, circle-lossy.webp with -q 80), a 1000 x 600 scene
// (scene-lossy.webp, -q 80, resized by the processor) and a two-frame
// animated WebP (img2webp). A lossless WebP and a BMP of a shape's pixels
// are the PNG's vector; a lossy WebP is within 0.98 of it (0.989 in Transformers.js too) and nearest its
// own description; each is 260 tokens; the lossy WebP uploaded with the
// official Cloud Storage client and sent as file_data with no mime_type is
// the inline vector; and an animated WebP, HEIC, HEIF and AVIF are
// INVALID_ARGUMENT naming the part.
func TestEmbedContentEmbedsWebPAndBMPThroughThePredictionClient(t *testing.T) {
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
	image := func(what string, part *aiplatformpb.Part, tokens int32) []float32 {
		t.Helper()
		r, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, part)
		if err != nil {
			t.Fatalf("%s: EmbedContent: %v", what, err)
		}
		v := r.GetEmbedding().GetValues()
		if len(v) != 768 || math.Abs(norm(v)-1) > 1e-3 || r.GetUsageMetadata().GetPromptTokenCount() != tokens {
			t.Errorf("%s: %d values, norm %.6f, %d tokens, want 768, 1 and %d", what, len(v), norm(v), r.GetUsageMetadata().GetPromptTokenCount(), tokens)
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

	trianglePNG := shapeImage(t, "triangle", color.RGBA{20, 180, 40, 255})
	squarePNG := shapeImage(t, "square", color.RGBA{20, 40, 220, 255})
	circlePNG := shapeImage(t, "circle", color.RGBA{220, 20, 30, 255})
	src, err := png.Decode(bytes.NewReader(squarePNG))
	if err != nil {
		t.Fatal(err)
	}
	var squareBMP bytes.Buffer
	if err := bmp.Encode(&squareBMP, src); err != nil {
		t.Fatal(err)
	}
	circleWebP := read("circle-lossy.webp")

	// 256 soft tokens, the image markers, BOS and EOS.
	triangle := image("triangle PNG", inline("image/png", trianglePNG), 260)
	square := image("square PNG", inline("image/png", squarePNG), 260)
	circle := image("circle PNG", inline("image/png", circlePNG), 260)
	got := map[string][]float32{
		"lossless WebP": image("lossless WebP", inline("image/webp", read("triangle-lossless.webp")), 260),
		"BMP":           image("BMP", inline("image/bmp", squareBMP.Bytes()), 260),
		"lossy WebP":    image("lossy WebP", inline("image/webp", circleWebP), 260),
	}
	if c := cosine(got["lossless WebP"], triangle); c < 0.99999 {
		t.Errorf("lossless WebP: cosine with the PNG %.6f, want the same vector", c)
	}
	if c := cosine(got["BMP"], square); c < 0.99999 {
		t.Errorf("BMP: cosine with the PNG %.6f, want the same vector", c)
	}
	// Compression moves the vector: Transformers.js gives 0.9893 for these
	// two files too.
	if c := cosine(got["lossy WebP"], circle); c < 0.98 {
		t.Errorf("lossy WebP: cosine with the PNG %.6f, want at least 0.98", c)
	}
	queries := map[string][]float32{"a green triangle": query("a green triangle"), "a blue square": query("a blue square"), "a red circle": query("a red circle")}
	for what, want := range map[string]string{"lossless WebP": "a green triangle", "BMP": "a blue square", "lossy WebP": "a red circle"} {
		for q, qv := range queries {
			if q != want && cosine(qv, got[what]) >= cosine(queries[want], got[what]) {
				t.Errorf("%s: cosine with %q %.4f >= with %q %.4f", what, q, cosine(qv, got[what]), want, cosine(queries[want], got[what]))
			}
		}
	}
	// A 1000 x 600 lossy WebP is resized as PNG and JPEG are: 36 x 63
	// patches, 252 soft tokens.
	image("1000 x 600 lossy WebP", inline("image/webp", read("scene-lossy.webp")), 256)
	// Text with a WebP is one vector.
	if r, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, text("A drawing:"), inline("image/webp", circleWebP)); err != nil ||
		len(r.GetEmbedding().GetValues()) != 768 || r.GetUsageMetadata().GetPromptTokenCount() <= 260 {
		t.Errorf("text and a WebP: %v %v", r.GetUsageMetadata(), err)
	}

	// The lossy WebP in this instance's Cloud Storage, as file_data with no
	// mime_type: the object's content type stands in.
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageEndpoint+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	bucket := fmt.Sprintf("embed-webp-%d", time.Now().UnixNano())
	if err := sc.Bucket(bucket).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	w := sc.Bucket(bucket).Object("circle.webp").NewWriter(ctx)
	w.ContentType = "image/webp"
	if _, err := w.Write(circleWebP); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() {
		_ = sc.Bucket(bucket).Object("circle.webp").Delete(context.Background())
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	fd := image("file_data WebP", &aiplatformpb.Part{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{FileUri: "gs://" + bucket + "/circle.webp"}}}, 260)
	if c := cosine(fd, got["lossy WebP"]); c < 0.99999 {
		t.Errorf("file_data WebP: cosine with inline_data %.6f", c)
	}

	for what, c := range map[string]struct {
		part *aiplatformpb.Part
		want string
	}{
		"animated WebP": {inline("image/webp", read("animated.webp")), "content.parts[0].inline_data: animated WebP images are not supported"},
		"HEIC":          {inline("image/heic", []byte{0}), "image/heic"},
		"HEIF":          {inline("image/heif", []byte{0}), "image/heif"},
		"AVIF":          {inline("image/avif", []byte{0}), "image/avif"},
	} {
		_, err := embed(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, c.part)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want INVALID_ARGUMENT naming %q", what, err, c.want)
		}
	}
}
