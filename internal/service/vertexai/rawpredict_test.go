package vertexai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

func rawReq(model, contentType, body string) *aiplatformpb.RawPredictRequest {
	return &aiplatformpb.RawPredictRequest{
		Endpoint: "projects/p/locations/us-central1/publishers/google/models/" + model,
		HttpBody: &httpbody.HttpBody{ContentType: contentType, Data: []byte(body)},
	}
}

func TestGRPCRawPredictOnTheEmbeddingModel(t *testing.T) {
	s := NewServer(nil)
	s.SetEmbedder(fakeEmbedder{}, nil)
	c, _ := startMixed(t, s)
	var hdr metadata.MD
	resp, err := c.RawPredict(context.Background(), rawReq(localai.EmbeddingGemmaONNXID, "application/json",
		`{"instances":[{"content":"hello","task_type":"RETRIEVAL_QUERY"}],"parameters":{"outputDimensionality":128}}`), grpc.Header(&hdr))
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	var got struct {
		Predictions []struct {
			Embeddings struct{ Values []float64 } `json:"embeddings"`
		} `json:"predictions"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(resp.GetData(), &got); err != nil || len(got.Predictions) != 1 {
		t.Fatalf("body %s: %v", resp.GetData(), err)
	}
	if n := len(got.Predictions[0].Embeddings.Values); n != 128 || got.Model != localai.EmbeddingGemmaONNXID {
		t.Errorf("%d values from %q", n, got.Model)
	}
	if resp.GetContentType() != "application/json" || len(hdr.Get(strings.ToLower(ProvenanceHeader))) != 1 {
		t.Errorf("content type %q, provenance %v", resp.GetContentType(), hdr)
	}
	// The :predict subset's refusals hold.
	if _, err := c.RawPredict(context.Background(), rawReq(localai.EmbeddingGemmaONNXID, "", `{"instances":[{"content":"x","nosuch":1}]}`)); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown field: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.RawPredict(context.Background(), rawReq(localai.EmbeddingGemmaONNXID, "text/plain", `hello`)); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "http_body.content_type") {
		t.Errorf("text/plain: %v, want INVALID_ARGUMENT naming http_body.content_type", err)
	}
}

func TestGRPCRawPredictOnTheGenerationModel(t *testing.T) {
	c, _ := startMixed(t, NewServer(&EchoGenerator{ID: testModel}))
	resp, err := c.RawPredict(context.Background(), rawReq(testModel, "application/json; charset=utf-8",
		`{"contents":[{"role":"user","parts":[{"text":"hello there"}]}]}`))
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	var got GenerateContentResponse
	if err := json.Unmarshal(resp.GetData(), &got); err != nil || len(got.Candidates) != 1 {
		t.Fatalf("body %s: %v", resp.GetData(), err)
	}
	if got.Candidates[0].Content.Parts[0].Text != "HELLO THERE" || got.Candidates[0].FinishReason != FinishReasonStop || got.ModelVersion != testModel {
		t.Errorf("response = %s", resp.GetData())
	}
	if _, err := c.RawPredict(context.Background(), rawReq(testModel, "", `{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"temperature":0.2}}`)); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "temperature") {
		t.Errorf("temperature: %v, want INVALID_ARGUMENT naming it", err)
	}
	if _, err := c.RawPredict(context.Background(), rawReq("gemini-2.0-flash", "", `{"contents":[{"parts":[{"text":"x"}]}]}`)); status.Code(err) != codes.NotFound {
		t.Errorf("gemini: %v, want NOT_FOUND", err)
	}
	r := rawReq(testModel, "", `{}`)
	r.Endpoint = "projects/p/locations/l/publishers/meta/models/llama"
	if _, err := c.RawPredict(context.Background(), r); status.Code(err) != codes.NotFound {
		t.Errorf("another publisher: %v, want NOT_FOUND", err)
	}
	// No runtime: the generation methods' FAILED_PRECONDITION.
	c2, _ := startMixed(t, NewServer(nil))
	if _, err := c2.RawPredict(context.Background(), rawReq(testModel, "", `{"contents":[{"parts":[{"text":"x"}]}]}`)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("no runtime: %v, want FAILED_PRECONDITION", err)
	}
}

func TestRawPredictREST(t *testing.T) {
	s := NewServer(&EchoGenerator{ID: testModel})
	s.SetEmbedder(fakeEmbedder{}, nil)
	for _, version := range []string{"v1", APIVersion} {
		post := func(model, body string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost,
				"/"+version+"/projects/p/locations/us-central1/publishers/google/models/"+model+":rawPredict", strings.NewReader(body))
			req.Host = "127.0.0.1"
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			return rec
		}
		rec := post(testModel, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"HI"`) {
			t.Errorf("%s generation: %d %s", version, rec.Code, rec.Body)
		}
		rec = post(localai.EmbeddingGemmaONNXID, `{"instances":[{"content":"hi"}]}`)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"predictions"`) || rec.Header().Get(ProvenanceHeader) == "" {
			t.Errorf("%s embedding: %d %s", version, rec.Code, rec.Body)
		}
		rec = post(testModel, `{"contents":[{"parts":[{"text":"x"}]}],"tools":[{}]}`)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "tools") {
			t.Errorf("%s tools: %d %s", version, rec.Code, rec.Body)
		}
	}
}
