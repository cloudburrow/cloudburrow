package compute

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// dataVMs is fakeVMs whose disks hold a string, and which records each
// instance's last start, so a test reads what a VM sees.
type dataVMs struct {
	fakeVMs
	mu     sync.Mutex
	data   map[string]string
	starts map[string]VMSpec
}

func newDataVMs() *dataVMs { return &dataVMs{data: map[string]string{}, starts: map[string]VMSpec{}} }

func (f *dataVMs) CreateDisk(_ context.Context, d DiskSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[d.Key] = f.data[d.SourceSnapshot]
	return nil
}
func (f *dataVMs) CopyDisk(_ context.Context, src, dst DiskSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[dst.Key] = f.data[src.Key]
	return nil
}
func (f *dataVMs) Snapshot(_ context.Context, d DiskSpec, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = f.data[d.Key]
	return nil
}
func (f *dataVMs) Start(_ context.Context, vm VMSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts[vm.Name] = vm
	return nil
}

// write is what a guest writing to its disk device does.
func (f *dataVMs) write(t *testing.T, vm, device, text string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.starts[vm].Disks {
		if d.DeviceName == device {
			if d.ReadOnly {
				t.Fatalf("%s writes to %s, which it has read-only", vm, device)
			}
			f.data[d.Disk.Key] = text
			return
		}
	}
	t.Fatalf("%s has no disk %s", vm, device)
}

func (f *dataVMs) read(t *testing.T, vm, device string) (string, bool) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.starts[vm].Disks {
		if d.DeviceName == device {
			return f.data[d.Disk.Key], d.ReadOnly
		}
	}
	t.Fatalf("%s has no disk %s", vm, device)
	return "", false
}

func mustCall(t *testing.T, s *Server, method, path string, body obj) obj {
	t.Helper()
	out, err := vmCall(t, s, method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return out
}

func wantStatus(t *testing.T, s *Server, code int, want, method, path string, body obj) {
	t.Helper()
	_, err := vmCall(t, s, method, path, body)
	if ErrorStatus(err) != code || !strings.Contains(err.Error(), want) {
		t.Errorf("%s %s: %v; want %d with %q", method, path, err, code, want)
	}
}

func newVM(t *testing.T, s *Server, zone, name, mt string) {
	t.Helper()
	z := "zones/" + zone + "/"
	mustCall(t, s, "POST", z+"instances", obj{"name": name, "machineType": z + "machineTypes/" + mt,
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}}})
}

// TestRegionalDisks (#2206): the documented limits of a regional disk, its
// types, attaching it in either replica zone only, the failover by
// force-attach (the other VM fenced read-only, the data the same disk's),
// a snapshot and a clone of it.
func TestRegionalDisks(t *testing.T) {
	vms := newDataVMs()
	s, err := New(Options{VMs: vms})
	if err != nil {
		t.Fatal(err)
	}
	r := "regions/us-central1/"
	zs := []any{"zones/us-central1-a", "projects/p/zones/us-central1-b"}
	types := mustCall(t, s, "GET", r+"diskTypes", nil)
	if items, _ := types["items"].([]any); len(items) != 4 {
		t.Fatalf("regionDiskTypes: %v", types)
	}
	if pd := mustCall(t, s, "GET", r+"diskTypes/pd-standard", nil); pd["validDiskSize"] != "200GB-65536GB" {
		t.Errorf("regional pd-standard: %v", pd)
	}
	wantStatus(t, s, 404, "not found", "GET", r+"diskTypes/pd-extreme", nil)
	for _, c := range []struct {
		body obj
		want string
	}{
		{obj{"name": "d", "sizeGb": "10"}, "replicaZones"},
		{obj{"name": "d", "sizeGb": "10", "replicaZones": []any{"zones/us-central1-a"}}, "exactly two"},
		{obj{"name": "d", "sizeGb": "10", "replicaZones": []any{"zones/us-central1-a", "zones/us-central1-a"}}, "distinct"},
		{obj{"name": "d", "sizeGb": "10", "replicaZones": []any{"zones/us-central1-a", "zones/us-east1-b"}}, "region"},
		{obj{"name": "d", "sizeGb": "100", "type": r + "diskTypes/pd-standard", "replicaZones": zs}, "between 200 GB"},
		{obj{"name": "d", "sizeGb": "10", "type": r + "diskTypes/pd-extreme", "replicaZones": zs}, "not valid for a regional disk"},
		{obj{"name": "d", "sizeGb": "70000", "replicaZones": zs}, "between 10 GB"},
	} {
		wantStatus(t, s, 400, c.want, "POST", r+"disks", c.body)
	}
	mustCall(t, s, "POST", r+"disks", obj{"name": "rd", "sizeGb": "10", "replicaZones": zs, "labels": obj{"env": "test"}})
	d := mustCall(t, s, "GET", r+"disks/rd", nil)
	if d["status"] != "READY" || d["region"] != "projects/p/regions/us-central1" || len(d["replicaZones"].([]any)) != 2 ||
		d["type"] != "projects/p/regions/us-central1/diskTypes/pd-balanced" {
		t.Fatalf("regional disk: %v", d)
	}
	agg := mustCall(t, s, "GET", "aggregated/disks", nil)
	if _, ok := agg["items"].(map[string]any)["regions/us-central1"]; !ok {
		t.Errorf("aggregated disks: %v", agg)
	}

	newVM(t, s, "us-central1-a", "vma", "e2-small")
	newVM(t, s, "us-central1-b", "vmb", "e2-small")
	newVM(t, s, "us-central1-c", "vmc", "e2-small")
	newVM(t, s, "us-central1-a", "vmd", "c3-standard-4")
	attach := func(vm, zone string, q map[string][]string) error {
		out, err := s.Call("POST", "p", "zones/"+zone+"/instances/"+vm+"/attachDisk", q, obj{"source": r + "disks/rd", "deviceName": "data"})
		if err == nil {
			s.waitOp(context.Background(), str(out, "selfLink"))
		}
		return err
	}
	if err := attach("vmc", "us-central1-c", nil); ErrorStatus(err) != 400 || !strings.Contains(err.Error(), "replicas") {
		t.Errorf("attach outside the replica zones: %v", err)
	}
	if err := attach("vmd", "us-central1-a", nil); ErrorStatus(err) != 400 || !strings.Contains(err.Error(), "E2, N1, N2, and N2D") {
		t.Errorf("attach to a C3 VM: %v", err)
	}
	if err := attach("vma", "us-central1-a", nil); err != nil {
		t.Fatal(err)
	}
	vms.write(t, "vma", "data", "written in zone a")
	if err := attach("vmb", "us-central1-b", nil); ErrorStatus(err) != 400 || !strings.Contains(err.Error(), "already being used") {
		t.Errorf("attach while attached elsewhere: %v", err)
	}
	if err := attach("vmb", "us-central1-b", map[string][]string{"forceAttach": {"true"}}); err != nil {
		t.Fatalf("force-attach: %v", err)
	}
	if got, ro := vms.read(t, "vmb", "data"); got != "written in zone a" || ro {
		t.Errorf("zone b's VM reads %q (read-only %v); want what zone a's wrote, read-write", got, ro)
	}
	// Zone a's VM is restarted with the disk read-only by its own
	// operation.
	for i := 0; ; i++ {
		if _, ro := vms.read(t, "vma", "data"); ro {
			break
		}
		if i == 200 {
			t.Fatal("after the force-attach zone a's VM can still write the disk")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mustCall(t, s, "POST", "zones/us-central1-a/disks", obj{"name": "zd", "sizeGb": "10"})
	wantStatus(t, s, 400, "regional disks only", "POST", "zones/us-central1-a/instances/vma/attachDisk",
		obj{"source": "zones/us-central1-a/disks/zd", "deviceName": "zd", "forceAttach": true})
	wantStatus(t, s, 400, "already being used", "DELETE", r+"disks/rd", nil)

	// Resize, labels, a snapshot and a clone.
	wantStatus(t, s, 400, "must be larger", "POST", r+"disks/rd/resize", obj{"sizeGb": "10"})
	mustCall(t, s, "POST", r+"disks/rd/resize", obj{"sizeGb": "20"})
	mustCall(t, s, "POST", r+"disks/rd/setLabels", obj{"labels": obj{"env": "prod"}, "labelFingerprint": d["labelFingerprint"]})
	mustCall(t, s, "POST", r+"disks/rd/createSnapshot", obj{"name": "rsnap"})
	snap := mustCall(t, s, "GET", "global/snapshots/rsnap", nil)
	if snap["sourceDisk"] != "projects/p/regions/us-central1/disks/rd" || snap["storageLocations"].([]any)[0] != "us-central1" {
		t.Errorf("snapshot of a regional disk: %v", snap)
	}
	mustCall(t, s, "POST", r+"disks", obj{"name": "fromsnap", "sourceSnapshot": "global/snapshots/rsnap", "replicaZones": zs})
	mustCall(t, s, "POST", "zones/us-central1-b/disks", obj{"name": "zclone", "sourceDisk": r + "disks/rd"})
	wantStatus(t, s, 400, "own zone", "POST", "zones/us-central1-c/disks", obj{"name": "zclone2", "sourceDisk": r + "disks/rd"})
	for _, n := range []string{"fromsnap", "zclone"} {
		path := r + "disks/" + n
		if n == "zclone" {
			path = "zones/us-central1-b/disks/zclone"
		}
		got := mustCall(t, s, "GET", path, nil)
		k := vmKey("disk", got)
		vms.mu.Lock()
		v := vms.data[k]
		vms.mu.Unlock()
		if v != "written in zone a" {
			t.Errorf("%s holds %q", n, v)
		}
	}
	if out, err := s.Call("POST", "p", "zones/us-central1-b/instances/vmb/detachDisk", map[string][]string{"deviceName": {"data"}}, nil); err != nil {
		t.Fatal(err)
	} else {
		s.waitOp(context.Background(), str(out, "selfLink"))
	}
	if d := mustCall(t, s, "GET", r+"disks/rd", nil); len(strs(d, "users")) != 1 {
		t.Errorf("after vmb detached: users %v", d["users"])
	}
}

// TestAsyncReplication (#2206): a secondary disk in the primary's continent
// only, blank and of the same size; start, the documented refusals while
// it replicates, the copy when it stops, and a consistency group stopped
// and cloned as one.
func TestAsyncReplication(t *testing.T) {
	vms := newDataVMs()
	s, err := New(Options{VMs: vms})
	if err != nil {
		t.Fatal(err)
	}
	r, r2 := "regions/us-central1/", "regions/us-east1/"
	zs := []any{"zones/us-central1-a", "zones/us-central1-b"}
	zs2 := []any{"zones/us-east1-b", "zones/us-east1-c"}
	mustCall(t, s, "POST", r+"resourcePolicies", obj{"name": "cg", "diskConsistencyGroupPolicy": obj{}})
	wantStatus(t, s, 400, "snapshotSchedulePolicy.schedule", "POST", r+"resourcePolicies", obj{"name": "sched", "snapshotSchedulePolicy": obj{}})
	wantStatus(t, s, 501, "instanceSchedulePolicy", "POST", r+"resourcePolicies", obj{"name": "is", "instanceSchedulePolicy": obj{}})
	mustCall(t, s, "POST", r+"disks", obj{"name": "prim", "sizeGb": "10", "replicaZones": zs, "resourcePolicies": []any{r + "resourcePolicies/cg"}})
	mustCall(t, s, "POST", r+"resourcePolicies", obj{"name": "cg-other", "diskConsistencyGroupPolicy": obj{}})
	wantStatus(t, s, 400, "only one consistency group", "POST", r+"disks/prim/addResourcePolicies", obj{"resourcePolicies": []any{"cg-other"}})
	key := func(path string) string { return vmKey("disk", mustCall(t, s, "GET", path, nil)) }
	vms.mu.Lock()
	vms.data[key(r+"disks/prim")] = "v1"
	vms.mu.Unlock()
	prim := "projects/p/regions/us-central1/disks/prim"
	wantStatus(t, s, 400, "same size", "POST", r2+"disks", obj{"name": "sec", "sizeGb": "20", "replicaZones": zs2, "asyncPrimaryDisk": obj{"disk": prim}})
	wantStatus(t, s, 400, "continent", "POST", "regions/europe-west1/disks", obj{"name": "sec", "replicaZones": []any{"zones/europe-west1-b", "zones/europe-west1-c"}, "asyncPrimaryDisk": obj{"disk": prim}})
	wantStatus(t, s, 400, "blank", "POST", r2+"disks", obj{"name": "sec", "replicaZones": zs2, "sourceSnapshot": "global/snapshots/x", "asyncPrimaryDisk": obj{"disk": prim}})
	mustCall(t, s, "POST", r2+"disks", obj{"name": "sec", "replicaZones": zs2, "asyncPrimaryDisk": obj{"disk": prim}})
	sec := mustCall(t, s, "GET", r2+"disks/sec", nil)
	if sec["sizeGb"] != "10" || sec["resourceStatus"].(map[string]any)["asyncPrimaryDisk"].(map[string]any)["state"] != "CREATED" {
		t.Fatalf("secondary: %v", sec)
	}
	wantStatus(t, s, 400, "not in an active", "POST", r+"disks/prim/stopAsyncReplication", nil)
	mustCall(t, s, "POST", r+"disks/prim/startAsyncReplication", obj{"asyncSecondaryDisk": "projects/p/regions/us-east1/disks/sec"})
	p := mustCall(t, s, "GET", r+"disks/prim", nil)
	if st := p["resourceStatus"].(map[string]any)["asyncSecondaryDisks"].(map[string]any)["projects/p/regions/us-east1/disks/sec"].(map[string]any)["state"]; st != "ACTIVE" {
		t.Fatalf("primary after start: %v", p)
	}
	wantStatus(t, s, 400, "already active", "POST", r+"disks/prim/startAsyncReplication", obj{"asyncSecondaryDisk": "projects/p/regions/us-east1/disks/sec"})
	wantStatus(t, s, 400, "replication", "DELETE", r+"disks/prim", nil)
	wantStatus(t, s, 400, "replication", "POST", r2+"disks/sec/createSnapshot", obj{"name": "s"})
	wantStatus(t, s, 400, "can't remove a primary", "POST", r+"disks/prim/removeResourcePolicies", obj{"resourcePolicies": []any{"cg"}})
	newVM(t, s, "us-east1-b", "dr", "e2-small")
	wantStatus(t, s, 400, "until replication stops", "POST", "zones/us-east1-b/instances/dr/attachDisk", obj{"source": r2 + "disks/sec"})
	vms.mu.Lock()
	vms.data[key(r+"disks/prim")] = "v2"
	vms.mu.Unlock()
	// A clone of the group's secondaries, in the secondaries' region: the
	// group's policy there holds them.
	mustCall(t, s, "POST", r2+"resourcePolicies", obj{"name": "cg2", "diskConsistencyGroupPolicy": obj{}})
	mustCall(t, s, "POST", r2+"disks/sec/addResourcePolicies", obj{"resourcePolicies": []any{"cg2"}})
	wantStatus(t, s, 400, "primary disks", "POST", r+"disks/bulkInsert", obj{"sourceConsistencyGroupPolicy": r + "resourcePolicies/cg"})
	mustCall(t, s, "POST", r2+"disks/bulkInsert", obj{"sourceConsistencyGroupPolicy": "projects/p/regions/us-east1/resourcePolicies/cg2"})
	clone := mustCall(t, s, "GET", r2+"disks/sec-clone", nil)
	vms.mu.Lock()
	got := vms.data[vmKey("disk", clone)]
	vms.mu.Unlock()
	if got != "v2" || clone["sourceConsistencyGroupPolicy"] != "projects/p/regions/us-east1/resourcePolicies/cg2" {
		t.Errorf("clone of the group: %q %v", got, clone)
	}
	vms.mu.Lock()
	vms.data[key(r+"disks/prim")] = "v3"
	vms.mu.Unlock()
	mustCall(t, s, "POST", r2+"disks/stopGroupAsyncReplication", obj{"resourcePolicy": "projects/p/regions/us-east1/resourcePolicies/cg2"})
	sec = mustCall(t, s, "GET", r2+"disks/sec", nil)
	vms.mu.Lock()
	got = vms.data[vmKey("disk", sec)]
	vms.mu.Unlock()
	if got != "v3" || sec["resourceStatus"].(map[string]any)["asyncPrimaryDisk"].(map[string]any)["state"] != "STOPPED" {
		t.Errorf("secondary after the group stopped: %q %v", got, sec)
	}
	wantStatus(t, s, 400, "cannot be restarted", "POST", r+"disks/prim/startAsyncReplication", obj{"asyncSecondaryDisk": "projects/p/regions/us-east1/disks/sec"})
	mustCall(t, s, "POST", "zones/us-east1-b/instances/dr/attachDisk", obj{"source": r2 + "disks/sec", "deviceName": "sec"})
	wantStatus(t, s, 400, "already being used", "DELETE", r+"resourcePolicies/cg", nil)
	if _, err := vmCall(t, s, "DELETE", r+"disks/prim", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := vmCall(t, s, "DELETE", r+"resourcePolicies/cg", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("GET", "p", r+"resourcePolicies/cg", nil, nil); ErrorStatus(err) != http.StatusNotFound {
		t.Errorf("deleted policy: %v", err)
	}
}

// TestZonalAsyncReplication (#2206): Asynchronous Replication between
// zonal disks at the zonal paths: a zonal secondary of a zonal primary,
// the secondary in a consistency group of its region, the group cloned in
// the secondary's zone by disks.bulkInsert, stopGroupAsyncReplication at a
// zonal path, and removeResourcePolicies.
func TestZonalAsyncReplication(t *testing.T) {
	vms := newDataVMs()
	s, err := New(Options{VMs: vms})
	if err != nil {
		t.Fatal(err)
	}
	za, zb := "zones/us-central1-a/", "zones/us-east1-b/"
	mustCall(t, s, "POST", za+"disks", obj{"name": "prim", "sizeGb": "10"})
	key := func(path string) string { return vmKey("disk", mustCall(t, s, "GET", path, nil)) }
	vms.mu.Lock()
	vms.data[key(za+"disks/prim")] = "z1"
	vms.mu.Unlock()
	mustCall(t, s, "POST", zb+"disks", obj{"name": "sec", "asyncPrimaryDisk": obj{"disk": "projects/p/zones/us-central1-a/disks/prim"}})
	mustCall(t, s, "POST", za+"disks/prim/startAsyncReplication", obj{"asyncSecondaryDisk": "projects/p/zones/us-east1-b/disks/sec"})
	mustCall(t, s, "POST", "regions/us-east1/resourcePolicies", obj{"name": "zcg", "diskConsistencyGroupPolicy": obj{}})
	mustCall(t, s, "POST", zb+"disks/sec/addResourcePolicies", obj{"resourcePolicies": []any{"projects/p/regions/us-east1/resourcePolicies/zcg"}})
	vms.mu.Lock()
	vms.data[key(za+"disks/prim")] = "z2"
	vms.mu.Unlock()
	mustCall(t, s, "POST", zb+"disks/bulkInsert", obj{"sourceConsistencyGroupPolicy": "projects/p/regions/us-east1/resourcePolicies/zcg"})
	vms.mu.Lock()
	got := vms.data[key(zb+"disks/sec-clone")]
	vms.mu.Unlock()
	if got != "z2" {
		t.Errorf("the zonal clone holds %q, want the primary's z2", got)
	}
	mustCall(t, s, "POST", zb+"disks/stopGroupAsyncReplication", obj{"resourcePolicy": "projects/p/regions/us-east1/resourcePolicies/zcg"})
	sec := mustCall(t, s, "GET", zb+"disks/sec", nil)
	if sec["resourceStatus"].(map[string]any)["asyncPrimaryDisk"].(map[string]any)["state"] != "STOPPED" {
		t.Errorf("the zonal secondary after the group stopped: %v", sec)
	}
	mustCall(t, s, "POST", zb+"disks/sec/removeResourcePolicies", obj{"resourcePolicies": []any{"projects/p/regions/us-east1/resourcePolicies/zcg"}})
	if sec := mustCall(t, s, "GET", zb+"disks/sec", nil); sec["resourcePolicies"] != nil {
		t.Errorf("the zonal secondary after removeResourcePolicies: %v", sec["resourcePolicies"])
	}
}

// TestAggregatedDiskTypes (#2206): diskTypes.aggregatedList has the zonal
// types by zone and the regional types by region.
func TestAggregatedDiskTypes(t *testing.T) {
	s, err := New(Options{VMs: newDataVMs()})
	if err != nil {
		t.Fatal(err)
	}
	m := mustCall(t, s, "GET", "aggregated/diskTypes", nil)
	items := m["items"].(map[string]any)
	r := items["regions/us-central1"].(map[string]any)["diskTypes"].([]any)
	z := items["zones/us-central1-a"].(map[string]any)["diskTypes"].([]any)
	if len(r) != len(regionDiskTypes) || len(z) != len(diskTypes) {
		t.Fatalf("aggregated diskTypes: %d regional, %d zonal", len(r), len(z))
	}
}
