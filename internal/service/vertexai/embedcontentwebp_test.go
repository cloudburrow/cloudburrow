package vertexai

import (
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// TestEmbedContentWebPAndBMPOnEmbeddingGemma2 (#1651): WebP (lossless,
// lossy, with alpha) and BMP images are embedded as PNG and JPEG are,
// inline or as file_data, and an animated WebP, HEIC, HEIF and AVIF are
// INVALID_ARGUMENT naming the part.
func TestEmbedContentWebPAndBMPOnEmbeddingGemma2(t *testing.T) {
	e := &multimodalEmbedder{}
	s := NewServer(nil)
	s.SwapModelEmbedder(localai.EmbeddingGemma2ONNXID, e, nil)
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile("../../embedding/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	inline := func(mt, file string) string {
		return `{"content":{"parts":[{"text":"a picture"},{"inlineData":{"mimeType":"` + mt + `","data":"` +
			base64.StdEncoding.EncodeToString(read(file)) + `"}}]}}`
	}
	for _, c := range []struct{ mt, file string }{
		{"image/webp", "pattern-lossless.webp"},
		{"image/webp", "pattern-lossy.webp"},
		{"image/webp", "pattern-alpha.webp"},
		{"image/bmp", "pattern-24.bmp"},
		{"image/bmp", "pattern-4.bmp"},
	} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, inline(c.mt, c.file))
		if rec.Code != http.StatusOK || len(e.in.Parts) != 2 || e.in.Parts[1].Image == nil || e.in.Parts[1].Image.Bounds().Dx() != 32 {
			t.Errorf("%s: status %d: %s", c.file, rec.Code, rec.Body)
		}
	}

	// file_data: the object's content type stands in for a missing mime_type.
	s.UseStorage(fakeObjects{
		"gs://b/pic.webp": {read("pattern-lossy.webp"), "image/webp"},
		"gs://b/pic.bmp":  {read("pattern-24.bmp"), "image/bmp"},
	})
	for _, uri := range []string{"gs://b/pic.webp", "gs://b/pic.bmp"} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, `{"content":{"parts":[{"fileData":{"fileUri":"`+uri+`"}}]}}`)
		if rec.Code != http.StatusOK || len(e.in.Parts) != 1 || e.in.Parts[0].Image == nil {
			t.Errorf("file_data %s: status %d: %s", uri, rec.Code, rec.Body)
		}
	}

	for name, c := range map[string]struct{ body, want string }{
		"animated": {inline("image/webp", "pattern-animated.webp"), "parts[1].inline_data: animated WebP images are not supported"},
		"bad bmp":  {inline("image/bmp", "pattern-lossless.webp"), "not a valid image/bmp"},
		"heif":     {`{"content":{"parts":[{"inlineData":{"mimeType":"image/heif","data":"AA=="}}]}}`, "image/heif"},
		"avif":     {`{"content":{"parts":[{"inlineData":{"mimeType":"image/avif","data":"AA=="}}]}}`, "image/avif"},
	} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: status %d, want 400 naming %q: %s", name, rec.Code, c.want, rec.Body)
		}
	}
}
