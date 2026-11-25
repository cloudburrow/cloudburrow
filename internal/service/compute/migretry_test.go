package compute

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// flakyVMs is a VM backend whose starts fail while fail is set.
type flakyVMs struct {
	fakeVMs
	fail *atomic.Bool
}

func (f flakyVMs) Start(ctx context.Context, vm VMSpec) error {
	if f.fail.Load() {
		return errors.New("ZONE_RESOURCE_POOL_EXHAUSTED: the zone does not have enough resources available to fulfill the request")
	}
	return nil
}

func migRetryFixture(t *testing.T) (*Server, *time.Time, *atomic.Bool, func(method, path string, q map[string][]string, body obj) obj) {
	t.Helper()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	fail := &atomic.Bool{}
	s, err := New(Options{VMs: flakyVMs{fail: fail}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, q map[string][]string, body obj) obj {
		t.Helper()
		out, err := s.Call(method, "p2220", path, q, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		if method != http.MethodGet {
			waitIdle(t, s)
		}
		return out
	}
	return s, &now, fail, call
}

// retryPass runs one pass of the retry loop and waits for what it began.
func retryPass(t *testing.T, s *Server) {
	t.Helper()
	s.rollOnce()
	waitIdle(t, s)
	// The goroutine that sees an attempt's outcome takes the lock after
	// its operation is done.
	time.Sleep(50 * time.Millisecond)
}

// TestMIGRetriesFailedCreations (#2220): a group whose template's network
// is gone keeps its VMs pending, CREATING with lastAttempt errors and
// status.isStable false, records the failures after the immediate retry
// for listErrors, and makes the VMs, under the same names, once the
// network is back.
func TestMIGRetriesFailedCreations(t *testing.T) {
	ctx := context.Background()
	s, now, _, call := migRetryFixture(t)
	call("POST", "global/networks", nil, obj{"name": "nw", "autoCreateSubnetworks": true})
	call("POST", "global/instanceTemplates", nil, obj{"name": "t", "properties": obj{"machineType": "e2-small",
		"disks":             []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{map[string]any{"network": "global/networks/nw"}}}})
	call("DELETE", "global/networks/nw", nil, nil)

	_, opts := newTestServerFrom(t, s)
	igms, err := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	op, err := igms.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: "p2220", Zone: "us-central1-a",
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String("mig"), BaseInstanceName: proto.String("mig"),
			InstanceTemplate: proto.String("global/instanceTemplates/t"), TargetSize: proto.Int32(2)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("insert failed although its VMs are retried: %v", err)
	}
	waitIdle(t, s)
	list := func() []*computepb.ManagedInstance {
		var out []*computepb.ManagedInstance
		it := igms.ListManagedInstances(ctx, &computepb.ListManagedInstancesInstanceGroupManagersRequest{Project: "p2220", Zone: "us-central1-a", InstanceGroupManager: "mig"})
		for {
			mi, err := it.Next()
			if err != nil {
				return out
			}
			out = append(out, mi)
		}
	}
	get := func() *computepb.InstanceGroupManager {
		m, err := igms.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: "p2220", Zone: "us-central1-a", InstanceGroupManager: "mig"})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	mis := list()
	if len(mis) != 2 {
		t.Fatalf("managed instances = %v", mis)
	}
	names := map[string]bool{}
	for _, mi := range mis {
		names[mi.GetInstance()] = true
		errs := mi.GetLastAttempt().GetErrors().GetErrors()
		if mi.GetCurrentAction() != "CREATING" || mi.Id != nil || mi.GetInstanceStatus() != "" || len(errs) != 1 ||
			errs[0].GetCode() != "RESOURCE_NOT_FOUND" || !strings.Contains(errs[0].GetMessage(), "networks/nw") ||
			!strings.Contains(mi.GetInstance(), "/zones/us-central1-a/instances/mig-") || !strings.HasSuffix(mi.GetVersion().GetInstanceTemplate(), "/instanceTemplates/t") {
			t.Fatalf("pending instance = %v", mi)
		}
	}
	m := get()
	if m.GetStatus().GetIsStable() || m.GetCurrentActions().GetCreating() != 2 || m.GetCurrentActions().GetNone() != 0 || m.GetTargetSize() != 2 {
		t.Fatalf("group while failing = %v", m)
	}
	listErrs := func() []*computepb.InstanceManagedByIgmError {
		var out []*computepb.InstanceManagedByIgmError
		it := igms.ListErrors(ctx, &computepb.ListErrorsInstanceGroupManagersRequest{Project: "p2220", Zone: "us-central1-a", InstanceGroupManager: "mig"})
		for {
			e, err := it.Next()
			if err != nil {
				return out
			}
			out = append(out, e)
		}
	}
	if e := listErrs(); len(e) != 0 {
		t.Fatalf("errors recorded before the immediate retry: %v", e)
	}
	retryPass(t, s) // the immediate retry, failing too
	errs := listErrs()
	if len(errs) != 2 {
		t.Fatalf("listErrors after the immediate retry = %v", errs)
	}
	for _, e := range errs {
		d := e.GetInstanceActionDetails()
		if e.GetError().GetCode() != "RESOURCE_NOT_FOUND" || !strings.Contains(e.GetError().GetMessage(), "creation failed: ") ||
			d.GetAction() != "CREATING" || !names[d.GetInstance()] || !strings.HasSuffix(d.GetVersion().GetInstanceTemplate(), "/instanceTemplates/t") || e.GetTimestamp() == "" {
			t.Fatalf("error = %v (names %v)", e, names)
		}
	}
	retryPass(t, s) // backing off: no attempt yet
	if len(listErrs()) != 2 {
		t.Fatal("retried before its backoff")
	}
	*now = now.Add(6 * time.Second)
	retryPass(t, s)
	if len(listErrs()) != 4 {
		t.Fatalf("listErrors after the backoff = %d", len(listErrs()))
	}
	// paging
	it := igms.ListErrors(ctx, &computepb.ListErrorsInstanceGroupManagersRequest{Project: "p2220", Zone: "us-central1-a", InstanceGroupManager: "mig", MaxResults: proto.Uint32(3)})
	if p, err := it.Next(); err != nil || p == nil || it.PageInfo().Token == "" {
		t.Fatalf("paged listErrors: %v %v", p, err)
	}

	call("POST", "global/networks", nil, obj{"name": "nw", "autoCreateSubnetworks": true})
	*now = now.Add(time.Minute)
	retryPass(t, s)
	retryPass(t, s)
	mis = list()
	if len(mis) != 2 {
		t.Fatalf("after the network came back: %v", mis)
	}
	for _, mi := range mis {
		if !names[mi.GetInstance()] || mi.GetInstanceStatus() != "RUNNING" || mi.GetCurrentAction() != "NONE" || mi.LastAttempt != nil || mi.Id == nil {
			t.Fatalf("made instance = %v", mi)
		}
	}
	if m := get(); !m.GetStatus().GetIsStable() || m.GetCurrentActions().GetNone() != 2 {
		t.Fatalf("group once made = %v", m)
	}
}

// TestMIGRetriesFailedStarts (#2220): a VM whose creation fails in its
// operation (the backend refusing it) is retried until it is made; a
// resize down drops a pending VM first; deleteInstances of a pending VM
// stops its creation and lowers the target size.
func TestMIGRetriesFailedStarts(t *testing.T) {
	s, now, fail, call := migRetryFixture(t)
	call("POST", "global/instanceTemplates", nil, obj{"name": "t", "properties": obj{"machineType": "e2-small",
		"disks":             []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{map[string]any{"network": "global/networks/default"}}}})
	call("POST", "zones/us-central1-a/instanceGroupManagers", nil, obj{"name": "g", "baseInstanceName": "g",
		"instanceTemplate": "global/instanceTemplates/t", "targetSize": 1.0})
	fail.Store(true)
	call("POST", "zones/us-central1-a/instanceGroupManagers/g/resize", map[string][]string{"size": {"3"}}, nil)
	time.Sleep(50 * time.Millisecond)
	mis := listOf(call("POST", "zones/us-central1-a/instanceGroupManagers/g/listManagedInstances", nil, obj{})["managedInstances"])
	pending := 0
	for _, e := range mis {
		mi := e.(obj)
		if mi["currentAction"] == "CREATING" && mi["instanceStatus"] == nil {
			la, _ := mi["lastAttempt"].(obj)
			if la == nil {
				t.Fatalf("pending without lastAttempt: %v", mi)
			}
			pending++
		}
	}
	if len(mis) != 3 || pending != 2 {
		t.Fatalf("managed instances = %v", mis)
	}
	// Resize down to 2 drops a pending VM, not the running one.
	call("POST", "zones/us-central1-a/instanceGroupManagers/g/resize", map[string][]string{"size": {"2"}}, nil)
	s.mu.Lock()
	m := s.res["projects/p2220/zones/us-central1-a/instanceGroupManagers/g"]
	left := len(pendingOf(m))
	live := len(s.liveManagedLocked(str(m, "selfLink")))
	var one string
	for vm := range pendingOf(m) {
		one = vm
	}
	s.mu.Unlock()
	if left != 1 || live != 1 {
		t.Fatalf("after resize to 2: %d pending, %d live", left, live)
	}
	// deleteInstances of the pending one.
	call("POST", "zones/us-central1-a/instanceGroupManagers/g/deleteInstances", nil, obj{"instances": []any{one}})
	g := call("GET", "zones/us-central1-a/instanceGroupManagers/g", nil, nil)
	if g["targetSize"] != 1.0 || g["status"].(obj)["isStable"] != true {
		t.Fatalf("after deleting the pending VM: %v", g)
	}
	// Grow again while failing, then let it succeed.
	call("POST", "zones/us-central1-a/instanceGroupManagers/g/resize", map[string][]string{"size": {"2"}}, nil)
	time.Sleep(50 * time.Millisecond)
	if g := call("GET", "zones/us-central1-a/instanceGroupManagers/g", nil, nil); g["status"].(obj)["isStable"] != false {
		t.Fatalf("stable while a VM fails: %v", g)
	}
	fail.Store(false)
	*now = now.Add(time.Minute)
	retryPass(t, s)
	g = call("GET", "zones/us-central1-a/instanceGroupManagers/g", nil, nil)
	if g["status"].(obj)["isStable"] != true || g["currentActions"].(obj)["none"] != 2.0 {
		t.Fatalf("after the retry: %v", g)
	}
	// Deleting the group with nothing pending.
	call("DELETE", "zones/us-central1-a/instanceGroupManagers/g", nil, nil)
}

// TestMIGRecreateRetries (#2220): a VM recreated from another template
// whose creation fails is listed RECREATING with its error, and made once
// the failure clears.
func TestMIGRecreateRetries(t *testing.T) {
	s, now, fail, call := migRetryFixture(t)
	for _, n := range []string{"t1", "t2"} {
		call("POST", "global/instanceTemplates", nil, obj{"name": n, "properties": obj{"machineType": "e2-small",
			"disks":             []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{map[string]any{"network": "global/networks/default"}}}})
	}
	call("POST", "zones/us-central1-a/instanceGroupManagers", nil, obj{"name": "g", "baseInstanceName": "g",
		"instanceTemplate": "global/instanceTemplates/t1", "targetSize": 1.0})
	mis := listOf(call("POST", "zones/us-central1-a/instanceGroupManagers/g/listManagedInstances", nil, obj{})["managedInstances"])
	vm := str(mis[0].(obj), "instance")
	call("POST", "zones/us-central1-a/instanceGroupManagers/g/setInstanceTemplate", nil, obj{"instanceTemplate": "global/instanceTemplates/t2"})
	fail.Store(true)
	call("POST", "zones/us-central1-a/instanceGroupManagers/g/recreateInstances", nil, obj{"instances": []any{vm}})
	time.Sleep(100 * time.Millisecond)
	waitIdle(t, s)
	time.Sleep(50 * time.Millisecond)
	mis = listOf(call("POST", "zones/us-central1-a/instanceGroupManagers/g/listManagedInstances", nil, obj{})["managedInstances"])
	if len(mis) != 1 || mis[0].(obj)["currentAction"] != "RECREATING" || mis[0].(obj)["lastAttempt"] == nil || !strings.HasSuffix(str(mis[0].(obj), "instance"), lastSeg(vm)) {
		t.Fatalf("recreating = %v", mis)
	}
	fail.Store(false)
	*now = now.Add(time.Minute)
	retryPass(t, s)
	mis = listOf(call("POST", "zones/us-central1-a/instanceGroupManagers/g/listManagedInstances", nil, obj{})["managedInstances"])
	if len(mis) != 1 || mis[0].(obj)["currentAction"] != "NONE" || mis[0].(obj)["instanceStatus"] != "RUNNING" ||
		!strings.HasSuffix(str(mis[0].(obj)["version"].(obj), "instanceTemplate"), "/t2") {
		t.Fatalf("recreated = %v", mis)
	}
}
