//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/protobuf/types/known/structpb"
)

// hfClusterModel is the small public model TestModelGardenDeploysAHubModelOnTheCluster
// deploys: 70 MB of safetensors weights, Apache-2.0, 384-dimensional
// sentence embeddings, served by Text Embeddings Inference.
const hfClusterModel = "sentence-transformers/paraphrase-MiniLM-L3-v2"

// covers: google.cloud.aiplatform.v1.ModelGardenService/Deploy, google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict, google.cloud.aiplatform.v1.EndpointService/GetEndpoint, google.cloud.aiplatform.v1.EndpointService/UndeployModel, google.cloud.aiplatform.v1.EndpointService/DeleteEndpoint, google.cloud.aiplatform.v1.ModelService/GetModel, google.cloud.aiplatform.v1.ModelService/DeleteModel
//
// TestModelGardenDeploysAHubModelOnTheCluster (#2148): through the official
// aiplatform Go clients, ModelGardenService.Deploy deploys a small public
// sentence-transformers model from Hugging Face on the instance's
// Kubernetes cluster (a Text Embeddings Inference Deployment whose pod pulls
// the weights from the Hub), and the operation completes with the Endpoint
// and Model. Predict answers TEI's vectors in Model Garden's shape and
// RawPredict relays TEI's own {"inputs"} body; Undeploy, DeleteEndpoint and
// DeleteModel remove it all.
func TestModelGardenDeploysAHubModelOnTheCluster(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skipf("%s is not set: the local AI endpoint, which serves Model Garden, is not running", EnvLocalAI)
	}
	opts := gardenOpts(h, EnvLocalAI)
	// The deploy pulls TEI's image (about 0.3 GB) and the model's weights
	// inside the pod: minutes, not the harness's 60-second context.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
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
	pc := predictionClientAt(t, h, EnvLocalAI)
	parent := runParent(h)

	op, err := mg.Deploy(ctx, &aiplatformpb.DeployRequest{Destination: parent,
		Artifacts:      &aiplatformpb.DeployRequest_HuggingFaceModelId{HuggingFaceModelId: hfClusterModel},
		ModelConfig:    &aiplatformpb.DeployRequest_ModelConfig{ModelDisplayName: "compat-minilm-model"},
		EndpointConfig: &aiplatformpb.DeployRequest_EndpointConfig{EndpointDisplayName: "compat-minilm-endpoint"}})
	if err != nil {
		t.Fatalf("Deploy %s: %v", hfClusterModel, err)
	}
	var endpoint, model string
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
		if model != "" {
			if op, err := mc.DeleteModel(cctx, &aiplatformpb.DeleteModelRequest{Name: model}); err == nil {
				_ = op.Wait(cctx)
			}
		}
	})
	md, err := op.Metadata()
	if err != nil || md.GetModelId() == "" || md.GetPublisherModel() != "publishers/hf-sentence-transformers/models/paraphrase-minilm-l3-v2@001" {
		t.Fatalf("Deploy metadata = %v, %v", md, err)
	}
	model = parent + "/models/" + md.GetModelId()
	resp, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("Deploy %s: waiting: %v", hfClusterModel, err)
	}
	endpoint = resp.GetEndpoint()
	if !strings.HasPrefix(endpoint, parent+"/endpoints/") || resp.GetModel() != model {
		t.Fatalf("Deploy = %v; want an endpoint and %s", resp, model)
	}
	e, err := ec.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if e.GetDisplayName() != "compat-minilm-endpoint" || len(e.GetDeployedModels()) != 1 || e.GetDeployedModels()[0].GetModel() != model {
		t.Fatalf("GetEndpoint = %v; want one deployed model of %s", e, model)
	}
	m, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: model})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.GetContainerSpec().GetImageUri(), "text-embeddings-inference") ||
		m.GetModelSourceInfo().GetSourceType() != aiplatformpb.ModelSourceInfo_MODEL_GARDEN {
		t.Errorf("GetModel = %v; want TEI's image and a Model Garden source", m)
	}

	// Predict: one 384-dimensional vector per input, in Model Garden's
	// predictions.
	inst, _ := structpb.NewValue(map[string]any{"inputs": "What is deep learning?"})
	pr, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: []*structpb.Value{inst}})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(pr.GetPredictions()) != 1 {
		t.Fatalf("Predict = %v; want one prediction", pr)
	}
	vecs := pr.GetPredictions()[0].GetListValue().GetValues()
	if len(vecs) != 1 || len(vecs[0].GetListValue().GetValues()) != 384 || pr.GetDeployedModelId() != e.GetDeployedModels()[0].GetId() {
		t.Fatalf("Predict = %v; want one 384-dimensional vector from the deployed model", pr.GetPredictions()[0])
	}

	// RawPredict: TEI's own body and answer.
	raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: endpoint,
		HttpBody: &httpbody.HttpBody{ContentType: "application/json", Data: []byte(`{"inputs":["one","two"]}`)}})
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	var out [][]float64
	if err := json.Unmarshal(raw.GetData(), &out); err != nil || len(out) != 2 || len(out[0]) != 384 {
		t.Fatalf("RawPredict = %.200s (%v); want two 384-dimensional vectors", raw.GetData(), err)
	}

	uop, err := ec.UndeployModel(ctx, &aiplatformpb.UndeployModelRequest{Endpoint: endpoint, DeployedModelId: e.GetDeployedModels()[0].GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	dop, err := ec.DeleteEndpoint(ctx, &aiplatformpb.DeleteEndpointRequest{Name: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	endpoint = ""
	mop, err := mc.DeleteModel(ctx, &aiplatformpb.DeleteModelRequest{Name: model})
	if err != nil {
		t.Fatal(err)
	}
	if err := mop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	model = ""
}
