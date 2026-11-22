package compute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

// parityFixture is a server with VMs and a settable clock.
func parityFixture(t *testing.T) (*Server, *time.Time, func(method, path string, q map[string][]string, body obj) obj) {
	t.Helper()
	now := time.Date(2026, 11, 22, 8, 30, 0, 0, time.UTC)
	s, err := New(Options{VMs: fakeVMs{free: 1 << 40}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, q map[string][]string, body obj) obj {
		t.Helper()
		out, err := s.Call(method, "p2139", path, q, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		if method != http.MethodGet {
			s.waitOp(context.Background(), "")
			waitIdle(t, s)
		}
		return out
	}
	return s, &now, call
}

// waitIdle waits until no operation is running.
func waitIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.waiters)
		for vm, q := range s.queued {
			// A managed VM being made again (recreateManagedFromLocked).
			if _, listed := s.res[vm]; q > 0 && !listed {
				n++
			}
		}
		s.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("operations never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func vmBody(name string, extra obj) obj {
	b := obj{"name": name, "machineType": "zones/us-central1-a/machineTypes/e2-small",
		"disks":             []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{map[string]any{"network": "global/networks/default"}}}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// TestCustomImagesAndCloneDisk (#2139): an image of a disk, its family,
// a disk made from it (a copy of the image's storage running the source's
// public image), Deprecate, getFromFamily skipping the deprecated, Clone
// disk, and Delete.
func TestCustomImagesAndCloneDisk(t *testing.T) {
	s, _, call := parityFixture(t)
	call("POST", "zones/us-central1-a/disks", nil, obj{"name": "src", "sourceImage": "projects/debian-cloud/global/images/family/debian-12", "sizeGb": "20"})
	call("POST", "global/images", nil, obj{"name": "img1", "sourceDisk": "zones/us-central1-a/disks/src", "family": "app", "description": "v1"})
	img := call("GET", "global/images/img1", nil, nil)
	if img["status"] != "READY" || img["diskSizeGb"] != "20" || !strings.HasSuffix(str(img, "sourceDisk"), "/disks/src") || img["family"] != "app" {
		t.Fatalf("image = %v", img)
	}
	if fam := call("GET", "global/images/family/app", nil, nil); fam["name"] != "img1" {
		t.Fatalf("getFromFamily = %v", fam)
	}
	call("POST", "zones/us-central1-a/disks", nil, obj{"name": "from-img", "sourceImage": "global/images/img1"})
	s.mu.Lock()
	d := s.res["projects/p2139/zones/us-central1-a/disks/from-img"]
	spec := s.diskSpecLocked(d)
	s.mu.Unlock()
	if spec.SourceSnapshot == "" || spec.Image == nil || spec.Image.Family != "debian-12" || d["sizeGb"] != "20" {
		t.Fatalf("a disk from the custom image: %+v, %v", spec, d)
	}
	call("POST", "global/images/img1/deprecate", nil, obj{"state": "DEPRECATED"})
	if _, err := s.Call("GET", "p2139", "global/images/family/app", nil, nil); err == nil {
		t.Error("getFromFamily answered a deprecated image")
	}
	call("POST", "global/images/img1/deprecate", nil, obj{"state": "OBSOLETE"})
	if _, err := s.Call("POST", "p2139", "zones/us-central1-a/disks", nil, obj{"name": "late", "sourceImage": "global/images/img1"}); err == nil {
		t.Error("a disk was made from an obsolete image")
	}
	// Clone disk.
	call("POST", "zones/us-central1-a/disks", nil, obj{"name": "clone", "sourceDisk": "zones/us-central1-a/disks/src"})
	s.mu.Lock()
	cl := s.res["projects/p2139/zones/us-central1-a/disks/clone"]
	cspec := s.diskSpecLocked(cl)
	s.mu.Unlock()
	if cl["sizeGb"] != "20" || !strings.HasPrefix(cspec.SourceSnapshot, "disk-") || cspec.Image == nil {
		t.Fatalf("clone = %v %+v", cl, cspec)
	}
	if _, err := s.Call("POST", "p2139", "zones/us-central1-a/disks", nil, obj{"name": "far", "sourceDisk": "projects/p2139/zones/us-east1-b/disks/src"}); err == nil {
		t.Error("a clone of a disk of another zone was accepted")
	}
	call("DELETE", "global/images/img1", nil, nil)
	if _, err := s.Call("GET", "p2139", "global/images/img1", nil, nil); err == nil {
		t.Error("the image is still there after Delete")
	}
}

// TestSnapshotSchedulesRun (#2139): a daily schedule, a disk given it, its
// snapshot taken at the next start time (autoCreated, named after the
// disk and zone), the retention deleting it, and Delete refused while
// attached.
func TestSnapshotSchedulesRun(t *testing.T) {
	s, now, call := parityFixture(t)
	if _, err := s.Call("POST", "p2139", "regions/us-central1/resourcePolicies", nil, obj{"name": "bad",
		"snapshotSchedulePolicy": obj{"schedule": obj{"dailySchedule": obj{"daysInCycle": 1.0, "startTime": "04:30"}}}}); err == nil {
		t.Error("a start time not on the hour was accepted")
	}
	call("POST", "regions/us-central1/resourcePolicies", nil, obj{"name": "daily",
		"snapshotSchedulePolicy": obj{"schedule": obj{"dailySchedule": obj{"daysInCycle": 1.0, "startTime": "09:00"}},
			"retentionPolicy":    obj{"maxRetentionDays": 2.0},
			"snapshotProperties": obj{"labels": obj{"auto": "yes"}}}})
	call("POST", "zones/us-central1-a/disks", nil, obj{"name": "data", "sizeGb": "10"})
	call("POST", "zones/us-central1-a/disks/data/addResourcePolicies", nil, obj{"resourcePolicies": []any{"regions/us-central1/resourcePolicies/daily"}})
	if _, err := s.Call("DELETE", "p2139", "regions/us-central1/resourcePolicies/daily", nil, nil); err == nil || !strings.Contains(err.Error(), "already being used by") {
		t.Fatalf("delete while attached: %v", err)
	}
	snaps := func() []obj {
		l := call("GET", "global/snapshots", nil, nil)
		items, _ := l["items"].([]any)
		var out []obj
		for _, e := range items {
			out = append(out, e.(map[string]any))
		}
		return out
	}
	run := func(to time.Time) {
		s.mu.Lock()
		from := *now
		*now = to
		s.runSchedulesLocked(from, to)
		s.mu.Unlock()
		s.waitOp(context.Background(), "")
		waitIdle(t, s)
	}
	run(now.Add(10 * time.Minute)) // 08:40: not yet due
	if l := snaps(); len(l) != 0 {
		t.Fatalf("snapshots before the start time: %v", l)
	}
	run(now.Add(30 * time.Minute)) // 09:10
	l := snaps()
	if len(l) != 1 || l[0]["autoCreated"] != true || !strings.HasPrefix(str(l[0], "name"), "data-us-central1-a-20261122") ||
		!strings.HasSuffix(str(l[0], "sourceSnapshotSchedulePolicy"), "/resourcePolicies/daily") {
		t.Fatalf("scheduled snapshots = %v", l)
	}
	if lb, _ := l[0]["labels"].(map[string]any); lb["auto"] != "yes" {
		t.Errorf("the schedule's labels: %v", l[0]["labels"])
	}
	run(now.Add(time.Hour)) // 10:10: the same day's, already taken
	if l := snaps(); len(l) != 1 {
		t.Fatalf("a second snapshot the same day: %d", len(l))
	}
	run(now.Add(23 * time.Hour)) // the next day 09:10
	run(now.Add(24 * time.Hour)) // and the next: the first is past 2 days
	l = snaps()
	if len(l) != 2 {
		t.Fatalf("after the retention, %d snapshots: %v", len(l), l)
	}
	call("POST", "zones/us-central1-a/disks/data/removeResourcePolicies", nil, obj{"resourcePolicies": []any{"regions/us-central1/resourcePolicies/daily"}})
	call("DELETE", "regions/us-central1/resourcePolicies/daily", nil, nil)
}

// TestInstanceSchedulesStartAndStop (#2139): an instance schedule's stop
// and start cron times stop and start its instance.
func TestInstanceSchedulesStartAndStop(t *testing.T) {
	s, now, call := parityFixture(t)
	if _, err := s.Call("POST", "p2139", "regions/us-central1/resourcePolicies", nil, obj{"name": "bad",
		"instanceSchedulePolicy": obj{"vmStartSchedule": obj{"schedule": "61 * * * *"}, "timeZone": "UTC"}}); err == nil {
		t.Error("a bad cron was accepted")
	}
	call("POST", "regions/us-central1/resourcePolicies", nil, obj{"name": "office",
		"instanceSchedulePolicy": obj{"vmStartSchedule": obj{"schedule": "0 9 * * *"}, "vmStopSchedule": obj{"schedule": "45 8 * * *"},
			"timeZone": "UTC"}})
	call("POST", "zones/us-central1-a/instances", nil, vmBody("worker", obj{"resourcePolicies": []any{"regions/us-central1/resourcePolicies/office"}}))
	status := func() string { return str(call("GET", "zones/us-central1-a/instances/worker", nil, nil), "status") }
	if st := status(); st != stRunning {
		t.Fatalf("worker is %s", st)
	}
	tick := func(to time.Time) {
		s.mu.Lock()
		from := *now
		*now = to
		s.runSchedulesLocked(from, to)
		s.mu.Unlock()
		s.waitOp(context.Background(), "")
		waitIdle(t, s)
	}
	tick(now.Add(16 * time.Minute)) // 08:46
	if st := status(); st != stTerminated {
		t.Fatalf("after the stop time worker is %s", st)
	}
	tick(now.Add(15 * time.Minute)) // 09:01
	if st := status(); st != stRunning {
		t.Fatalf("after the start time worker is %s", st)
	}
	if _, err := s.Call("DELETE", "p2139", "regions/us-central1/resourcePolicies/office", nil, nil); err == nil {
		t.Error("an instance schedule in use was deleted")
	}
}

// TestTemplatesMachineImagesAndGroups (#2139): an instance template, a VM
// from it, a Spot template refused with automatic restart, a machine image
// of the VM and a VM from it, an unmanaged group, a managed group of
// three, resized to one, recreated, and deleted with its VMs.
func TestTemplatesMachineImagesAndGroups(t *testing.T) {
	ctx := context.Background()
	s, _, call := parityFixture(t)
	call("POST", "global/instanceTemplates", nil, obj{"name": "web", "properties": obj{"machineType": "e2-small",
		"disks":    []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"tags":     obj{"items": []any{"http-server"}},
		"metadata": obj{"items": []any{obj{"key": "role", "value": "web"}}}}})
	if _, err := s.Call("POST", "p2139", "global/instanceTemplates", nil, obj{"name": "spot", "properties": obj{"machineType": "e2-small",
		"disks":      []any{map[string]any{"boot": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"scheduling": obj{"provisioningModel": "SPOT", "automaticRestart": true}}}); err == nil {
		t.Error("a Spot template that restarts automatically was accepted")
	}
	call("POST", "zones/us-central1-a/instances", map[string][]string{"sourceInstanceTemplate": {"global/instanceTemplates/web"}}, obj{"name": "w1"})
	w1 := call("GET", "zones/us-central1-a/instances/w1", nil, nil)
	tags, _ := w1["tags"].(map[string]any)
	if !strings.HasSuffix(str(w1, "machineType"), "/e2-small") || !strings.HasSuffix(str(w1, "sourceInstanceTemplate"), "/global/instanceTemplates/web") ||
		len(listOf(tags["items"])) != 1 {
		t.Fatalf("w1 from the template = %v", w1)
	}
	call("POST", "global/machineImages", nil, obj{"name": "w1-image", "sourceInstance": "zones/us-central1-a/instances/w1"})
	mi := call("GET", "global/machineImages/w1-image", nil, nil)
	if mi["status"] != "READY" || len(listOf(mi["savedDisks"])) != 1 {
		t.Fatalf("machine image = %v", mi)
	}
	call("POST", "zones/us-central1-b/instances", nil, obj{"name": "w2", "sourceMachineImage": "global/machineImages/w1-image"})
	s.mu.Lock()
	w2 := s.res["projects/p2139/zones/us-central1-b/instances/w2"]
	spec := s.vmSpecLocked(w2)
	s.mu.Unlock()
	if !strings.HasPrefix(spec.BootDisk.SourceSnapshot, "mi-") || spec.Image.Family != "debian-12" || !strings.HasSuffix(str(w2, "sourceMachineImage"), "/w1-image") {
		t.Fatalf("w2 from the machine image: %+v %v", spec, w2)
	}

	// Unmanaged group.
	call("POST", "zones/us-central1-a/instanceGroups", nil, obj{"name": "ug"})
	call("POST", "zones/us-central1-a/instanceGroups/ug/addInstances", nil, obj{"instances": []any{obj{"instance": "zones/us-central1-a/instances/w1"}}})
	if _, err := s.Call("POST", "p2139", "zones/us-central1-a/instanceGroups/ug/addInstances", nil, obj{"instances": []any{obj{"instance": "zones/us-central1-b/instances/w2"}}}); err == nil {
		t.Error("an instance of another zone joined the group")
	}
	li := call("POST", "zones/us-central1-a/instanceGroups/ug/listInstances", nil, obj{})
	if items := listOf(li["items"]); len(items) != 1 || call("GET", "zones/us-central1-a/instanceGroups/ug", nil, nil)["size"] != 1.0 {
		t.Fatalf("listInstances = %v", li)
	}

	// Managed group through the Go client.
	_, opts := newTestServerFrom(t, s)
	igms, err := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	op, err := igms.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: "p2139", Zone: "us-central1-a",
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String("mig"), BaseInstanceName: proto.String("mig"),
			InstanceTemplate: proto.String("global/instanceTemplates/web"), TargetSize: proto.Int32(3)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	waitIdle(t, s)
	m, err := igms.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: "p2139", Zone: "us-central1-a", InstanceGroupManager: "mig"})
	if err != nil || m.GetTargetSize() != 3 || !m.GetStatus().GetIsStable() || !strings.HasSuffix(m.GetInstanceGroup(), "/instanceGroups/mig") {
		t.Fatalf("mig = %v, %v", m, err)
	}
	it := igms.ListManagedInstances(ctx, &computepb.ListManagedInstancesInstanceGroupManagersRequest{Project: "p2139", Zone: "us-central1-a", InstanceGroupManager: "mig"})
	var names []string
	for {
		mi, err := it.Next()
		if err != nil {
			break
		}
		names = append(names, mi.GetInstance())
	}
	if len(names) != 3 || !strings.Contains(names[0], "/instances/mig-") {
		t.Fatalf("managed instances = %v", names)
	}
	if _, err := s.Call("DELETE", "p2139", "global/instanceTemplates/web", nil, nil); err == nil {
		t.Error("a template the group uses was deleted")
	}
	op, err = igms.RecreateInstances(ctx, &computepb.RecreateInstancesInstanceGroupManagerRequest{Project: "p2139", Zone: "us-central1-a", InstanceGroupManager: "mig",
		InstanceGroupManagersRecreateInstancesRequestResource: &computepb.InstanceGroupManagersRecreateInstancesRequest{Instances: names[:1]}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	waitIdle(t, s)
	op, err = igms.Resize(ctx, &computepb.ResizeInstanceGroupManagerRequest{Project: "p2139", Zone: "us-central1-a", InstanceGroupManager: "mig", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	waitIdle(t, s)
	s.mu.Lock()
	left := s.managedLocked("projects/p2139/zones/us-central1-a/instanceGroupManagers/mig")
	s.mu.Unlock()
	if len(left) != 1 {
		t.Fatalf("after resize to 1: %v", left)
	}
	op, err = igms.Delete(ctx, &computepb.DeleteInstanceGroupManagerRequest{Project: "p2139", Zone: "us-central1-a", InstanceGroupManager: "mig"})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	waitIdle(t, s)
	s.mu.Lock()
	_, vmLeft := s.res[left[0]]
	_, grp := s.res["projects/p2139/zones/us-central1-a/instanceGroups/mig"]
	s.mu.Unlock()
	if vmLeft || grp {
		t.Errorf("after Delete: VM left %v, group left %v", vmLeft, grp)
	}
	call("DELETE", "global/instanceTemplates/web", nil, nil)
}

// TestSpotScheduling (#2139): a Spot VM's scheduling, and the refusals.
func TestSpotScheduling(t *testing.T) {
	s, _, call := parityFixture(t)
	call("POST", "zones/us-central1-a/instances", nil, vmBody("spot", obj{"scheduling": obj{"provisioningModel": "SPOT", "instanceTerminationAction": "DELETE"}}))
	sc, _ := call("GET", "zones/us-central1-a/instances/spot", nil, nil)["scheduling"].(map[string]any)
	if sc["provisioningModel"] != "SPOT" || sc["instanceTerminationAction"] != "DELETE" || sc["automaticRestart"] != false || sc["onHostMaintenance"] != "TERMINATE" {
		t.Fatalf("spot scheduling = %v", sc)
	}
	for name, sched := range map[string]obj{
		"a bad model":                    {"provisioningModel": "CHEAP"},
		"a bad termination action":       {"provisioningModel": "SPOT", "instanceTerminationAction": "PAUSE"},
		"a standard VM's termination":    {"instanceTerminationAction": "STOP"},
		"a Spot VM that live-migrates":   {"provisioningModel": "SPOT", "onHostMaintenance": "MIGRATE"},
		"a Spot VM that restarts itself": {"provisioningModel": "SPOT", "automaticRestart": true},
	} {
		if _, err := s.Call("POST", "p2139", "zones/us-central1-a/instances", nil, vmBody("x", obj{"scheduling": sched})); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// newTestServerFrom serves s over HTTP for the Go client.
func newTestServerFrom(t *testing.T, s *Server) (*Server, []option.ClientOption) {
	t.Helper()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, []option.ClientOption{option.WithEndpoint(ts.URL), option.WithoutAuthentication()}
}
