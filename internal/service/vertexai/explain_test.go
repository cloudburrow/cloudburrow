package vertexai

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func shapley(pathCount int32, inputs map[string][]any) *aiplatformpb.ExplanationSpecOverride {
	md := &aiplatformpb.ExplanationMetadataOverride{Inputs: map[string]*aiplatformpb.ExplanationMetadataOverride_InputMetadataOverride{}}
	for name, bs := range inputs {
		in := &aiplatformpb.ExplanationMetadataOverride_InputMetadataOverride{}
		for _, b := range bs {
			v, _ := structpb.NewValue(b)
			in.InputBaselines = append(in.InputBaselines, v)
		}
		md.Inputs[name] = in
	}
	return &aiplatformpb.ExplanationSpecOverride{
		Parameters: &aiplatformpb.ExplanationParameters{Method: &aiplatformpb.ExplanationParameters_SampledShapleyAttribution{
			SampledShapleyAttribution: &aiplatformpb.SampledShapleyAttribution{PathCount: pathCount}}},
		Metadata: md,
	}
}

// A one-feature model's attribution is exact: the doubler's output for 3,
// from a baseline of 0, is all the feature's.
func TestExplainSampledShapleyOnAScalarModel(t *testing.T) {
	c, base := startMixed(t, endpointServer(t, func(n int) int { return n }))
	ctx := context.Background()

	resp, err := c.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: numbers(t, 3, 5),
		ExplanationSpecOverride: shapley(4, map[string][]any{"x": nil})})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if resp.GetDeployedModelId() != "doubler-v1" || len(resp.GetPredictions()) != 2 || resp.GetPredictions()[1].GetNumberValue() != 10 {
		t.Errorf("response = %v", resp)
	}
	if len(resp.GetExplanations()) != 2 {
		t.Fatalf("explanations = %v, want one per instance", resp.GetExplanations())
	}
	a := resp.GetExplanations()[0].GetAttributions()
	if len(a) != 1 || a[0].GetBaselineOutputValue() != 0 || a[0].GetInstanceOutputValue() != 6 || len(a[0].GetOutputIndex()) != 0 ||
		a[0].GetFeatureAttributions().GetStructValue().GetFields()["x"].GetNumberValue() != 6 {
		t.Errorf("attributions = %v, want x: 6 from 0 to 6", a)
	}

	// The JSON route answers the same.
	r, err := http.Post(base+"/v1/"+endpointName+":explain", "application/json", strings.NewReader(
		`{"instances":[3],"explanationSpecOverride":{"parameters":{"sampledShapleyAttribution":{"pathCount":2}},"metadata":{"inputs":{"x":{"inputBaselines":[1]}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), `"x":4`) || !strings.Contains(string(b), `"baselineOutputValue":2`) {
		t.Errorf("REST :explain = %d %s", r.StatusCode, b)
	}
}

// structEndpoint serves a model of struct instances {a, b} predicting
// [a*b, a+b].
func structEndpoint(t *testing.T) *Server {
	t.Helper()
	host := "predictor.default.cloudburrow.localhost"
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Instances []struct{ A, B float64 } `json:"instances"`
		}
		if r.Host != host || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		out := [][]float64{}
		for _, in := range req.Instances {
			out = append(out, []float64{in.A * in.B, in.A + in.B})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"predictions": out, "deployedModelId": "m"})
	}))
	t.Cleanup(gw.Close)
	s := NewServer(nil)
	s.SetEndpoints(fakeResolver{id: "predictor", url: "http://" + host + "/v1/predict"},
		func() string { return strings.TrimPrefix(gw.URL, "http://") }, nil)
	return s
}

// Attributions sum to the output's change from the baseline, a linear output
// is attributed exactly, and output_indices and top_k pick what is explained.
func TestExplainSampledShapleyOnAStructModel(t *testing.T) {
	c, _ := startMixed(t, structEndpoint(t))
	ctx := context.Background()
	inst := []*structpb.Value{structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{
		"a": structpb.NewNumberValue(3), "b": structpb.NewNumberValue(4)}})}

	spec := shapley(10, map[string][]any{"a": {1.0}, "b": nil})
	spec.Parameters.OutputIndices, _ = structpb.NewList([]any{0, 1})
	resp, err := c.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: inst, ExplanationSpecOverride: spec})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	attrs := resp.GetExplanations()[0].GetAttributions()
	if len(attrs) != 2 {
		t.Fatalf("attributions = %v, want outputs 0 and 1", attrs)
	}
	for _, a := range attrs {
		f := a.GetFeatureAttributions().GetStructValue().GetFields()
		sum := f["a"].GetNumberValue() + f["b"].GetNumberValue()
		if math.Abs(sum-(a.GetInstanceOutputValue()-a.GetBaselineOutputValue())) > 1e-9 {
			t.Errorf("output %v: attributions sum to %v, want %v - %v", a.GetOutputIndex(), sum, a.GetInstanceOutputValue(), a.GetBaselineOutputValue())
		}
	}
	// Output 0 is a*b: 12 from a baseline of 1*0.
	if a := attrs[0]; a.GetOutputIndex()[0] != 0 || a.GetInstanceOutputValue() != 12 || a.GetBaselineOutputValue() != 0 {
		t.Errorf("output 0 = %v", a)
	}
	// Output 1 is a+b: a moved 1 → 3 and b 0 → 4, on every path.
	f := attrs[1].GetFeatureAttributions().GetStructValue().GetFields()
	if attrs[1].GetOutputIndex()[0] != 1 || f["a"].GetNumberValue() != 2 || f["b"].GetNumberValue() != 4 {
		t.Errorf("output 1 = %v, want a: 2, b: 4", attrs[1])
	}

	// top_k defaults to 1: the largest output, a*b = 12.
	resp, err = c.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: inst,
		ExplanationSpecOverride: shapley(3, map[string][]any{"a": nil, "b": nil})})
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.GetExplanations()[0].GetAttributions(); len(a) != 1 || a[0].GetOutputIndex()[0] != 0 {
		t.Errorf("default top_k = %v, want output 0 only", a)
	}

	// Multiple baselines are averaged over.
	resp, err = c.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: inst,
		ExplanationSpecOverride: shapley(1, map[string][]any{"a": {0.0, 2.0}, "b": {0.0, 0.0}})})
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.GetExplanations()[0].GetAttributions()[0]; a.GetBaselineOutputValue() != 0 {
		t.Errorf("baselines averaged = %v, want a baseline output of 0", a)
	}
}

func TestExplainRefusals(t *testing.T) {
	c, _ := startMixed(t, endpointServer(t, func(n int) int { return n }))
	ctx := context.Background()
	ig := &aiplatformpb.ExplanationSpecOverride{Parameters: &aiplatformpb.ExplanationParameters{
		Method: &aiplatformpb.ExplanationParameters_IntegratedGradientsAttribution{IntegratedGradientsAttribution: &aiplatformpb.IntegratedGradientsAttribution{StepCount: 50}}}}
	for _, tt := range []struct {
		name string
		req  *aiplatformpb.ExplainRequest
		code codes.Code
		want string
	}{
		{"no spec", &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: numbers(t, 1)}, codes.FailedPrecondition, "explanation_spec_override"},
		{"integrated gradients", &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: numbers(t, 1), ExplanationSpecOverride: ig}, codes.Unimplemented, "gradients"},
		{"path count", &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: numbers(t, 1), ExplanationSpecOverride: shapley(51, map[string][]any{"x": nil})}, codes.InvalidArgument, "path_count"},
		{"no inputs", &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: numbers(t, 1), ExplanationSpecOverride: shapley(1, nil)}, codes.InvalidArgument, "metadata.inputs"},
		{"no instances", &aiplatformpb.ExplainRequest{Endpoint: endpointName, ExplanationSpecOverride: shapley(1, map[string][]any{"x": nil})}, codes.InvalidArgument, "instances"},
		{"non-number baseline", &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: numbers(t, "s"), ExplanationSpecOverride: shapley(1, map[string][]any{"x": nil})}, codes.InvalidArgument, "input_baselines"},
		{"deployed model", &aiplatformpb.ExplainRequest{Endpoint: endpointName, Instances: numbers(t, 1), DeployedModelId: "other", ExplanationSpecOverride: shapley(1, map[string][]any{"x": nil})}, codes.NotFound, "doubler-v1"},
		{"publisher model", &aiplatformpb.ExplainRequest{Endpoint: embedEndpoint, Instances: numbers(t, 1), ExplanationSpecOverride: shapley(1, map[string][]any{"x": nil})}, codes.Unimplemented, "publisher model"},
		{"not deployed", &aiplatformpb.ExplainRequest{Endpoint: "projects/p/locations/us-central1/endpoints/nope", Instances: numbers(t, 1), ExplanationSpecOverride: shapley(1, map[string][]any{"x": nil})}, codes.NotFound, "nope"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Explain(ctx, tt.req)
			if status.Code(err) != tt.code || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Explain = %v, want %v naming %q", err, tt.code, tt.want)
			}
		})
	}
}
