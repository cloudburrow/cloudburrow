package console

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// unitEmbedder returns e0 of the requested size for every input.
type unitEmbedder struct{}

func (unitEmbedder) Model() string { return localai.EmbeddingGemmaONNXID }
func (unitEmbedder) Close() error  { return nil }
func (unitEmbedder) Embed(_ context.Context, in []embedding.Input, dim int, _ bool) ([]embedding.Output, error) {
	out := make([]embedding.Output, len(in))
	for i := range in {
		v := make([]float32, embedding.Dimensions)
		v[0] = 1
		out[i] = embedding.Output{Values: embedding.Truncate(v, dim), TokenCount: 4}
	}
	return out, nil
}

// TestEmbeddingsScreenSendsBothMethods (#1244): the Embeddings screen's
// relay reaches the real :predict and :embedContent routes of the vertexai
// server, and returns their answers, and their 501 without a runtime,
// unchanged.
func TestEmbeddingsScreenSendsBothMethods(t *testing.T) {
	api := vertexai.NewServer(nil)
	api.SetEmbedder(unitEmbedder{}, nil)
	apiSrv := httptest.NewServer(api.Handler())
	defer apiSrv.Close()
	addr := strings.TrimPrefix(apiSrv.URL, "http://")
	srv := newPlaygroundServer(t, &Playground{Addr: func() string { return addr }, Model: "m",
		EmbeddingModel: localai.EmbeddingGemmaONNXID})

	code, body := getJSON(t, srv, "/api/ai/embeddings")
	var st struct {
		Methods   []string          `json:"methods"`
		Endpoints map[string]string `json:"endpoints"`
	}
	if err := json.Unmarshal([]byte(body), &st); code != 200 || err != nil {
		t.Fatalf("status %d: %s", code, body)
	}
	if strings.Join(st.Methods, ",") != "predict,embedContent,rawPredict" || !strings.HasSuffix(st.Endpoints["embedContent"], ":embedContent") ||
		!strings.HasSuffix(st.Endpoints["rawPredict"], ":rawPredict") {
		t.Fatalf("status lists %v, %v", st.Methods, st.Endpoints)
	}

	resp, out := postJSON(t, srv, "/api/ai/embeddings?method=embedContent",
		`{"content":{"role":"user","parts":[{"text":"hi"}]},"embedContentConfig":{"taskType":"CLUSTERING","outputDimensionality":256}}`)
	var ec struct {
		Embedding struct{ Values []float32 } `json:"embedding"`
	}
	if err := json.Unmarshal([]byte(out), &ec); resp.StatusCode != 200 || err != nil || len(ec.Embedding.Values) != 256 {
		t.Fatalf("embedContent: %d %s", resp.StatusCode, out)
	}
	resp, out = postJSON(t, srv, "/api/ai/embeddings",
		`{"instances":[{"content":"a"},{"content":"b"}],"parameters":{"outputDimensionality":128}}`)
	if resp.StatusCode != 200 || strings.Count(out, `"values"`) != 2 {
		t.Fatalf("predict: %d %s", resp.StatusCode, out)
	}
	// rawPredict (#1284): the :predict body, unwrapped, the same answer.
	resp, out = postJSON(t, srv, "/api/ai/embeddings?method=rawPredict",
		`{"instances":[{"content":"a"}],"parameters":{"outputDimensionality":128}}`)
	if resp.StatusCode != 200 || strings.Count(out, `"values"`) != 1 {
		t.Fatalf("rawPredict: %d %s", resp.StatusCode, out)
	}
	// The API's refusal, verbatim.
	resp, out = postJSON(t, srv, "/api/ai/embeddings?method=embedContent",
		`{"content":{"parts":[{"text":"hi"}]},"embedContentConfig":{"outputDimensionality":300}}`)
	if resp.StatusCode != 400 || !strings.Contains(out, "output_dimensionality") {
		t.Fatalf("bad dimension: %d %s", resp.StatusCode, out)
	}
	if resp, _ := postJSON(t, srv, "/api/ai/embeddings?method=explain", `{}`); resp.StatusCode != 400 {
		t.Fatalf("unknown method relayed: %d", resp.StatusCode)
	}

	api.SetEmbedder(nil, errors.New("not compiled"))
	resp, out = postJSON(t, srv, "/api/ai/embeddings?method=embedContent", `{"content":{"parts":[{"text":"hi"}]}}`)
	if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(out, "not compiled") {
		t.Fatalf("no runtime: %d %s", resp.StatusCode, out)
	}
}

// TestEmbeddingsScreenSendsTheNamedModel (#1305): ?model= sends the request
// for the model the user typed, and the API's 404 for a Vertex model that is
// not served comes back verbatim, naming the one that is; a name that is not
// a model ID is refused before anything is sent.
func TestEmbeddingsScreenSendsTheNamedModel(t *testing.T) {
	api := vertexai.NewServer(nil)
	api.SetEmbedder(unitEmbedder{}, nil)
	apiSrv := httptest.NewServer(api.Handler())
	defer apiSrv.Close()
	addr := strings.TrimPrefix(apiSrv.URL, "http://")
	srv := newPlaygroundServer(t, &Playground{Addr: func() string { return addr }, Model: "m",
		EmbeddingModel: localai.EmbeddingGemmaONNXID})

	code, body := getJSON(t, srv, "/api/ai/embeddings")
	if code != 200 || !strings.Contains(body, `"textembedding-gecko@003"`) || !strings.Contains(body, `"text-embedding-005"`) {
		t.Fatalf("status lacks the not-served names: %d %s", code, body)
	}
	for _, m := range []string{"text-embedding-005", "textembedding-gecko@003"} {
		resp, out := postJSON(t, srv, "/api/ai/embeddings?model="+m, `{"instances":[{"content":"a"}]}`)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(out, localai.EmbeddingGemmaONNXID) ||
			!strings.Contains(out, "No substitution") {
			t.Fatalf("%s: %d %s", m, resp.StatusCode, out)
		}
	}
	resp, out := postJSON(t, srv, "/api/ai/embeddings?model="+localai.EmbeddingGemmaONNXID,
		`{"instances":[{"content":"a"}],"parameters":{"outputDimensionality":768}}`)
	if resp.StatusCode != 200 || strings.Count(out, `"values"`) != 1 {
		t.Fatalf("served model: %d %s", resp.StatusCode, out)
	}
	for _, bad := range []string{"..%2Fx", "a%3Apredict%3Fx", "a%2Fb"} {
		if resp, _ := postJSON(t, srv, "/api/ai/embeddings?model="+bad, `{}`); resp.StatusCode != 400 {
			t.Fatalf("%s relayed: %d", bad, resp.StatusCode)
		}
	}
}

// unitMultimodal puts every text and image on e0.
type unitMultimodal struct{}

func (unitMultimodal) Model() string { return localai.CLIPONNXID }
func (unitMultimodal) Close() error  { return nil }
func (unitMultimodal) EmbedText(_ context.Context, _ string, dim int) (embedding.Output, error) {
	v := make([]float32, embedding.CLIPDimensions)
	v[0] = 1
	return embedding.Output{Values: embedding.Truncate(v, dim), TokenCount: 3}, nil
}
func (u unitMultimodal) EmbedImage(ctx context.Context, _ image.Image, dim int) (embedding.Output, error) {
	return u.EmbedText(ctx, "", dim)
}

// TestEmbeddingsScreenEmbedsAnImage (#1454): the status names the
// multimodal model and its dimensions, and the relay carries an inline image
// larger than the old 1 MB bound to the real multimodal :predict, returning
// its answer, and its 501 while the model is not loaded, unchanged.
func TestEmbeddingsScreenEmbedsAnImage(t *testing.T) {
	api := vertexai.NewServer(nil)
	api.SetEmbedder(unitEmbedder{}, nil)
	apiSrv := httptest.NewServer(api.Handler())
	defer apiSrv.Close()
	addr := strings.TrimPrefix(apiSrv.URL, "http://")
	srv := newPlaygroundServer(t, &Playground{Addr: func() string { return addr }, Model: "m",
		EmbeddingModel: localai.EmbeddingGemmaONNXID})

	code, body := getJSON(t, srv, "/api/ai/embeddings")
	var st struct {
		Multimodal struct {
			Model      string `json:"model"`
			Endpoint   string `json:"endpoint"`
			Dimensions []int  `json:"dimensions"`
		} `json:"multimodal"`
	}
	if err := json.Unmarshal([]byte(body), &st); code != 200 || err != nil || st.Multimodal.Model != localai.CLIPONNXID ||
		!strings.HasSuffix(st.Multimodal.Endpoint, localai.CLIPONNXID+":predict") || len(st.Multimodal.Dimensions) != 3 {
		t.Fatalf("status %d: %s", code, body)
	}

	// A noisy 800x800 PNG: well over 1 MB once base64-encoded.
	img := image.NewRGBA(image.Rect(0, 0, 800, 800))
	x := uint32(2463534242)
	for i := range img.Pix {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Pix[i] = byte(x)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	if len(b64) < 1<<20 {
		t.Fatalf("test image only %d bytes encoded", len(b64))
	}
	req := `{"instances":[{"image":{"bytesBase64Encoded":"` + b64 + `","mimeType":"image/png"}}],"parameters":{"dimension":256}}`
	path := "/api/ai/embeddings?method=predict&model=" + localai.CLIPONNXID
	if resp, out := postJSON(t, srv, path, req); resp.StatusCode != http.StatusNotImplemented || !strings.Contains(out, "Local models") {
		t.Fatalf("unloaded: %d %s", resp.StatusCode, out)
	}
	api.SwapMultimodalEmbedder(unitMultimodal{}, nil)
	resp, out := postJSON(t, srv, path, req)
	var got struct {
		Predictions []struct {
			ImageEmbedding []float32 `json:"imageEmbedding"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal([]byte(out), &got); resp.StatusCode != 200 || err != nil || len(got.Predictions) != 1 || len(got.Predictions[0].ImageEmbedding) != 256 {
		t.Fatalf("loaded: %d %.300s", resp.StatusCode, out)
	}
}
