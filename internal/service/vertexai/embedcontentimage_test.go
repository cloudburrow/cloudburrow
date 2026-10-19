package vertexai

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// multimodalEmbedder is recordingEmbedder for EmbeddingGemma 2 (#1454).
type multimodalEmbedder struct{ recordingEmbedder }

func (*multimodalEmbedder) Model() string      { return localai.EmbeddingGemma2ONNXID }
func (*multimodalEmbedder) EmbedsImages() bool { return true }

func testPNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func TestEmbedContentImagesOnEmbeddingGemma2(t *testing.T) {
	e := &multimodalEmbedder{}
	s := NewServer(nil)
	// Not loaded: the 501 says how to load it.
	rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, `{"content":{"parts":[{"text":"x"}]}}`)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "Local models") {
		t.Fatalf("not loaded: status %d: %s", rec.Code, rec.Body)
	}
	s.SwapModelEmbedder(localai.EmbeddingGemma2ONNXID, e, nil)
	img := testPNG(t, 64, 48)

	rec = postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID,
		`{"content":{"role":"user","parts":[{"text":"a cat"},{"inlineData":{"mimeType":"image/png","data":"`+img+`"}},{"text":"on a mat"}]},"embedContentConfig":{"taskType":"RETRIEVAL_DOCUMENT","outputDimensionality":256}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	resp := &aiplatformpb.EmbedContentResponse{}
	if err := protojson.Unmarshal(rec.Body.Bytes(), resp); err != nil || len(resp.GetEmbedding().GetValues()) != 256 {
		t.Fatalf("response %s (%v)", rec.Body, err)
	}
	if len(e.in.Parts) != 3 || e.in.Parts[0].Text != "a cat" || e.in.Parts[1].Image == nil ||
		e.in.Parts[1].Image.Bounds().Dx() != 64 || e.in.Parts[2].Text != "on a mat" || e.in.TaskType != "RETRIEVAL_DOCUMENT" {
		t.Fatalf("embedder got %+v", e.in)
	}
	// Text alone on the multimodal model is the joined text, as on EmbeddingGemma.
	if rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, `{"content":{"parts":[{"text":"a"},{"text":"b"}]}}`); rec.Code != http.StatusOK || e.in.Content != "a\nb" || e.in.Parts != nil {
		t.Fatalf("text: status %d, got %+v: %s", rec.Code, e.in, rec.Body)
	}
	// An image alone.
	if rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, `{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"`+img+`"}}]}}`); rec.Code != http.StatusOK || len(e.in.Parts) != 1 {
		t.Fatalf("image: status %d: %s", rec.Code, rec.Body)
	}

	seven := strings.Repeat(`{"inlineData":{"mimeType":"image/png","data":"`+img+`"}},`, 7)
	for name, c := range map[string]struct{ body, field string }{
		"file data":   {`{"content":{"parts":[{"fileData":{"mimeType":"image/png","fileUri":"gs://b/o.png"}}]}}`, "file_data"},
		"video":       {`{"content":{"parts":[{"inlineData":{"mimeType":"video/mp4","data":"AA=="}}]}}`, "video, audio and PDF"},
		"pdf":         {`{"content":{"parts":[{"inlineData":{"mimeType":"application/pdf","data":"AA=="}}]}}`, "inline_data.mime_type"},
		"webp":        {`{"content":{"parts":[{"inlineData":{"mimeType":"image/webp","data":"AA=="}}]}}`, "image/webp"},
		"bad png":     {`{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}`, "parts[0].inline_data"},
		"seven":       {`{"content":{"parts":[` + strings.TrimSuffix(seven, ",") + `]}}`, "at most 6"},
		"ocr":         {`{"content":{"parts":[{"text":"x"}]},"embedContentConfig":{"documentOcr":true}}`, "document_ocr"},
		"audio track": {`{"content":{"parts":[{"text":"x"}]},"embedContentConfig":{"audioTrackExtraction":true}}`, "audio_track_extraction"},
	} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.field) {
			t.Errorf("%s: status %d, want 400 naming %q: %s", name, rec.Code, c.field, rec.Body)
		}
	}

	// EmbeddingGemma still embeds text only, whatever is loaded beside it.
	s.SetEmbedder(&recordingEmbedder{}, nil)
	rec = postEmbed(t, s, "v1", localai.EmbeddingGemmaONNXID, `{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"`+img+`"}}]}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "embeds text only") {
		t.Fatalf("EmbeddingGemma image: status %d: %s", rec.Code, rec.Body)
	}
	// Unloading one model leaves the other served.
	s.SwapModelEmbedder(localai.EmbeddingGemma2ONNXID, nil, nil)
	if !s.EmbedderLoaded() || s.ModelEmbedderLoaded(localai.EmbeddingGemma2ONNXID) {
		t.Fatal("unloading EmbeddingGemma 2 changed EmbeddingGemma")
	}
}

func TestPredictReportsEachModelsDisplayName(t *testing.T) {
	s := NewServer(nil)
	s.SwapModelEmbedder(localai.EmbeddingGemma2ONNXID, &multimodalEmbedder{}, nil)
	resp, err := s.embedPredict(context.Background(), localai.EmbeddingGemma2ONNXID, strings.NewReader(`{"instances":[{"content":"x"}]}`))
	if err != nil || resp["modelDisplayName"] != EmbeddingModel2DisplayName || resp["model"] != localai.EmbeddingGemma2ONNXID {
		t.Fatalf("%v %v", resp, err)
	}
	var _ embedding.Multimodal = (*multimodalEmbedder)(nil)
}
