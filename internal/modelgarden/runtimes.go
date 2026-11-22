package modelgarden

import (
	"embed"
	"path"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
)

// The runtimes beyond vLLM and TEI (#2148), each permissively licensed and
// pinned, for the Hugging Face models those two do not serve:
//
//   - Hugging Face PyTorch Inference: the server of Google's Hugging Face
//     PyTorch Inference container (huggingface-inference-toolkit, Apache-2.0),
//     which Google's Model Garden notebooks deploy for the transformers
//     pipeline tasks vLLM and TEI do not serve (classification, token
//     classification, question answering, summarisation, translation,
//     speech recognition, image classification, object detection...) and
//     for diffusers text-to-image. Google's container image is in Google's
//     Artifact Registry and amd64 only; here the same server, at the same
//     release, runs on the pinned multi-arch Python image with the toolkit's
//     own pinned packages, installed when the pod starts.
//   - llama.cpp's server (MIT) for GGUF repositories, its official
//     multi-arch image.
//   - mlx-lm's server (MIT) on MLX's Linux CPU backend for MLX repositories.
//   - ONNX Runtime (MIT), through optimum's ORTModel classes (Apache-2.0),
//     for ONNX-only repositories.
//
// Google's Model Garden documents none of GGUF, MLX or ONNX; their Predict
// and RawPredict follow the shapes of the documented runtimes they are
// closest to: llama.cpp's and mlx-lm's are OpenAI-compatible servers, as
// vLLM is, and ONNX Runtime serves pipeline tasks, as the PyTorch Inference
// container does.

// PythonImage is the official Python image (PSF-2.0), a multi-arch index,
// that the PyTorch Inference, MLX and ONNX runtimes run on
// (dependencies.json localai.modelGardenPython).
const PythonImage = "docker.io/library/python:3.11-slim-bookworm@sha256:a83ba18aef0f865b1a4a611c2cf460fe446f2795673bbb9fe43be570439e3b39"

// LlamaCppImage is llama.cpp's server, release b12211 (MIT), a linux/amd64
// and linux/arm64 index (dependencies.json localai.modelGardenLlamaCpp).
const LlamaCppImage = "ghcr.io/ggml-org/llama.cpp:server-b12211@sha256:30300ecfcb7ef05dc9413867dd7d03c9ce66eef00fec3576a6a15e9bf2b0bae7"

// ToolkitSource is huggingface-inference-toolkit 0.5.5's source archive
// (Apache-2.0; not published on PyPI), checked against its SHA-256.
const ToolkitSource = "https://github.com/huggingface/huggingface-inference-toolkit/archive/" +
	"e80584693c9762ec4a7c94570bab9ac2ba3e0961.tar.gz#sha256=82143e4db157abf81a49dd46c09a11a7ae64f101e2d6b993ec049a34eb76cb8d"

// torchIndex is PyTorch's CPU wheel index: PyPI's Linux x86_64 torch pulls
// the CUDA libraries, gigabytes no CPU node uses.
const torchIndex = "--extra-index-url https://download.pytorch.org/whl/cpu"

// requirements are each Python runtime's pinned packages, installed with
// their dependencies; requirementsNoDeps are installed without (the
// toolkit's own install_requires builds kenlm from git, which only its
// speech-with-language-model decoding uses).
var requirements = map[string][]string{
	EngineHFPyTorch: {torchIndex, "torch==2.5.1", "torchaudio==2.5.1", "transformers[sentencepiece,vision]==4.51.2",
		"huggingface_hub==0.30.2", "diffusers==0.33.1", "accelerate==1.6.0", "starlette==0.46.2", "uvicorn==0.34.2",
		"orjson==3.10.16", "numpy==1.26.4", "Pillow==11.2.1"},
	EngineMLX: {"mlx[cpu]==0.32.4", "mlx-lm[cpu]==0.32.2", "transformers==5.22.2"},
	EngineONNX: {torchIndex, "torch==2.5.1", "optimum-onnx[onnxruntime]==0.1.0", "optimum==2.1.0", "onnxruntime==1.23.2",
		"transformers[sentencepiece,vision]==4.57.1", "numpy==1.26.4", "Pillow==11.2.1"},
}

var requirementsNoDeps = map[string][]string{
	EngineHFPyTorch: {"huggingface-inference-toolkit @ " + ToolkitSource},
}

//go:embed runtime/*
var runtimeFiles embed.FS

// runtimeConfig is a Python runtime's ConfigMap: its start script, its
// pinned requirements and its server modules.
func runtimeConfig(engine string) map[string]string {
	files := map[string]string{
		"requirements.txt":        strings.Join(requirements[engine], "\n") + "\n",
		"requirements-nodeps.txt": strings.Join(requirementsNoDeps[engine], "\n") + "\n",
	}
	names := []string{"start.sh", "fetch.py"}
	switch engine {
	case EngineHFPyTorch:
		names = append(names, "cloudburrow_hf.py", "cloudburrow_hf_audio.py")
	case EngineONNX:
		names = append(names, "onnx_server.py", "cloudburrow_hf_audio.py")
	}
	for _, n := range names {
		b, err := runtimeFiles.ReadFile("runtime/" + n)
		if err != nil {
			panic(err)
		}
		files[n] = string(b)
	}
	return files
}

// pipelineTasks are the transformers pipeline tasks the PyTorch Inference
// container serves (huggingface-inference-toolkit's handler: any
// transformers pipeline, and diffusers' text-to-image).
var pipelineTasks = map[string]bool{
	"text-classification": true, "token-classification": true, "question-answering": true, "fill-mask": true,
	"summarization": true, "translation": true, "zero-shot-classification": true, "table-question-answering": true,
	"automatic-speech-recognition": true, "audio-classification": true, "image-classification": true,
	"object-detection": true, "image-segmentation": true, "zero-shot-image-classification": true,
	"zero-shot-object-detection": true, "image-to-text": true, "depth-estimation": true, "image-feature-extraction": true,
	"visual-question-answering": true, "document-question-answering": true, "text-to-audio": true, "text-to-speech": true,
	"mask-generation": true, "video-classification": true, "feature-extraction": true, "sentence-similarity": true,
	"text-generation": true, "text2text-generation": true,
}

// onnxTasks are the tasks the ONNX runtime has an ORTModel class for
// (runtime/onnx_server.py).
var onnxTasks = map[string]bool{
	"text-classification": true, "zero-shot-classification": true, "token-classification": true, "question-answering": true,
	"fill-mask": true, "feature-extraction": true, "sentence-similarity": true, "text-generation": true,
	"text2text-generation": true, "summarization": true, "translation": true, "image-classification": true,
	"automatic-speech-recognition": true, "audio-classification": true,
}

// ggufFile picks the GGUF file a llama.cpp deployment loads: Q4_K_M, which
// llama.cpp's -hf picks by default, then Q8_0, then the smallest file;
// projector (mmproj) files and later shards are never the model.
func ggufFile(files []hfmodels.File) string {
	var cands []hfmodels.File
	for _, f := range files {
		base := strings.ToLower(path.Base(f.Path))
		if !strings.HasSuffix(base, ".gguf") || strings.Contains(base, "mmproj") {
			continue
		}
		if i := strings.Index(base, "-of-"); i > 0 && !strings.HasSuffix(base[:i], "00001") {
			continue
		}
		cands = append(cands, f)
	}
	if len(cands) == 0 {
		return ""
	}
	for _, want := range []string{"q4_k_m", "q8_0"} {
		for _, f := range cands {
			if strings.Contains(strings.ToLower(path.Base(f.Path)), want) {
				return f.Path
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Size < cands[j].Size })
	return cands[0].Path
}

// onnxFile picks the ONNX graph an ONNX deployment loads: the full
// precision model.onnx (in onnx/, transformers.js' layout, or at the root),
// then a quantized one, then the first .onnx file; a sequence-to-sequence
// model's encoder names its folder.
func onnxFile(files []hfmodels.File) string {
	have := map[string]bool{}
	var first string
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".onnx") {
			have[f.Path] = true
			if first == "" {
				first = f.Path
			}
		}
	}
	for _, p := range []string{"onnx/model.onnx", "model.onnx", "onnx/model_quantized.onnx", "model_quantized.onnx",
		"onnx/encoder_model.onnx", "encoder_model.onnx"} {
		if have[p] {
			return p
		}
	}
	return first
}

// onnxPatterns are the files an ONNX deployment downloads: the chosen graph
// (and its external data, and a sequence-to-sequence model's other graphs
// of the same precision), and every configuration and tokenizer file, not
// the repository's other precisions.
func onnxPatterns(file string) []string {
	dir := path.Dir(file)
	prefix := ""
	if dir != "." {
		prefix = dir + "/"
	}
	out := []string{"*.json", "*.txt", "*.model", "*.tiktoken", file, file + "_data", file + ".data"}
	if strings.HasPrefix(path.Base(file), "encoder_model") {
		suffix := strings.TrimPrefix(path.Base(file), "encoder_model")
		out = append(out, prefix+"decoder_model"+suffix, prefix+"decoder_with_past_model"+suffix,
			prefix+"decoder_model_merged"+suffix)
	}
	return out
}
