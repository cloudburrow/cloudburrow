//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// embeddingModel2 is the multimodal embedding model (#1454): a community ONNX
// conversion of EmbeddingGemma 2, NOT an official Google artifact.
const embeddingModel2 = "embeddinggemma-2-onnx-community"

// shapeImage is a 768 x 768 PNG of one coloured shape on white: the size the
// processor keeps unchanged, 2304 patches and 256 soft tokens.
func shapeImage(t *testing.T, kind string, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 768, 768))
	for y := 0; y < 768; y++ {
		for x := 0; x < 768; x++ {
			dx, dy := x-384, y-384
			in := dx*dx+dy*dy < 250*250
			switch kind {
			case "square":
				in = dx > -220 && dx < 220 && dy > -220 && dy < 220
			case "triangle":
				in = y > 150 && y < 620 && math.Abs(float64(dx)) < float64(y-150)/2
			}
			if in {
				img.Set(x, y, c)
			} else {
				img.Set(x, y, color.RGBA{255, 255, 255, 255})
			}
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// loadEmbeddingModel2 downloads EmbeddingGemma 2 (about 541 MB, once per
// state directory) and loads it, through the console's Local models API.
func loadEmbeddingModel2(t *testing.T, ctx context.Context, addr string) {
	t.Helper()
	act := func(action, body string) (int, string) {
		return consoleDo(t, addr, http.MethodPost, "/api/ai/local-models/"+embeddingModel2+"/"+action, body)
	}
	m := consoleLocalModelState(t, addr, embeddingModel2)
	if !m.Pinned || !m.Loadable {
		t.Fatalf("%s on the Local models page: %+v, want pinned and loadable", embeddingModel2, m)
	}
	if m.State != "downloaded" {
		if code, body := act("download", fmt.Sprintf(`{"confirmBytes":%d}`, m.Bytes)); code != http.StatusOK {
			t.Fatalf("download = %d %s", code, body)
		}
		for {
			s := consoleLocalModelState(t, addr, embeddingModel2)
			if s.State == "downloaded" {
				break
			}
			if s.State != "downloading" {
				t.Fatalf("download ended %+v", s)
			}
			select {
			case <-ctx.Done():
				t.Fatalf("download did not finish: %+v", s)
			case <-time.After(2 * time.Second):
			}
		}
	}
	if !consoleLocalModelState(t, addr, embeddingModel2).Loaded {
		if code, body := act("load", "{}"); code != http.StatusOK {
			t.Fatalf("load = %d %s", code, body)
		}
	}
}

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent
//
// TestEmbedContentEmbedsImagesThroughThePredictionClient (#1454) loads
// EmbeddingGemma 2 on the console's Local models page and embeds images
// through the official aiplatform PredictionClient's EmbedContent, over gRPC,
// as Google documents for gemini-embedding-2: an inline_data image part, alone
// or with text parts in one Content, gives one 768-value unit vector in the
// space of the model's text vectors. Three shapes are each nearest their own
// text description; the token count is the image's 256 soft tokens plus its
// begin and end markers and the BOS and EOS tokens; Matryoshka sizes apply;
// the console's Embeddings API returns the same vector for the same image;
// and what is not served (file_data, a video, document OCR, an image on the
// text-only model) is INVALID_ARGUMENT naming the field.
func TestEmbedContentEmbedsImagesThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	loadEmbeddingModel2(t, ctx, addr)
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel2

	imagePart := func(data []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: "image/png", Data: data}}}
	}
	textPart := func(s string) *aiplatformpb.Part { return &aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: s}} }
	embedParts := func(task aiplatformpb.EmbedContentRequest_EmbeddingTaskType, dim int32, parts ...*aiplatformpb.Part) *aiplatformpb.EmbedContentResponse {
		t.Helper()
		cfg := &aiplatformpb.EmbedContentRequest_EmbedContentConfig{TaskType: &task}
		if dim > 0 {
			cfg.OutputDimensionality = &dim
		}
		resp, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(name), Content: &aiplatformpb.Content{Role: "user", Parts: parts}, EmbedContentConfig: cfg,
		})
		if err != nil {
			t.Fatalf("EmbedContent: %v", err)
		}
		return resp
	}

	shapes := []struct {
		name, kind string
		c          color.RGBA
	}{
		{"a red circle", "circle", color.RGBA{220, 20, 30, 255}},
		{"a blue square", "square", color.RGBA{20, 40, 220, 255}},
		{"a green triangle", "triangle", color.RGBA{20, 180, 40, 255}},
	}
	pngs := make([][]byte, len(shapes))
	images := make([][]float32, len(shapes))
	queries := make([][]float32, len(shapes))
	for i, s := range shapes {
		pngs[i] = shapeImage(t, s.kind, s.c)
		r := embedParts(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, 0, imagePart(pngs[i]))
		images[i] = r.GetEmbedding().GetValues()
		if len(images[i]) != 768 || math.Abs(norm(images[i])-1) > 1e-3 {
			t.Fatalf("%s image: %d values, norm %.6f", s.name, len(images[i]), norm(images[i]))
		}
		if n := r.GetUsageMetadata().GetPromptTokenCount(); n != 260 {
			t.Errorf("%s image: prompt token count %d, want 260 (256 soft tokens, 2 image markers, BOS, EOS)", s.name, n)
		}
		queries[i] = embedParts(aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY, 0, textPart(s.name)).GetEmbedding().GetValues()
	}
	for i, q := range queries {
		for j := range images {
			if j != i && cosine(q, images[j]) >= cosine(q, images[i]) {
				t.Errorf("%q: cosine with %s's image %.4f >= with its own %.4f", shapes[i].name, shapes[j].kind,
					cosine(q, images[j]), cosine(q, images[i]))
			}
		}
	}
	// The same image twice is the same vector: nothing is sampled.
	if again := embedParts(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, 0, imagePart(pngs[0])).GetEmbedding().GetValues(); cosine(again, images[0]) < 0.99999 {
		t.Errorf("the same image twice: cosine %.6f", cosine(again, images[0]))
	}
	// Matryoshka: the renormalised prefix.
	small := embedParts(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, 256, imagePart(pngs[0])).GetEmbedding().GetValues()
	if len(small) != 256 || math.Abs(norm(small)-1) > 1e-3 || cosine(small, images[0][:256]) < 0.9999 {
		t.Errorf("256 dimensions: %d values, norm %.4f", len(small), norm(small))
	}
	// Text and an image in one Content are one vector, near both.
	mixed := embedParts(aiplatformpb.EmbedContentRequest_RETRIEVAL_DOCUMENT, 0, textPart("A drawing:"), imagePart(pngs[1]))
	mv := mixed.GetEmbedding().GetValues()
	if len(mv) != 768 || mixed.GetUsageMetadata().GetPromptTokenCount() <= 260 || cosine(mv, images[1]) > 0.9999 || cosine(mv, queries[1]) <= cosine(mv, queries[0]) {
		t.Errorf("text and image: %d values, %d tokens, cosine with the image %.4f, with its query %.4f, with another %.4f",
			len(mv), mixed.GetUsageMetadata().GetPromptTokenCount(), cosine(mv, images[1]), cosine(mv, queries[1]), cosine(mv, queries[0]))
	}

	// Text on this model through Predict is EmbedContent's vector.
	in, _ := structpb.NewValue(map[string]any{"content": shapes[0].name, "task_type": "RETRIEVAL_QUERY"})
	pr, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: name, Instances: []*structpb.Value{in}})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	var pv []float32
	for _, x := range pr.GetPredictions()[0].GetStructValue().GetFields()["embeddings"].GetStructValue().GetFields()["values"].GetListValue().GetValues() {
		pv = append(pv, float32(x.GetNumberValue()))
	}
	if cosine(pv, queries[0]) < 0.9999 || pr.GetModel() != embeddingModel2 || !strings.Contains(pr.GetModelDisplayName(), "NOT an official Google artifact") {
		t.Errorf("Predict: cosine with EmbedContent %.6f, model %q (%q)", cosine(pv, queries[0]), pr.GetModel(), pr.GetModelDisplayName())
	}

	// The console's Embeddings screen sends the same request and gets the same vector.
	code, body := consoleDo(t, addr, http.MethodPost, "/api/ai/embeddings?method=embedContent&model="+embeddingModel2,
		`{"content":{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"`+base64.StdEncoding.EncodeToString(pngs[2])+`"}}]},"embedContentConfig":{"taskType":"RETRIEVAL_DOCUMENT"}}`)
	var got struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
		UsageMetadata struct {
			PromptTokenCount int `json:"promptTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil {
		t.Fatalf("console :embedContent = %d %s", code, body)
	}
	if cosine(got.Embedding.Values, images[2]) < 0.9999 || got.UsageMetadata.PromptTokenCount != 260 {
		t.Errorf("console: cosine with the SDK's %.6f, %d tokens", cosine(got.Embedding.Values, images[2]), got.UsageMetadata.PromptTokenCount)
	}

	ocr := true
	for what, c := range map[string]struct {
		model string
		parts []*aiplatformpb.Part
		cfg   *aiplatformpb.EmbedContentRequest_EmbedContentConfig
		field string
	}{
		"file_data": {name, []*aiplatformpb.Part{{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{MimeType: "image/png", FileUri: "gs://bucket/shape.png"}}}}, nil, "file_data"},
		"video":     {name, []*aiplatformpb.Part{{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: "video/mp4", Data: []byte{0}}}}}, nil, "inline_data.mime_type"},
		"ocr":       {name, []*aiplatformpb.Part{textPart("x")}, &aiplatformpb.EmbedContentRequest_EmbedContentConfig{DocumentOcr: &ocr}, "document_ocr"},
		"text-only model": {"projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel,
			[]*aiplatformpb.Part{imagePart(pngs[0])}, nil, "content.parts[0]"},
	} {
		_, err := pc.EmbedContent(ctx, &aiplatformpb.EmbedContentRequest{
			Model: proto.String(c.model), Content: &aiplatformpb.Content{Parts: c.parts}, EmbedContentConfig: c.cfg})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: %v, want INVALID_ARGUMENT naming %q", what, err, c.field)
		}
	}
}
