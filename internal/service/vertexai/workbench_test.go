package vertexai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	notebooks "cloud.google.com/go/notebooks/apiv2"
	nbpb "cloud.google.com/go/notebooks/apiv2/notebookspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// outputsHost is a fakeHost that serves JupyterLab (JupyterHost).
type outputsHost struct{ *fakeHost }

func (h outputsHost) JupyterEndpoint(key string) (string, string, bool) {
	st, _ := h.State(key)
	return "127.0.0.1:1", "tok-" + key, st == "CONNECTED"
}

func (h outputsHost) DeleteDisk(volume string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = append(h.ran, "delete-disk "+volume)
	return nil
}

func wbInstance(t *testing.T, op interface {
	GetDone() bool
}, s *Server, w *workbenchService, name string) *nbpb.Instance {
	t.Helper()
	in, err := w.GetInstance(context.Background(), &nbpb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestWorkbenchInstances(t *testing.T) {
	ctx := context.Background()
	s := NewServer(nil)
	host := outputsHost{newFakeHost()}
	file := filepath.Join(t.TempDir(), "wb.json")
	s.SetWorkbench(host, file)
	s.SetWorkbenchConsole(func() string { return "127.0.0.1:9091" })
	w := &workbenchService{s: s}
	parent := "projects/p/locations/us-central1-a"

	for _, bad := range []*nbpb.CreateInstanceRequest{
		{Parent: parent, InstanceId: "Bad", Instance: &nbpb.Instance{}},
		{Parent: parent, InstanceId: "ends-", Instance: &nbpb.Instance{}},
		{Parent: parent, InstanceId: "a" + strings.Repeat("b", 63), Instance: &nbpb.Instance{}},
		{Parent: parent, InstanceId: "ok"},
		{Parent: parent, InstanceId: "ok", Instance: &nbpb.Instance{InstanceOwners: []string{"a@x.com", "b@x.com"}}},
		{Parent: parent, InstanceId: "ok", Instance: &nbpb.Instance{Labels: map[string]string{"Bad": "x"}}},
		{Parent: parent, InstanceId: "ok", Instance: &nbpb.Instance{Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{DataDisks: []*nbpb.DataDisk{{}, {}}}}}},
		{Parent: parent, InstanceId: "ok", Instance: &nbpb.Instance{Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{BootDisk: &nbpb.BootDisk{DiskSizeGb: 64001}}}}},
		{Parent: parent, InstanceId: "ok", Instance: &nbpb.Instance{Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{Metadata: map[string]string{"idle-timeout-seconds": "60"}}}}},
	} {
		if _, err := w.CreateInstance(ctx, bad); code(err) != codes.InvalidArgument {
			t.Errorf("create %v = %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	if _, err := w.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: "gpu", Instance: &nbpb.Instance{Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{
		AcceleratorConfigs: []*nbpb.AcceleratorConfig{{Type: nbpb.AcceleratorConfig_NVIDIA_TESLA_T4, CoreCount: 1}}}}}}); code(err) != codes.FailedPrecondition {
		t.Errorf("create with a GPU = %v, want FAILED_PRECONDITION", err)
	}

	op, err := w.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: "my-instance", Instance: &nbpb.Instance{Labels: map[string]string{"env": "dev"},
		Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{Metadata: map[string]string{"idle-timeout-seconds": "3600"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	op = waitNotebookOp(t, s, op)
	if op.GetError() != nil {
		t.Fatalf("create failed: %v", op.GetError())
	}
	name := parent + "/instances/my-instance"
	if !strings.HasPrefix(op.GetName(), parent+"/operations/") {
		t.Errorf("operation name %s, want under %s/operations", op.GetName(), parent)
	}
	in := wbInstance(t, op, s, w, name)
	g := in.GetGceSetup()
	if in.GetState() != nbpb.State_ACTIVE || in.GetHealthState() != nbpb.HealthState_HEALTHY || in.GetId() == "" ||
		g.GetMachineType() != "n1-standard-4" || g.GetBootDisk().GetDiskSizeGb() != 150 || g.GetDataDisks()[0].GetDiskSizeGb() != 100 ||
		g.GetVmImage().GetProject() != "cloud-notebooks-managed" {
		t.Errorf("instance = %v", in)
	}
	if o := host.opts[workbenchKey(name)]; o.IdleTimeout.Seconds() != 3600 {
		t.Errorf("idle timeout = %v, want 1h from the metadata", o.IdleTimeout)
	}
	if _, err := w.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: "my-instance", Instance: &nbpb.Instance{}}); code(err) != codes.AlreadyExists {
		t.Errorf("duplicate = %v", err)
	}
	// JupyterLab: the instance's setup, and its proxy_uri, which names
	// the instance's JupyterLab.
	j := host.opts[workbenchKey(name)].Jupyter
	if j == nil || j.Volume != "cb-workbench-"+in.GetId() || j.Name != "my-instance" || j.Zone != "us-central1-a" || j.ID != in.GetId() || j.Mint == nil {
		t.Errorf("jupyter setup = %+v", j)
	}
	label, port, ok := strings.Cut(in.GetProxyUri(), "."+WorkbenchProxyDomain+":")
	if !ok || port != "9091" || !strings.HasSuffix(label, "-dot-us-central1") {
		t.Errorf("proxy_uri = %q", in.GetProxyUri())
	}
	if addr, tok, n, err := s.WorkbenchJupyterLab(label); err != nil || addr != "127.0.0.1:1" || tok != "tok-"+workbenchKey(name) || n != name {
		t.Errorf("JupyterLab of %s = %s %s %s %v", label, addr, tok, n, err)
	}
	if _, _, _, err := s.WorkbenchJupyterLab("nope-dot-us-central1"); code(err) != codes.NotFound {
		t.Errorf("unknown proxy host = %v", err)
	}

	// Update: labels and metadata; machine type only while STOPPED.
	op, err = w.UpdateInstance(ctx, &nbpb.UpdateInstanceRequest{Instance: &nbpb.Instance{Name: name, Labels: map[string]string{"env": "prod"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil || waitNotebookOp(t, s, op).GetError() != nil {
		t.Fatalf("update labels = %v, %v", op, err)
	}
	if in := wbInstance(t, op, s, w, name); in.GetLabels()["env"] != "prod" {
		t.Errorf("labels = %v", in.GetLabels())
	}
	mt := &nbpb.UpdateInstanceRequest{Instance: &nbpb.Instance{Name: name, Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{MachineType: "e2-standard-8"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"gceSetup.machineType"}}}
	if _, err := w.UpdateInstance(ctx, mt); code(err) != codes.FailedPrecondition {
		t.Errorf("machine type while ACTIVE = %v", err)
	}
	if _, err := w.UpdateInstance(ctx, &nbpb.UpdateInstanceRequest{Instance: &nbpb.Instance{Name: name}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"creator"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("update creator = %v", err)
	}
	if _, err := w.UpdateInstance(ctx, &nbpb.UpdateInstanceRequest{Instance: &nbpb.Instance{Name: name}}); code(err) != codes.InvalidArgument {
		t.Errorf("update without a mask = %v", err)
	}

	// Stop, then the machine type, Start, Reset.
	op, err = w.StopInstance(ctx, &nbpb.StopInstanceRequest{Name: name})
	if err != nil || waitNotebookOp(t, s, op).GetError() != nil {
		t.Fatalf("stop = %v, %v", op, err)
	}
	if in := wbInstance(t, op, s, w, name); in.GetState() != nbpb.State_STOPPED {
		t.Errorf("after stop: %v", in.GetState())
	}
	if _, _, _, err := s.WorkbenchJupyterLab(label); code(err) != codes.FailedPrecondition {
		t.Errorf("JupyterLab of a stopped instance = %v", err)
	}
	if in := wbInstance(t, op, s, w, name); in.GetProxyUri() == "" {
		t.Error("a stopped instance lost its proxy_uri")
	}
	if _, err := w.ResetInstance(ctx, &nbpb.ResetInstanceRequest{Name: name}); code(err) != codes.FailedPrecondition {
		t.Errorf("reset a stopped instance = %v", err)
	}
	if op, err = w.UpdateInstance(ctx, mt); err != nil || waitNotebookOp(t, s, op).GetError() != nil {
		t.Fatalf("machine type while STOPPED = %v, %v", op, err)
	}
	op, err = w.StartInstance(ctx, &nbpb.StartInstanceRequest{Name: name})
	if err != nil || waitNotebookOp(t, s, op).GetError() != nil {
		t.Fatalf("start = %v, %v", op, err)
	}
	if in := wbInstance(t, op, s, w, name); in.GetState() != nbpb.State_ACTIVE || in.GetGceSetup().GetMachineType() != "e2-standard-8" {
		t.Errorf("after start: %v %s", in.GetState(), in.GetGceSetup().GetMachineType())
	}
	op, err = w.ResetInstance(ctx, &nbpb.ResetInstanceRequest{Name: name})
	if err != nil || waitNotebookOp(t, s, op).GetError() != nil {
		t.Fatalf("reset = %v, %v", op, err)
	}
	if u, err := w.CheckInstanceUpgradability(ctx, &nbpb.CheckInstanceUpgradabilityRequest{NotebookInstance: name}); err != nil || u.GetUpgradeable() {
		t.Errorf("upgradability = %v, %v", u, err)
	}
	if _, err := w.UpgradeInstance(ctx, &nbpb.UpgradeInstanceRequest{Name: name}); code(err) != codes.FailedPrecondition {
		t.Errorf("upgrade = %v", err)
	}

	// List and filter, REST.
	if _, err := w.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: "second", Instance: &nbpb.Instance{}}); err != nil {
		t.Fatal(err)
	}
	l, err := w.ListInstances(ctx, &nbpb.ListInstancesRequest{Parent: parent, Filter: `labels.env="prod"`})
	if err != nil || len(l.GetInstances()) != 1 || l.GetInstances()[0].GetName() != name {
		t.Errorf("filtered list = %v, %v", l, err)
	}
	for _, f := range []string{`labels.env="prod" AND state=ACTIVE`, `name="` + name + `"`} {
		l, err := w.ListInstances(ctx, &nbpb.ListInstancesRequest{Parent: parent, Filter: f})
		if err != nil || len(l.GetInstances()) != 1 || l.GetInstances()[0].GetName() != name {
			t.Errorf("list %s = %v, %v", f, l, err)
		}
	}
	if l, err := w.ListInstances(ctx, &nbpb.ListInstancesRequest{Parent: "projects/p/locations/-"}); err != nil || len(l.GetInstances()) != 2 {
		t.Errorf("list under locations/- = %v, %v", l, err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v2/" + parent + "/instances")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"my-instance"`) && !strings.Contains(string(b), "instances/my-instance") {
		t.Errorf("REST list = %d %s", resp.StatusCode, b)
	}
	resp, err = http.Post(srv.URL+"/v2/"+name+":stop", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var restOp struct {
		Name string `json:"name"`
		Done bool   `json:"done"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(b, &restOp) != nil || !restOp.Done {
		t.Fatalf("REST stop = %d %s", resp.StatusCode, b)
	}
	resp, err = http.Get(srv.URL + "/v2/" + restOp.Name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "google.cloud.notebooks.v2.Instance") {
		t.Errorf("REST operation = %d %s", resp.StatusCode, b)
	}

	// Persistence: a restarted instance is STOPPED.
	s2 := NewServer(nil)
	s2.SetWorkbench(outputsHost{newFakeHost()}, file)
	w2 := &workbenchService{s: s2}
	if in, err := w2.GetInstance(ctx, &nbpb.GetInstanceRequest{Name: parent + "/instances/second"}); err != nil || in.GetState() != nbpb.State_STOPPED {
		t.Errorf("restored = %v, %v", in, err)
	}

	op, err = w.DeleteInstance(ctx, &nbpb.DeleteInstanceRequest{Name: name})
	if err != nil || waitNotebookOp(t, s, op).GetError() != nil {
		t.Fatalf("delete = %v, %v", op, err)
	}
	if _, err := w.GetInstance(ctx, &nbpb.GetInstanceRequest{Name: name}); code(err) != codes.NotFound {
		t.Errorf("get after delete = %v", err)
	}
	if _, ok := host.state[workbenchKey(name)]; ok {
		t.Error("the deleted instance's runtime is still running")
	}
	if !strings.Contains(strings.Join(host.ran, "\n"), "delete-disk cb-workbench-"+in.GetId()) {
		t.Errorf("the data disk was not deleted: %v", host.ran)
	}

	// Idle shutdown off: idle-timeout-seconds present and empty.
	op, err = w.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: "no-idle", Instance: &nbpb.Instance{
		Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{Metadata: map[string]string{"idle-timeout-seconds": ""}}}}})
	if err != nil || waitNotebookOp(t, s, op).GetError() != nil {
		t.Fatalf("create without idle shutdown = %v, %v", op, err)
	}
	if o := host.opts[workbenchKey(parent+"/instances/no-idle")]; !o.IdleDisabled {
		t.Errorf("idle shutdown with an empty idle-timeout-seconds = %+v, want off", o)
	}
	if o := host.opts[workbenchKey(parent+"/instances/second")]; o.IdleDisabled || o.IdleTimeout != defaultIdleTimeout {
		t.Errorf("default idle shutdown = %+v, want 180 minutes", o)
	}
}

// TestWorkbenchRESTClient drives the JSON surface with the official REST
// client: its create, update (camelCase mask), stop and delete operations.
func TestWorkbenchRESTClient(t *testing.T) {
	ctx := context.Background()
	s := NewServer(nil)
	s.SetWorkbench(outputsHost{newFakeHost()}, "")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	c, err := notebooks.NewNotebookRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	parent := "projects/p/locations/us-central1-a"
	op, err := c.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: "rest-one", Instance: &nbpb.Instance{}})
	if err != nil {
		t.Fatal(err)
	}
	in, err := op.Wait(ctx)
	if err != nil || in.GetState() != nbpb.State_ACTIVE {
		t.Fatalf("create = %v, %v", in, err)
	}
	up, err := c.UpdateInstance(ctx, &nbpb.UpdateInstanceRequest{Instance: &nbpb.Instance{Name: in.GetName(), Labels: map[string]string{"a": "b"},
		Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{Metadata: map[string]string{"idle-timeout-seconds": "1200"}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels", "gce_setup.metadata"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := up.Wait(ctx); err != nil || got.GetLabels()["a"] != "b" || got.GetGceSetup().GetMetadata()["idle-timeout-seconds"] != "1200" {
		t.Errorf("update = %v, %v", got, err)
	}
	stop, err := c.StopInstance(ctx, &nbpb.StopInstanceRequest{Name: in.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := stop.Wait(ctx); err != nil || got.GetState() != nbpb.State_STOPPED {
		t.Errorf("stop = %v, %v", got, err)
	}
	if _, err := c.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: "rest-one", Instance: &nbpb.Instance{}}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate over REST = %v", err)
	}
	del, err := c.DeleteInstance(ctx, &nbpb.DeleteInstanceRequest{Name: in.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := del.Wait(ctx); err != nil {
		t.Errorf("delete: %v", err)
	}
}
