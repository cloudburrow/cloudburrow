//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/base64"
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

// multimodalModel is the local CLIP model's catalogue identity.
const multimodalModel = "clip-vit-base-patch32-onnx-community"

// solidPNG is a 64x64 PNG of one colour.
func solidPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func cosine32(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / math.Sqrt(na*nb)
}

// loadMultimodal downloads the CLIP model through the console's Local models
// actions, the product's own pinned, checksummed fetch, and loads it.
func loadMultimodal(ctx context.Context, t *testing.T, addr string) {
	t.Helper()
	m := consoleLocalModelState(t, addr, multimodalModel)
	if !m.Pinned {
		t.Fatalf("%s is not offered for download: %+v", multimodalModel, m)
	}
	if m.State != "downloaded" {
		if code, body := consoleDo(t, addr, http.MethodPost, "/api/ai/local-models/"+multimodalModel+"/download",
			fmt.Sprintf(`{"confirmBytes":%d}`, m.Bytes)); code != http.StatusOK && code != http.StatusAccepted {
			t.Fatalf("download = %d %s", code, body)
		}
		for m.State != "downloaded" {
			if ctx.Err() != nil || m.Error != "" {
				t.Fatalf("download did not finish: %+v (%v)", m, ctx.Err())
			}
			time.Sleep(2 * time.Second)
			m = consoleLocalModelState(t, addr, multimodalModel)
		}
	}
	if !m.Loaded {
		if code, body := consoleDo(t, addr, http.MethodPost, "/api/ai/local-models/"+multimodalModel+"/load", "{}"); code != http.StatusOK {
			t.Fatalf("load = %d %s", code, body)
		}
	}
}

// covers: google.cloud.aiplatform.v1.PredictionService/EmbedContent, google.cloud.aiplatform.v1.PredictionService/Predict
//
// TestMultimodalEmbeddingsThroughThePredictionClient (#1454) embeds text and
// images through the official aiplatform PredictionClient with the local
// CLIP model, downloaded and loaded through the Local models actions: the
// image of a red square is closer to "a red square" than to "a blue square",
// and the blue one the other way, by Predict (multimodalembedding's
// instance shape, inline bytes and gs://) and by EmbedContent (inline_data and
// a gs:// file_data from this instance's Cloud Storage) alike. What CLIP has
// no encoder for — a video part, a PDF, 1408 dimensions — is UNIMPLEMENTED,
// and an undocumented dimension INVALID_ARGUMENT.
func TestMultimodalEmbeddingsThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	loadMultimodal(ctx, t, addr)

	red, blue := solidPNG(t, color.RGBA{220, 20, 20, 255}), solidPNG(t, color.RGBA{20, 20, 220, 255})
	bh := bucket(t, h, sc)
	w := bh.Object("blue.png").NewWriter(ctx)
	w.ContentType = "image/png"
	if _, err := w.Write(blue); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	blueURI := "gs://" + w.Attrs().Bucket + "/blue.png"
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + multimodalModel

	predict := func(instance map[string]any, dim int) (*structpb.Struct, error) {
		in, err := structpb.NewValue(instance)
		if err != nil {
			t.Fatal(err)
		}
		params, _ := structpb.NewValue(map[string]any{"dimension": dim})
		resp, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: name, Instances: []*structpb.Value{in}, Parameters: params})
		if err != nil {
			return nil, err
		}
		if len(resp.GetPredictions()) != 1 {
			t.Fatalf("%d predictions", len(resp.GetPredictions()))
		}
		return resp.GetPredictions()[0].GetStructValue(), nil
	}
	vec := func(s *structpb.Struct, field string) []float32 {
		t.Helper()
		l := s.GetFields()[field].GetListValue().GetValues()
		out := make([]float32, len(l))
		for i, v := range l {
			out[i] = float32(v.GetNumberValue())
		}
		if len(out) == 0 {
			t.Fatalf("no %s in %v", field, s)
		}
		return out
	}

	// Predict: one instance with a caption and an image gives both.
	p, err := predict(map[string]any{"text": "a red square",
		"image": map[string]any{"bytesBase64Encoded": base64.StdEncoding.EncodeToString(red), "mimeType": "image/png"}}, 512)
	if err != nil {
		t.Fatalf("Predict text and image: %v", err)
	}
	redText, redImg := vec(p, "textEmbedding"), vec(p, "imageEmbedding")
	if len(redText) != 512 || len(redImg) != 512 {
		t.Fatalf("sizes %d %d", len(redText), len(redImg))
	}
	p, err = predict(map[string]any{"text": "a blue square", "image": map[string]any{"gcsUri": blueURI}}, 512)
	if err != nil {
		t.Fatalf("Predict with a gs:// image: %v", err)
	}
	blueText, blueImg := vec(p, "textEmbedding"), vec(p, "imageEmbedding")
	if m, x := cosine32(redImg, redText), cosine32(redImg, blueText); m <= x {
		t.Errorf("red image: cos to \"a red square\" %.4f, to \"a blue square\" %.4f; want the match closer", m, x)
	}
	if m, x := cosine32(blueImg, blueText), cosine32(blueImg, redText); m <= x {
		t.Errorf("blue image: cos to \"a blue square\" %.4f, to \"a red square\" %.4f; want the match closer", m, x)
	}
	if p, err := predict(map[string]any{"text": "a red square"}, 128); err != nil || len(vec(p, "textEmbedding")) != 128 {
		t.Errorf("Predict dimension 128: %v", err)
	}

	// EmbedContent: the same space, from inline_data and a gs:// file_data.
	embed := func(part *aiplatformpb.Part, dim int32) ([]float32, error) {
		req := &aiplatformpb.EmbedContentRequest{Model: proto.String(name),
			Content: &aiplatformpb.Content{Role: "user", Parts: []*aiplatformpb.Part{part}}}
		if dim > 0 {
			req.EmbedContentConfig = &aiplatformpb.EmbedContentRequest_EmbedContentConfig{OutputDimensionality: &dim}
		}
		resp, err := pc.EmbedContent(ctx, req)
		return resp.GetEmbedding().GetValues(), err
	}
	ecRed, err := embed(&aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: "image/png", Data: red}}}, 0)
	if err != nil {
		t.Fatalf("EmbedContent inline image: %v", err)
	}
	ecBlue, err := embed(&aiplatformpb.Part{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{MimeType: "image/png", FileUri: blueURI}}}, 0)
	if err != nil {
		t.Fatalf("EmbedContent gs:// image: %v", err)
	}
	ecRedText, err := embed(&aiplatformpb.Part{Data: &aiplatformpb.Part_Text{Text: "a red square"}}, 0)
	if err != nil {
		t.Fatalf("EmbedContent text: %v", err)
	}
	if c := cosine32(ecRed, redImg); c < 0.999 {
		t.Errorf("EmbedContent and Predict give different vectors for the same image: cos %.6f", c)
	}
	if m, x := cosine32(ecRed, ecRedText), cosine32(ecBlue, ecRedText); m <= x {
		t.Errorf("EmbedContent: \"a red square\" to the red image %.4f, to the blue %.4f; want the match closer", m, x)
	}

	// Refusals, with the code naming what is missing.
	for label, c := range map[string]struct {
		err  error
		want codes.Code
	}{
		"video part": {func() error {
			_, err := embed(&aiplatformpb.Part{Data: &aiplatformpb.Part_FileData{FileData: &aiplatformpb.FileData{MimeType: "video/mp4", FileUri: "gs://b/v.mp4"}}}, 0)
			return err
		}(), codes.Unimplemented},
		"pdf part": {func() error {
			_, err := embed(&aiplatformpb.Part{Data: &aiplatformpb.Part_InlineData{InlineData: &aiplatformpb.Blob{MimeType: "application/pdf", Data: []byte("%PDF-1.4")}}}, 0)
			return err
		}(), codes.Unimplemented},
		"dimension 1408": {func() error { _, err := predict(map[string]any{"text": "x"}, 1408); return err }(), codes.Unimplemented},
		"dimension 300":  {func() error { _, err := predict(map[string]any{"text": "x"}, 300); return err }(), codes.InvalidArgument},
		"video instance": {func() error {
			_, err := predict(map[string]any{"video": map[string]any{"gcsUri": "gs://b/v.mp4"}}, 512)
			return err
		}(), codes.Unimplemented},
	} {
		if status.Code(c.err) != c.want || !strings.Contains(fmt.Sprint(c.err), multimodalModel) && c.want == codes.Unimplemented {
			t.Errorf("%s: %v, want %v naming %s", label, c.err, c.want, multimodalModel)
		}
	}
}
