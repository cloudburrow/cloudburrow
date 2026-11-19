package modelgarden

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
)

// fakeDocker runs an httptest server in place of each engine container.
type fakeDocker struct {
	mu      sync.Mutex
	engine  http.Handler
	servers map[string]*httptest.Server
	specs   map[string]RunSpec
	failRun bool
}

func (d *fakeDocker) Run(_ context.Context, s RunSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failRun {
		return errors.New("no such image")
	}
	if d.servers == nil {
		d.servers, d.specs = map[string]*httptest.Server{}, map[string]RunSpec{}
	}
	d.servers[s.Name] = httptest.NewServer(d.engine)
	d.specs[s.Name] = s
	return nil
}

func (d *fakeDocker) HostPort(_ context.Context, name string, _ int) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.servers[name]
	if !ok {
		return 0, errors.New("no container")
	}
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	return strconv.Atoi(p)
}

func (d *fakeDocker) Running(_ context.Context, name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.servers[name]
	return ok
}
func (d *fakeDocker) Start(context.Context, string) error { return errors.New("gone") }
func (d *fakeDocker) Stop(ctx context.Context, n string) error {
	return d.Remove(ctx, n)
}
func (d *fakeDocker) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.servers[name]; ok {
		s.Close()
		delete(d.servers, name)
	}
	return nil
}
func (d *fakeDocker) Logs(context.Context, string, int) string { return "engine log" }

// fakeVLLM answers vLLM's health, completions and chat routes.
func fakeVLLM(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "acme/tiny" {
			http.Error(w, `{"error":{"message":"model not found"}}`, http.StatusNotFound)
			return
		}
		if _, ok := req["raw_response"]; ok {
			t.Errorf("raw_response reached vLLM")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": req["model"], "choices": []any{map[string]any{"text": " Paris."}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"object\":\"chat.completion.chunk\",\"n\":1}\n\ndata: {\"object\":\"chat.completion.chunk\",\"n\":2}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "chat.completion", "model": req["model"],
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "hello"}}}})
	})
	return mux
}

const tinyConfig = `{"max_position_embeddings": 1024}`

// hfStub serves the Hugging Face API for one model.
func hfStub(t *testing.T, id, tag, gated string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/"+id:
			g := any(false)
			if gated != "" {
				g = gated
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "sha": strings.Repeat("ab", 20), "pipeline_tag": tag,
				"gated": g, "cardData": map[string]any{"license": "apache-2.0"},
				"siblings": []any{map[string]any{"rfilename": "config.json", "size": len(tinyConfig)}}})
		case strings.HasSuffix(r.URL.Path, "/config.json"):
			_, _ = io.WriteString(w, tinyConfig)
		default:
			http.NotFound(w, r)
		}
	}))
}

func newManager(t *testing.T, tag, gated string) (*Manager, *fakeDocker) {
	t.Helper()
	hf := hfStub(t, "acme/tiny", tag, gated)
	t.Cleanup(hf.Close)
	d := &fakeDocker{engine: fakeVLLM(t)}
	dir := t.TempDir()
	m := &Manager{Instance: "t", StateFile: filepath.Join(dir, "deployments.json"), Docker: d, ReadyTimeout: 5 * time.Second,
		Models: &hfmodels.Manager{Dir: filepath.Join(dir, "hf"), Base: hf.URL, MinFree: 1,
			FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, Token: func() string { return "" }}}
	return m, d
}

func codeOf(err error) codes.Code { return apierror.From(err).Code }

func TestDeployServesVLLMAndUndeploys(t *testing.T) {
	m, d := newManager(t, "text-generation", "")
	ctx := context.Background()
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "us-central1", HFModelID: "acme/tiny", MachineType: "n1-standard-4"})
	if err != nil {
		t.Fatal(err)
	}
	if p.PublisherModel != "publishers/hf-acme/models/tiny@001" {
		t.Errorf("publisher model = %q", p.PublisherModel)
	}
	if _, err := m.Target("p", "us-central1", p.Endpoint); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("Target while deploying = %v; want FAILED_PRECONDITION", err)
	}
	if err := m.Run(ctx, "p", "us-central1", p); err != nil {
		t.Fatal(err)
	}
	spec := d.specs[m.containerName(p.Endpoint)]
	if spec.Image != VLLMImage || spec.ShmSize == "" || !contains(spec.Args, "--max-model-len=1024") ||
		!contains(spec.Args, "--served-model-name=acme/tiny") || spec.Env["HF_TOKEN"] != "" || spec.Labels["cloudburrow.instance"] != "t" {
		t.Errorf("container spec = %+v; want vLLM capped at the model's 1024 context, no token", spec)
	}
	e, _ := m.Endpoint("p", "us-central1", p.Endpoint)
	if e.Deployed == nil || e.Deployed.State != StateReady || e.Deployed.MachineType != "n1-standard-4" {
		t.Fatalf("endpoint = %+v; want READY", e)
	}

	out, err := m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":[{"prompt":"Capital?","max_tokens":4,"raw_response":false},{"prompt":"x","raw_response":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var pr predictResponse
	_ = json.Unmarshal(out, &pr)
	var p0, p1 string
	_ = json.Unmarshal(pr.Predictions[0], &p0)
	_ = json.Unmarshal(pr.Predictions[1], &p1)
	if p0 != "Prompt:\nCapital?\nOutput:\n Paris." || p1 != " Paris." || pr.DeployedModelID != e.Deployed.ID {
		t.Errorf("predictions = %q, %q (%s); want Model Garden's Prompt:/Output: framing and the raw text", p0, p1, pr.DeployedModelID)
	}
	out, err = m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":[{"@requestFormat":"chatCompletions","messages":[{"role":"user","content":"hi"}]}]}`))
	if err != nil || !strings.Contains(string(out), `"chat.completion"`) {
		t.Errorf("chatCompletions predict = %s, %v", out, err)
	}
	if _, err := m.Predict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"instances":[{"max_tokens":1}]}`)); codeOf(err) != codes.InvalidArgument {
		t.Errorf("predict without prompt = %v; want INVALID_ARGUMENT", err)
	}

	st, _, body, err := m.RawPredict(ctx, "p", "us-central1", p.Endpoint, []byte(`{"prompt":"x"}`))
	if err != nil || st != 200 || !strings.Contains(string(body), `"model":"acme/tiny"`) {
		t.Errorf("rawPredict = %d %s %v; want the served model filled in", st, body, err)
	}
	var events []string
	err = m.Stream(ctx, "p", "us-central1", p.Endpoint, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), true,
		func(_ string, b []byte) error { events = append(events, string(b)); return nil })
	if err != nil || len(events) != 3 || events[2] != "data: [DONE]\n\n" {
		t.Errorf("stream = %q, %v; want three events", events, err)
	}

	if err := m.DeleteModel("p", "us-central1", p.Model); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("DeleteModel while deployed = %v; want FAILED_PRECONDITION", err)
	}
	if err := m.DeleteEndpoint("p", "us-central1", p.Endpoint); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("DeleteEndpoint while deployed = %v; want FAILED_PRECONDITION", err)
	}
	if err := m.Undeploy(ctx, "p", "us-central1", p.Endpoint, e.Deployed.ID); err != nil {
		t.Fatal(err)
	}
	if d.Running(ctx, m.containerName(p.Endpoint)) {
		t.Error("the container survived Undeploy")
	}
	if err := m.DeleteEndpoint("p", "us-central1", p.Endpoint); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteModel("p", "us-central1", p.Model); err != nil {
		t.Fatal(err)
	}
	// The downloaded files stay until asked for.
	if _, _, ok := m.Models.Downloaded("acme/tiny"); !ok {
		t.Error("deleting the model deleted its downloaded files")
	}
	// The state file survives a restart of the manager.
	m2 := &Manager{StateFile: m.StateFile}
	if n := len(m2.AllEndpoints()); n != 0 {
		t.Errorf("reloaded endpoints = %d; want 0", n)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestPrepareRefuses(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t, "text-generation", "manual")
	for _, c := range []struct {
		name string
		r    Request
		code codes.Code
		want string
	}{
		{"token", Request{HFModelID: "acme/tiny", TokenSet: true}, codes.InvalidArgument, "HF_TOKEN"},
		{"no model", Request{}, codes.InvalidArgument, "hugging_face_model_id"},
		{"bad id", Request{HFModelID: "nope"}, codes.InvalidArgument, "not a Hugging Face model ID"},
		{"bad user id", Request{HFModelID: "acme/tiny", EndpointUserID: "Bad_ID"}, codes.InvalidArgument, "endpoint_user_id"},
		{"gated no eula", Request{HFModelID: "acme/tiny"}, codes.FailedPrecondition, "accept_eula"},
	} {
		c.r.Project, c.r.Location = "p", "l"
		_, err := m.Prepare(ctx, c.r)
		if codeOf(err) != c.code || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %v naming %q", c.name, err, c.code, c.want)
		}
	}
	m, _ = newManager(t, "text-to-image", "")
	if _, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny"}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("text-to-image = %v; want INVALID_ARGUMENT", err)
	}
}

func TestRunFailureMarksFailedAndKeepsResources(t *testing.T) {
	m, d := newManager(t, "feature-extraction", "")
	d.failRun = true
	ctx := context.Background()
	p, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", EndpointUserID: "my-ep"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Endpoint != "my-ep" {
		t.Errorf("endpoint ID = %q; want the user's", p.Endpoint)
	}
	if err := m.Run(ctx, "p", "l", p); err == nil {
		t.Fatal("Run succeeded with no image")
	}
	e, err := m.Endpoint("p", "l", "my-ep")
	if err != nil || e.Deployed.State != StateFailed || !strings.Contains(e.Deployed.Message, "Text Embeddings Inference") {
		t.Errorf("endpoint = %+v, %v; want FAILED naming the engine", e.Deployed, err)
	}
	if _, err := m.Prepare(ctx, Request{Project: "p", Location: "l", HFModelID: "acme/tiny", EndpointUserID: "my-ep"}); codeOf(err) != codes.AlreadyExists {
		t.Errorf("second deploy to my-ep = %v; want ALREADY_EXISTS", err)
	}
	_ = os.Remove(m.StateFile)
}
