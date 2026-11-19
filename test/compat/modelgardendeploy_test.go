//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	aiplatformbeta "cloud.google.com/go/aiplatform/apiv1beta1"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// EnvHFDeploy opts in to TestModelGardenDeploysHuggingFaceModels. It
// downloads about 1.3 GB of models from huggingface.co and runs the vLLM CPU
// (about 5.5 GB) and Text Embeddings Inference CPU (about 0.7 GB, emulated on
// arm64) images, which is more than a CI shard's download and disk budget,
// so it is gated rather than run in every shard.
const EnvHFDeploy = "CLOUDBURROW_TEST_HF_DEPLOY"

// The models: small public ones, Apache-2.0 and MIT.
const (
	hfTextModel  = "Qwen/Qwen2.5-0.5B-Instruct" // 0.95 GB, text-generation
	hfEmbedModel = "BAAI/bge-small-en-v1.5"     // 0.38 GB, feature-extraction
)

func gardenOpts(h *Harness, envVar string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(h.Endpoint(envVar)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// covers: google.cloud.aiplatform.v1.ModelGardenService/Deploy, google.cloud.aiplatform.v1.EndpointService/GetEndpoint, google.cloud.aiplatform.v1.EndpointService/ListEndpoints, google.cloud.aiplatform.v1.EndpointService/UndeployModel, google.cloud.aiplatform.v1.EndpointService/DeleteEndpoint, google.cloud.aiplatform.v1.ModelService/GetModel, google.cloud.aiplatform.v1.ModelService/ListModels, google.cloud.aiplatform.v1.ModelService/DeleteModel, google.cloud.aiplatform.v1beta1.PredictionService/ChatCompletions
//
// TestModelGardenDeploysHuggingFaceModels (#1840): through the official
// aiplatform Go clients, ModelGardenService.Deploy deploys a small public
// text-generation model (served by vLLM) and a small embedding model
// (served by Text Embeddings Inference) from Hugging Face; each operation
// completes with the Endpoint and Model it created. Predict answers in the
// shapes Google documents for Model Garden deployments (vLLM's "Prompt:
// ... Output: ..." strings, TEI's vectors), RawPredict relays an OpenAI
// completions body, StreamRawPredict streams chat completion events and
// v1beta1 ChatCompletions answers the OpenAI chat body. A
// hugging_face_access_token in the request is refused. Undeploy frees the
// containers, and DeleteEndpoint and DeleteModel delete the rest; deleting
// a deployed model is FAILED_PRECONDITION.
func TestModelGardenDeploysHuggingFaceModels(t *testing.T) {
	if os.Getenv(EnvHFDeploy) == "" {
		t.Skipf("set %s=1 to deploy %s and %s from huggingface.co (about 1.3 GB of models and 6 GB of engine images)",
			EnvHFDeploy, hfTextModel, hfEmbedModel)
	}
	h := New(t)
	opts := gardenOpts(h, EnvPrediction)
	// Deploying downloads the models and starts the engines: minutes, not
	// the harness's 60-second context.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()
	mg, err := aiplatform.NewModelGardenClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mg.Close() }) // after the cleanup below, which uses it
	ec, err := aiplatform.NewEndpointClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() }) // after the cleanup below, which uses it
	mc, err := aiplatform.NewModelClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mc.Close() }) // after the cleanup below, which uses it
	pc := predictionClientAt(t, h, EnvPrediction)
	bc, err := aiplatformbeta.NewPredictionClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close() }) // after the cleanup below, which uses it
	parent := runParent(h)

	// A token in the request is refused before anything is created.
	_, err = mg.Deploy(ctx, &aiplatformpb.DeployRequest{Destination: parent,
		Artifacts:   &aiplatformpb.DeployRequest_HuggingFaceModelId{HuggingFaceModelId: hfTextModel},
		ModelConfig: &aiplatformpb.DeployRequest_ModelConfig{HuggingFaceAccessToken: "hf_not-a-real-token"}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "HF_TOKEN") || strings.Contains(err.Error(), "hf_not-a-real-token") {
		t.Fatalf("Deploy with hugging_face_access_token = %v; want INVALID_ARGUMENT naming HF_TOKEN, without the token", err)
	}

	// Whatever a failed run leaves is removed: every compat-qwen or
	// compat-bge endpoint, its deployed model and its model.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for it := ec.ListEndpoints(cctx, &aiplatformpb.ListEndpointsRequest{Parent: parent}); ; {
			e, err := it.Next()
			if err != nil {
				break
			}
			if e.GetDisplayName() != "compat-qwen-endpoint" && e.GetDisplayName() != "compat-bge-endpoint" {
				continue
			}
			for _, d := range e.GetDeployedModels() {
				if op, err := ec.UndeployModel(cctx, &aiplatformpb.UndeployModelRequest{Endpoint: e.GetName(), DeployedModelId: d.GetId()}); err == nil {
					_, _ = op.Wait(cctx)
				}
				if op, err := mc.DeleteModel(cctx, &aiplatformpb.DeleteModelRequest{Name: d.GetModel()}); err == nil {
					_ = op.Wait(cctx)
				}
			}
			if op, err := ec.DeleteEndpoint(cctx, &aiplatformpb.DeleteEndpointRequest{Name: e.GetName()}); err == nil {
				_ = op.Wait(cctx)
			}
		}
		for it := mc.ListModels(cctx, &aiplatformpb.ListModelsRequest{Parent: parent}); ; {
			m, err := it.Next()
			if err != nil {
				break
			}
			if m.GetDisplayName() == "compat-qwen-model" || m.GetDisplayName() == "compat-bge-model" {
				if op, err := mc.DeleteModel(cctx, &aiplatformpb.DeleteModelRequest{Name: m.GetName()}); err == nil {
					_ = op.Wait(cctx)
				}
			}
		}
	})

	deploy := func(id, user string) *aiplatformpb.DeployResponse {
		t.Helper()
		op, err := mg.Deploy(ctx, &aiplatformpb.DeployRequest{Destination: parent,
			Artifacts:      &aiplatformpb.DeployRequest_HuggingFaceModelId{HuggingFaceModelId: id},
			ModelConfig:    &aiplatformpb.DeployRequest_ModelConfig{AcceptEula: true, ModelDisplayName: user + "-model"},
			EndpointConfig: &aiplatformpb.DeployRequest_EndpointConfig{EndpointDisplayName: user + "-endpoint"},
			DeployConfig: &aiplatformpb.DeployRequest_DeployConfig{DedicatedResources: &aiplatformpb.DedicatedResources{
				MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-4"}, MinReplicaCount: 1}}})
		if err != nil {
			t.Fatalf("Deploy %s: %v", id, err)
		}
		md, err := op.Metadata()
		if err != nil || md.GetDestination() != parent || md.GetModelId() == "" {
			t.Fatalf("Deploy %s metadata = %v, %v; want destination %s and a model ID", id, md, err, parent)
		}
		wctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
		defer cancel()
		resp, err := op.Wait(wctx)
		if err != nil {
			t.Fatalf("Deploy %s: waiting: %v", id, err)
		}
		if !strings.HasPrefix(resp.GetEndpoint(), parent+"/endpoints/") || !strings.HasPrefix(resp.GetModel(), parent+"/models/") ||
			!strings.HasPrefix(resp.GetPublisherModel(), "publishers/hf-") {
			t.Fatalf("Deploy %s = %v; want an endpoint, a model and the hf- publisher model", id, resp)
		}
		return resp
	}
	text := deploy(hfTextModel, "compat-qwen")
	embed := deploy(hfEmbedModel, "compat-bge")

	// The registry has both.
	e, err := ec.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: text.GetEndpoint()})
	if err != nil {
		t.Fatal(err)
	}
	if e.GetDisplayName() != "compat-qwen-endpoint" || len(e.GetDeployedModels()) != 1 || e.GetDeployedModels()[0].GetModel() != text.GetModel() {
		t.Fatalf("GetEndpoint = %v; want compat-qwen-endpoint with one deployed model of %s", e, text.GetModel())
	}
	dm := e.GetDeployedModels()[0]
	if dm.GetDedicatedResources().GetMachineSpec().GetMachineType() != "n1-standard-4" || e.GetTrafficSplit()[dm.GetId()] != 100 {
		t.Errorf("deployed model = %v, traffic %v; want n1-standard-4 and 100%% traffic", dm, e.GetTrafficSplit())
	}
	found := 0
	for it := ec.ListEndpoints(ctx, &aiplatformpb.ListEndpointsRequest{Parent: parent}); ; {
		x, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if x.GetName() == text.GetEndpoint() || x.GetName() == embed.GetEndpoint() {
			found++
		}
	}
	if found != 2 {
		t.Errorf("ListEndpoints found %d of the 2 deployed endpoints", found)
	}
	m, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: text.GetModel()})
	if err != nil {
		t.Fatal(err)
	}
	if m.GetDisplayName() != "compat-qwen-model" || !strings.Contains(m.GetContainerSpec().GetImageUri(), "vllm") ||
		m.GetBaseModelSource().GetModelGardenSource().GetPublicModelName() != text.GetPublisherModel() || len(m.GetDeployedModels()) != 1 {
		t.Fatalf("GetModel = %v; want the vLLM container, its Model Garden source and one deployment", m)
	}
	nModels := 0
	for it := mc.ListModels(ctx, &aiplatformpb.ListModelsRequest{Parent: parent}); ; {
		x, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if x.GetName() == text.GetModel() || x.GetName() == embed.GetModel() {
			nModels++
		}
	}
	if nModels != 2 {
		t.Errorf("ListModels found %d of the 2 models", nModels)
	}

	// vLLM :predict, Model Garden's prompt instance.
	inst, _ := structpb.NewValue(map[string]any{"prompt": "The capital of France is", "max_tokens": 8, "temperature": 0})
	pr, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: text.GetEndpoint(), Instances: []*structpb.Value{inst}})
	if err != nil {
		t.Fatalf("Predict (vLLM): %v", err)
	}
	if len(pr.GetPredictions()) != 1 || pr.GetDeployedModelId() != dm.GetId() {
		t.Fatalf("Predict (vLLM) = %v; want one prediction from deployed model %s", pr, dm.GetId())
	}
	got := pr.GetPredictions()[0].GetStringValue()
	if !strings.HasPrefix(got, "Prompt:\nThe capital of France is\nOutput:\n") || !strings.Contains(got, "Paris") {
		t.Errorf("Predict (vLLM) prediction = %q; want Model Garden's Prompt:/Output: string naming Paris", got)
	}
	// The chatCompletions request format through :predict.
	chatInst, _ := structpb.NewValue(map[string]any{"@requestFormat": "chatCompletions", "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": "Reply with the word hello."}}})
	pr, err = pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: text.GetEndpoint(), Instances: []*structpb.Value{chatInst}})
	if err != nil {
		t.Fatalf("Predict (chatCompletions): %v", err)
	}
	choices := pr.GetPredictions()[0].GetStructValue().GetFields()["choices"].GetListValue().GetValues()
	if len(choices) == 0 || choices[0].GetStructValue().GetFields()["message"].GetStructValue().GetFields()["content"].GetStringValue() == "" {
		t.Errorf("Predict (chatCompletions) = %v; want a chat completion with a message", pr.GetPredictions()[0])
	}

	// :rawPredict with an OpenAI completions body.
	raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: text.GetEndpoint(), HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"prompt":"1, 2, 3,","max_tokens":4,"temperature":0}`)}})
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	var comp struct {
		Model   string `json:"model"`
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw.GetData(), &comp); err != nil || len(comp.Choices) != 1 || comp.Model != hfTextModel {
		t.Errorf("RawPredict = %s (%v); want an OpenAI completion from %s", raw.GetData(), err, hfTextModel)
	}

	// :streamRawPredict streams chat completion chunks.
	sr, err := pc.StreamRawPredict(ctx, &aiplatformpb.StreamRawPredictRequest{Endpoint: text.GetEndpoint(), HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"messages":[{"role":"user","content":"Count to three."}],"max_tokens":16}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var events int
	var done bool
	for {
		b, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("StreamRawPredict: %v", err)
		}
		s := string(b.GetData())
		if strings.HasPrefix(s, "data: {") && strings.Contains(s, "chat.completion.chunk") {
			events++
		}
		done = done || strings.Contains(s, "data: [DONE]")
	}
	if events < 2 || !done {
		t.Errorf("StreamRawPredict sent %d chunk events (done %v); want several and data: [DONE]", events, done)
	}

	// v1beta1 ChatCompletions, the OpenAI-compatible path; model may be "".
	cs, err := bc.ChatCompletions(ctx, &betapb.ChatCompletionsRequest{Endpoint: text.GetEndpoint(), HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"model":"","messages":[{"role":"user","content":"What is the capital of France? Answer in one word."}],"max_tokens":8,"temperature":0}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	for {
		b, err := cs.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ChatCompletions: %v", err)
		}
		body = append(body, b.GetData()...)
	}
	var chat struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chat); err != nil || chat.Object != "chat.completion" || len(chat.Choices) != 1 ||
		!strings.Contains(chat.Choices[0].Message.Content, "Paris") {
		t.Errorf("ChatCompletions = %s (%v); want a chat.completion naming Paris", body, err)
	}

	// TEI :predict: Model Garden's {"inputs"} instances, one vector each.
	in1, _ := structpb.NewValue(map[string]any{"inputs": "What is deep learning?"})
	in2, _ := structpb.NewValue(map[string]any{"inputs": "Deep learning is a kind of machine learning."})
	pr, err = pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: embed.GetEndpoint(), Instances: []*structpb.Value{in1, in2}})
	if err != nil {
		t.Fatalf("Predict (TEI): %v", err)
	}
	if len(pr.GetPredictions()) != 2 {
		t.Fatalf("Predict (TEI) = %d predictions; want 2", len(pr.GetPredictions()))
	}
	for i, p := range pr.GetPredictions() {
		vecs := p.GetListValue().GetValues()
		if len(vecs) != 1 || len(vecs[0].GetListValue().GetValues()) != 384 {
			t.Errorf("Predict (TEI) prediction %d = %d vectors; want one 384-dimensional bge-small vector", i, len(vecs))
		}
	}
	// ChatCompletions on an embedding endpoint is refused.
	if cs, err := bc.ChatCompletions(ctx, &betapb.ChatCompletionsRequest{Endpoint: embed.GetEndpoint(), HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}}); err == nil {
		_, err = cs.Recv()
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("ChatCompletions on the TEI endpoint = %v; want FAILED_PRECONDITION", err)
		}
	}

	// A deployed model cannot be deleted; undeploy, then delete everything.
	if _, err := mc.DeleteModel(ctx, &aiplatformpb.DeleteModelRequest{Name: text.GetModel()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteModel of a deployed model = %v; want FAILED_PRECONDITION", err)
	}
	for _, r := range []*aiplatformpb.DeployResponse{text, embed} {
		e, err := ec.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: r.GetEndpoint()})
		if err != nil {
			t.Fatal(err)
		}
		op, err := ec.UndeployModel(ctx, &aiplatformpb.UndeployModelRequest{Endpoint: r.GetEndpoint(), DeployedModelId: e.GetDeployedModels()[0].GetId()})
		if err != nil {
			t.Fatalf("UndeployModel: %v", err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("UndeployModel: %v", err)
		}
		if _, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: r.GetEndpoint(), Instances: []*structpb.Value{in1}}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("Predict after UndeployModel = %v; want FAILED_PRECONDITION", err)
		}
		dop, err := ec.DeleteEndpoint(ctx, &aiplatformpb.DeleteEndpointRequest{Name: r.GetEndpoint()})
		if err != nil {
			t.Fatalf("DeleteEndpoint: %v", err)
		}
		if err := dop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		mop, err := mc.DeleteModel(ctx, &aiplatformpb.DeleteModelRequest{Name: r.GetModel()})
		if err != nil {
			t.Fatalf("DeleteModel: %v", err)
		}
		if err := mop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := ec.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: r.GetEndpoint()}); status.Code(err) != codes.NotFound {
			t.Errorf("GetEndpoint after DeleteEndpoint = %v; want NOT_FOUND", err)
		}
		if _, err := mc.GetModel(ctx, &aiplatformpb.GetModelRequest{Name: r.GetModel()}); status.Code(err) != codes.NotFound {
			t.Errorf("GetModel after DeleteModel = %v; want NOT_FOUND", err)
		}
	}
}
