// Package modelgarden deploys Hugging Face models from Model Garden on a
// local endpoint (#1840): ModelGardenService.Deploy's Endpoint, Model and
// DeployedModel, served by the engine Google's Model Garden uses for the
// model's task.
//
// Google picks the serving container from the model's Hugging Face pipeline
// task (docs: models/open-models/use-hugging-face-models): vLLM for text
// generation and TEI (Hugging Face Text Embeddings Inference) for text
// embeddings. CloudBurrow runs the upstream CPU builds of those two engines,
// pinned by digest in dependencies.json, with the serving arguments Google's
// Model Garden deployments pass (models/open-models/vllm/use-vllm), and
// translates Vertex's predict bodies to the engines' own routes the way
// Google's Model Garden containers do.
package modelgarden

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// The pinned engine images (dependencies.json, localai.modelGardenVLLM and
// localai.modelGardenTEI).
const (
	// VLLMImage is vLLM's official CPU image (Apache-2.0), a linux/arm64 and
	// linux/amd64 index.
	VLLMImage = "docker.io/vllm/vllm-openai-cpu:v0.34.0@sha256:1640fdc27ae499572f611ffd4dc55e0b07ce97608f33566d5834c98671b115d4"
	// TEIImage is Hugging Face Text Embeddings Inference's CPU image
	// (Apache-2.0). It is published for linux/amd64 only, so an arm64 host
	// runs it emulated.
	TEIImage = "ghcr.io/huggingface/text-embeddings-inference:cpu-1.9@sha256:2538ea1c9640d3763b15af668039d24172d063b42337b0c27796fc2be180c78d"
	// TEIArm64Image is the same release's arm64 CPU build (Apache-2.0,
	// dependencies.json localai.modelGardenTEIArm64), which a cluster whose
	// node is linux/arm64 runs natively (#2148).
	TEIArm64Image = "ghcr.io/huggingface/text-embeddings-inference:cpu-arm64-1.9@sha256:798cd7615e060a72ccbdf19aa0ada52289c6a0147490195bafaece6ce04d70f1"
)

// Engine names.
const (
	EngineVLLM = "vllm"
	EngineTEI  = "tei"
)

// containerPort is the port both engines serve on inside their container,
// Vertex's AIP_HTTP_PORT default and Model Garden's serving_container_ports.
const containerPort = 8080

// Engine is one serving runtime.
type Engine struct {
	Name     string
	Title    string
	Image    string
	Platform string // set when the image is published for one platform only
	// HealthRoute and PredictRoute are the engine's own routes, recorded in
	// the Model's containerSpec.
	HealthRoute, PredictRoute string
	ShmSize                   string
}

var engines = map[string]Engine{
	EngineVLLM: {Name: EngineVLLM, Title: "vLLM", Image: VLLMImage, HealthRoute: "/health", PredictRoute: "/v1/completions",
		// Model Garden sets serving_container_shared_memory_size_mb to 16 GB
		// on GPUs; vLLM's CPU engine needs more than Docker's 64 MB default.
		ShmSize: "2g"},
	EngineTEI: {Name: EngineTEI, Title: "Text Embeddings Inference", Image: TEIImage, Platform: "linux/amd64",
		HealthRoute: "/health", PredictRoute: "/embed"},
}

// EngineByName returns a known engine.
func EngineByName(name string) (Engine, bool) {
	e, ok := engines[name]
	return e, ok
}

// EngineFor picks the engine for a Hugging Face pipeline task, as Model
// Garden does: the documented tasks are text-generation, text2text-generation,
// image-text-to-text (vLLM), feature-extraction and sentence-similarity (TEI)
// and text-to-image (a diffusers container, not served here).
func EngineFor(pipelineTag string) (Engine, error) {
	switch pipelineTag {
	case "text-generation", "text2text-generation", "image-text-to-text":
		return engines[EngineVLLM], nil
	case "feature-extraction", "sentence-similarity":
		return engines[EngineTEI], nil
	case "":
		return Engine{}, fmt.Errorf("the model declares no Hugging Face pipeline task, so no serving container can be chosen; " +
			"Model Garden deploys text-generation, text2text-generation, image-text-to-text, feature-extraction and sentence-similarity models")
	}
	return Engine{}, fmt.Errorf("pipeline task %q is not served locally: CloudBurrow deploys text-generation, text2text-generation and "+
		"image-text-to-text models on vLLM and feature-extraction and sentence-similarity models on Text Embeddings Inference", pipelineTag)
}

// EngineForModel picks the engine for a Hub model from its task, library
// and tags (#2148): a sentence-transformers model, or a feature-extraction
// or sentence-similarity one, on Text Embeddings Inference; a causal or
// seq2seq LM, or an image-text-to-text model, on vLLM, which loads
// transformers weights (safetensors or PyTorch), not a GGUF, MLX or ONNX
// export alone. Everything else is refused with the reason, which Model
// Garden's card shows beside a disabled Deploy.
func EngineForModel(task, library string, tags []string) (Engine, error) {
	has := map[string]bool{}
	for _, t := range tags {
		has[t] = true
	}
	switch library {
	case "gguf", "mlx", "onnx", "openvino", "llama.cpp", "ctranslate2":
		if !has["safetensors"] && !has["pytorch"] {
			return Engine{}, fmt.Errorf("the model is published as %s only; vLLM and Text Embeddings Inference load "+
				"transformers weights (safetensors or PyTorch)", library)
		}
	}
	if library == "sentence-transformers" && (task == "" || task == "feature-extraction" || task == "sentence-similarity") {
		return engines[EngineTEI], nil
	}
	if library == "diffusers" {
		return Engine{}, fmt.Errorf("diffusers models (%s) need Model Garden's diffusers container, which is not served locally", task)
	}
	return EngineFor(task)
}

// ImageFor is the engine's image for a node architecture (amd64, arm64).
func (e Engine) ImageFor(arch string) string {
	if e.Name == EngineTEI && arch == "arm64" {
		return TEIArm64Image
	}
	return e.Image
}

// PodArgs are the engine's arguments in a cluster pod, which pulls the
// weights itself from the Hub at revision into /data (#2148), with the
// rest of Model Garden's serving arguments as Args gives them.
func (e Engine) PodArgs(hfID, revision string, contextLen int) []string {
	switch e.Name {
	case EngineVLLM:
		n := maxModelLen
		if contextLen > 0 && contextLen < n {
			n = contextLen
		}
		return []string{"--host=0.0.0.0", "--port=" + strconv.Itoa(containerPort), "--model=" + hfID,
			"--revision=" + revision, "--served-model-name=" + hfID, "--max-model-len=" + strconv.Itoa(n),
			"--dtype=auto", "--max-num-seqs=256", "--disable-log-stats", "--download-dir=/data"}
	case EngineTEI:
		return []string{"--model-id=" + hfID, "--revision=" + revision, "--hostname=0.0.0.0",
			"--port=" + strconv.Itoa(containerPort), "--huggingface-hub-cache=/data"}
	}
	return nil
}

// PodEnv is the pod's environment: Model Garden's MODEL_ID and the Hub
// cache. HF_TOKEN is not here: a gated model's comes from its Secret.
func (e Engine) PodEnv(hfID string) map[string]string {
	env := map[string]string{"MODEL_ID": hfID, "HF_HOME": "/data", "HF_HUB_DISABLE_TELEMETRY": "1"}
	if e.Name == EngineVLLM {
		env["VLLM_CPU_KVCACHE_SPACE"] = "2"
		env["VLLM_NO_USAGE_STATS"] = "1"
		env["DO_NOT_TRACK"] = "1"
	}
	return env
}

// ContextLength reads a config.json's context length, or 0.
func ContextLength(config []byte) int {
	var c map[string]any
	if json.Unmarshal(config, &c) != nil {
		return 0
	}
	for _, k := range []string{"max_position_embeddings", "n_positions", "max_sequence_length", "seq_length"} {
		if f, ok := c[k].(float64); ok && f > 0 {
			return int(f)
		}
	}
	return 0
}

// maxModelLen is Model Garden's vLLM default (max_model_len = 4096), capped
// at the model's own context length so a short-context model still starts.
const maxModelLen = 4096

// contextLength reads the model's context length from its config.json, or 0.
func contextLength(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return 0
	}
	return ContextLength(b)
}

// Args are the engine's serving arguments for a model mounted at /model and
// served under name, mirroring Model Garden's: vLLM's "--host=0.0.0.0
// --port=8080 --model=... --max-model-len=4096 --dtype=auto
// --max-num-seqs=256 --disable-log-stats" (GPU-only flags such as
// --tensor-parallel-size, --swap-space and --gpu-memory-utilization have no
// CPU meaning and are left out), and TEI's model and port.
func (e Engine) Args(dir, name string) []string {
	switch e.Name {
	case EngineVLLM:
		n := maxModelLen
		if c := contextLength(dir); c > 0 && c < n {
			n = c
		}
		return []string{"--host=0.0.0.0", "--port=" + strconv.Itoa(containerPort), "--model=/model",
			"--served-model-name=" + name, "--max-model-len=" + strconv.Itoa(n), "--dtype=auto",
			"--max-num-seqs=256", "--disable-log-stats"}
	case EngineTEI:
		return []string{"--model-id=/model", "--hostname=0.0.0.0", "--port=" + strconv.Itoa(containerPort)}
	}
	return nil
}

// Env is the container's environment: Model Garden's MODEL_ID, and offline
// mode, since the files are already local. No token is ever passed in.
func (e Engine) Env(name string) map[string]string {
	env := map[string]string{"MODEL_ID": name, "HF_HUB_OFFLINE": "1"}
	if e.Name == EngineVLLM {
		// The CPU engine's KV cache, in GiB: Model Garden's
		// --gpu-memory-utilization has no CPU meaning.
		env["VLLM_CPU_KVCACHE_SPACE"] = "2"
	}
	return env
}
