package vertexai

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
)

type fakeEmbedder struct{}

func (fakeEmbedder) Model() string { return localai.EmbeddingGemmaONNXID }
func (fakeEmbedder) Close() error  { return nil }
func (fakeEmbedder) Embed(_ context.Context, in []embedding.Input, dim int, _ bool) ([]embedding.Output, error) {
	out := make([]embedding.Output, len(in))
	for i := range in {
		v := make([]float32, embedding.Dimensions)
		v[0] = 1
		out[i] = embedding.Output{Values: embedding.Truncate(v, dim), TokenCount: 3}
	}
	return out, nil
}

func postPredict(t *testing.T, s *Server, model, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/projects/p/locations/us-central1/publishers/google/models/"+model+":predict", strings.NewReader(body))
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPredictWithoutRuntimeIs501(t *testing.T) {
	s := NewServer(nil)
	s.SetEmbedder(nil, errors.New("not compiled"))
	rec := postPredict(t, s, localai.EmbeddingGemmaONNXID, `{"instances":[{"content":"x"}]}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestPredictServesOnlyTheCommunityIdentity(t *testing.T) {
	s := NewServer(nil)
	s.SetEmbedder(fakeEmbedder{}, nil)
	if rec := postPredict(t, s, "text-embedding-005", `{"instances":[{"content":"x"}]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("substituted: %d", rec.Code)
	}
	rec := postPredict(t, s, localai.EmbeddingGemmaONNXID,
		`{"instances":[{"content":"x","task_type":"RETRIEVAL_DOCUMENT"}],"parameters":{"outputDimensionality":256}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Header().Get(ProvenanceHeader), "not an official Google artifact") {
		t.Fatal("provenance header missing")
	}
	var resp struct {
		Predictions []struct {
			Embeddings struct{ Values []float32 } `json:"embeddings"`
		} `json:"predictions"`
		ModelDisplayName string `json:"modelDisplayName"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Predictions) != 1 || len(resp.Predictions[0].Embeddings.Values) != 256 {
		t.Fatalf("bad predictions: %s", rec.Body)
	}
	if !strings.Contains(resp.ModelDisplayName, "NOT an official Google artifact") {
		t.Fatal("display name does not carry the provenance")
	}
	if rec := postPredict(t, s, localai.EmbeddingGemmaONNXID,
		`{"instances":[{"content":"x"}],"parameters":{"outputDimensionality":300}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad dimension accepted: %d", rec.Code)
	}
}
