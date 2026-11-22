package modelgarden

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

const testToken = "hf_fakeTokenOnlyForTests0123456789"

// fakeCluster stands in for the instance's cluster: Apply starts an
// httptest server as the workload's pod, which becomes ready after a few
// observations, the way a pod pulls, downloads and loads.
type fakeCluster struct {
	mu       sync.Mutex
	engine   http.Handler
	servers  map[string]*httptest.Server
	applied  map[string]Workload
	tokens   map[string]string
	seen     map[string]int
	failWith string
}

func (c *fakeCluster) Arch(context.Context) (string, error) { return "arm64", nil }

func (c *fakeCluster) Apply(_ context.Context, w Workload, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.servers == nil {
		c.servers, c.applied, c.tokens, c.seen = map[string]*httptest.Server{}, map[string]Workload{}, map[string]string{}, map[string]int{}
	}
	c.applied[w.Name] = w
	if token != "" {
		c.tokens[w.Name] = token
	}
	if _, ok := c.servers[w.Name]; !ok {
		c.servers[w.Name] = httptest.NewServer(c.engine)
	}
	return nil
}

func (c *fakeCluster) Observe(_ context.Context, name string) (Observation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.servers[name]; !ok {
		return Observation{Phase: PhaseMissing}, nil
	}
	if c.failWith != "" {
		return Observation{Phase: PhaseFailed, Message: c.failWith}, nil
	}
	c.seen[name]++
	switch c.seen[name] {
	case 1:
		return Observation{Phase: PhasePulling, Pulled: 50e6, PullTotal: 100e6}, nil
	case 2:
		return Observation{Phase: PhaseLoading, Message: "Downloading the weights"}, nil
	}
	return Observation{Phase: PhaseReady}, nil
}

func (c *fakeCluster) Forward(_ context.Context, name string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.servers[name]
	if !ok {
		return 0, errors.New("no service")
	}
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	return strconv.Atoi(p)
}

func (c *fakeCluster) Unforward(string) {}
func (c *fakeCluster) UnforwardAll()    {}

func (c *fakeCluster) Remove(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.servers[name]; ok {
		s.Close()
		delete(c.servers, name)
	}
	delete(c.tokens, name)
	return nil
}

func (c *fakeCluster) Logs(context.Context, string, int) string { return "pod log" }

// hubCardStub serves a Hub model's card, its README and, for a gated
// model, a config.json that only testToken opens.
func hubCardStub(t *testing.T, id, task, library, gated string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/"+id:
			if r.Header.Get("Authorization") != "" {
				t.Errorf("the card was read with a token")
			}
			g := any(false)
			if gated != "" {
				g = gated
			}
			tags := []string{library}
			siblings := []any{map[string]any{"rfilename": "config.json", "size": len(tinyConfig)}}
			switch library {
			case "gguf":
				siblings = append(siblings, map[string]any{"rfilename": "tiny-Q8_0.gguf", "size": 2000},
					map[string]any{"rfilename": "tiny-Q4_K_M.gguf", "size": 1000})
			case "transformers.js", "onnx":
				tags = append(tags, "onnx")
				siblings = append(siblings, map[string]any{"rfilename": "onnx/model.onnx", "size": 2000},
					map[string]any{"rfilename": "onnx/model_quantized.onnx", "size": 1000},
					map[string]any{"rfilename": "tokenizer.json", "size": 10})
			default:
				tags = append(tags, "safetensors")
				siblings = append(siblings, map[string]any{"rfilename": "model.safetensors", "size": 1000})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "sha": strings.Repeat("cd", 20), "pipeline_tag": task,
				"library_name": library, "gated": g, "tags": tags,
				"cardData": map[string]any{"license": "apache-2.0"},
				"siblings": siblings})
		case strings.HasSuffix(r.URL.Path, "/config.json"):
			if gated != "" && r.Header.Get("Authorization") != "Bearer "+testToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(tinyConfig))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newClusterManager(t *testing.T, task, library, gated string, engine http.Handler) (*Manager, *fakeCluster) {
	t.Helper()
	hub := hubCardStub(t, "acme/tiny", task, library, gated)
	c := &fakeCluster{engine: engine}
	dir := t.TempDir()
	m := &Manager{Instance: "t", StateFile: filepath.Join(dir, "deployments.json"), Docker: &fakeDocker{engine: engine},
		Cluster: c, ReadyTimeout: 20 * time.Second,
		Models: &hfmodels.Manager{Dir: filepath.Join(dir, "hf"), Base: hub.URL, Token: func() string { return "" }}}
	return m, c
}

func fakeTEI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /embed", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[[0.1,0.2,0.3]]`))
	})
	return mux
}

// A sentence-transformers model deploys as a TEI workload on the cluster:
// its arm64 image, its pod pulling the model at the card's revision, the
// deploy's progress through the pull, and prediction through the tunnel.
func TestDeployOnTheClusterServesTEI(t *testing.T) {
	m, c := newClusterManager(t, "sentence-similarity", "sentence-transformers", "", fakeTEI())
	ctx := context.Background()
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "us-central1", HFModelID: "acme/tiny"})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := m.Endpoint("p", "us-central1", p.Endpoint)
	if !e.Deployed.OnCluster() || e.Deployed.Container != workloadName(p.Endpoint) {
		t.Fatalf("deployed = %+v; want a cluster workload", e.Deployed)
	}
	if err := m.Run(ctx, "p", "us-central1", p); err != nil {
		t.Fatal(err)
	}
	w := c.applied[workloadName(p.Endpoint)]
	if w.Image != TEIArm64Image || !contains(w.Args, "--model-id=acme/tiny") || !contains(w.Args, "--revision="+strings.Repeat("cd", 20)) ||
		w.Env["HF_TOKEN"] != "" || w.Labels["cloudburrow.dev/modelgarden-endpoint"] != p.Endpoint {
		t.Errorf("workload = %+v", w)
	}
	if _, ok := c.tokens[w.Name]; ok {
		t.Error("an ungated model's workload got a token Secret")
	}
	e, _ = m.Endpoint("p", "us-central1", p.Endpoint)
	if e.Deployed.State != StateReady || e.Deployed.HostPort == 0 {
		t.Fatalf("endpoint = %+v; want READY with a tunnel", e.Deployed)
	}
	model, _ := m.Model("p", "us-central1", p.Model)
	if model.Image != TEIArm64Image || model.Engine != EngineTEI {
		t.Errorf("model = %+v", model)
	}
	out, err := m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":[{"inputs":"hello"}]}`))
	if err != nil || !strings.Contains(string(out), "0.1") {
		t.Fatalf("predict = %s, %v", out, err)
	}
	if err := m.Undeploy(ctx, "p", "us-central1", p.Endpoint, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.servers[w.Name]; ok {
		t.Error("the workload survived Undeploy")
	}
}

// A gated model needs the end user's token, which the Hub must accept; it
// reaches the workload's Secret and is in no state, message or error.
func TestDeployAGatedModelPutsTheTokenOnlyInTheSecret(t *testing.T) {
	m, c := newClusterManager(t, "text-generation", "transformers", "manual", fakeVLLM(t))
	ctx := context.Background()
	if _, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", AcceptEULA: true}); codeOf(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "is a gated Hugging Face model") {
		t.Errorf("no token = %v; want INVALID_ARGUMENT asking for the token", err)
	}
	_, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", AcceptEULA: true, HFToken: "hf_notTheRightOne99"})
	if codeOf(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "The token is not valid or does not have permission") ||
		strings.Contains(err.Error(), "hf_notTheRightOne99") {
		t.Errorf("refused token = %v; want Google's words without the token", err)
	}
	if _, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", HFToken: testToken}); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("no accept_eula = %v; want FAILED_PRECONDITION", err)
	}
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", AcceptEULA: true, HFToken: testToken, TokenSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Run(ctx, "p", "l", p); err != nil {
		t.Fatal(err)
	}
	name := workloadName(p.Endpoint)
	if c.tokens[name] != testToken {
		t.Errorf("the Secret holds %q", c.tokens[name])
	}
	w := c.applied[name]
	if w.Image != VLLMImage || !contains(w.Args, "--max-model-len=1024") || !contains(w.Args, "--served-model-name=acme/tiny") {
		t.Errorf("workload = %+v; want vLLM capped at the gated config's 1024 context", w)
	}
	for _, a := range append(append([]string{}, w.Args...), w.Image) {
		if strings.Contains(a, testToken) {
			t.Errorf("the workload's spec carries the token: %q", a)
		}
	}
	b, _ := os.ReadFile(m.StateFile)
	if strings.Contains(string(b), testToken) || strings.Contains(string(b), "hf_") {
		t.Errorf("the state file holds the token")
	}
	e, _ := m.Endpoint("p", "l", p.Endpoint)
	if !e.Deployed.Gated || e.Deployed.State != StateReady {
		t.Errorf("deployed = %+v", e.Deployed)
	}
	if err := m.Undeploy(ctx, "p", "l", p.Endpoint, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.tokens[name]; ok {
		t.Error("the token Secret survived Undeploy")
	}
}

// A workload whose pod fails is FAILED with the pod's words, and removed.
func TestClusterFailureIsFailedAndRemoved(t *testing.T) {
	m, c := newClusterManager(t, "feature-extraction", "sentence-transformers", "", fakeTEI())
	c.failWith = "The serving container exited (Error, code 1): no such model"
	ctx := context.Background()
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Run(ctx, "p", "l", p); err == nil {
		t.Fatal("Run succeeded")
	}
	e, _ := m.Endpoint("p", "l", p.Endpoint)
	if e.Deployed.State != StateFailed || !strings.Contains(e.Deployed.Message, "no such model") {
		t.Errorf("deployed = %+v", e.Deployed)
	}
	if len(c.servers) != 0 {
		t.Error("the failed workload was left in the cluster")
	}
}

// Without a cluster, and for the console's Deploy of a downloaded model, a
// token is refused as before; a model the engines cannot load is refused
// on the cluster too.
func TestClusterPrepareRefuses(t *testing.T) {
	ctx := context.Background()
	m, _ := newClusterManager(t, "text-to-image", "gguf", "", fakeVLLM(t))
	if _, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny"}); codeOf(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "llama.cpp serves GGUF text-generation") {
		t.Errorf("a GGUF text-to-image model = %v", err)
	}
	if _, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", HFToken: testToken, Runtime: RuntimeDocker}); codeOf(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "HF_TOKEN") {
		t.Errorf("a token on Docker = %v", err)
	}
}

// fakeKube records kubectl calls.
type fakeKube struct {
	mu    sync.Mutex
	calls [][]string
	stdin []string
	out   map[string]string
}

func (f *fakeKube) Run(_ context.Context, stdin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	f.stdin = append(f.stdin, stdin)
	joined := strings.Join(args, " ")
	for k, v := range f.out {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return "", nil
}

// KubeCluster's manifests: owned, labelled, the token as an optional
// secretKeyRef, and the Secret applied server-side so no annotation copies
// it; the token is in that one call's stdin and nowhere else.
func TestKubeClusterApplyAndObserve(t *testing.T) {
	f := &fakeKube{out: map[string]string{}}
	c := &KubeCluster{Namespace: "ns", Instance: "inst", Kube: k8s.NewWith(f, "", "", "ns"),
		Meter: func(context.Context, string, string) (images.PullProgress, error) {
			return images.PullProgress{Fetched: 10, Total: 40}, nil
		}}
	w := Workload{Name: "mg-1", Image: TEIArm64Image, Args: []string{"--model-id=acme/tiny"}, Env: map[string]string{"HF_HOME": "/data"},
		ShmSize: "2g", HealthRoute: "/health"}
	if err := c.Apply(context.Background(), w, testToken); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("calls = %v", f.calls)
	}
	withToken := 0
	for i, in := range f.stdin {
		if strings.Contains(in, testToken) {
			withToken++
			if !strings.Contains(in, `"kind":"Secret"`) || !contains(f.calls[i], "--server-side") {
				t.Errorf("the token went to %v", f.calls[i])
			}
		}
	}
	if withToken != 1 {
		t.Errorf("the token is in %d calls; want the Secret's alone", withToken)
	}
	var d struct {
		Metadata struct{ Labels map[string]string }
		Spec     struct {
			Template struct {
				Spec struct {
					AutomountServiceAccountToken bool `json:"automountServiceAccountToken"`
					Containers                   []struct {
						Image string
						Env   []struct {
							Name      string
							ValueFrom *struct {
								SecretKeyRef struct {
									Name     string
									Optional bool
								}
							}
						}
						ReadinessProbe struct{ HTTPGet struct{ Path string } }
					}
					Volumes []struct {
						Name     string
						EmptyDir map[string]any
					}
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(f.stdin[1]), &d); err != nil {
		t.Fatal(err)
	}
	if d.Metadata.Labels[k8s.OwnedLabel] != "true" || d.Metadata.Labels[k8s.InstanceLabel] != "inst" || d.Metadata.Labels[WorkloadLabel] != "mg-1" {
		t.Errorf("labels = %v", d.Metadata.Labels)
	}
	ct := d.Spec.Template.Spec.Containers[0]
	ref := ct.Env[len(ct.Env)-1]
	if ref.Name != "HF_TOKEN" || ref.ValueFrom == nil || ref.ValueFrom.SecretKeyRef.Name != "mg-1-hf-token" || !ref.ValueFrom.SecretKeyRef.Optional {
		t.Errorf("HF_TOKEN env = %+v", ref)
	}
	if ct.ReadinessProbe.HTTPGet.Path != "/health" || d.Spec.Template.Spec.AutomountServiceAccountToken ||
		len(d.Spec.Template.Spec.Volumes) != 2 || d.Spec.Template.Spec.Volumes[1].EmptyDir["sizeLimit"] != "2Gi" {
		t.Errorf("pod spec = %+v", d.Spec.Template.Spec)
	}

	// The pod pulling its image reports the node's bytes.
	f.out["get pods"] = `{"items":[{"metadata":{"name":"mg-1-x"},"spec":{"nodeName":"node"},"status":{"phase":"Pending",
		"containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"ContainerCreating"}}}]}}]}`
	f.out["jsonpath={.spec.template"] = TEIArm64Image
	o, err := c.Observe(context.Background(), "mg-1")
	if err != nil || o.Phase != PhasePulling || o.Pulled != 10 || o.PullTotal != 40 {
		t.Errorf("observe while pulling = %+v, %v", o, err)
	}
	f.out["get pods"] = `{"items":[{"metadata":{"name":"mg-1-x"},"status":{"phase":"Running",
		"containerStatuses":[{"ready":true,"state":{"running":{}}}]}}]}`
	if o, _ := c.Observe(context.Background(), "mg-1"); o.Phase != PhaseReady {
		t.Errorf("observe ready = %+v", o)
	}
	f.out["get pods"] = `{"items":[{"metadata":{"name":"mg-1-x"},"status":{"phase":"Running",
		"containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"reason":"Error","exitCode":1}}}]}}]}`
	if o, _ := c.Observe(context.Background(), "mg-1"); o.Phase != PhaseFailed || !strings.Contains(o.Message, "code 1") {
		t.Errorf("observe crash = %+v", o)
	}

	f.calls = nil
	if err := c.Remove(context.Background(), "mg-1"); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, call := range f.calls {
		got = append(got, strings.Join(call[2:4], " "))
	}
	if strings.Join(got, ",") != "delete deployment,delete service,delete secret,delete configmap" {
		t.Errorf("remove = %v", f.calls)
	}
}

func TestWorkloadNameIsADNSLabel(t *testing.T) {
	for _, id := range []string{"1234567890123456789", "my-endpoint", strings.Repeat("a", 63)} {
		n := workloadName(id)
		if len(n) > 52 || !strings.HasPrefix(n, "mg-") || strings.ToLower(n) != n {
			t.Errorf("workloadName(%q) = %q", id, n)
		}
	}
	if workloadName(strings.Repeat("a", 63)) == workloadName(strings.Repeat("a", 62)+"b") {
		t.Error("two long IDs share a workload name")
	}
}

func TestEngineForModel(t *testing.T) {
	for _, c := range []struct {
		task, library string
		tags          []string
		want          string
	}{
		{"sentence-similarity", "sentence-transformers", nil, EngineTEI},
		{"", "sentence-transformers", nil, EngineTEI},
		{"feature-extraction", "transformers", nil, EngineTEI},
		{"text-generation", "transformers", []string{"safetensors"}, EngineVLLM},
		{"image-text-to-text", "transformers", nil, EngineVLLM},
		{"text-generation", "gguf", []string{"gguf"}, EngineLlamaCpp},
		{"text-generation", "llama.cpp", []string{"gguf"}, EngineLlamaCpp},
		{"feature-extraction", "gguf", []string{"gguf"}, EngineLlamaCpp},
		{"text-to-image", "gguf", []string{"gguf"}, ""},
		{"text-generation", "mlx", []string{"mlx", "safetensors"}, EngineMLX},
		{"image-classification", "mlx", []string{"mlx"}, ""},
		{"text-classification", "transformers.js", []string{"onnx"}, EngineONNX},
		{"text-classification", "onnx", []string{"onnx"}, EngineONNX},
		{"", "onnx", []string{"onnx"}, ""},
		{"text-to-image", "onnx", []string{"onnx"}, ""},
		{"text-classification", "transformers", []string{"onnx", "safetensors"}, EngineHFPyTorch},
		{"text-to-image", "diffusers", nil, EngineHFPyTorch},
		{"image-to-image", "diffusers", nil, ""},
		{"automatic-speech-recognition", "transformers", nil, EngineHFPyTorch},
		{"object-detection", "transformers", nil, EngineHFPyTorch},
		{"translation", "transformers", nil, EngineHFPyTorch},
		{"question-answering", "transformers", nil, EngineHFPyTorch},
		{"token-classification", "transformers", nil, EngineHFPyTorch},
		{"summarization", "transformers", nil, EngineHFPyTorch},
		{"robotics", "lerobot", nil, ""},
		{"text-generation", "openvino", []string{"openvino"}, ""},
	} {
		e, err := EngineForModel(c.task, c.library, c.tags)
		if (c.want == "") != (err != nil) || e.Name != c.want {
			t.Errorf("EngineForModel(%q, %q) = %q, %v; want %q", c.task, c.library, e.Name, err, c.want)
		}
	}
}
