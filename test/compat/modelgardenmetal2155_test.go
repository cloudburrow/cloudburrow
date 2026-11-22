//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	aiplatformbeta "cloud.google.com/go/aiplatform/apiv1beta1"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/protobuf/types/known/structpb"
)

// EnvMetal runs the Apple GPU tests (#2155): they download llama.cpp's
// macOS release, an MLX Python environment and two models of a few
// hundred MB, and need a Mac with Apple silicon.
const EnvMetal = "CLOUDBURROW_TEST_METAL"

// The models the Apple GPU tests deploy, both ungated and under 500 MB.
const (
	// metalGGUFModel's Q4_K_M file is 491 MB, Apache-2.0.
	metalGGUFModel = "Qwen/Qwen2.5-0.5B-Instruct-GGUF"
	// metalMLXModel is a 290 MB 4-bit MLX conversion, Apache-2.0.
	metalMLXModel = "mlx-community/Qwen2.5-0.5B-Instruct-4bit"
)

func metalContext(t *testing.T) (*Harness, context.Context) {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skipf("the Apple GPU (Metal) is on macOS on Apple silicon only; this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if os.Getenv(EnvMetal) == "" {
		t.Skipf("set %s=1 to run it: it downloads llama.cpp, mlx-lm and two models of a few hundred MB", EnvMetal)
	}
	return hfRuntimeContext(t, false)
}

func onL4() *aiplatformpb.DeployRequest_DeployConfig {
	return &aiplatformpb.DeployRequest_DeployConfig{DedicatedResources: &aiplatformpb.DedicatedResources{
		MachineSpec:     &aiplatformpb.MachineSpec{AcceleratorType: aiplatformpb.AcceleratorType_NVIDIA_L4, AcceleratorCount: 1},
		MinReplicaCount: 1}}
}

func onCPU() *aiplatformpb.DeployRequest_DeployConfig {
	return &aiplatformpb.DeployRequest_DeployConfig{DedicatedResources: &aiplatformpb.DedicatedResources{
		MachineSpec: &aiplatformpb.MachineSpec{MachineType: "local-cpu"}, MinReplicaCount: 1}}
}

// wantAccelerator checks the deployed model's machine spec.
func wantAccelerator(t *testing.T, h *Harness, ctx context.Context, endpoint string, want aiplatformpb.AcceleratorType) {
	t.Helper()
	ec, err := aiplatform.NewEndpointClient(ctx, gardenOpts(h, EnvLocalAI)...)
	if err != nil {
		t.Fatal(err)
	}
	defer ec.Close()
	e, err := ec.GetEndpoint(ctx, &aiplatformpb.GetEndpointRequest{Name: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.GetDeployedModels()) != 1 {
		t.Fatalf("endpoint %s has %d deployed models", endpoint, len(e.GetDeployedModels()))
	}
	ms := e.GetDeployedModels()[0].GetDedicatedResources().GetMachineSpec()
	if ms.GetAcceleratorType() != want || (want != aiplatformpb.AcceleratorType_ACCELERATOR_TYPE_UNSPECIFIED && ms.GetAcceleratorCount() != 1) {
		t.Errorf("machine spec = %v; want %s", ms, want)
	}
}

// chat sends an OpenAI chat body through v1beta1 ChatCompletions.
func chat(t *testing.T, h *Harness, ctx context.Context, endpoint, question string) string {
	t.Helper()
	bc, err := aiplatformbeta.NewPredictionClient(ctx, gardenOpts(h, EnvLocalAI)...)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	cs, err := bc.ChatCompletions(ctx, &betapb.ChatCompletionsRequest{Endpoint: endpoint, HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"model":"","messages":[{"role":"user","content":` + jsonString(question) +
			`}],"max_tokens":16,"temperature":0}`)}})
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
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Choices) != 1 {
		t.Fatalf("ChatCompletions = %s, %v", body, err)
	}
	return out.Choices[0].Message.Content
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// predictAndRaw checks Predict's Model Garden instances and RawPredict's
// OpenAI completions body.
func predictAndRaw(t *testing.T, h *Harness, ctx context.Context, endpoint string) {
	t.Helper()
	pc := predictionClientAt(t, h, EnvLocalAI)
	pr, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: []*structpb.Value{
		structInstance(t, map[string]any{"prompt": "The capital of France is", "max_tokens": 8, "temperature": 0})}})
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if got := pr.GetPredictions(); len(got) != 1 || !strings.HasPrefix(got[0].GetStringValue(), "Prompt:\nThe capital of France is\nOutput:\n") ||
		!strings.Contains(got[0].GetStringValue(), "Paris") {
		t.Errorf("Predict = %v; want Model Garden's Prompt/Output answer naming Paris", got)
	}
	raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: endpoint, HttpBody: &httpbody.HttpBody{
		ContentType: "application/json", Data: []byte(`{"prompt":"The capital of France is","max_tokens":8,"temperature":0}`)}})
	if err != nil {
		t.Fatalf("RawPredict: %v", err)
	}
	var c struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw.GetData(), &c); err != nil || len(c.Choices) != 1 || !strings.Contains(c.Choices[0].Text, "Paris") {
		t.Errorf("RawPredict = %s, %v", raw.GetData(), err)
	}
}

// tokensPerSecond times a 128-token completion through RawPredict, after
// one warm-up: llama.cpp's own generation rate when it reports one, else
// the completion tokens over the call's wall time.
func tokensPerSecond(t *testing.T, h *Harness, ctx context.Context, endpoint string) float64 {
	t.Helper()
	pc := predictionClientAt(t, h, EnvLocalAI)
	body := []byte(`{"prompt":"Write a long story about a fox who learns to fly.","max_tokens":128,"temperature":0,"ignore_eos":true}`)
	best := 0.0
	for i := 0; i < 4; i++ {
		start := time.Now()
		raw, err := pc.RawPredict(ctx, &aiplatformpb.RawPredictRequest{Endpoint: endpoint,
			HttpBody: &httpbody.HttpBody{ContentType: "application/json", Data: body}})
		if err != nil {
			t.Fatalf("RawPredict: %v", err)
		}
		wall := time.Since(start).Seconds()
		var r struct {
			Usage struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Timings struct {
				PredictedPerSecond float64 `json:"predicted_per_second"`
			} `json:"timings"`
		}
		_ = json.Unmarshal(raw.GetData(), &r)
		tps := r.Timings.PredictedPerSecond
		if tps == 0 && wall > 0 {
			tps = float64(r.Usage.CompletionTokens) / wall
		}
		if i > 0 && tps > best {
			best = tps
		}
	}
	return best
}

// covers: google.cloud.aiplatform.v1.ModelGardenService/Deploy, google.cloud.aiplatform.v1.EndpointService/GetEndpoint, google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict, google.cloud.aiplatform.v1beta1.PredictionService/ChatCompletions, google.logging.v2.LoggingServiceV2/ListLogEntries
//
// TestModelGardenDeploysAGGUFModelOnTheAppleGPU (#2155): a GGUF model
// deployed with machine_spec NVIDIA_L4 × 1 runs llama-server on the Apple
// GPU, as a host process; Predict, RawPredict and ChatCompletions answer as
// on the cluster; the server's log in Cloud Logging names the Metal device
// and every layer offloaded to the GPU; and its generation is faster than
// the same model deployed on CPU (machine type local-cpu) in the cluster.
func TestModelGardenDeploysAGGUFModelOnTheAppleGPU(t *testing.T) {
	h, ctx := metalContext(t)
	gpu := hfDeploymentWith(t, h, ctx, metalGGUFModel, onL4())
	wantAccelerator(t, h, ctx, gpu, aiplatformpb.AcceleratorType_NVIDIA_L4)
	predictAndRaw(t, h, ctx, gpu)
	if got := chat(t, h, ctx, gpu, "What is the capital of France? Answer in one word."); !strings.Contains(got, "Paris") {
		t.Errorf("ChatCompletions = %q; want Paris", got)
	}
	gpuRate := tokensPerSecond(t, h, ctx, gpu)

	if h.Endpoint(EnvLogging) != "" {
		lc, err := vlogging.NewClient(ctx, loggingClientOptions(h)...)
		if err != nil {
			t.Fatal(err)
		}
		defer lc.Close()
		id := gpu[strings.LastIndexByte(gpu, '/')+1:]
		it := lc.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/" + h.Project()},
			Filter:   `logName="projects/` + h.Project() + `/logs/aiplatform.googleapis.com%2Fprediction_container" AND resource.type="aiplatform.googleapis.com/Endpoint" AND resource.labels.endpoint_id="` + id + `"`,
			PageSize: 1000})
		var device, offload bool
		for {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListLogEntries: %v", err)
			}
			line := e.GetTextPayload()
			if strings.Contains(line, "ggml_metal_init: found device") || strings.Contains(line, "using device MTL0") {
				device = true
				t.Logf("log: %s", line)
			}
			if strings.Contains(line, "load_tensors: offloaded") && strings.Contains(line, "layers to GPU") {
				offload = true
				t.Logf("log: %s", line)
				if strings.Contains(line, "offloaded 0/") {
					t.Errorf("no layer was offloaded to the GPU: %s", line)
				}
			}
		}
		if !device || !offload {
			t.Errorf("Cloud Logging has the Metal device %v and the offload line %v; want both", device, offload)
		}
	} else {
		t.Logf("%s is not set: the server's Metal log lines are not checked in Cloud Logging", EnvLogging)
	}

	cpu := hfDeploymentWith(t, h, ctx, metalGGUFModel, onCPU())
	wantAccelerator(t, h, ctx, cpu, aiplatformpb.AcceleratorType_ACCELERATOR_TYPE_UNSPECIFIED)
	predictAndRaw(t, h, ctx, cpu)
	cpuRate := tokensPerSecond(t, h, ctx, cpu)
	t.Logf("%s generation: Apple GPU (Metal, host) %.1f tokens/s; CPU (llama.cpp in the cluster) %.1f tokens/s; %.1fx",
		metalGGUFModel, gpuRate, cpuRate, gpuRate/cpuRate)
	if gpuRate <= cpuRate {
		t.Errorf("the Apple GPU generated %.1f tokens/s, no faster than the CPU's %.1f", gpuRate, cpuRate)
	}
}

// covers: google.cloud.aiplatform.v1.ModelGardenService/Deploy, google.cloud.aiplatform.v1.PredictionService/Predict, google.cloud.aiplatform.v1.PredictionService/RawPredict, google.cloud.aiplatform.v1beta1.PredictionService/ChatCompletions
//
// TestModelGardenDeploysAnMLXModelOnTheAppleGPU (#2155): a 4-bit MLX
// model deployed with NVIDIA_L4 runs mlx-lm's server on MLX's Metal
// backend, as a host process from CloudBurrow's own venv.
func TestModelGardenDeploysAnMLXModelOnTheAppleGPU(t *testing.T) {
	h, ctx := metalContext(t)
	gpu := hfDeploymentWith(t, h, ctx, metalMLXModel, onL4())
	wantAccelerator(t, h, ctx, gpu, aiplatformpb.AcceleratorType_NVIDIA_L4)
	predictAndRaw(t, h, ctx, gpu)
	if got := chat(t, h, ctx, gpu, "What is the capital of France? Answer in one word."); !strings.Contains(got, "Paris") {
		t.Errorf("ChatCompletions = %q; want Paris", got)
	}
	t.Logf("%s generation on the Apple GPU (MLX Metal, host): %.1f tokens/s", metalMLXModel, tokensPerSecond(t, h, ctx, gpu))
}
