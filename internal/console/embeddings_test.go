package console

import (
	"context"
	"encoding/json"
	"errors"
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
