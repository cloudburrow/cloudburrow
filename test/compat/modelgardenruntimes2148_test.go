//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/protobuf/types/known/structpb"
)

// The tiny public models one per runtime family deploys (#2148), each under
// 200 MB.
const (
	// hfGGUFModel's Q4_K_M file, 105 MB, Apache-2.0: llama.cpp.
	hfGGUFModel = "bartowski/SmolLM2-135M-Instruct-GGUF"
	// hfPipelineModel is a 17.8 MB BERT sentiment classifier: the Hugging
	// Face PyTorch Inference container.
	hfPipelineModel = "M-FAC/bert-tiny-finetuned-sst2"
	// hfDiffusersModel is diffusers' 8.8 MB test pipeline, Apache-2.0:
	// text-to-image on the same container.
	hfDiffusersModel = "hf-internal-testing/tiny-stable-diffusion-torch"
	// hfONNXModel is an ONNX-only (transformers.js) BERT spam classifier,
	// its full-precision graph 17 MB: ONNX Runtime.
	hfONNXModel = "onnx-community/bert-tiny-finetuned-sms-spam-detection-ONNX"
	// hfMLXModel is a 79 MB 4-bit MLX conversion of SmolLM, Apache-2.0:
	// mlx-lm on MLX's Linux CPU backend.
	hfMLXModel = "mlx-community/SmolLM-135M-Instruct-4bit"
)

// hfDeployment deploys hfID through ModelGardenService.Deploy and returns
// its endpoint, undeploying and deleting it all when the test ends.
func hfDeployment(t *testing.T, h *Harness, ctx context.Context, hfID string) string {
	t.Helper()
	opts := gardenOpts(h, EnvLocalAI)
	mg, err := aiplatform.NewModelGardenClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mg.Close() })
	ec, err := aiplatform.NewEndpointClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	mc, err := aiplatform.NewModelClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mc.Close() })
	parent := runParent(h)
	op, err := mg.Deploy(ctx, &aiplatformpb.DeployRequest{Destination: parent,
		Artifacts: &aiplatformpb.DeployRequest_HuggingFaceModelId{HuggingFaceModelId: hfID}})
	if err != nil {
		t.Fatalf("Deploy %s: %v", hfID, err)
	}
	md, err := op.Metadata()
	if err != nil || md.GetModelId() == "" {
		t.Fatalf("Deploy %s metadata = %v, %v", hfID, md, err)
	}
	model := parent + "/models/" + md.GetModelId()
	var endpoint string
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if endpoint != "" {
			if e, err := ec.GetEndpoint(cctx, &aiplatformpb.GetEndpointRequest{Name: endpoint}); err == nil {
				for _, d := range e.GetDeployedModels() {
					if op, err := ec.UndeployModel(cctx, &aiplatformpb.UndeployModelRequest{Endpoint: endpoint, DeployedModelId: d.GetId()}); err == nil {
						_, _ = op.Wait(cctx)
					}
				}
				if op, err := ec.DeleteEndpoint(cctx, &aiplatformpb.DeleteEndpointRequest{Name: endpoint}); err == nil {
					_ = op.Wait(cctx)
				}
			}
		}
		if op, err := mc.DeleteModel(cctx, &aiplatformpb.DeleteModelRequest{Name: model}); err == nil {
			_ = op.Wait(cctx)
		}
	})
	resp, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("Deploy %s: waiting: %v", hfID, err)
	}
	endpoint = resp.GetEndpoint()
	if resp.GetModel() != model {
		t.Fatalf("Deploy = %v; want %s", resp, model)
	}
	return endpoint
}

func hfRuntimeContext(t *testing.T, gated bool) (*Harness, context.Context) {
	t.Helper()
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skipf("%s is not set: the local AI endpoint, which serves Model Garden, is not running", EnvLocalAI)
	}
	if gated && os.Getenv(EnvHFDeploy) == "" {
		t.Skipf("set %s=1 to run it: the runtime installs PyTorch or MLX (hundreds of MB) when its pod starts", EnvHFDeploy)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	return h, ctx
}

func structInstance(t *testing.T, v any) *structpb.Value {
	t.Helper()
	s, err := structpb.NewValue(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// covers: google.cloud.aiplatform.v1.ModelGardenService/Deploy, google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict
//
// TestModelGardenDeploysAGGUFModelOnLlamaCpp (#2148): a GGUF repository
// deploys on llama.cpp's server on the instance's cluster, its pod
// downloading the Q4_K_M file at the card's revision. Predict takes Model
// Garden's prompt instances and answers "Prompt:\n…\nOutput:\n…", as on
// vLLM; RawPredict relays an OpenAI chat completions body.
func TestModelGardenDeploysAGGUFModelOnLlamaCpp(t *testing.T) {
	h, ctx := hfRuntimeContext(t, false)
	endpoint := hfDeployment(t, h, ctx, hfGGUFModel)
	pc := predictionClientAt(t, h, EnvLocalAI)
	pr, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: []*structpb.Value{
		structInstance(t, map[string]any{"prompt": "The capital of France is", "max_tokens": 8, "temperature": 0})}})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if got := pr.GetPredictions(); len(got) != 1 || !strings.HasPrefix(got[0].GetStringValue(), "Prompt:\nThe capital of France is\nOutput:\n") ||
		len(got[0].GetStringValue()) <= len("Prompt:\nThe capital of France is\nOutput:\n") {
		t.Fatalf("Predict = %v", got)
	}
	raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: endpoint, HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"messages":[{"role":"user","content":"Say hi"}],"max_tokens":8}`)}})
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	var chat struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw.GetData(), &chat); err != nil || len(chat.Choices) != 1 || chat.Choices[0].Message.Content == "" {
		t.Fatalf("RawPredict = %.300s (%v)", raw.GetData(), err)
	}
}

// covers: google.cloud.aiplatform.v1.ModelGardenService/Deploy, google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict
//
// TestModelGardenDeploysOnHuggingFacePyTorchInference (#2148, opt-in): a
// transformers text-classification model deploys on the Hugging Face
// PyTorch Inference container's server; Predict answers one list of
// labels and scores per instance, as Google's container does, and
// RawPredict relays the container's own body. A diffusers text-to-image
// pipeline on the same runtime answers a base64 PNG per prompt.
func TestModelGardenDeploysOnHuggingFacePyTorchInference(t *testing.T) {
	h, ctx := hfRuntimeContext(t, true)
	pc := predictionClientAt(t, h, EnvLocalAI)
	endpoint := hfDeployment(t, h, ctx, hfPipelineModel)
	pr, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: []*structpb.Value{
		structpb.NewStringValue("I love this film"), structpb.NewStringValue("This was a waste of time")}})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(pr.GetPredictions()) != 2 {
		t.Fatalf("Predict = %v; want two predictions", pr.GetPredictions())
	}
	for i, p := range pr.GetPredictions() {
		l := p.GetListValue().GetValues()
		if len(l) == 0 || l[0].GetStructValue().GetFields()["label"].GetStringValue() == "" ||
			l[0].GetStructValue().GetFields()["score"].GetNumberValue() <= 0 {
			t.Errorf("prediction %d = %v; want labels and scores", i, p)
		}
	}
	raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: endpoint, HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"instances":["great"],"parameters":{"top_k":2}}`)}})
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	var out struct {
		Predictions [][]struct {
			Label string  `json:"label"`
			Score float64 `json:"score"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal(raw.GetData(), &out); err != nil || len(out.Predictions) != 1 || len(out.Predictions[0]) != 2 {
		t.Fatalf("RawPredict = %.300s (%v); want two labels", raw.GetData(), err)
	}

	img := hfDeployment(t, h, ctx, hfDiffusersModel)
	ir, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: img, Instances: []*structpb.Value{structpb.NewStringValue("a red square")},
		Parameters: structInstance(t, map[string]any{"num_inference_steps": 2, "height": 64, "width": 64})})
	if err != nil {
		t.Fatalf("Predict text-to-image: %v", err)
	}
	png, err := base64.StdEncoding.DecodeString(ir.GetPredictions()[0].GetStringValue())
	if err != nil || len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Fatalf("text-to-image prediction is not a base64 PNG: %.100s (%v)", ir.GetPredictions()[0].GetStringValue(), err)
	}
}

// covers: google.cloud.aiplatform.v1.ModelGardenService/Deploy, google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict
//
// TestModelGardenDeploysAnONNXModel (#2148, opt-in): an ONNX-only
// (transformers.js) repository deploys on ONNX Runtime, its pod
// downloading only the full-precision graph and the tokenizer; Predict and
// RawPredict take and answer the PyTorch Inference container's shapes.
func TestModelGardenDeploysAnONNXModel(t *testing.T) {
	h, ctx := hfRuntimeContext(t, true)
	pc := predictionClientAt(t, h, EnvLocalAI)
	endpoint := hfDeployment(t, h, ctx, hfONNXModel)
	pr, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: []*structpb.Value{
		structpb.NewStringValue("WINNER!! Claim your free prize now"), structpb.NewStringValue("See you at lunch")}})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(pr.GetPredictions()) != 2 {
		t.Fatalf("Predict = %v", pr.GetPredictions())
	}
	for i, p := range pr.GetPredictions() {
		l := p.GetListValue().GetValues()
		if len(l) == 0 || l[0].GetStructValue().GetFields()["label"].GetStringValue() == "" {
			t.Errorf("prediction %d = %v; want a label", i, p)
		}
	}
	raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: endpoint, HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"instances":["hello"]}`)}})
	if err != nil || !strings.Contains(string(raw.GetData()), `"label"`) {
		t.Fatalf("RawPredict = %.300s, %v", raw.GetData(), err)
	}
}

// covers: google.cloud.aiplatform.v1.ModelGardenService/Deploy, google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict
//
// TestModelGardenDeploysAnMLXModel (#2148, opt-in): an MLX repository
// deploys on mlx-lm's server with MLX's Linux CPU backend; Predict and
// RawPredict map as on vLLM.
func TestModelGardenDeploysAnMLXModel(t *testing.T) {
	h, ctx := hfRuntimeContext(t, true)
	pc := predictionClientAt(t, h, EnvLocalAI)
	endpoint := hfDeployment(t, h, ctx, hfMLXModel)
	pr, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: []*structpb.Value{
		structInstance(t, map[string]any{"prompt": "Hello", "max_tokens": 8, "raw_response": true})}})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if got := pr.GetPredictions(); len(got) != 1 || got[0].GetStringValue() == "" {
		t.Fatalf("Predict = %v", got)
	}
	raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: endpoint, HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"messages":[{"role":"user","content":"Hi"}],"max_tokens":8}`)}})
	if err != nil || !strings.Contains(string(raw.GetData()), `"choices"`) {
		t.Fatalf("RawPredict = %.300s, %v", raw.GetData(), err)
	}
}

// covers: google.cloud.aiplatform.v1.ModelGardenService/GetPublisherModel
//
// TestModelGardenHuggingFacePublisherModel (#2148): GetPublisherModel with
// is_hugging_face_model reads the model from the Hub and describes its two
// deployment environments: supported_actions.deploy (the serving container
// Deploy runs) and deploy_gke.gke_yaml_configs, the Kubernetes manifests of
// its GKE deployment, with Google's Model Garden labels and hf-secret.
func TestModelGardenHuggingFacePublisherModel(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skipf("%s is not set", EnvLocalAI)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mg, err := aiplatform.NewModelGardenClient(ctx, gardenOpts(h, EnvLocalAI)...)
	if err != nil {
		t.Fatal(err)
	}
	defer mg.Close()
	pm, err := mg.GetPublisherModel(ctx, &aiplatformpb.GetPublisherModelRequest{Name: "publishers/onnx-community/models/bert-tiny-finetuned-sms-spam-detection-ONNX",
		IsHuggingFaceModel: true})
	if err != nil {
		t.Fatalf("GetPublisherModel: %v", err)
	}
	a := pm.GetSupportedActions()
	if pm.GetName() != "publishers/hf-onnx-community/models/bert-tiny-finetuned-sms-spam-detection-onnx@001" ||
		!strings.Contains(a.GetDeploy().GetContainerSpec().GetImageUri(), "python") || len(a.GetDeployGke().GetGkeYamlConfigs()) != 3 {
		t.Fatalf("GetPublisherModel = %v", pm)
	}
	all := strings.Join(a.GetDeployGke().GetGkeYamlConfigs(), "---\n")
	for _, want := range []string{"kind: Deployment", "kind: Service", "kind: ConfigMap", "examples.ai.gke.io/source: model-garden",
		"ai.gke.io/inference-server: onnx", "ONNX_FILE"} {
		if !strings.Contains(all, want) {
			t.Errorf("gke_yaml_configs lack %q", want)
		}
	}
}
