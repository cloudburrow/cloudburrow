package vertexai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// callsEmbedder records every Embed call's inputs and dimension.
type callsEmbedder struct {
	fakeEmbedder
	mu    sync.Mutex
	calls [][]embedding.Input
	dims  []int
}

func (*callsEmbedder) Model() string      { return localai.EmbeddingGemma2ONNXID }
func (*callsEmbedder) EmbedsImages() bool { return true }

func (c *callsEmbedder) Embed(ctx context.Context, in []embedding.Input, dim int, auto bool) ([]embedding.Output, error) {
	c.mu.Lock()
	c.calls, c.dims = append(c.calls, in), append(c.dims, dim)
	c.mu.Unlock()
	return c.fakeEmbedder.Embed(ctx, in, dim, auto)
}

func testGIF(t *testing.T) string {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, 20, 10), color.Palette{color.Black, color.White})
	img.SetColorIndex(3, 3, 1)
	var b bytes.Buffer
	if err := gif.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

type mmPrediction struct {
	TextEmbedding   []float32 `json:"textEmbedding"`
	ImageEmbedding  []float32 `json:"imageEmbedding"`
	VideoEmbeddings []struct {
		StartOffsetSec int       `json:"startOffsetSec"`
		EndOffsetSec   int       `json:"endOffsetSec"`
		Embedding      []float32 `json:"embedding"`
	} `json:"videoEmbeddings"`
}

// TestMultimodalEmbeddingPredict (#1651): multimodalembedding@001's
// :predict on EmbeddingGemma 2: text, image and video in one instance, each
// its own embedding, the video one per videoSegmentConfig interval, with
// Google's documented limits and errors.
func TestMultimodalEmbeddingPredict(t *testing.T) {
	s := NewServer(nil)
	e := &callsEmbedder{}
	s.SwapModelEmbedder(localai.EmbeddingGemma2ONNXID, e, nil)
	video := testdataB64(t, "testsrc-10s-h264.mp4")
	post := func(body string) (int, string) {
		t.Helper()
		rec := postPredict(t, s, localai.EmbeddingGemma2ONNXID, body)
		return rec.Code, rec.Body.String()
	}
	ok := func(body string) mmPrediction {
		t.Helper()
		code, out := post(body)
		if code != http.StatusOK {
			t.Fatalf("status %d: %s", code, out)
		}
		var r struct{ Predictions []mmPrediction }
		if err := json.Unmarshal([]byte(out), &r); err != nil || len(r.Predictions) != 1 {
			t.Fatalf("response %s: %v", out, err)
		}
		return r.Predictions[0]
	}

	p := ok(`{"instances":[{"text":"a test pattern","image":{"bytesBase64Encoded":"` + testPNG(t, 32, 32) + `"}}],"parameters":{"dimension":256}}`)
	if len(p.TextEmbedding) != 256 || len(p.ImageEmbedding) != 256 || p.VideoEmbeddings != nil {
		t.Fatalf("text and image at 256: %d, %d, %v", len(p.TextEmbedding), len(p.ImageEmbedding), p.VideoEmbeddings)
	}
	last := e.calls[len(e.calls)-1]
	if len(last) != 2 || last[0].Content != "a test pattern" || last[1].Parts[0].Image == nil {
		t.Fatalf("embedder got %+v", last)
	}
	// GIF and BMP, which multimodalembedding@001 documents.
	for _, img := range []string{testGIF(t), testdataB64(t, "pattern-24.bmp")} {
		if p := ok(`{"instances":[{"image":{"bytesBase64Encoded":"` + img + `"}}]}`); len(p.ImageEmbedding) != embedding.Dimensions {
			t.Fatalf("image: %d values", len(p.ImageEmbedding))
		}
	}

	// The default: 0 to 120 seconds every 16, ending at the video's end.
	p = ok(`{"instances":[{"video":{"bytesBase64Encoded":"` + video + `"}}],"parameters":{"dimension":128}}`)
	if len(p.VideoEmbeddings) != 1 || p.VideoEmbeddings[0].StartOffsetSec != 0 || p.VideoEmbeddings[0].EndOffsetSec != 10 ||
		len(p.VideoEmbeddings[0].Embedding) != embedding.Dimensions {
		t.Fatalf("default segments: %+v", p.VideoEmbeddings)
	}
	if n := len(e.calls[len(e.calls)-1][0].Parts[0].Video); n != 10 {
		t.Fatalf("default segment: %d frames, want 10 at 1 a second", n)
	}
	p = ok(`{"instances":[{"video":{"bytesBase64Encoded":"` + video + `","videoSegmentConfig":{"startOffsetSec":1,"endOffsetSec":9,"intervalSec":4}}}]}`)
	if len(p.VideoEmbeddings) != 2 || p.VideoEmbeddings[0].StartOffsetSec != 1 || p.VideoEmbeddings[0].EndOffsetSec != 5 ||
		p.VideoEmbeddings[1].StartOffsetSec != 5 || p.VideoEmbeddings[1].EndOffsetSec != 9 {
		t.Fatalf("1 to 9 every 4: %+v", p.VideoEmbeddings)
	}
	if segs := e.calls[len(e.calls)-1]; len(segs) != 2 || len(segs[0].Parts[0].Video) != 4 || len(segs[1].Parts[0].Video) != 4 {
		t.Fatalf("segments' frames: %+v", segs)
	}
	p = ok(`{"instances":[{"video":{"bytesBase64Encoded":"` + video + `","videoSegmentConfig":{"intervalSec":6}}}]}`)
	if len(p.VideoEmbeddings) != 2 || p.VideoEmbeddings[1].StartOffsetSec != 6 || p.VideoEmbeddings[1].EndOffsetSec != 10 {
		t.Fatalf("every 6: %+v", p.VideoEmbeddings)
	}

	// From this instance's Cloud Storage.
	s.UseStorage(fakeObjects{
		"gs://b/v.mp4":   {mustB64(t, video), "video/mp4"},
		"gs://b/i.png":   {mustB64(t, testPNG(t, 8, 8)), "application/octet-stream"},
		"gs://b/v.webm":  {mustB64(t, testdataB64(t, "testsrc-2s-vp9.webm")), ""},
		"gs://b/x.webp":  {mustB64(t, testdataB64(t, "pattern-lossy.webp")), "image/webp"},
		"gs://b/txt.bin": {[]byte("hello"), ""},
		"gs://b/v.avi":   {[]byte("RIFF\x00\x00\x00\x00AVI LIST"), ""},
	})
	p = ok(`{"instances":[{"text":"x","image":{"gcsUri":"gs://b/i.png"},"video":{"gcsUri":"gs://b/v.mp4","videoSegmentConfig":{"intervalSec":5}}}]}`)
	if len(p.TextEmbedding) == 0 || len(p.ImageEmbedding) == 0 || len(p.VideoEmbeddings) != 2 {
		t.Fatalf("gcsUri: %+v", p)
	}
	// WebM (and MKV), which multimodalembedding@001 takes, sniffed.
	p = ok(`{"instances":[{"video":{"gcsUri":"gs://b/v.webm"}}]}`)
	if len(p.VideoEmbeddings) != 1 || p.VideoEmbeddings[0].EndOffsetSec != 2 || len(e.calls[len(e.calls)-1][0].Parts[0].Video) != 2 {
		t.Fatalf("WebM: %+v", p.VideoEmbeddings)
	}

	for _, c := range []struct {
		body string
		code int
		want string
	}{
		{`{"instances":[]}`, 400, "must not be empty"},
		{`{"instances":[{"text":"a"},{"text":"b"}]}`, 400, "one instance per request"},
		{`{"instances":[{"text":" "}]}`, 400, "text must not be empty"},
		{`{"instances":[{"text":"a"}],"parameters":{"dimension":768}}`, 400, "128, 256, 512 or 1408"},
		{`{"instances":[{"text":"a"}],"parameters":{"dimension":1408}}`, 501, "768 values"},
		{`{"instances":[{"text":"a","nope":1}]}`, 400, "malformed"},
		{`{"instances":[{"image":{}}]}`, 400, "set bytesBase64Encoded or gcsUri"},
		{`{"instances":[{"image":{"bytesBase64Encoded":"QQ==","gcsUri":"gs://b/i.png"}}]}`, 400, "not both"},
		{`{"instances":[{"image":{"bytesBase64Encoded":"%%"}}]}`, 400, "not valid base64"},
		{`{"instances":[{"image":{"gcsUri":"gs://b/x.webp"}}]}`, 400, "BMP, GIF, JPEG or PNG"},
		{`{"instances":[{"image":{"gcsUri":"https://example.com/i.png"}}]}`, 400, "gs://{bucket}/{object}"},
		{`{"instances":[{"image":{"gcsUri":"gs://b/missing.png"}}]}`, 404, "not found"},
		{`{"instances":[{"video":{"gcsUri":"gs://b/v.avi"}}]}`, 501, "AVI container"},
		{`{"instances":[{"video":{"gcsUri":"gs://b/txt.bin"}}]}`, 400, "is not supported"},
		{`{"instances":[{"video":{"gcsUri":"gs://b/v.mp4","videoSegmentConfig":{"intervalSec":3}}}]}`, 400, "minimum of 4"},
		{`{"instances":[{"video":{"gcsUri":"gs://b/v.mp4","videoSegmentConfig":{"startOffsetSec":-1}}}]}`, 400, "must not be negative"},
		{`{"instances":[{"video":{"gcsUri":"gs://b/v.mp4","videoSegmentConfig":{"startOffsetSec":5,"endOffsetSec":5}}}]}`, 400, "must be after"},
		{`{"instances":[{"video":{"gcsUri":"gs://b/v.mp4","videoSegmentConfig":{"startOffsetSec":11}}}]}`, 400, "past the video's end"},
		{`{"instances":[{"video":{"gcsUri":"gs://b/v.mp4","videoSegmentConfig":{"intervalSec":4.5}}}]}`, 400, "malformed"},
	} {
		code, out := post(c.body)
		if code != c.code || !strings.Contains(out, c.want) {
			t.Errorf("%s: status %d %s, want %d naming %q", c.body, code, out, c.code, c.want)
		}
	}

	// multimodalembedding@001 itself is not substituted; the error says
	// where its request is served.
	code, out := post(`{"instances":[{"text":"a"}]}`)
	if code != 200 {
		t.Fatalf("text alone: %d %s", code, out)
	}
	rec := postPredict(t, s, "multimodalembedding@001", `{"instances":[{"text":"a"}]}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), localai.EmbeddingGemma2ONNXID) {
		t.Fatalf("multimodalembedding@001: %d %s", rec.Code, rec.Body)
	}
	// EmbeddingGemma embeds text only.
	s.SetEmbedder(fakeEmbedder{}, nil)
	rec = postPredict(t, s, localai.EmbeddingGemmaONNXID, `{"instances":[{"text":"a"}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), localai.EmbeddingGemma2ONNXID) {
		t.Fatalf("on EmbeddingGemma: %d %s", rec.Code, rec.Body)
	}
}

func TestSniffMedia(t *testing.T) {
	for _, c := range []struct {
		b    []byte
		want string
	}{
		{[]byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00"), "video/quicktime"},
		{[]byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00"), "video/mp4"},
		{[]byte("FLV\x01\x05"), "video/x-flv"},
		{[]byte("RIFF\x00\x00\x00\x00AVI LIST"), "video/x-msvideo"},
		{[]byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6}, "video/x-ms-wmv"},
		{[]byte("\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01\x42\x82\x88matroska"), "video/x-matroska"},
		{[]byte{0, 0, 1, 0xBA, 0x44}, "video/mpeg"},
		{[]byte("GIF89a"), "image/gif"},
	} {
		if got := sniffMedia(c.b); got != c.want {
			t.Errorf("%q: %s, want %s", c.b, got, c.want)
		}
	}
}
