package vertexai

import (
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// TestEmbedContentHEICAndAnimatedWebPOnEmbeddingGemma2 (#1724): HEIC and
// HEIF images and an animated WebP (its first frame) are embedded as the
// other images are, inline or as file_data; AVIF, an AV1-coded HEIF and a
// broken HEIC are INVALID_ARGUMENT naming the part.
func TestEmbedContentHEICAndAnimatedWebPOnEmbeddingGemma2(t *testing.T) {
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
	inline := func(mt string, data []byte) string {
		return `{"content":{"parts":[{"text":"a picture"},{"inlineData":{"mimeType":"` + mt + `","data":"` +
			base64.StdEncoding.EncodeToString(data) + `"}}]}}`
	}
	for _, c := range []struct {
		mt, file string
		width    int
	}{
		{"image/heic", "pattern-lossless.heic", 32},
		{"image/heic", "pattern-lossy.heic", 32},
		{"image/heif", "pattern-lossy.heic", 32},
		{"image/heic", "pattern-sequence.heic", 32},
		{"image/webp", "anim-lossy.webp", 32},
		{"image/webp", "anim-offset-alpha.webp", 36},
	} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, inline(c.mt, read(c.file)))
		if rec.Code != http.StatusOK || len(e.in.Parts) != 2 || e.in.Parts[1].Image == nil || e.in.Parts[1].Image.Bounds().Dx() != c.width {
			t.Errorf("%s as %s: status %d: %s", c.file, c.mt, rec.Code, rec.Body)
		}
	}

	// file_data: the object's content type stands in for a missing mime_type.
	s.UseStorage(fakeObjects{
		"gs://b/pic.heic": {read("pattern-lossy.heic"), "image/heic"},
		"gs://b/pic.heif": {read("pattern-lossy.heic"), "image/heif"},
		"gs://b/anim":     {read("anim-lossy.webp"), "image/webp"},
	})
	for _, uri := range []string{"gs://b/pic.heic", "gs://b/pic.heif", "gs://b/anim"} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, `{"content":{"parts":[{"fileData":{"fileUri":"`+uri+`"}}]}}`)
		if rec.Code != http.StatusOK || len(e.in.Parts) != 1 || e.in.Parts[0].Image == nil {
			t.Errorf("file_data %s: status %d: %s", uri, rec.Code, rec.Body)
		}
	}

	heic := read("pattern-lossy.heic")
	for name, c := range map[string]struct{ body, want string }{
		"avif":         {inline("image/avif", read("pattern.avif")), `parts[1].inline_data: mime_type \"image/avif\" is not supported`},
		"avif as heif": {inline("image/heif", read("pattern.avif")), "parts[1].inline_data: the data is not a valid image/heif image"},
		"cut heic":     {inline("image/heic", heic[:len(heic)/2]), "parts[1].inline_data: the data is not a valid image/heic image"},
	} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: status %d, want 400 naming %q: %s", name, rec.Code, c.want, rec.Body)
		}
	}
}
