package modelgarden

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
)

// fakePipeline is the PyTorch Inference container's (and ONNX Runtime's)
// server: {"instances"} in, {"predictions"} out, one per instance, and a
// raw image body as one instance.
func fakePipeline(t *testing.T, seen *[]string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /predict", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ct := r.Header.Get("Content-Type")
		*seen = append(*seen, ct)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(ct, "image/") {
			_, _ = w.Write([]byte(`{"predictions":[[{"label":"tabby cat","score":0.9}]]}`))
			return
		}
		var req struct {
			Instances  []any          `json:"instances"`
			Parameters map[string]any `json:"parameters"`
		}
		if json.Unmarshal(b, &req) != nil || req.Instances == nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"The request body must contain a key 'instances' with a list of instances."}`))
			return
		}
		preds := []any{}
		for range req.Instances {
			preds = append(preds, []any{map[string]any{"label": "POSITIVE", "score": 0.99, "top_k": req.Parameters["top_k"]}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"predictions": preds})
	})
	return mux
}

// fakeOpenAI is llama.cpp's or mlx-lm's server, which must be asked for
// the model it serves.
func fakeOpenAI(t *testing.T, served string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	check := func(w http.ResponseWriter, r *http.Request) map[string]any {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != served {
			t.Errorf("%s asked for model %v; it serves %s", r.URL.Path, req["model"], served)
		}
		for _, k := range []string{"adapters", "draft_model"} {
			if _, ok := req[k]; ok && served == "default_model" {
				t.Errorf("%s reached mlx-lm", k)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		return req
	}
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) {
		check(w, r)
		_, _ = w.Write([]byte(`{"choices":[{"text":" Paris."}]}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		check(w, r)
		_, _ = w.Write([]byte(`{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		check(w, r)
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0.3,0.4]},{"index":0,"embedding":[0.1,0.2]}]}`))
	})
	return mux
}

// Each runtime family deploys on the cluster with its pinned image, its
// pod spec and, for the Python runtimes, its ConfigMap; Predict and
// RawPredict map onto its routes.
func TestDeployOnTheClusterServesEachRuntime(t *testing.T) {
	rev := strings.Repeat("cd", 20)
	ctx := context.Background()

	t.Run("pytorch inference", func(t *testing.T) {
		var seen []string
		m, c := newClusterManager(t, "text-classification", "transformers", "", fakePipeline(t, &seen))
		p := deploy(t, m)
		w := c.applied[workloadName(p.Endpoint)]
		if w.Image != PythonImage || strings.Join(w.Command, " ") != "sh /etc/cloudburrow/start.sh" ||
			w.Env["HF_MODEL_ID"] != "acme/tiny" || w.Env["HF_TASK"] != "text-classification" || w.Env["HF_REVISION"] != rev ||
			w.Env["AIP_MODE"] != "PREDICTION" || w.Env["AIP_PREDICT_ROUTE"] != "/predict" || w.Env["RUNTIME"] != EngineHFPyTorch {
			t.Errorf("workload = %+v", w)
		}
		if !strings.Contains(w.Files["requirements-nodeps.txt"], ToolkitSource) || !strings.Contains(w.Files["requirements.txt"], "torch==2.5.1") ||
			!strings.Contains(w.Files["cloudburrow_hf.py"], "webservice_starlette import app") || w.Files["start.sh"] == "" {
			t.Errorf("runtime files = %v", keys(w.Files))
		}
		out, err := m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":["I love it","meh"],"parameters":{"top_k":1}}`))
		if err != nil {
			t.Fatal(err)
		}
		var pr predictResponse
		if json.Unmarshal(out, &pr) != nil || len(pr.Predictions) != 2 || !strings.Contains(string(pr.Predictions[0]), `"top_k":1`) ||
			pr.DeployedModelID == "" {
			t.Errorf("predict = %s", out)
		}
		st, ct, body, err := m.RawPredict(ctx, "p", "us-central1", p.Endpoint, []byte{0x89, 'P', 'N', 'G'}, "image/png")
		if err != nil || st != 200 || ct != "application/json" || !strings.Contains(string(body), "tabby") || seen[len(seen)-1] != "image/png" {
			t.Errorf("rawPredict of an image = %d %s %s, %v (%v)", st, ct, body, err, seen)
		}
		st, _, body, err = m.RawPredict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"inputs":"x"}`), "application/json")
		if err != nil || st != 400 || !strings.Contains(string(body), "instances") {
			t.Errorf("rawPredict without instances = %d %s, %v", st, body, err)
		}
		if err := m.Stream(ctx, "p", "us-central1", p.Endpoint, []byte(`{"messages":[]}`), false, nil); codeOf(err).String() != "FailedPrecondition" {
			t.Errorf("chat completions = %v", err)
		}
	})

	t.Run("diffusers text-to-image", func(t *testing.T) {
		var seen []string
		m, c := newClusterManager(t, "text-to-image", "diffusers", "", fakePipeline(t, &seen))
		p := deploy(t, m)
		w := c.applied[workloadName(p.Endpoint)]
		if w.Env["HF_TASK"] != "text-to-image" || !strings.Contains(w.Files["requirements.txt"], "diffusers==0.33.1") {
			t.Errorf("workload = %+v", w.Env)
		}
	})

	t.Run("onnx", func(t *testing.T) {
		var seen []string
		m, c := newClusterManager(t, "text-classification", "transformers.js", "", fakePipeline(t, &seen))
		p := deploy(t, m)
		w := c.applied[workloadName(p.Endpoint)]
		model, _ := m.Model("p", "us-central1", p.Model)
		if model.Engine != EngineONNX || model.File != "onnx/model.onnx" || w.Env["ONNX_FILE"] != "onnx/model.onnx" ||
			!strings.Contains(w.Env["MODEL_FILES"], "onnx/model.onnx\n") || strings.Contains(w.Env["MODEL_FILES"], "quantized") ||
			!strings.Contains(w.Files["requirements.txt"], "onnxruntime==1.23.2") || w.Files["onnx_server.py"] == "" {
			t.Errorf("model = %+v, workload = %+v", model, w.Env)
		}
		out, err := m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":["great"]}`))
		if err != nil || !strings.Contains(string(out), "POSITIVE") {
			t.Errorf("predict = %s, %v", out, err)
		}
	})

	t.Run("gguf", func(t *testing.T) {
		m, c := newClusterManager(t, "text-generation", "gguf", "", fakeOpenAI(t, "acme/tiny"))
		p := deploy(t, m)
		w := c.applied[workloadName(p.Endpoint)]
		args := strings.Join(w.Args, " ")
		if w.Image != LlamaCppImage || len(w.Command) != 0 || len(w.Files) != 0 ||
			!strings.Contains(args, "--model-url https://huggingface.co/acme/tiny/resolve/"+rev+"/tiny-Q4_K_M.gguf") ||
			!strings.Contains(args, "--alias acme/tiny") || !strings.Contains(args, "--ctx-size 1024") || w.Env["LLAMA_CACHE"] == "" {
			t.Errorf("workload = %+v", w)
		}
		out, err := m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":[{"prompt":"The capital of France is"}]}`))
		if err != nil || !strings.Contains(string(out), `Prompt:\nThe capital of France is\nOutput:\n Paris.`) {
			t.Errorf("predict = %s, %v", out, err)
		}
		st, _, body, err := m.RawPredict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), "")
		if err != nil || st != 200 || !strings.Contains(string(body), "chat.completion") {
			t.Errorf("rawPredict = %d %s, %v", st, body, err)
		}
		var got []byte
		if err := m.Stream(ctx, "p", "us-central1", p.Endpoint, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), false,
			func(_ string, b []byte) error { got = append(got, b...); return nil }); err != nil || !strings.Contains(string(got), "hi") {
			t.Errorf("chat completions = %s, %v", got, err)
		}
	})

	t.Run("gguf embeddings", func(t *testing.T) {
		m, c := newClusterManager(t, "feature-extraction", "gguf", "", fakeOpenAI(t, "acme/tiny"))
		p := deploy(t, m)
		if w := c.applied[workloadName(p.Endpoint)]; !contains(w.Args, "--embeddings") {
			t.Errorf("args = %v", w.Args)
		}
		out, err := m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":[{"inputs":["a","b"]}]}`))
		if err != nil || !strings.Contains(string(out), `"predictions":[[[0.1,0.2],[0.3,0.4]]]`) {
			t.Errorf("predict = %s, %v", out, err)
		}
	})

	t.Run("mlx", func(t *testing.T) {
		m, c := newClusterManager(t, "text-generation", "mlx", "", fakeOpenAI(t, "default_model"))
		p := deploy(t, m)
		w := c.applied[workloadName(p.Endpoint)]
		if w.Image != PythonImage || w.Env["RUNTIME"] != EngineMLX || !strings.Contains(w.Files["requirements.txt"], "mlx-lm[cpu]==0.32.2") {
			t.Errorf("workload = %+v", w.Env)
		}
		out, err := m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":[{"prompt":"x","raw_response":true,"model":"evil/other"}]}`))
		if err != nil || !strings.Contains(string(out), `" Paris."`) {
			t.Errorf("predict = %s, %v", out, err)
		}
		st, _, body, err := m.RawPredict(ctx, "p", "us-central1", p.Endpoint,
			[]byte(`{"model":"evil/other","adapters":"evil/lora","draft_model":"evil/d","prompt":"x"}`), "")
		if err != nil || st != 200 || !strings.Contains(string(body), "Paris") {
			t.Errorf("rawPredict = %d %s, %v", st, body, err)
		}
	})
}

func deploy(t *testing.T, m *Manager) Prepared {
	t.Helper()
	ctx := context.Background()
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "us-central1", HFModelID: "acme/tiny"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Run(ctx, "p", "us-central1", p); err != nil {
		t.Fatal(err)
	}
	return p
}

func keys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestFileChoice(t *testing.T) {
	f := func(paths ...string) []hfmodels.File {
		var out []hfmodels.File
		for i, p := range paths {
			out = append(out, hfmodels.File{Path: p, Size: int64(100 - i)})
		}
		return out
	}
	for _, c := range []struct {
		files []hfmodels.File
		want  string
	}{
		{f("a-Q8_0.gguf", "a-Q4_K_M.gguf", "mmproj-a-Q4_K_M.gguf"), "a-Q4_K_M.gguf"},
		{f("a-F16.gguf", "a-IQ2.gguf"), "a-IQ2.gguf"},
		{f("big-Q4_K_M-00002-of-00002.gguf", "big-Q4_K_M-00001-of-00002.gguf"), "big-Q4_K_M-00001-of-00002.gguf"},
		{f("README.md"), ""},
	} {
		if got := ggufFile(c.files); got != c.want {
			t.Errorf("ggufFile(%v) = %q; want %q", c.files, got, c.want)
		}
	}
	if got := onnxFile(f("onnx/model_quantized.onnx", "onnx/model.onnx")); got != "onnx/model.onnx" {
		t.Errorf("onnxFile = %q", got)
	}
	if got := onnxFile(f("onnx/encoder_model.onnx", "onnx/decoder_model.onnx")); got != "onnx/encoder_model.onnx" {
		t.Errorf("onnxFile seq2seq = %q", got)
	}
	p := strings.Join(onnxPatterns("onnx/encoder_model.onnx"), " ")
	if !strings.Contains(p, "onnx/decoder_model.onnx") || !strings.Contains(p, "*.json") {
		t.Errorf("onnxPatterns = %s", p)
	}
}
