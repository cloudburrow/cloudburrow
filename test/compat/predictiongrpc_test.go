//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// EnvPrediction points at the local AI endpoint of an instance with Cloud Run
// enabled, which serves google.cloud.aiplatform.v1.PredictionService for the
// custom prediction containers it runs (#1193). It is not EnvLocalAI: that
// one means a generation model is configured, which this instance need not
// have.
const EnvPrediction = "CLOUDBURROW_TEST_PREDICTION"

// predictionClientAt is the official Vertex AI Go client, which speaks gRPC
// only, pointed at envVar's endpoint.
func predictionClientAt(t *testing.T, h *Harness, envVar string) *aiplatform.PredictionClient {
	t.Helper()
	addr := h.Endpoint(envVar)
	c, err := aiplatform.NewPredictionClient(h.Context(),
		option.WithEndpoint(addr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("aiplatform.NewPredictionClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// covers: google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict, google.cloud.aiplatform.v1.PredictionService/Explain
//
// TestPredictionClientReachesACustomEndpoint (#1193): a custom prediction
// container deployed through the official Cloud Run SDK, on non-default
// AIP_* routes, is called through the official aiplatform PredictionClient
// as projects/{p}/locations/us-central1/endpoints/{service}. Predict returns
// one prediction per instance and the container's deployedModelId; the
// container's refusal of a bad instance is INVALID_ARGUMENT in its own words;
// an endpoint that is not deployed is NOT_FOUND; RawPredict relays the body;
// Explain answers Sampled Shapley attributions (#1454); and DirectRawPredict
// on a model with no grpcPorts is FAILED_PRECONDITION (#1477).
func TestPredictionClientReachesACustomEndpoint(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvPrediction)
	c := runClient(t, h)
	buildPredictor(t, h.Context())

	id := "compat-grpc-predictor"
	routes := prediction.Routes{Port: 8080, Health: "/healthz", Predict: "/v1/predict"}
	deployPredictor(t, c, h, id, routes, nil)
	endpoint := runParent(h) + "/endpoints/" + id

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inst := func(xs ...any) []*structpb.Value {
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

	resp, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: inst(1, 2, 3.5)})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	want := []float64{2, 4, 7}
	if len(resp.GetPredictions()) != len(want) {
		t.Fatalf("predictions = %v, want %v", resp.GetPredictions(), want)
	}
	for i, p := range resp.GetPredictions() {
		if p.GetNumberValue() != want[i] {
			t.Errorf("prediction[%d] = %v, want %v", i, p, want[i])
		}
	}
	if resp.GetDeployedModelId() != "doubler-v1" {
		t.Errorf("deployedModelId = %q, want doubler-v1", resp.GetDeployedModelId())
	}

	// parameters reach the container: a zero delay is accepted.
	params, _ := structpb.NewValue(map[string]any{"delayMs": 0})
	if _, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: inst(4), Parameters: params}); err != nil {
		t.Errorf("Predict with parameters: %v", err)
	}

	_, err = pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: inst("abc")})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a non-numeric instance: %v, want INVALID_ARGUMENT", err)
	}
	_, err = pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("no instances: %v, want INVALID_ARGUMENT", err)
	}
	_, err = pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: runParent(h) + "/endpoints/compat-not-deployed", Instances: inst(1)})
	if status.Code(err) != codes.NotFound {
		t.Errorf("an endpoint that is not deployed: %v, want NOT_FOUND", err)
	}

	raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{
		Endpoint: endpoint,
		HttpBody: &httpbody.HttpBody{ContentType: "application/json", Data: []byte(`{"instances":[5]}`)},
	})
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	if !strings.Contains(string(raw.GetData()), `"predictions":[10]`) {
		t.Errorf("RawPredict = %s", raw.GetData())
	}

	// Explain (#1454): Sampled Shapley over the container's predictions, the
	// spec from explanation_spec_override. The doubler's one feature takes
	// all of 2*3 from its baseline of 0.
	_, err = pc.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: endpoint, Instances: inst(1)})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Explain with no explanation spec: %v, want FAILED_PRECONDITION", err)
	}
	ex, err := pc.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: endpoint, Instances: inst(3),
		ExplanationSpecOverride: &aiplatformpb.ExplanationSpecOverride{
			Parameters: &aiplatformpb.ExplanationParameters{Method: &aiplatformpb.ExplanationParameters_SampledShapleyAttribution{
				SampledShapleyAttribution: &aiplatformpb.SampledShapleyAttribution{PathCount: 5}}},
			Metadata: &aiplatformpb.ExplanationMetadataOverride{Inputs: map[string]*aiplatformpb.ExplanationMetadataOverride_InputMetadataOverride{"x": {}}},
		}})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(ex.GetExplanations()) != 1 || len(ex.GetExplanations()[0].GetAttributions()) != 1 || ex.GetDeployedModelId() != "doubler-v1" ||
		len(ex.GetPredictions()) != 1 || ex.GetPredictions()[0].GetNumberValue() != 6 {
		t.Fatalf("Explain = %v", ex)
	}
	a := ex.GetExplanations()[0].GetAttributions()[0]
	if a.GetBaselineOutputValue() != 0 || a.GetInstanceOutputValue() != 6 ||
		math.Abs(a.GetFeatureAttributions().GetStructValue().GetFields()["x"].GetNumberValue()-6) > 1e-9 {
		t.Errorf("attribution = %v, want x: 6 from 0 to 6", a)
	}
	// Vertex documents integrated gradients and XRAI for TensorFlow models
	// in a prebuilt TensorFlow container, and example-based explanations for
	// TensorFlow models that output an embedding; a custom container gets
	// INVALID_ARGUMENT (#1454).
	for name, p := range map[string]*aiplatformpb.ExplanationParameters{
		"integrated gradients": {Method: &aiplatformpb.ExplanationParameters_IntegratedGradientsAttribution{
			IntegratedGradientsAttribution: &aiplatformpb.IntegratedGradientsAttribution{StepCount: 50}}},
		"XRAI": {Method: &aiplatformpb.ExplanationParameters_XraiAttribution{
			XraiAttribution: &aiplatformpb.XraiAttribution{StepCount: 50}}},
		"examples": {Method: &aiplatformpb.ExplanationParameters_Examples{Examples: &aiplatformpb.Examples{NeighborCount: 3}}},
	} {
		_, err = pc.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: endpoint, Instances: inst(1),
			ExplanationSpecOverride: &aiplatformpb.ExplanationSpecOverride{Parameters: p}})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "custom container") {
			t.Errorf("Explain with %s: %v, want INVALID_ARGUMENT naming the custom container", name, err)
		}
	}

	// The doubler's model declares no grpcPorts (#1477): DirectRawPredict
	// has no gRPC server to call.
	_, err = pc.DirectRawPredict(ctx, &aiplatformpb.DirectRawPredictRequest{Endpoint: endpoint, MethodName: "/m.S/Predict"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "grpcPorts") {
		t.Errorf("DirectRawPredict: %v, want FAILED_PRECONDITION naming grpcPorts", err)
	}
}

// consoleExplainResult is the console's answer to POST
// /api/ai/predict/explain, as much as this test reads.
type consoleExplainResult struct {
	URL, Request, Body, Error, ErrorStatus, DeployedModelID string
	Status                                                  int
	Explanations                                            []struct {
		Prediction   json.RawMessage
		Attributions []struct {
			BaselineOutputValue, InstanceOutputValue float64
			Features                                 []struct {
				Feature     string
				Attribution float64
			}
		}
	}
}

// covers: google.cloud.aiplatform.v1.PredictionService/Explain
//
// TestConsoleExplainMatchesThePredictionClient (#1454): Explain sent from the
// console's Online prediction page, with a Sampled Shapley path count and a
// feature's input_baselines, to a predictor deployed through the official
// Cloud Run SDK, answers the attributions per feature that the official
// aiplatform PredictionClient gets for the same ExplainRequest; the API's
// refusal of a path count out of range comes back through the console as the
// status and message the client sees; and the page names integrated
// gradients, XRAI and example-based explanations as not served.
func TestConsoleExplainMatchesThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvPrediction)
	addr := consoleAddr(t, h)
	c := runClient(t, h)
	buildPredictor(t, h.Context())

	id := "compat-console-explainer"
	routes := prediction.Routes{Port: 8080, Health: "/healthz", Predict: "/v1/predict"}
	svc := deployPredictor(t, c, h, id, routes, nil)
	project := h.Project()

	code, body := consoleDo(t, addr, http.MethodGet, "/api/ai/predict?project="+project, "")
	var st struct {
		Explain   bool
		NotServed []string
	}
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil || !st.Explain {
		t.Fatalf("the Online prediction page does not offer Explain: %d %s", code, body)
	}
	for _, want := range []string{"integrated gradients", "XRAI", "Example-based"} {
		if !strings.Contains(strings.Join(st.NotServed, "\n"), want) {
			t.Errorf("the page does not name %q as not served: %v", want, st.NotServed)
		}
	}

	consoleExplain := func(pathCount int) (int, consoleExplainResult, string) {
		t.Helper()
		req, _ := json.Marshal(map[string]any{"endpoint": svc.GetName(), "instances": "[3, 5]",
			"pathCount": pathCount, "baselines": `{"x": [1]}`})
		code, got := consoleDo(t, addr, http.MethodPost, "/api/ai/predict/explain?project="+project, string(req))
		var r consoleExplainResult
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(got), &r); err != nil {
				t.Fatalf("decode the console's explain result: %v: %s", err, got)
			}
		}
		return code, r, got
	}
	code, r, raw := consoleExplain(5)
	if code != http.StatusOK || r.Status != http.StatusOK || len(r.Explanations) != 2 {
		t.Fatalf("Explain through the console = %d: %s", code, raw)
	}
	if !strings.HasSuffix(r.URL, "/v1/"+runParent(h)+"/endpoints/"+id+":explain") || r.DeployedModelID != "doubler-v1" {
		t.Errorf("the console sent %s to %s, deployedModelId %q", r.Request, r.URL, r.DeployedModelID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	one, _ := structpb.NewValue(1)
	override := func(pathCount int32) *aiplatformpb.ExplanationSpecOverride {
		return &aiplatformpb.ExplanationSpecOverride{
			Parameters: &aiplatformpb.ExplanationParameters{Method: &aiplatformpb.ExplanationParameters_SampledShapleyAttribution{
				SampledShapleyAttribution: &aiplatformpb.SampledShapleyAttribution{PathCount: pathCount}}},
			Metadata: &aiplatformpb.ExplanationMetadataOverride{Inputs: map[string]*aiplatformpb.ExplanationMetadataOverride_InputMetadataOverride{
				"x": {InputBaselines: []*structpb.Value{one}}}},
		}
	}
	three, _ := structpb.NewValue(3)
	five, _ := structpb.NewValue(5)
	ex, err := pc.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: runParent(h) + "/endpoints/" + id,
		Instances: []*structpb.Value{three, five}, ExplanationSpecOverride: override(5)})
	if err != nil {
		t.Fatalf("Explain through the PredictionClient: %v", err)
	}
	if len(ex.GetExplanations()) != len(r.Explanations) {
		t.Fatalf("the client got %d explanations, the console %d", len(ex.GetExplanations()), len(r.Explanations))
	}
	for i, e := range ex.GetExplanations() {
		want := e.GetAttributions()[0]
		got := r.Explanations[i].Attributions[0]
		if got.BaselineOutputValue != want.GetBaselineOutputValue() || got.InstanceOutputValue != want.GetInstanceOutputValue() {
			t.Errorf("instance %d: the console shows %v to %v, the client %v to %v", i,
				got.BaselineOutputValue, got.InstanceOutputValue, want.GetBaselineOutputValue(), want.GetInstanceOutputValue())
		}
		wantX := want.GetFeatureAttributions().GetStructValue().GetFields()["x"].GetNumberValue()
		if len(got.Features) != 1 || got.Features[0].Feature != "x" || math.Abs(got.Features[0].Attribution-wantX) > 1e-9 {
			t.Errorf("instance %d: the console shows %+v; the client x = %v", i, got.Features, wantX)
		}
		if string(r.Explanations[i].Prediction) != strconv.FormatFloat(ex.GetPredictions()[i].GetNumberValue(), 'f', -1, 64) {
			t.Errorf("instance %d: prediction %s, the client %v", i, r.Explanations[i].Prediction, ex.GetPredictions()[i])
		}
	}
	// 3 from a baseline of 1 is the doubler's 6 from 2: x carries all 4.
	if x := r.Explanations[0].Attributions[0]; x.BaselineOutputValue != 2 || x.InstanceOutputValue != 6 ||
		math.Abs(x.Features[0].Attribution-4) > 1e-9 {
		t.Errorf("instance 0's attribution = %+v, want x: 4 from 2 to 6", x)
	}

	// The API's refusal, through the console as the client sees it.
	code, r, raw = consoleExplain(51)
	_, err = pc.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: runParent(h) + "/endpoints/" + id,
		Instances: []*structpb.Value{three}, ExplanationSpecOverride: override(51)})
	if code != http.StatusOK || r.Status != http.StatusBadRequest || r.ErrorStatus != "INVALID_ARGUMENT" ||
		status.Code(err) != codes.InvalidArgument || r.Error != status.Convert(err).Message() {
		t.Errorf("a path count of 51: the console shows %d %s; the client %v", code, raw, err)
	}
}

// TestEmbeddingGemmaThroughThePredictionClient (#1193) embeds through the
// official aiplatform PredictionClient, over gRPC, on the same endpoint and
// with the same answer the genai SDK gets over REST: a 768-value unit vector,
// a renormalised prefix at a smaller outputDimensionality, and the model's
// provenance in the response.
func TestEmbeddingGemmaThroughThePredictionClient(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvEmbeddings)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := "projects/" + h.Project() + "/locations/us-central1/publishers/google/models/" + embeddingModel

	vector := func(dim int) []float64 {
		in, _ := structpb.NewValue(map[string]any{"content": "The quick brown fox jumps over the lazy dog.", "task_type": "RETRIEVAL_QUERY"})
		req := &aiplatformpb.PredictRequest{Endpoint: name, Instances: []*structpb.Value{in}}
		if dim > 0 {
			req.Parameters, _ = structpb.NewValue(map[string]any{"outputDimensionality": dim})
		}
		resp, err := pc.Predict(ctx, req)
		if err != nil {
			t.Fatalf("Predict (dim %d): %v", dim, err)
		}
		if resp.GetModel() != embeddingModel || !strings.Contains(resp.GetModelDisplayName(), "NOT an official Google artifact") {
			t.Errorf("model %q, display name %q", resp.GetModel(), resp.GetModelDisplayName())
		}
		vals := resp.GetPredictions()[0].GetStructValue().GetFields()["embeddings"].GetStructValue().GetFields()["values"].GetListValue().GetValues()
		out := make([]float64, len(vals))
		for i, v := range vals {
			out[i] = v.GetNumberValue()
		}
		return out
	}
	unitNorm := func(v []float64) float64 {
		var s float64
		for _, x := range v {
			s += x * x
		}
		return math.Sqrt(s)
	}
	full := vector(0)
	if len(full) != 768 || math.Abs(unitNorm(full)-1) > 1e-3 {
		t.Fatalf("full vector: %d values, norm %.6f", len(full), unitNorm(full))
	}
	small := vector(128)
	if len(small) != 128 || math.Abs(unitNorm(small)-1) > 1e-3 {
		t.Fatalf("128 vector: %d values, norm %.6f", len(small), unitNorm(small))
	}
	var dot float64
	for i := range small {
		dot += small[i] * full[i]
	}
	if c := dot / unitNorm(full[:128]); c < 0.9999 {
		t.Errorf("outputDimensionality 128 is not the renormalised prefix of 768 (cosine %.6f)", c)
	}

	bad, _ := structpb.NewValue(map[string]any{"content": "x", "task_type": "NOT_A_TASK"})
	if _, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: name, Instances: []*structpb.Value{bad}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown task type: %v, want INVALID_ARGUMENT", err)
	}
}
