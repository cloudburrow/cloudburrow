package vertexai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// fakeMultimodal maps "red"/"blue" text and red/blue images to the same two
// axes, as a real joint space would put a caption beside its picture.
type fakeMultimodal struct{}

func (fakeMultimodal) Model() string { return localai.CLIPONNXID }
func (fakeMultimodal) Close() error  { return nil }

func axis(red bool, dim int) []float32 {
	v := make([]float32, embedding.CLIPDimensions)
	if red {
		v[0] = 1
	} else {
		v[1] = 1
	}
	return embedding.Truncate(v, dim)
}

func (fakeMultimodal) EmbedText(_ context.Context, text string, dim int) (embedding.Output, error) {
	return embedding.Output{Values: axis(strings.Contains(text, "red"), dim), TokenCount: 3}, nil
}

func (fakeMultimodal) EmbedImage(_ context.Context, img image.Image, dim int) (embedding.Output, error) {
	r, _, b, _ := img.At(0, 0).RGBA()
	return embedding.Output{Values: axis(r > b, dim)}, nil
}

func pngOf(c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func multimodalServer() *Server {
	s := NewServer(nil)
	s.SwapMultimodalEmbedder(fakeMultimodal{}, nil)
	red := pngOf(color.RGBA{255, 0, 0, 255})
	s.SetObjectReader(func(_ context.Context, bucket, object string) (io.ReadCloser, error) {
		if bucket == "b" && object == "red.png" {
			return io.NopCloser(bytes.NewReader(red)), nil
		}
		return nil, apierror.NotFound("gs://%s/%s not found", bucket, object)
	})
	return s
}

func TestMultimodalPredictShapes(t *testing.T) {
	s := multimodalServer()
	blue := base64.StdEncoding.EncodeToString(pngOf(color.RGBA{0, 0, 255, 255}))
	rec := postPredict(t, s, localai.CLIPONNXID,
		`{"instances":[{"text":"a red square","image":{"bytesBase64Encoded":"`+blue+`","mimeType":"image/png"}}],"parameters":{"dimension":256}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Predictions []struct {
			TextEmbedding  []float32 `json:"textEmbedding"`
			ImageEmbedding []float32 `json:"imageEmbedding"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Predictions) != 1 {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	p := got.Predictions[0]
	if len(p.TextEmbedding) != 256 || len(p.ImageEmbedding) != 256 || p.TextEmbedding[0] != 1 || p.ImageEmbedding[1] != 1 {
		t.Fatalf("prediction %v %v", p.TextEmbedding[:2], p.ImageEmbedding[:2])
	}

	// A gs:// image is read from the instance's Cloud Storage.
	rec = postPredict(t, s, localai.CLIPONNXID, `{"instances":[{"image":{"gcsUri":"gs://b/red.png"}}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"imageEmbedding":[1,`) {
		t.Fatalf("gcsUri: %d %s", rec.Code, rec.Body)
	}

	for body, want := range map[string]int{
		`{"instances":[{"text":"x"}],"parameters":{"dimension":1408}}`: http.StatusNotImplemented,
		`{"instances":[{"text":"x"}],"parameters":{"dimension":300}}`:  http.StatusBadRequest,
		`{"instances":[{"video":{"gcsUri":"gs://b/v.mp4"}}]}`:          http.StatusNotImplemented,
		`{"instances":[{"text":"x"},{"text":"y"}]}`:                    http.StatusBadRequest,
		`{"instances":[{}]}`: http.StatusBadRequest,
		`{"instances":[{"image":{"gcsUri":"gs://b/missing.png"}}]}`:                               http.StatusNotFound,
		`{"instances":[{"image":{"bytesBase64Encoded":"` + blue + `","mimeType":"image/jpeg"}}]}`: http.StatusBadRequest,
		`{"instances":[{"content":"x"}]}`:                                                         http.StatusBadRequest,
	} {
		if rec := postPredict(t, s, localai.CLIPONNXID, body); rec.Code != want {
			t.Errorf("%s: status %d, want %d: %s", body, rec.Code, want, rec.Body)
		}
	}
}

func TestMultimodalUnloadedIs501(t *testing.T) {
	s := NewServer(nil)
	if rec := postPredict(t, s, localai.CLIPONNXID, `{"instances":[{"text":"x"}]}`); rec.Code != http.StatusNotImplemented ||
		!strings.Contains(rec.Body.String(), "Local models") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec := postEmbed(t, s, "v1", localai.CLIPONNXID, `{"content":{"parts":[{"text":"x"}]}}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("embedContent status %d: %s", rec.Code, rec.Body)
	}
}

func TestMultimodalEmbedContent(t *testing.T) {
	s := multimodalServer()
	red := pngOf(color.RGBA{255, 0, 0, 255})
	embed := func(req *aiplatformpb.EmbedContentRequest) (*aiplatformpb.EmbedContentResponse, int) {
		t.Helper()
		b, _ := protojson.Marshal(req)
		rec := postEmbed(t, s, "v1", localai.CLIPONNXID, string(b))
		if rec.Code != http.StatusOK {
			return nil, rec.Code
		}
		resp := &aiplatformpb.EmbedContentResponse{}
		if err := protojson.Unmarshal(rec.Body.Bytes(), resp); err != nil {
			t.Fatal(err)
		}
		return resp, rec.Code
	}
	content := func(parts ...*aiplatformpb.Part) *aiplatformpb.EmbedContentRequest {
		return &aiplatformpb.EmbedContentRequest{Content: &aiplatformpb.Content{Parts: parts}}
	}
	text := func(s string) *aiplatformpb.Part { return &aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: s}} }
	inline := func(mime string, b []byte) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: mime, Data: b}}}
	}
	file := func(mime, uri string) *aiplatformpb.Part {
		return &aiplatformpb.Part{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{MimeType: mime, FileUri: uri}}}
	}

	if r, code := embed(content(inline("image/png", red))); code != 200 || len(r.GetEmbedding().GetValues()) != 512 || r.GetEmbedding().GetValues()[0] != 1 {
		t.Fatalf("inline image: %d %v", code, r)
	}
	req := content(file("image/png", "gs://b/red.png"))
	req.EmbedContentConfig = &aiplatformpb.EmbedContentRequest_EmbedContentConfig{OutputDimensionality: proto.Int32(128)}
	if r, code := embed(req); code != 200 || len(r.GetEmbedding().GetValues()) != 128 {
		t.Fatalf("gs:// image: %d %v", code, r)
	}
	if r, code := embed(content(text("a red square"))); code != 200 || r.GetEmbedding().GetValues()[0] != 1 || r.GetUsageMetadata().GetPromptTokenCount() != 3 {
		t.Fatalf("text: %d %v", code, r)
	}
	task := aiplatformpb.EmbedContentRequest_RETRIEVAL_QUERY
	withTask := content(text("x"))
	withTask.TaskType = &task
	for name, c := range map[string]struct {
		req  *aiplatformpb.EmbedContentRequest
		want int
	}{
		"video":      {content(file("video/mp4", "gs://b/v.mp4")), 501},
		"audio":      {content(inline("audio/wav", []byte("RIFF"))), 501},
		"pdf":        {content(inline("application/pdf", []byte("%PDF"))), 501},
		"mixed":      {content(text("x"), inline("image/png", red)), 501},
		"two images": {content(inline("image/png", red), inline("image/png", red)), 501},
		"task type":  {withTask, 501},
		"not image":  {content(inline("text/csv", []byte("a,b"))), 400},
		"bad bytes":  {content(inline("image/png", []byte("nope"))), 400},
		"missing":    {content(file("image/png", "gs://b/none.png")), 404},
		"bad uri":    {content(file("image/png", "https://example.com/x.png")), 400},
		"empty":      {content(), 400},
	} {
		if _, code := embed(c.req); code != c.want {
			t.Errorf("%s: status %d, want %d", name, code, c.want)
		}
	}
}
