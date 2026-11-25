package compute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeVMs is a VMBackend that does nothing, with free space to spare.
type fakeVMs struct{ free uint64 }

func (fakeVMs) CreateDisk(context.Context, DiskSpec) error           { return nil }
func (fakeVMs) CopyDisk(context.Context, DiskSpec, DiskSpec) error   { return nil }
func (fakeVMs) DeleteDisk(context.Context, DiskSpec) error           { return nil }
func (fakeVMs) ResizeDisk(context.Context, DiskSpec) error           { return nil }
func (fakeVMs) Snapshot(context.Context, DiskSpec, string) error     { return nil }
func (fakeVMs) DeleteSnapshot(context.Context, string) error         { return nil }
func (fakeVMs) SealDisk(context.Context, DiskSpec) error             { return nil }
func (fakeVMs) UnsealDisk(context.Context, DiskSpec) error           { return nil }
func (fakeVMs) Start(context.Context, VMSpec) error                  { return nil }
func (fakeVMs) Stop(context.Context, VMSpec) error                   { return nil }
func (fakeVMs) Suspend(context.Context, VMSpec) error                { return nil }
func (fakeVMs) Resume(context.Context, VMSpec) error                 { return nil }
func (fakeVMs) SerialOutput(context.Context, VMSpec) (string, error) { return "console\n", nil }
func (fakeVMs) SSHAddress(VMSpec) string                             { return "127.0.0.1:2222" }
func (f fakeVMs) FreeBytes() (uint64, error) {
	if f.free == 0 {
		return 1 << 40, nil
	}
	return f.free, nil
}

func vmCall(t *testing.T, s *Server, method, path string, body obj) (obj, error) {
	t.Helper()
	out, err := s.Call(method, "p", path, nil, body)
	if err == nil && out != nil && out["kind"] == "compute#operation" && out["status"] != "DONE" {
		s.waitOp(context.Background(), str(out, "selfLink"))
	}
	return out, err
}

func TestVMLifecycleOnTheBackend(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	z := "zones/us-central1-a/"
	if _, err := vmCall(t, s, "POST", z+"instances", obj{"name": "vm1", "machineType": z + "machineTypes/e2-small",
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}},
		"metadata":          obj{"items": []any{obj{"key": "startup-script", "value": "echo hi"}}}}); err != nil {
		t.Fatal(err)
	}
	inst, err := s.Call("GET", "p", z+"instances/vm1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	nic := inst["networkInterfaces"].([]any)[0].(map[string]any)
	if inst["status"] != "RUNNING" || nic["networkIP"] != "10.128.0.2" || inst["cloudburrowSshAddress"] != "127.0.0.1:2222" {
		t.Fatalf("instance: %v", inst)
	}
	if d, err := s.Call("GET", "p", z+"disks/vm1", nil, nil); err != nil || d["sizeGb"] != "10" || len(d["users"].([]any)) != 1 {
		t.Fatalf("boot disk: %v %v", d, err)
	}
	// The second instance gets the subnet's next address.
	if _, err := vmCall(t, s, "POST", z+"instances", obj{"name": "vm2", "machineType": z + "machineTypes/e2-micro",
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/rocky-linux-cloud/global/images/family/rocky-linux-9", "diskSizeGb": "10"}}},
		"networkInterfaces": []any{obj{}}}); err == nil || !strings.Contains(err.Error(), "image size (20 GB)") {
		t.Fatalf("a disk smaller than its image: %v", err)
	}
	if _, err := vmCall(t, s, "POST", z+"instances/vm1/setMachineType", obj{"machineType": "e2-medium"}); ErrorStatus(err) != http.StatusBadRequest {
		t.Fatalf("setMachineType while running: %v", err)
	}
	if _, err := vmCall(t, s, "POST", z+"instances/vm1/stop", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := vmCall(t, s, "POST", z+"instances/vm1/setMachineType", obj{"machineType": z + "machineTypes/e2-medium"}); err != nil {
		t.Fatal(err)
	}
	if _, err := vmCall(t, s, "POST", z+"instances/vm1/start", nil); err != nil {
		t.Fatal(err)
	}
	inst, _ = s.Call("GET", "p", z+"instances/vm1", nil, nil)
	if inst["status"] != "RUNNING" || !strings.HasSuffix(str(inst, "machineType"), "/e2-medium") {
		t.Fatalf("after stop, setMachineType and start: %v %v", inst["status"], inst["machineType"])
	}
	// The guest reads its metadata under its token.
	var token string
	for k, o := range s.res {
		if strings.HasSuffix(k, "/instances/vm1") {
			token = str(o, "_guestToken")
		}
	}
	req := httptest.NewRequest("GET", GuestPrefix+token+"/computeMetadata/v1/instance/attributes/startup-script", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "echo hi" {
		t.Fatalf("guest metadata: %d %q", rec.Code, rec.Body)
	}
	req = httptest.NewRequest("GET", GuestPrefix+"nottoken/computeMetadata/v1/instance/id", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("an unknown token: %d", rec.Code)
	}
	if _, err := vmCall(t, s, "DELETE", z+"disks/vm1", nil); ErrorStatus(err) != http.StatusBadRequest {
		t.Fatalf("deleting a boot disk in use: %v", err)
	}
	if _, err := vmCall(t, s, "DELETE", z+"instances/vm1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("GET", "p", z+"disks/vm1", nil, nil); ErrorStatus(err) != http.StatusNotFound {
		t.Fatalf("auto-delete boot disk: %v", err)
	}
}

func TestVMDiskBudget(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{free: 1 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Call("POST", "p", "zones/us-central1-a/disks", nil, obj{"name": "d1", "sizeGb": "10"})
	if ErrorStatus(err) != http.StatusTooManyRequests || !strings.Contains(err.Error(), "RESOURCE_EXHAUSTED") {
		t.Fatalf("below the threshold: %v", err)
	}
}

func TestVMsWithoutABackendAre501(t *testing.T) {
	s, _ := New(Options{})
	if _, err := s.Call("GET", "p", "zones/us-central1-a/instances", nil, nil); ErrorStatus(err) != http.StatusNotImplemented {
		t.Fatalf("instances without a backend: %v", err)
	}
	if _, err := s.Call("GET", "p", "zones/us-central1-a/machineTypes", nil, nil); err != nil {
		t.Fatalf("machine types need no backend: %v", err)
	}
}

// slowStartVMs is fakeVMs whose Start blocks until release is closed, and
// which records the order of Start and Stop calls.
type slowStartVMs struct {
	fakeVMs
	release chan struct{}
	mu      sync.Mutex
	calls   []string
}

func (f *slowStartVMs) Start(context.Context, VMSpec) error {
	<-f.release
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	return nil
}

func (f *slowStartVMs) Stop(context.Context, VMSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	return nil
}

// TestStopWaitsForAnAttachDiskRestart (#2052): an attachDisk restarts a
// running VM's container in the background; a Stop asked meanwhile runs
// after it, so the VM ends TERMINATED, its container stopped, and is not
// set RUNNING again by the attach finishing last.
func TestStopWaitsForAnAttachDiskRestart(t *testing.T) {
	vms := &slowStartVMs{release: make(chan struct{})}
	close(vms.release)
	s, err := New(Options{VMs: vms})
	if err != nil {
		t.Fatal(err)
	}
	z := "zones/us-central1-a/"
	if _, err := vmCall(t, s, "POST", z+"instances", obj{"name": "vm1", "machineType": z + "machineTypes/e2-small",
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := vmCall(t, s, "POST", z+"disks", obj{"name": "data", "sizeGb": "10"}); err != nil {
		t.Fatal(err)
	}
	vms.release = make(chan struct{})
	vms.mu.Lock()
	vms.calls = nil
	vms.mu.Unlock()
	attach, err := s.Call("POST", "p", z+"instances/vm1/attachDisk", nil, obj{"source": z + "disks/data", "deviceName": "data"})
	if err != nil {
		t.Fatal(err)
	}
	stop, err := s.Call("POST", "p", z+"instances/vm1/stop", nil, nil)
	if err != nil {
		t.Fatalf("a stop while the attach restarts the VM: %v", err)
	}
	close(vms.release)
	s.waitOp(context.Background(), str(attach, "selfLink"))
	s.waitOp(context.Background(), str(stop, "selfLink"))
	inst, _ := s.Call("GET", "p", z+"instances/vm1", nil, nil)
	vms.mu.Lock()
	calls := strings.Join(vms.calls, ",")
	vms.mu.Unlock()
	if inst["status"] != "TERMINATED" || calls != "stop,start,stop" {
		t.Fatalf("after attachDisk then stop: status %v, backend calls %s; want TERMINATED, stop,start,stop", inst["status"], calls)
	}
}
