package vertexai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// startMixed starts s on loopback and returns a PredictionService client
// dialled to the same port the JSON routes are on, and that port's base URL.
func startMixed(t *testing.T, s *Server) (aiplatformpb.PredictionServiceClient, string) {
	t.Helper()
	s.lis.addr = "127.0.0.1:0"
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	conn, err := grpc.NewClient(s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return aiplatformpb.NewPredictionServiceClient(conn), "http://" + s.Addr()
}

// fakeResolver resolves one endpoint ID to a predict URL.
type fakeResolver struct{ id, url string }

func (f fakeResolver) PredictURL(_ context.Context, _, _, endpoint string) (string, error) {
	if endpoint != f.id {
		return "", apierror.NotFound("no custom prediction endpoint %q is deployed", endpoint)
	}
	return f.url, nil
}

// fakeIngress is the cluster gateway: it routes on Host as Knative does and
// serves a doubling predictor on /v1/predict.
func fakeIngress(t *testing.T, host string, predictions func(n int) int) *httptest.Server {
	t.Helper()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || r.URL.Path != "/v1/predict" {
			http.Error(w, "no route", http.StatusNotFound)
			return
		}
		var req struct {
			Instances []json.RawMessage `json:"instances"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "malformed request: " + err.Error()})
			return
		}
		out := []float64{}
		for _, in := range req.Instances {
			var f float64
			if err := json.Unmarshal(in, &f); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "instance is not a number"})
				return
			}
			out = append(out, 2*f)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"predictions": out[:predictions(len(out))], "deployedModelId": "doubler-v1"})
	}))
	t.Cleanup(gw.Close)
	return gw
}

func endpointServer(t *testing.T, count func(int) int) *Server {
	t.Helper()
	gw := fakeIngress(t, "predictor.default.cloudburrow.localhost", count)
	s := NewServer(nil)
	s.SetEndpoints(fakeResolver{id: "predictor", url: "http://predictor.default.cloudburrow.localhost/v1/predict"},
		func() string { return strings.TrimPrefix(gw.URL, "http://") }, nil)
	return s
}

const endpointName = "projects/p/locations/us-central1/endpoints/predictor"

func numbers(t *testing.T, xs ...any) []*structpb.Value {
	t.Helper()
	out := make([]*structpb.Value, len(xs))
	for i, x := range xs {
		v, err := structpb.NewValue(x)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = v
	}
	return out
}

func TestGRPCPredictRelaysToTheCustomEndpoint(t *testing.T) {
	c, base := startMixed(t, endpointServer(t, func(n int) int { return n }))
	ctx := context.Background()

	resp, err := c.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpointName, Instances: numbers(t, 1, 2, 3.5)})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	var got []float64
	for _, p := range resp.GetPredictions() {
		got = append(got, p.GetNumberValue())
	}
	if len(got) != 3 || got[0] != 2 || got[1] != 4 || got[2] != 7 {
		t.Errorf("predictions = %v, want [2 4 7]", got)
	}
	if resp.GetDeployedModelId() != "doubler-v1" {
		t.Errorf("deployedModelId = %q", resp.GetDeployedModelId())
	}

	// The JSON route on the same port answers the same.
	r, err := http.Post(base+"/v1/"+endpointName+":predict", "application/json", strings.NewReader(`{"instances":[1,2,3.5]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || !strings.Contains(string(body), `"predictions":[2,4,7]`) {
		t.Errorf("JSON :predict = %d %s", r.StatusCode, body)
	}
}

func TestGRPCPredictErrors(t *testing.T) {
	c, _ := startMixed(t, endpointServer(t, func(n int) int { return n }))
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		req  *aiplatformpb.PredictRequest
		code codes.Code
		msg  string
	}{
		{"no instances", &aiplatformpb.PredictRequest{Endpoint: endpointName}, codes.InvalidArgument, "at least one instance"},
		{"container refuses", &aiplatformpb.PredictRequest{Endpoint: endpointName, Instances: numbers(t, "abc")},
			codes.InvalidArgument, "instance is not a number"},
		{"unknown endpoint", &aiplatformpb.PredictRequest{Endpoint: "projects/p/locations/us-central1/endpoints/nope",
			Instances: numbers(t, 1)}, codes.NotFound, "nope"},
		{"bad name", &aiplatformpb.PredictRequest{Endpoint: "endpoints/x", Instances: numbers(t, 1)},
			codes.InvalidArgument, "not a resource name"},
		{"other publisher", &aiplatformpb.PredictRequest{
			Endpoint: "projects/p/locations/l/publishers/meta/models/m", Instances: numbers(t, 1)}, codes.NotFound, "publisher"},
	} {
		_, err := c.Predict(ctx, tc.req)
		if status.Code(err) != tc.code || !strings.Contains(status.Convert(err).Message(), tc.msg) {
			t.Errorf("%s: err = %v, want %s containing %q", tc.name, err, tc.code, tc.msg)
		}
	}
}

func TestGRPCPredictRefusesAContractBreak(t *testing.T) {
	c, _ := startMixed(t, endpointServer(t, func(n int) int { return n - 1 }))
	_, err := c.Predict(context.Background(), &aiplatformpb.PredictRequest{Endpoint: endpointName, Instances: numbers(t, 1, 2)})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "one per instance") {
		t.Fatalf("err = %v, want INTERNAL naming the contract", err)
	}
}

func TestGRPCEndpointsUnimplementedWithoutCloudRun(t *testing.T) {
	c, _ := startMixed(t, NewServer(nil))
	_, err := c.Predict(context.Background(), &aiplatformpb.PredictRequest{Endpoint: endpointName, Instances: numbers(t, 1)})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "Cloud Run") {
		t.Fatalf("err = %v, want UNIMPLEMENTED naming Cloud Run", err)
	}
}

func TestGRPCRawPredictPassesTheBodyThrough(t *testing.T) {
	c, _ := startMixed(t, endpointServer(t, func(n int) int { return n }))
	resp, err := c.RawPredict(context.Background(), &aiplatformpb.RawPredictRequest{
		Endpoint: endpointName,
		HttpBody: &httpbody.HttpBody{ContentType: "application/json", Data: []byte(`{"instances":[5]}`)},
	})
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	if !strings.Contains(string(resp.GetData()), `"predictions":[10]`) || !strings.HasPrefix(resp.GetContentType(), "application/json") {
		t.Errorf("RawPredict = %s %s", resp.GetContentType(), resp.GetData())
	}
	_, err = c.RawPredict(context.Background(), &aiplatformpb.RawPredictRequest{
		Endpoint: "projects/p/locations/l/publishers/google/models/" + localai.EmbeddingGemmaONNXID,
	})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("RawPredict on a publisher model: %v, want UNIMPLEMENTED", err)
	}
}

func TestGRPCPredictEmbedsLikeTheJSONRoute(t *testing.T) {
	s := NewServer(nil)
	s.SetEmbedder(fakeEmbedder{}, nil)
	c, _ := startMixed(t, s)
	name := "projects/p/locations/us-central1/publishers/google/models/" + localai.EmbeddingGemmaONNXID

	inst, _ := structpb.NewValue(map[string]any{"content": "hello", "task_type": "RETRIEVAL_QUERY"})
	params, _ := structpb.NewValue(map[string]any{"outputDimensionality": 128})
	var hdr metadata.MD
	resp, err := c.Predict(context.Background(), &aiplatformpb.PredictRequest{
		Endpoint: name, Instances: []*structpb.Value{inst}, Parameters: params,
	}, grpc.Header(&hdr))
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	vals := resp.GetPredictions()[0].GetStructValue().GetFields()["embeddings"].GetStructValue().GetFields()["values"].GetListValue().GetValues()
	if len(vals) != 128 {
		t.Errorf("got %d values, want 128", len(vals))
	}
	if resp.GetModel() != localai.EmbeddingGemmaONNXID || resp.GetModelDisplayName() != EmbeddingModelDisplayName {
		t.Errorf("model = %q / %q", resp.GetModel(), resp.GetModelDisplayName())
	}
	if got := hdr.Get(strings.ToLower(ProvenanceHeader)); len(got) != 1 || !strings.Contains(got[0], "not an official") {
		t.Errorf("provenance metadata = %v", got)
	}

	// The same JSON request over REST gives the same answer.
	rec := postPredict(t, s, localai.EmbeddingGemmaONNXID,
		`{"instances":[{"content":"hello","task_type":"RETRIEVAL_QUERY"}],"parameters":{"outputDimensionality":128}}`)
	var rest struct {
		Predictions []struct {
			Embeddings struct {
				Values []float64 `json:"values"`
			} `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rest); err != nil || len(rest.Predictions) != 1 {
		t.Fatalf("REST: %v %s", err, rec.Body)
	}
	for i, v := range rest.Predictions[0].Embeddings.Values {
		if vals[i].GetNumberValue() != v {
			t.Fatalf("value %d: gRPC %v, REST %v", i, vals[i].GetNumberValue(), v)
		}
	}

	// A field the subset does not know is refused on both.
	bad, _ := structpb.NewValue(map[string]any{"content": "x", "nosuch": 1})
	if _, err := c.Predict(context.Background(), &aiplatformpb.PredictRequest{Endpoint: name, Instances: []*structpb.Value{bad}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown instance field: %v, want INVALID_ARGUMENT", err)
	}
	// Another model is not substituted.
	if _, err := c.Predict(context.Background(), &aiplatformpb.PredictRequest{
		Endpoint: "projects/p/locations/l/publishers/google/models/text-embedding-005", Instances: []*structpb.Value{inst},
	}); status.Code(err) != codes.NotFound {
		t.Errorf("other model: %v, want NOT_FOUND", err)
	}
}

func TestGRPCGenerateContentMatchesTheJSONRoute(t *testing.T) {
	s := NewServer(&EchoGenerator{ID: testModel})
	c, _ := startMixed(t, s)
	name := "projects/p/locations/us-central1/publishers/google/models/" + testModel
	user := []*aiplatformpb.Content{{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_Text{Text: "hello there"}}}}}

	resp, err := c.GenerateContent(context.Background(), &aiplatformpb.GenerateContentRequest{Model: name, Contents: user})
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	cand := resp.GetCandidates()
	if len(cand) != 1 || cand[0].GetFinishReason() != aiplatformpb.Candidate_STOP || cand[0].GetContent().GetRole() != "model" {
		t.Fatalf("response = %v", resp)
	}
	if got := cand[0].GetContent().GetParts()[0].GetText(); got != "HELLO THERE" {
		t.Errorf("text = %q, want the echo", got)
	}
	if resp.GetModelVersion() != testModel {
		t.Errorf("modelVersion = %q", resp.GetModelVersion())
	}

	// An option the runtime cannot honour is refused, as over JSON.
	temp := float32(0.2)
	_, err = c.GenerateContent(context.Background(), &aiplatformpb.GenerateContentRequest{
		Model: name, Contents: user, GenerationConfig: &aiplatformpb.GenerationConfig{Temperature: &temp},
	})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "temperature") {
		t.Errorf("temperature: %v, want INVALID_ARGUMENT naming it", err)
	}
	// No substitution.
	_, err = c.GenerateContent(context.Background(), &aiplatformpb.GenerateContentRequest{
		Model: "projects/p/locations/l/publishers/google/models/gemini-2.0-flash", Contents: user,
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("gemini: %v, want NOT_FOUND", err)
	}
}

func TestGRPCOtherMethodsAreUnimplemented(t *testing.T) {
	c, _ := startMixed(t, endpointServer(t, func(n int) int { return n }))
	_, err := c.Explain(context.Background(), &aiplatformpb.ExplainRequest{Endpoint: endpointName})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("Explain: %v, want UNIMPLEMENTED", err)
	}
	_, err = c.EmbedContent(context.Background(), &aiplatformpb.EmbedContentRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("EmbedContent: %v, want UNIMPLEMENTED", err)
	}
	st, err := c.StreamGenerateContent(context.Background(), &aiplatformpb.GenerateContentRequest{})
	if err == nil {
		_, err = st.Recv()
	}
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("StreamGenerateContent: %v, want UNIMPLEMENTED", err)
	}
}

