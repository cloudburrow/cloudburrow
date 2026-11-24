package vertexai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// fakeHost is a NotebookHost whose runtimes connect at once.
type fakeHost struct {
	mu      sync.Mutex
	state   map[string]string
	opts    map[string]NotebookRuntimeOptions
	ran     []string
	failRun bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{state: map[string]string{}, opts: map[string]NotebookRuntimeOptions{}}
}

func (h *fakeHost) Start(key, _ string, o NotebookRuntimeOptions) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state[key], h.opts[key] = "CONNECTED", o
}

func (h *fakeHost) State(key string) (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.state[key]; ok {
		return s, ""
	}
	return "DISCONNECTED", ""
}

func (h *fakeHost) Exec(_ context.Context, _ string, code string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = append(h.ran, code)
	if h.failRun {
		return errors.New("exit 3")
	}
	return nil
}

func (h *fakeHost) Stop(key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, key)
	return nil
}

func (h *fakeHost) Version() string { return "test" }

func waitNotebookOp(t *testing.T, s *Server, op *longrunningpb.Operation) *longrunningpb.Operation {
	t.Helper()
	o := &operations{s: s}
	for i := 0; i < 100 && !op.GetDone(); i++ {
		time.Sleep(20 * time.Millisecond)
		var err error
		if op, err = o.GetOperation(context.Background(), &longrunningpb.GetOperationRequest{Name: op.GetName()}); err != nil {
			t.Fatal(err)
		}
	}
	if !op.GetDone() {
		t.Fatalf("operation %s not done", op.GetName())
	}
	return op
}

func TestNotebookRuntimeTemplatesAndRuntimes(t *testing.T) {
	ctx := context.Background()
	s := NewServer(nil)
	host := newFakeHost()
	file := filepath.Join(t.TempDir(), "nb.json")
	s.SetNotebookRuntimes(host, file)
	n := &notebookService{s: s}
	parent := "projects/p/locations/us-central1"

	// Validation.
	for _, bad := range []*aiplatformpb.NotebookRuntimeTemplate{
		{},
		{DisplayName: strings.Repeat("x", 129)},
		{DisplayName: "a", IdleShutdownConfig: &aiplatformpb.NotebookIdleShutdownConfig{IdleTimeout: durationpb.New(5 * time.Minute)}},
		{DisplayName: "a", DataPersistentDiskSpec: &aiplatformpb.PersistentDiskSpec{DiskSizeGb: 5}},
		{DisplayName: "a", DataPersistentDiskSpec: &aiplatformpb.PersistentDiskSpec{DiskType: "floppy"}},
		{DisplayName: "a", Labels: map[string]string{"Bad": "x"}},
		{DisplayName: "a", NetworkSpec: &aiplatformpb.NetworkSpec{Subnetwork: "default"}},
	} {
		if _, err := n.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplate: bad}); code(err) != codes.InvalidArgument {
			t.Errorf("create %v = %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	op, err := n.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: "tpl-a",
		NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "A", Labels: map[string]string{"env": "dev"},
			SoftwareConfig: &aiplatformpb.NotebookSoftwareConfig{Env: []*aiplatformpb.EnvVar{{Name: "FOO", Value: "bar"}},
				PostStartupScriptConfig: &aiplatformpb.PostStartupScriptConfig{PostStartupScript: "echo hi", PostStartupScriptBehavior: aiplatformpb.PostStartupScriptConfig_RUN_ONCE}}}})
	if err != nil || !op.GetDone() {
		t.Fatalf("create = %v, %v", op, err)
	}
	tpl, err := n.GetNotebookRuntimeTemplate(ctx, &aiplatformpb.GetNotebookRuntimeTemplateRequest{Name: parent + "/notebookRuntimeTemplates/tpl-a"})
	if err != nil {
		t.Fatal(err)
	}
	if tpl.GetMachineSpec().GetMachineType() != "e2-standard-4" || tpl.GetDataPersistentDiskSpec().GetDiskType() != "pd-standard" || tpl.GetDataPersistentDiskSpec().GetDiskSizeGb() != 100 ||
		tpl.GetIdleShutdownConfig().GetIdleTimeout().AsDuration() != 3*time.Hour || tpl.GetNotebookRuntimeType() != aiplatformpb.NotebookRuntimeType_USER_DEFINED || tpl.GetEtag() == "" {
		t.Errorf("defaults = %v", tpl)
	}
	if _, err := n.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: "tpl-a",
		NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "A"}}); code(err) != codes.AlreadyExists {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := n.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: "tpl-gpu",
		NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "GPU", MachineSpec: &aiplatformpb.MachineSpec{MachineType: "g2-standard-4",
			AcceleratorType: aiplatformpb.AcceleratorType_NVIDIA_L4, AcceleratorCount: 1}}}); err != nil {
		t.Fatal(err)
	}

	// List, filter and order.
	l, err := n.ListNotebookRuntimeTemplates(ctx, &aiplatformpb.ListNotebookRuntimeTemplatesRequest{Parent: parent, Filter: `machineType="g2-standard-4"`})
	if err != nil || len(l.GetNotebookRuntimeTemplates()) != 1 || l.GetNotebookRuntimeTemplates()[0].GetDisplayName() != "GPU" {
		t.Errorf("filtered list = %v, %v", l, err)
	}
	l, err = n.ListNotebookRuntimeTemplates(ctx, &aiplatformpb.ListNotebookRuntimeTemplatesRequest{Parent: parent, OrderBy: "display_name desc"})
	if err != nil || len(l.GetNotebookRuntimeTemplates()) != 2 || l.GetNotebookRuntimeTemplates()[0].GetDisplayName() != "GPU" {
		t.Errorf("ordered list = %v, %v", l, err)
	}
	if _, err := n.ListNotebookRuntimeTemplates(ctx, &aiplatformpb.ListNotebookRuntimeTemplatesRequest{Parent: parent, Filter: `description="x"`}); code(err) != codes.InvalidArgument {
		t.Errorf("undocumented filter field = %v", err)
	}

	// Update: only the documented mask paths.
	if _, err := n.UpdateNotebookRuntimeTemplate(ctx, &aiplatformpb.UpdateNotebookRuntimeTemplateRequest{NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{Name: tpl.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"machine_spec"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("update machine_spec = %v", err)
	}
	up, err := n.UpdateNotebookRuntimeTemplate(ctx, &aiplatformpb.UpdateNotebookRuntimeTemplateRequest{NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{Name: tpl.GetName(), DisplayName: "A2"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"displayName"}}})
	if err != nil || up.GetDisplayName() != "A2" || up.GetLabels()["env"] != "dev" {
		t.Errorf("update = %v, %v", up, err)
	}
	if _, err := n.UpdateNotebookRuntimeTemplate(ctx, &aiplatformpb.UpdateNotebookRuntimeTemplateRequest{NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{Name: tpl.GetName(), DisplayName: "A3", Etag: "stale"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); code(err) != codes.Aborted {
		t.Errorf("stale etag = %v", err)
	}

	// Assign, start, stop.
	if _, err := n.AssignNotebookRuntime(ctx, &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: parent + "/notebookRuntimeTemplates/tpl-gpu",
		NotebookRuntime: &aiplatformpb.NotebookRuntime{DisplayName: "g", RuntimeUser: "u@example.com"}}); code(err) != codes.FailedPrecondition {
		t.Errorf("assign with accelerators = %v", err)
	}
	if _, err := n.AssignNotebookRuntime(ctx, &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: tpl.GetName(),
		NotebookRuntime: &aiplatformpb.NotebookRuntime{DisplayName: "r"}}); code(err) != codes.InvalidArgument {
		t.Errorf("assign without runtime_user = %v", err)
	}
	op, err = n.AssignNotebookRuntime(ctx, &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: tpl.GetName(), NotebookRuntimeId: "rt-1",
		NotebookRuntime: &aiplatformpb.NotebookRuntime{DisplayName: "r", RuntimeUser: "u@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if op = waitNotebookOp(t, s, op); op.GetError() != nil {
		t.Fatalf("assign failed: %v", op.GetError())
	}
	name := parent + "/notebookRuntimes/rt-1"
	rt, err := n.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: name})
	if err != nil || rt.GetRuntimeState() != aiplatformpb.NotebookRuntime_RUNNING || rt.GetHealthState() != aiplatformpb.NotebookRuntime_HEALTHY ||
		rt.GetNotebookRuntimeTemplateRef().GetNotebookRuntimeTemplate() != tpl.GetName() || rt.GetMachineSpec().GetMachineType() != "e2-standard-4" {
		t.Errorf("runtime = %v, %v", rt, err)
	}
	if o := host.opts[runtimeKey(name)]; o.IdleTimeout != 3*time.Hour || o.Env["FOO"] != "bar" {
		t.Errorf("runtime options = %+v", o)
	}
	if len(host.ran) != 1 || !strings.Contains(host.ran[0], "echo hi") {
		t.Errorf("post-startup script runs = %q", host.ran)
	}
	// The same ID again is the existing assignment.
	op, err = n.AssignNotebookRuntime(ctx, &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: tpl.GetName(), NotebookRuntimeId: "rt-1",
		NotebookRuntime: &aiplatformpb.NotebookRuntime{DisplayName: "r", RuntimeUser: "u@example.com"}})
	if err != nil || !op.GetDone() || op.GetError() != nil {
		t.Errorf("existing assignment = %v, %v", op, err)
	}
	if _, err := n.StopNotebookRuntime(ctx, &aiplatformpb.StopNotebookRuntimeRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if rt, _ := n.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: name}); rt.GetRuntimeState() != aiplatformpb.NotebookRuntime_STOPPED {
		t.Errorf("after stop = %v", rt.GetRuntimeState())
	}
	op, err = n.StartNotebookRuntime(ctx, &aiplatformpb.StartNotebookRuntimeRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if op = waitNotebookOp(t, s, op); op.GetError() != nil {
		t.Fatalf("start failed: %v", op.GetError())
	}
	if len(host.ran) != 1 {
		t.Errorf("a RUN_ONCE script ran again: %q", host.ran)
	}
	l2, err := n.ListNotebookRuntimes(ctx, &aiplatformpb.ListNotebookRuntimesRequest{Parent: parent, Filter: `runtimeState=RUNNING AND runtimeUser="u@example.com"`})
	if err != nil || len(l2.GetNotebookRuntimes()) != 1 {
		t.Errorf("runtimes = %v, %v", l2, err)
	}
	// Idle shutdown (the host's runtime gone) is STOPPED.
	_ = host.Stop(runtimeKey(name))
	if rt, _ := n.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: name}); rt.GetRuntimeState() != aiplatformpb.NotebookRuntime_STOPPED {
		t.Errorf("after idle shutdown = %v", rt.GetRuntimeState())
	}

	// setDefault over REST.
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/"+parent+"/notebookRuntimeTemplates:setDefault", "application/json", strings.NewReader(`{"name":"`+tpl.GetName()+`"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("setDefault = %v, %v", resp, err)
	}
	resp.Body.Close()
	if s.DefaultNotebookRuntimeTemplate(parent) != tpl.GetName() {
		t.Errorf("default = %q", s.DefaultNotebookRuntimeTemplate(parent))
	}
	resp, err = http.Get(srv.URL + "/v1/" + parent + "/notebookRuntimeTemplates")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("REST list = %v, %v", resp, err)
	}
	resp.Body.Close()
	for _, c := range []struct{ method, path, body, want string }{
		{"POST", "/v1/" + parent + "/notebookRuntimeTemplates?notebookRuntimeTemplateId=json", `{"displayName":"J"}`, "/notebookRuntimeTemplates/json/operations/"},
		{"GET", "/v1/" + parent + "/notebookRuntimeTemplates/json", "", `"pd-standard"`},
		{"PATCH", "/v1/" + parent + "/notebookRuntimeTemplates/json?updateMask=displayName", `{"displayName":"K"}`, `"K"`},
		{"GET", "/v1/" + parent + "/notebookRuntimes", "", `"notebookRuntimes"`},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), c.want) {
			t.Errorf("%s %s = %d %s", c.method, c.path, resp.StatusCode, b)
		}
		if c.method == "POST" {
			var op struct{ Name string }
			_ = json.Unmarshal(b, &op)
			resp, err := http.Get(srv.URL + "/v1/" + op.Name)
			if err != nil || resp.StatusCode != 200 {
				t.Errorf("REST operation %s = %v, %v", op.Name, resp, err)
			} else {
				resp.Body.Close()
			}
		}
	}

	// The state survives a restart, the runtime STOPPED.
	s2 := NewServer(nil)
	s2.SetNotebookRuntimes(newFakeHost(), file)
	n2 := &notebookService{s: s2}
	if rt, err := n2.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: name}); err != nil || rt.GetRuntimeState() != aiplatformpb.NotebookRuntime_STOPPED {
		t.Errorf("reloaded runtime = %v, %v", rt, err)
	}
	if s2.DefaultNotebookRuntimeTemplate(parent) != tpl.GetName() {
		t.Errorf("reloaded default lost")
	}

	if _, err := n.DeleteNotebookRuntime(ctx, &aiplatformpb.DeleteNotebookRuntimeRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: name}); code(err) != codes.NotFound {
		t.Errorf("deleted runtime = %v", err)
	}
	if _, err := n.DeleteNotebookRuntimeTemplate(ctx, &aiplatformpb.DeleteNotebookRuntimeTemplateRequest{Name: tpl.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NotebookRuntimeTemplateOptions(tpl.GetName()); err == nil {
		t.Error("a deleted template still resolves")
	}
}

func TestOneClickRuntimeIsReassigned(t *testing.T) {
	ctx := context.Background()
	s := NewServer(nil)
	s.SetNotebookRuntimes(newFakeHost(), "")
	n := &notebookService{s: s}
	parent := "projects/p/locations/us-central1"
	if _, err := n.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: "one",
		NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "Default", NotebookRuntimeType: aiplatformpb.NotebookRuntimeType_ONE_CLICK}}); err != nil {
		t.Fatal(err)
	}
	req := &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: parent + "/notebookRuntimeTemplates/one",
		NotebookRuntime: &aiplatformpb.NotebookRuntime{DisplayName: "r", RuntimeUser: "u@example.com"}}
	op, err := n.AssignNotebookRuntime(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	waitNotebookOp(t, s, op)
	if _, err := n.AssignNotebookRuntime(ctx, req); err != nil {
		t.Fatal(err)
	}
	l, _ := n.ListNotebookRuntimes(ctx, &aiplatformpb.ListNotebookRuntimesRequest{Parent: parent})
	if len(l.GetNotebookRuntimes()) != 1 || l.GetNotebookRuntimes()[0].GetExpirationTime().AsTime().Sub(l.GetNotebookRuntimes()[0].GetCreateTime().AsTime()) != 24*time.Hour {
		t.Errorf("one-click runtimes = %v", l)
	}
}

func TestPostStartupScriptFailureFailsTheStart(t *testing.T) {
	ctx := context.Background()
	s := NewServer(nil)
	host := newFakeHost()
	host.failRun = true
	s.SetNotebookRuntimes(host, "")
	n := &notebookService{s: s}
	parent := "projects/p/locations/us-central1"
	if _, err := n.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: "s",
		NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "S", SoftwareConfig: &aiplatformpb.NotebookSoftwareConfig{
			PostStartupScriptConfig: &aiplatformpb.PostStartupScriptConfig{PostStartupScript: "exit 3"}}}}); err != nil {
		t.Fatal(err)
	}
	op, err := n.AssignNotebookRuntime(ctx, &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: parent + "/notebookRuntimeTemplates/s",
		NotebookRuntimeId: "x", NotebookRuntime: &aiplatformpb.NotebookRuntime{DisplayName: "x", RuntimeUser: "u@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if op = waitNotebookOp(t, s, op); op.GetError() == nil {
		t.Fatal("assign succeeded with a failing post-startup script")
	}
	if rt, _ := n.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: parent + "/notebookRuntimes/x"}); rt.GetRuntimeState() != aiplatformpb.NotebookRuntime_ERROR {
		t.Errorf("state = %v", rt.GetRuntimeState())
	}
}
