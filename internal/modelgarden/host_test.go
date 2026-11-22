package modelgarden

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
	"github.com/cloudburrow/cloudburrow/internal/hostai"
)

// fakeHost stands in for the Mac: Start serves engine on the spec's
// health port, as llama-server or mlx-lm would.
type fakeHost struct {
	mu      sync.Mutex
	metal   bool
	engine  http.Handler
	started []hostai.Spec
	procs   []*fakeProc
	reaped  int
	failRun error
}

func (h *fakeHost) Metal() (bool, string) {
	if h.metal {
		return true, ""
	}
	return false, "Metal is Apple's GPU API, on macOS only; this machine runs linux"
}
func (h *fakeHost) Device() string { return "Apple M9" }
func (h *fakeHost) LlamaServer(_ context.Context, progress func(string)) (string, error) {
	progress("Downloading llama.cpp")
	return "/cache/llama.cpp/llama-server", h.failRun
}
func (h *fakeHost) MLXPython(_ context.Context, progress func(string)) (string, error) {
	progress("Installing mlx-lm")
	return "/cache/mlx/venv/bin/python", h.failRun
}
func (h *fakeHost) Reap() []string { h.reaped++; return nil }

func (h *fakeHost) Start(spec hostai.Spec) (HostProcess, error) {
	u, _ := url.Parse(spec.Health)
	l, err := net.Listen("tcp", u.Host)
	if err != nil {
		return nil, err
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: h.engine}}
	srv.Start()
	spec.Log("stderr", "load_tensors: offloaded 25/25 layers to GPU")
	p := &fakeProc{srv: srv}
	h.mu.Lock()
	h.started = append(h.started, spec)
	h.procs = append(h.procs, p)
	h.mu.Unlock()
	return p, nil
}

type fakeProc struct {
	mu      sync.Mutex
	srv     *httptest.Server
	stopped bool
}

func (p *fakeProc) Wait(context.Context, time.Duration) error { return nil }
func (p *fakeProc) Healthy() bool                             { return !p.isStopped() }
func (p *fakeProc) Running() bool                             { return !p.isStopped() }
func (p *fakeProc) Tail(int) string                           { return "tail" }
func (p *fakeProc) isStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}
func (p *fakeProc) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.stopped = true
		p.srv.Close()
	}
	return nil
}

// hostHub serves a GGUF or MLX repository's card and its files.
func hostHub(t *testing.T, library, task string) *httptest.Server {
	t.Helper()
	rev := strings.Repeat("ab", 20)
	files := map[string]string{"config.json": tinyConfig, "README.md": "# tiny"}
	tags := []string{library}
	switch library {
	case "gguf":
		files["tiny-Q4_K_M.gguf"] = "GGUF-q4"
		files["tiny-Q8_0.gguf"] = "GGUF-q8-larger"
	case "mlx":
		files["model.safetensors"] = "weights"
		files["tokenizer.json"] = "{}"
		tags = append(tags, "safetensors")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/acme/tiny":
			var sib []any
			for name, body := range files {
				sib = append(sib, map[string]any{"rfilename": name, "size": len(body)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "acme/tiny", "sha": rev, "pipeline_tag": task,
				"library_name": library, "gated": false, "tags": tags, "cardData": map[string]any{"license": "mit"}, "siblings": sib})
		case strings.HasPrefix(r.URL.Path, "/acme/tiny/resolve/"+rev+"/"):
			body, ok := files[strings.TrimPrefix(r.URL.Path, "/acme/tiny/resolve/"+rev+"/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newHostManager(t *testing.T, library, task string, metal bool) (*Manager, *fakeHost, *fakeCluster, *[]LogLine) {
	t.Helper()
	hub := hostHub(t, library, task)
	h := &fakeHost{metal: metal, engine: fakeVLLM(t)}
	c := &fakeCluster{engine: fakeVLLM(t)}
	dir := t.TempDir()
	var mu sync.Mutex
	logs := &[]LogLine{}
	m := &Manager{Instance: "t", StateFile: filepath.Join(dir, "deployments.json"), Docker: &fakeDocker{engine: fakeVLLM(t)},
		Cluster: c, Host: h, ReadyTimeout: 20 * time.Second,
		Log: func(l LogLine) { mu.Lock(); *logs = append(*logs, l); mu.Unlock() },
		Models: &hfmodels.Manager{Dir: filepath.Join(dir, "hf"), Base: hub.URL, Token: func() string { return "" },
			FreeBytes: func(string) (int64, error) { return 1 << 50, nil }}}
	return m, h, c, logs
}

// A GGUF model deployed on NVIDIA_L4 on a Mac runs llama-server on the
// Apple GPU as a host process: its one Q4_K_M file downloaded, every layer
// offloaded, published to the cluster, its log lines kept, predict served,
// and everything gone on Undeploy.
func TestDeployOnTheAppleGPUServesLlamaCpp(t *testing.T) {
	m, h, c, logs := newHostManager(t, "gguf", "text-generation", true)
	ctx := context.Background()
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "us-central1", HFModelID: "acme/tiny",
		AcceleratorType: "NVIDIA_L4", AcceleratorCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := m.Endpoint("p", "us-central1", p.Endpoint)
	d := e.Deployed
	if !d.OnHost() || d.MachineType != MetalMachineType || d.AcceleratorType != "NVIDIA_L4" || d.AcceleratorCount != 1 ||
		d.Device != "Apple M9 GPU (Metal)" {
		t.Fatalf("deployed = %+v; want a host deployment on the Apple GPU", d)
	}
	if err := m.Run(ctx, "p", "us-central1", p); err != nil {
		t.Fatal(err)
	}
	if len(h.started) != 1 {
		t.Fatalf("started %d servers", len(h.started))
	}
	s := h.started[0]
	args := strings.Join(s.Args, " ")
	if s.Path != "/cache/llama.cpp/llama-server" || !strings.Contains(args, "--host 127.0.0.1 ") || !strings.Contains(args, "--n-gpu-layers 999") ||
		!strings.Contains(args, "--alias acme/tiny") || !strings.Contains(args, "--ctx-size 1024") || !strings.HasSuffix(filepath.Base(argAfter(s.Args, "--model")), "tiny-Q4_K_M.gguf") {
		t.Errorf("llama-server %s %s", s.Path, args)
	}
	dir := m.hostModelDir(p.Endpoint)
	if b, err := os.ReadFile(filepath.Join(dir, "tiny-Q4_K_M.gguf")); err != nil || string(b) != "GGUF-q4" {
		t.Errorf("the GGUF file: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tiny-Q8_0.gguf")); err == nil {
		t.Error("a second quantisation was downloaded")
	}
	e, _ = m.Endpoint("p", "us-central1", p.Endpoint)
	if e.Deployed.State != StateReady || e.Deployed.HostPort == 0 || c.published[workloadName(p.Endpoint)] != e.Deployed.HostPort {
		t.Fatalf("deployed = %+v, published %v", e.Deployed, c.published)
	}
	if len(*logs) == 0 || (*logs)[0].Endpoint != p.Endpoint || !strings.Contains((*logs)[0].Text, "offloaded 25/25 layers to GPU") {
		t.Errorf("logs = %+v", *logs)
	}
	out, err := m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":[{"prompt":"Capital of France?"}]}`))
	if err != nil || !strings.Contains(string(out), "Paris") {
		t.Fatalf("predict = %s, %v", out, err)
	}
	if err := m.Undeploy(ctx, "p", "us-central1", p.Endpoint, ""); err != nil {
		t.Fatal(err)
	}
	if h.procs[0].Running() {
		t.Error("the host server survived Undeploy")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the model's files survived Undeploy")
	}
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// An MLX model runs mlx-lm's server from the venv on its weights, config
// and tokenizer (not the README), offline.
func TestDeployOnTheAppleGPUServesMLX(t *testing.T) {
	m, h, _, _ := newHostManager(t, "mlx", "text-generation", true)
	ctx := context.Background()
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Run(ctx, "p", "l", p); err != nil {
		t.Fatal(err)
	}
	s := h.started[0]
	dir := m.hostModelDir(p.Endpoint)
	if s.Path != "/cache/mlx/venv/bin/python" || strings.Join(s.Args[:4], " ") != "-m mlx_lm server --model" || s.Args[4] != dir ||
		!contains(s.Env, "HF_HUB_OFFLINE=1") {
		t.Errorf("mlx-lm %s %v %v", s.Path, s.Args, s.Env)
	}
	for _, f := range []string{"model.safetensors", "tokenizer.json", "config.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s was not downloaded", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err == nil {
		t.Error("the README was downloaded")
	}
	// No machine_spec: the instance default, the Apple GPU here.
	e, _ := m.Endpoint("p", "l", p.Endpoint)
	if e.Deployed.AcceleratorType != MetalAcceleratorType || !e.Deployed.OnHost() {
		t.Errorf("default accelerator: %+v", e.Deployed)
	}
	// Close ends the server; Restore starts it again from its files.
	m.Close()
	if h.procs[0].Running() {
		t.Fatal("Close left the host server running")
	}
	m2 := &Manager{Instance: "t", StateFile: m.StateFile, Docker: m.Docker, Cluster: m.Cluster, Host: h, Models: m.Models, ReadyTimeout: m.ReadyTimeout}
	m2.Restore()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e, _ := m2.Endpoint("p", "l", p.Endpoint)
		if e.Deployed.State == StateReady && len(h.started) == 2 && m2.hostHealthy(workloadName(p.Endpoint)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not restored: %+v", e.Deployed)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h.reaped == 0 {
		t.Error("Restore did not reap servers left behind")
	}
	m2.Close()
}

// The instance default and the explicit choices: cpu keeps the cluster,
// a GPU on a runtime with no Metal build or on a machine with no Apple GPU
// runs on CPU (and says so), and a TPU is refused.
func TestAcceleratorChoice(t *testing.T) {
	ctx := context.Background()
	m, h, _, _ := newHostManager(t, "gguf", "text-generation", true)
	m.Accelerator = AcceleratorCPU
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny"})
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := m.Endpoint("p", "l", p.Endpoint); !e.Deployed.OnCluster() || e.Deployed.AcceleratorType != "" || e.Deployed.MachineType != CPUMachineType {
		t.Errorf("cpu default: %+v", e.Deployed)
	}
	// An explicit machine type without an accelerator is CPU too.
	m.Accelerator = ""
	p, _ = m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", MachineType: "g2-standard-8"})
	if e, _ := m.Endpoint("p", "l", p.Endpoint); !e.Deployed.OnCluster() {
		t.Errorf("machine type without accelerator: %+v", e.Deployed)
	}
	if _, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", AcceleratorType: "TPU_V5_LITEPOD", AcceleratorCount: 1}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("TPU = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", AcceleratorCount: 2}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("count without type = %v; want INVALID_ARGUMENT", err)
	}
	// Another NVIDIA type is the Apple GPU too.
	p, _ = m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", AcceleratorType: "NVIDIA_TESLA_T4", AcceleratorCount: 2})
	if e, _ := m.Endpoint("p", "l", p.Endpoint); !e.Deployed.OnHost() || e.Deployed.AcceleratorCount != 2 {
		t.Errorf("T4: %+v", e.Deployed)
	}
	if m.DefaultAccelerator() != MetalAcceleratorType {
		t.Errorf("DefaultAccelerator = %q", m.DefaultAccelerator())
	}

	// No Apple GPU: the GPU type is recorded, served on CPU in the cluster.
	h.metal = false
	p, _ = m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", AcceleratorType: "NVIDIA_L4", AcceleratorCount: 1})
	if e, _ := m.Endpoint("p", "l", p.Endpoint); !e.Deployed.OnCluster() || e.Deployed.AcceleratorType != "NVIDIA_L4" ||
		!strings.Contains(e.Deployed.AcceleratorNote, "on macOS only") {
		t.Errorf("no Metal: %+v", e.Deployed)
	}
	if m.DefaultAccelerator() != "" {
		t.Errorf("DefaultAccelerator without Metal = %q", m.DefaultAccelerator())
	}

	// A runtime with no Metal build stays in the cluster.
	m2, _, _, _ := newHostManager(t, "sentence-transformers", "sentence-similarity", true)
	p, err = m2.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", AcceleratorType: "NVIDIA_L4", AcceleratorCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := m2.Endpoint("p", "l", p.Endpoint); !e.Deployed.OnCluster() || !strings.Contains(e.Deployed.AcceleratorNote, "no Metal build") {
		t.Errorf("TEI on L4: %+v", e.Deployed)
	}
}

// A runtime that cannot be installed fails the deploy, which is FAILED
// with the reason and leaves nothing running.
func TestHostInstallFailure(t *testing.T) {
	m, h, _, _ := newHostManager(t, "gguf", "text-generation", true)
	h.failRun = errors.New("the llama.cpp archive's SHA-256 is 00, not the pinned 4d")
	ctx := context.Background()
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", AcceleratorType: "NVIDIA_L4"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Run(ctx, "p", "l", p); codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("Run = %v", err)
	}
	e, _ := m.Endpoint("p", "l", p.Endpoint)
	if e.Deployed.State != StateFailed || !strings.Contains(e.Deployed.Message, "not the pinned") || len(h.started) != 0 {
		t.Errorf("deployed = %+v", e.Deployed)
	}
}

func TestHostServiceManifests(t *testing.T) {
	c := &KubeCluster{Namespace: "cb", Instance: "demo"}
	svc, slice := c.HostServiceManifests("mg-1", "192.168.65.254", 41234)
	var s, e map[string]any
	_ = json.Unmarshal(svc, &s)
	_ = json.Unmarshal(slice, &e)
	spec := s["spec"].(map[string]any)
	if _, ok := spec["selector"]; ok {
		t.Error("the host Service has a selector")
	}
	port := spec["ports"].([]any)[0].(map[string]any)
	if port["port"] != float64(containerPort) || port["targetPort"] != float64(41234) {
		t.Errorf("service ports = %v", port)
	}
	labels := e["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["kubernetes.io/service-name"] != "mg-1" || labels[RuntimeLabel] != RuntimeHost {
		t.Errorf("slice labels = %v", labels)
	}
	ep := e["endpoints"].([]any)[0].(map[string]any)
	if ep["addresses"].([]any)[0] != "192.168.65.254" || e["ports"].([]any)[0].(map[string]any)["port"] != float64(41234) {
		t.Errorf("slice = %s", slice)
	}
}
