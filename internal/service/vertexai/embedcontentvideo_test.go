package vertexai

import (
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

func testdataB64(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../embedding/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// TestEmbedContentVideoAndPDFOnEmbeddingGemma2 (#1722, #1723): a video part
// is its frames sampled as video_metadata says, and a PDF part is its pages,
// each an image, each followed with document_ocr by its text; what Google
// does not take, or this build cannot decode, is refused naming it.
func TestEmbedContentVideoAndPDFOnEmbeddingGemma2(t *testing.T) {
	s := NewServer(nil)
	e := &multimodalEmbedder{}
	s.SwapModelEmbedder(localai.EmbeddingGemma2ONNXID, e, nil)
	video := testdataB64(t, "turtle-h264.mp4")
	mov := testdataB64(t, "turtle-h265.mov")
	pdf := testdataB64(t, "two-pages.pdf")
	inline := func(mt, data string) string {
		return `{"inlineData":{"mimeType":"` + mt + `","data":"` + data + `"}}`
	}
	post := func(body string) *strings.Builder {
		t.Helper()
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var b strings.Builder
		b.WriteString(rec.Body.String())
		return &b
	}

	post(`{"content":{"parts":[` + inline("video/mp4", video) + `]}}`)
	if len(e.in.Parts) != 1 || len(e.in.Parts[0].Video) != 3 {
		t.Fatalf("video: embedder got %d parts, %d frames; want 1 video of 3 frames", len(e.in.Parts), len(e.in.Parts[0].Video))
	}
	// VP9 and AV1 (#1731).
	for _, f := range []string{"turtle-vp9.mp4", "turtle-av1.mp4"} {
		post(`{"content":{"parts":[` + inline("video/mp4", testdataB64(t, f)) + `]}}`)
		if len(e.in.Parts) != 1 || len(e.in.Parts[0].Video) != 3 {
			t.Fatalf("%s: embedder got %d parts; want 1 video of 3 frames", f, len(e.in.Parts))
		}
	}
	post(`{"content":{"parts":[{"text":"a turtle"},` + inline("video/quicktime", mov) + `]}}`)
	if len(e.in.Parts) != 2 || e.in.Parts[0].Text != "a turtle" || len(e.in.Parts[1].Video) != 3 {
		t.Fatalf("text and MOV: embedder got %+v", e.in.Parts)
	}
	post(`{"content":{"parts":[{"inlineData":{"mimeType":"video/mp4","data":"` + video + `"},` +
		`"videoMetadata":{"startOffset":"1s","endOffset":"3s","fps":2}}]}}`)
	if len(e.in.Parts) != 1 || len(e.in.Parts[0].Video) != 4 {
		t.Fatalf("video_metadata 1s to 3s at 2 fps: embedder got %d frames, want 4", len(e.in.Parts[0].Video))
	}

	post(`{"content":{"parts":[` + inline("application/pdf", pdf) + `]}}`)
	if len(e.in.Parts) != 2 || e.in.Parts[0].Image == nil || e.in.Parts[1].Image == nil {
		t.Fatalf("PDF: embedder got %+v, want 2 pages as images", e.in.Parts)
	}
	post(`{"content":{"parts":[` + inline("application/pdf", pdf) + `]},"embedContentConfig":{"documentOcr":true}}`)
	if len(e.in.Parts) != 4 || e.in.Parts[0].Image == nil || e.in.Parts[1].Text != "Cats sleeping on a couch." ||
		e.in.Parts[2].Image == nil || !strings.Contains(e.in.Parts[3].Text, "sea turtle") {
		t.Fatalf("PDF with document_ocr: embedder got %+v, want each page and its text", e.in.Parts)
	}
	// document_ocr without a PDF changes nothing.
	post(`{"content":{"parts":[{"text":"x"}]},"embedContentConfig":{"documentOcr":true}}`)

	for name, c := range map[string]struct{ body, want string }{
		"two videos": {`{"content":{"parts":[` + inline("video/mp4", video) + `,` + inline("video/mp4", video) + `]}}`, "at most 1 video"},
		"vp9 10-bit": {`{"content":{"parts":[` + inline("video/mp4", testdataB64(t, "turtle-vp9-10bit.mp4")) + `]}}`, "VP9 profile 2"},
		"fps":        {`{"content":{"parts":[{"inlineData":{"mimeType":"video/mp4","data":"` + video + `"},"videoMetadata":{"fps":25}}]}}`, "(0.0, 24.0]"},
		"frames":     {`{"content":{"parts":[{"inlineData":{"mimeType":"video/mp4","data":"` + video + `"},"videoMetadata":{"fps":24,"endOffset":"200s"}}]}}`, ""},
		"two pdfs":   {`{"content":{"parts":[` + inline("application/pdf", pdf) + `,` + inline("application/pdf", pdf) + `]}}`, "at most 1 PDF"},
		"7 pages":    {`{"content":{"parts":[` + inline("application/pdf", testdataB64(t, "seven-pages.pdf")) + `]}}`, "at most 6 pages"},
		"scan ocr":   {`{"content":{"parts":[` + inline("application/pdf", testdataB64(t, "no-text.pdf")) + `]},"embedContentConfig":{"documentOcr":true}}`, "no OCR engine"},
		"audio track": {`{"content":{"parts":[` + inline("video/mp4", video) + `]},"embedContentConfig":{"audioTrackExtraction":true}}`,
			"audio_track_extraction"},
	} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, c.body)
		if name == "frames" {
			// 3 seconds at 24 fps is 72 frames, within Google's 120.
			if rec.Code != http.StatusOK || len(e.in.Parts[0].Video) != 72 {
				t.Errorf("frames: status %d, %d frames: %s", rec.Code, len(e.in.Parts[0].Video), rec.Body)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: status %d, want 400 naming %q: %s", name, rec.Code, c.want, rec.Body)
		}
	}
}

// TestEmbedContentAudioTrackExtraction (#1732): with
// embed_content_config.audio_track_extraction, a video's audio track is a
// clip of its own right after its frames, between video_metadata's offsets;
// a video with no sound track is its frames alone; the track counts
// against the request's 180 seconds of audio.
func TestEmbedContentAudioTrackExtraction(t *testing.T) {
	s := NewServer(nil)
	e := &audioEmbedder{}
	s.SwapModelEmbedder(localai.EmbeddingGemma2ONNXID, e, nil)
	post := func(body string) {
		t.Helper()
		if rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, body); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	}
	aac := testdataB64(t, "va-aac.mp4")
	post(`{"content":{"parts":[{"text":"a clip"},{"inlineData":{"mimeType":"video/mp4","data":"` + aac + `"}}]},"embedContentConfig":{"audioTrackExtraction":true}}`)
	if len(e.in.Parts) != 3 || e.in.Parts[0].Text != "a clip" || len(e.in.Parts[1].Video) != 1 || len(e.in.Parts[2].Audio) != 24000 {
		t.Fatalf("video with its audio track: embedder got %d parts, want text, 1 frame and 24000 samples", len(e.in.Parts))
	}
	post(`{"content":{"parts":[{"inlineData":{"mimeType":"video/mp4","data":"` + aac + `"},"videoMetadata":{"startOffset":"0.5s","endOffset":"1s"}}]},"embedContentConfig":{"audioTrackExtraction":true}}`)
	if len(e.in.Parts) != 2 || len(e.in.Parts[1].Audio) != 8000 {
		t.Fatalf("0.5s to 1s: embedder got %d parts, want the frame and 8000 samples", len(e.in.Parts))
	}
	// Off (Google's default), the frames alone.
	post(`{"content":{"parts":[{"inlineData":{"mimeType":"video/mp4","data":"` + aac + `"}}]}}`)
	if len(e.in.Parts) != 1 || e.in.Parts[0].Video == nil {
		t.Fatalf("without audio_track_extraction: embedder got %d parts", len(e.in.Parts))
	}
	// No sound track: the frames alone.
	post(`{"content":{"parts":[{"inlineData":{"mimeType":"video/mp4","data":"` + testdataB64(t, "va-silent.mp4") + `"}}]},"embedContentConfig":{"audioTrackExtraction":true}}`)
	if len(e.in.Parts) != 1 || e.in.Parts[0].Video == nil {
		t.Fatalf("no sound track: embedder got %d parts", len(e.in.Parts))
	}
	for name, c := range map[string]struct{ body, want string }{
		"opus": {`{"content":{"parts":[{"inlineData":{"mimeType":"video/mp4","data":"` + testdataB64(t, "va-opus.mp4") + `"}}]},"embedContentConfig":{"audioTrackExtraction":true}}`,
			`audio_track_extraction: the video's audio track: its codec \"Opus\" is not decoded`},
	} {
		rec := postEmbed(t, s, "v1", localai.EmbeddingGemma2ONNXID, c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: status %d, want 400 naming %q: %s", name, rec.Code, c.want, rec.Body)
		}
	}
}
