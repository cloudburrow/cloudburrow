// Package modelgarden deploys Hugging Face models from Model Garden on a
// local endpoint (#1840): ModelGardenService.Deploy's Endpoint, Model and
// DeployedModel, served by the engine Google's Model Garden uses for the
// model's task, as a workload on the instance's Kubernetes cluster
// (cluster.go, #2148) or, for a model downloaded to the host, a Docker
// container beside it (docker.go).
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
	"sort"
	"strconv"
	"strings"
)

// The pinned engine images (dependencies.json, localai.modelGardenVLLM and
// localai.modelGardenTEI).
const (
	// VLLMImage is vLLM's official CPU image (Apache-2.0), a linux/arm64 and
	// linux/amd64 index.
	VLLMImage = "docker.io/vllm/vllm-openai-cpu:v0.34.0@sha256:1640fdc27ae499572f611ffd4dc55e0b07ce97608f33566d5834c98671b115d4"
	// TEIImage is Hugging Face Text Embeddings Inference's CPU image
	// (Apache-2.0), linux/amd64 only: Docker on an arm64 host runs it
	// emulated, and an arm64 cluster node runs TEIArm64Image instead.
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
	// EngineHFPyTorch is Google's Hugging Face PyTorch Inference container's
	// server (#2148): the transformers pipeline tasks and diffusers'
	// text-to-image.
	EngineHFPyTorch = "hf-pytorch"
	// EngineLlamaCpp is llama.cpp's server, for GGUF repositories.
	EngineLlamaCpp = "llama-cpp"
	// EngineMLX is mlx-lm's server on MLX's Linux CPU backend.
	EngineMLX = "mlx"
	// EngineONNX is ONNX Runtime, for ONNX-only repositories.
	EngineONNX = "onnx"
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
	// Python is set for the runtimes that run on PythonImage, installing
	// their pinned packages when the pod starts (runtimes.go).
	Python bool
	// OpenAI is set for the OpenAI-compatible servers (vLLM, llama.cpp,
	// mlx-lm), which take chat completions.
	OpenAI bool
	// Pipeline is set for the runtimes whose :predict body is the
	// container's own ({"instances", "parameters"} in, {"predictions"}
	// out): the PyTorch Inference container and ONNX Runtime.
	Pipeline bool
}

var engines = map[string]Engine{
	EngineVLLM: {Name: EngineVLLM, Title: "vLLM", Image: VLLMImage, HealthRoute: "/health", PredictRoute: "/v1/completions",
		// Model Garden sets serving_container_shared_memory_size_mb to 16 GB
		// on GPUs; vLLM's CPU engine needs more than Docker's 64 MB default.
		ShmSize: "2g", OpenAI: true},
	EngineTEI: {Name: EngineTEI, Title: "Text Embeddings Inference", Image: TEIImage, Platform: "linux/amd64",
		HealthRoute: "/health", PredictRoute: "/embed"},
	EngineHFPyTorch: {Name: EngineHFPyTorch, Title: "Hugging Face PyTorch Inference", Image: PythonImage,
		HealthRoute: "/health", PredictRoute: "/predict", Python: true, Pipeline: true, ShmSize: "1g"},
	EngineLlamaCpp: {Name: EngineLlamaCpp, Title: "llama.cpp", Image: LlamaCppImage, HealthRoute: "/health",
		PredictRoute: "/v1/completions", OpenAI: true},
	EngineMLX: {Name: EngineMLX, Title: "MLX (mlx-lm, CPU)", Image: PythonImage, HealthRoute: "/health",
		PredictRoute: "/v1/completions", Python: true, OpenAI: true},
	EngineONNX: {Name: EngineONNX, Title: "ONNX Runtime", Image: PythonImage, HealthRoute: "/health",
		PredictRoute: "/predict", Python: true, Pipeline: true},
}

// EngineByName returns a known engine.
func EngineByName(name string) (Engine, bool) {
	e, ok := engines[name]
	return e, ok
}

// EngineFor picks the engine for a Hugging Face pipeline task, as Model
// Garden does: the documented tasks are text-generation, text2text-generation,
// image-text-to-text (vLLM), feature-extraction and sentence-similarity (TEI)
// and text-to-image (a diffusers container). This is the engine for a model
// downloaded to the host and served in Docker, which runs vLLM and TEI only;
// EngineForModel picks among every runtime for a cluster deployment.
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
	return Engine{}, fmt.Errorf("pipeline task %q is not served from a download in Docker, which runs vLLM and Text Embeddings "+
		"Inference only: deploy it from Model Garden's Hugging Face tab, which runs every runtime on the instance's cluster", pipelineTag)
}

// EngineForModel picks the runtime for a Hub model from its task, library
// and tags (#2148):
//
//   - a GGUF repository on llama.cpp; an MLX one on mlx-lm; an ONNX-only one
//     (ONNX files and no transformers weights) on ONNX Runtime;
//   - a sentence-transformers model, or a feature-extraction or
//     sentence-similarity one, on Text Embeddings Inference;
//   - a causal or seq2seq LM, or an image-text-to-text model, on vLLM;
//   - diffusers' text-to-image and the other transformers pipeline tasks on
//     the Hugging Face PyTorch Inference container's server.
//
// Everything else (a task no runtime serves, or a library with no
// runtime) is refused with the reason, which Model Garden's card shows
// beside a disabled Deploy.
func EngineForModel(task, library string, tags []string) (Engine, error) {
	has := map[string]bool{}
	for _, t := range tags {
		has[t] = true
	}
	weights := has["safetensors"] || has["pytorch"]
	switch {
	case library == "gguf" || (has["gguf"] && !weights):
		switch task {
		case "", "text-generation", "image-text-to-text", "feature-extraction", "sentence-similarity":
			return engines[EngineLlamaCpp], nil
		}
		return Engine{}, fmt.Errorf("llama.cpp serves GGUF text-generation and embedding models, not %s", task)
	case library == "mlx" || has["mlx"]:
		switch task {
		case "", "text-generation", "text2text-generation":
			return engines[EngineMLX], nil
		}
		return Engine{}, fmt.Errorf("mlx-lm serves MLX text-generation models, not %s", task)
	case library == "onnx" || library == "transformers.js" || (has["onnx"] && !weights):
		if onnxTasks[task] {
			return engines[EngineONNX], nil
		}
		if task == "" {
			return Engine{}, fmt.Errorf("the ONNX model declares no pipeline task, so ONNX Runtime cannot choose its inputs and outputs")
		}
		return Engine{}, fmt.Errorf("ONNX Runtime serves %s, not %s", strings.Join(sortedTrue(onnxTasks), ", "), task)
	case library == "openvino", library == "ctranslate2", library == "llama.cpp" && !has["gguf"]:
		if !weights {
			return Engine{}, fmt.Errorf("the model is published as %s only; CloudBurrow serves transformers weights, GGUF, MLX and ONNX", library)
		}
	}
	if library == "sentence-transformers" && (task == "" || task == "feature-extraction" || task == "sentence-similarity") {
		return engines[EngineTEI], nil
	}
	if library == "diffusers" {
		if task == "text-to-image" || task == "" {
			return engines[EngineHFPyTorch], nil
		}
		return Engine{}, fmt.Errorf("diffusers models are served for text-to-image, which Model Garden documents, not %s", task)
	}
	if e, err := EngineFor(task); err == nil {
		return e, nil
	}
	if pipelineTasks[task] {
		return engines[EngineHFPyTorch], nil
	}
	if task == "" {
		return Engine{}, fmt.Errorf("the model declares no Hugging Face pipeline task, so no serving container can be chosen")
	}
	return Engine{}, fmt.Errorf("pipeline task %q has no serving runtime: the transformers pipeline does not serve it", task)
}

func sortedTrue(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Plan is what a cluster pod serves: the model, its revision and task, and
// for GGUF and ONNX the one file the runtime loads.
type Plan struct {
	HFID, Revision, Task, File string
	// ContextLen is the model's context length (vLLM caps
	// --max-model-len at it), 0 when unknown.
	ContextLen int
}

// PodSpec is an engine's container in a cluster pod: its command (the
// image's entrypoint when empty), arguments, environment, and the files of
// its ConfigMap, mounted at /etc/cloudburrow (the Python runtimes').
type PodSpec struct {
	Command, Args []string
	Env           map[string]string
	Files         map[string]string
}

// Pod is the engine's container for p. The weights are pulled from the Hub
// at p.Revision inside the pod, into /data.
func (e Engine) Pod(p Plan) PodSpec {
	env := map[string]string{"MODEL_ID": p.HFID, "HF_HOME": "/data/hf", "HF_HUB_DISABLE_TELEMETRY": "1", "DO_NOT_TRACK": "1"}
	switch e.Name {
	case EngineVLLM, EngineTEI:
		return PodSpec{Args: e.PodArgs(p.HFID, p.Revision, p.ContextLen), Env: e.PodEnv(p.HFID)}
	case EngineLlamaCpp:
		env["LLAMA_CACHE"] = "/data/llama.cpp"
		args := []string{"--host", "0.0.0.0", "--port", strconv.Itoa(containerPort),
			"--model-url", "https://huggingface.co/" + p.HFID + "/resolve/" + p.Revision + "/" + p.File,
			"--alias", p.HFID, "--no-webui"}
		if p.Task == "feature-extraction" || p.Task == "sentence-similarity" {
			args = append(args, "--embeddings")
		} else {
			n := maxModelLen
			if p.ContextLen > 0 && p.ContextLen < n {
				n = p.ContextLen
			}
			args = append(args, "--ctx-size", strconv.Itoa(n))
		}
		return PodSpec{Args: args, Env: env}
	}
	// The Python runtimes: Vertex's AIP_ variables, as Google's containers
	// read them, and the model's.
	env["RUNTIME"] = e.Name
	env["AIP_MODE"] = "PREDICTION"
	env["AIP_HTTP_PORT"] = strconv.Itoa(containerPort)
	env["AIP_HEALTH_ROUTE"] = e.HealthRoute
	env["AIP_PREDICT_ROUTE"] = e.PredictRoute
	env["HF_REVISION"] = p.Revision
	env["HF_TASK"] = p.Task
	switch e.Name {
	case EngineHFPyTorch:
		// The toolkit downloads HF_MODEL_ID itself, into HF_MODEL_DIR.
		env["HF_MODEL_ID"] = p.HFID
		env["HF_MODEL_DIR"] = "/data/model"
		if p.Task == "" {
			env["HF_TASK"] = "text-to-image"
		}
	case EngineONNX:
		env["ONNX_FILE"] = p.File
		env["MODEL_FILES"] = strings.Join(onnxPatterns(p.File), "\n")
	}
	return PodSpec{Command: []string{"sh", "/etc/cloudburrow/start.sh"}, Env: env, Files: runtimeConfig(e.Name)}
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
