//go:build compat

package compat

import (
	"context"
	"math"
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

// covers: google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict
//
// TestPredictionClientReachesACustomEndpoint (#1193): a custom prediction
// container deployed through the official Cloud Run SDK, on non-default
// AIP_* routes, is called through the official aiplatform PredictionClient
// as projects/{p}/locations/us-central1/endpoints/{service}. Predict returns
// one prediction per instance and the container's deployedModelId; the
// container's refusal of a bad instance is INVALID_ARGUMENT in its own words;
// an endpoint that is not deployed is NOT_FOUND; RawPredict relays the body;
// and a method CloudBurrow does not implement is UNIMPLEMENTED.
func TestPredictionClientReachesACustomEndpoint(t *testing.T) {
	h := New(t)
	pc := predictionClientAt(t, h, EnvPrediction)
	c := runClient(t, h)
	buildPredictor(t, h.Context())

	id := "compat-grpc-predictor"
	routes := prediction.Routes{Port: 8080, Health: "/healthz", Predict: "/v1/predict"}
	deployPredictor(t, c, h, id, routes, nil)
	endpoint := runParent(h) + "/endpoints/" + id

	ctx, cancel := context.WithTimeout(h.Context(), 2*time.Minute)
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

	_, err = pc.Explain(ctx, &aiplatformpb.ExplainRequest{Endpoint: endpoint, Instances: inst(1)})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("Explain: %v, want UNIMPLEMENTED", err)
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
	ctx, cancel := context.WithTimeout(h.Context(), 5*time.Minute)
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
