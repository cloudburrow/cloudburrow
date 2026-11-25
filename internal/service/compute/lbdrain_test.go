package compute

import (
	"context"
	"strings"
	"testing"
	"time"
)

// groupHealth is getHealth's healthState by instance name.
func groupHealth(call func(method, path string, body obj) obj, bs, group string) map[string]string {
	out := map[string]string{}
	for _, e := range listOf(call("POST", bs+"/getHealth", obj{"group": group})["healthStatus"]) {
		h := objOf(e)
		out[lastSeg(str(h, "instance"))] = str(h, "healthState")
	}
	return out
}

func managedByName(call func(method, path string, body obj) obj, igm string) map[string]obj {
	out := map[string]obj{}
	for _, mi := range managedOf(call, igm) {
		out[str(mi, "name")] = mi
	}
	return out
}

// TestConnectionDrainingValidation: drainingTimeoutSec is 0 to 3600,
// 0 by default.
func TestConnectionDrainingValidation(t *testing.T) {
	s, _, _, call := autohealFixture(t)
	for _, v := range []any{-1, 3601, 1.5} {
		err := tryCall(t, s, "POST", "global/backendServices", obj{"name": "b", "connectionDraining": obj{"drainingTimeoutSec": v}})
		if err == nil || !strings.Contains(err.Error(), "drainingTimeoutSec") {
			t.Errorf("%v: %v", v, err)
		}
	}
	call("POST", "global/backendServices", obj{"name": "b"})
	if cd := objOf(call("GET", "global/backendServices/b", nil)["connectionDraining"]); cd["drainingTimeoutSec"] != 0.0 {
		t.Fatalf("default: %v", cd)
	}
	call("PATCH", "global/backendServices/b", obj{"connectionDraining": obj{"drainingTimeoutSec": 3600}})
	if cd := objOf(call("GET", "global/backendServices/b", nil)["connectionDraining"]); cd["drainingTimeoutSec"] != 3600.0 {
		t.Fatalf("patched: %v", cd)
	}
}

// TestConnectionDrainingManagedVMs: a VM its group deletes or recreates
// drains for the backend service's timeout first: DELETING or RECREATING,
// still RUNNING, DRAINING in getHealth and instanceHealth, out of new
// requests; then the group deletes or recreates it. A backend service of
// timeout 0 drops it at once.
func TestConnectionDrainingManagedVMs(t *testing.T) {
	s, _, clock, call := autohealFixture(t)
	z := "zones/us-central1-a/instanceGroupManagers"
	igm := z + "/web"
	call("POST", z, obj{"name": "web", "instanceTemplate": "global/instanceTemplates/t", "targetSize": 3,
		"namedPorts":          []any{obj{"name": "http", "port": 80}},
		"autoHealingPolicies": []any{obj{"healthCheck": "global/healthChecks/hc", "initialDelaySec": 300}}})
	group := "zones/us-central1-a/instanceGroups/web"
	call("POST", "global/backendServices", obj{"name": "bs", "loadBalancingScheme": "EXTERNAL_MANAGED", "portName": "http",
		"healthChecks": []any{"global/healthChecks/hc"}, "connectionDraining": obj{"drainingTimeoutSec": 30},
		"backends": []any{obj{"group": group}}})
	call("POST", "global/backendServices", obj{"name": "nodrain", "loadBalancingScheme": "EXTERNAL_MANAGED", "portName": "http",
		"healthChecks": []any{"global/healthChecks/hc"}, "backends": []any{obj{"group": group}}})
	bs, nodrain := "global/backendServices/bs", "global/backendServices/nodrain"
	waitFor(t, "three healthy VMs", func() bool {
		h := groupHealth(call, bs, group)
		if len(h) != 3 {
			return false
		}
		for _, v := range h {
			if v != "HEALTHY" {
				return false
			}
		}
		return true
	})
	var names []string
	for n := range managedByName(call, igm) {
		names = append(names, n)
	}
	gone, again := names[0], names[1]

	// deleteInstances: drains first.
	call("POST", igm+"/deleteInstances", obj{"instances": []any{"zones/us-central1-a/instances/" + gone}})
	mi := managedByName(call, igm)[gone]
	if str(mi, "currentAction") != "DELETING" || str(mi, "instanceStatus") != stRunning || healthOf(mi) != "DRAINING" {
		t.Fatalf("draining VM: %v", mi)
	}
	if h := groupHealth(call, bs, group); h[gone] != "UNHEALTHY" {
		t.Fatalf("getHealth while draining: %v", h)
	}
	if h := groupHealth(call, nodrain, group); h[gone] != "" {
		t.Fatalf("a backend service of timeout 0 still has the VM: %v", h)
	}
	s.mu.Lock()
	set, _ := s.backendSetLocked("projects/p/" + bs)
	s.mu.Unlock()
	for i := 0; i < 20; i++ {
		ep, ok := s.lb.bal.pick(set, "", nil, false)
		if !ok || lastSeg(ep.instance) == gone {
			t.Fatalf("a new request went to %v (%v)", ep.instance, ok)
		}
		s.lb.bal.done(ep)
	}
	if g := call("GET", igm, nil); objOf(g["currentActions"])["deleting"] != 1.0 || g["targetSize"] != 2.0 {
		t.Fatalf("group while draining: %v %v", g["currentActions"], g["targetSize"])
	}
	time.Sleep(500 * time.Millisecond)
	if _, ok := managedByName(call, igm)[gone]; !ok {
		t.Fatal("deleted before its drain ended")
	}
	clock.add(31 * time.Second)
	waitFor(t, "the drained VM deleted", func() bool {
		_, ok := managedByName(call, igm)[gone]
		return !ok
	})

	// recreateInstances: drains, then is recreated by the same name.
	id := managedByName(call, igm)[again]["id"]
	call("POST", igm+"/recreateInstances", obj{"instances": []any{"zones/us-central1-a/instances/" + again}})
	mi = managedByName(call, igm)[again]
	if str(mi, "currentAction") != "RECREATING" || healthOf(mi) != "DRAINING" || mi["id"] != id {
		t.Fatalf("recreating VM: %v", mi)
	}
	clock.add(31 * time.Second)
	waitFor(t, "the drained VM recreated and healthy", func() bool {
		mi := managedByName(call, igm)[again]
		return mi != nil && str(mi, "currentAction") == "NONE" && healthOf(mi) == "HEALTHY"
	})
	if g := call("GET", igm, nil); g["targetSize"] != 2.0 {
		t.Fatalf("size: %v", g["targetSize"])
	}
}

// TestConnectionDrainingUnmanagedGroup: removeInstances leaves the VM a
// member, DRAINING, until the timeout passes.
func TestConnectionDrainingUnmanagedGroup(t *testing.T) {
	_, _, clock, call := autohealFixture(t)
	call("POST", "zones/us-central1-a/instances", obj{"name": "vm1", "machineType": "zones/us-central1-a/machineTypes/e2-small",
		"disks":             []any{obj{"boot": true, "autoDelete": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}}})
	waitFor(t, "vm1 running", func() bool { return str(call("GET", "zones/us-central1-a/instances/vm1", nil), "status") == stRunning })
	call("POST", "zones/us-central1-a/instanceGroups", obj{"name": "ug", "namedPorts": []any{obj{"name": "http", "port": 80}}})
	group := "zones/us-central1-a/instanceGroups/ug"
	call("POST", group+"/addInstances", obj{"instances": []any{obj{"instance": "zones/us-central1-a/instances/vm1"}}})
	call("POST", "global/backendServices", obj{"name": "bs", "loadBalancingScheme": "EXTERNAL_MANAGED", "portName": "http",
		"healthChecks": []any{"global/healthChecks/hc"}, "connectionDraining": obj{"drainingTimeoutSec": 10},
		"backends": []any{obj{"group": group}}})
	bs := "global/backendServices/bs"
	waitFor(t, "vm1 healthy", func() bool { return groupHealth(call, bs, group)["vm1"] == "HEALTHY" })
	call("POST", group+"/removeInstances", obj{"instances": []any{obj{"instance": "zones/us-central1-a/instances/vm1"}}})
	if h := groupHealth(call, bs, group); h["vm1"] != "UNHEALTHY" {
		t.Fatalf("getHealth: %v", h)
	}
	members := func() int {
		return len(listOf(call("POST", group+"/listInstances", obj{})["items"]))
	}
	if n := members(); n != 1 {
		t.Fatalf("%d members while draining", n)
	}
	clock.add(11 * time.Second)
	waitFor(t, "vm1 removed", func() bool { return members() == 0 })
	if h := groupHealth(call, bs, group); len(h) != 0 {
		t.Fatalf("getHealth after the drain: %v", h)
	}
}

// TestDrainCutsRequestsInFlight: cut cancels the requests tracked to a VM,
// of one backend service or of every one.
func TestDrainCutsRequestsInFlight(t *testing.T) {
	var b lbBalanceState
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	endA := b.track("vm", "bs1", cancelA)
	endB := b.track("vm", "bs2", cancelB)
	b.cut("vm", "bs1")
	if ctxA.Err() == nil || ctxB.Err() != nil {
		t.Fatalf("cut of bs1: %v %v", ctxA.Err(), ctxB.Err())
	}
	b.cut("vm", "")
	if ctxB.Err() == nil {
		t.Fatal("cut of every backend service left bs2's request")
	}
	endA()
	endB()
	if len(b.flight.reqs) != 0 {
		t.Fatalf("left tracked: %v", b.flight.reqs)
	}
}

// TestConnectionDrainingNetworkEndpoints: an endpoint detached from a
// zonal network endpoint group drains: still listed, its health DRAINING
// in listNetworkEndpoints, no new request; gone once the timeout passes.
func TestConnectionDrainingNetworkEndpoints(t *testing.T) {
	s, _, clock, call := autohealFixture(t)
	call("POST", "zones/us-central1-a/instances", obj{"name": "vm1", "machineType": "zones/us-central1-a/machineTypes/e2-small",
		"disks":             []any{obj{"boot": true, "autoDelete": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}}})
	waitFor(t, "vm1 running", func() bool { return str(call("GET", "zones/us-central1-a/instances/vm1", nil), "status") == stRunning })
	neg := "zones/us-central1-a/networkEndpointGroups/neg"
	call("POST", "zones/us-central1-a/networkEndpointGroups", obj{"name": "neg", "networkEndpointType": "GCE_VM_IP_PORT",
		"network": "global/networks/default", "defaultPort": 80})
	ep := obj{"instance": "vm1", "port": 80}
	call("POST", neg+"/attachNetworkEndpoints", obj{"networkEndpoints": []any{ep}})
	call("POST", "global/backendServices", obj{"name": "bs", "loadBalancingScheme": "EXTERNAL_MANAGED",
		"healthChecks": []any{"global/healthChecks/hc"}, "connectionDraining": obj{"drainingTimeoutSec": 20},
		"backends": []any{obj{"group": neg, "balancingMode": "RATE", "maxRatePerEndpoint": 100}}})
	health := func() []string {
		var out []string
		for _, e := range listOf(call("POST", neg+"/listNetworkEndpoints", obj{"healthStatus": "SHOW"})["items"]) {
			for _, h := range listOf(objOf(e)["healths"]) {
				out = append(out, str(objOf(h), "healthState"))
			}
		}
		return out
	}
	waitFor(t, "the endpoint healthy", func() bool { h := health(); return len(h) == 1 && h[0] == "HEALTHY" })
	call("POST", neg+"/detachNetworkEndpoints", obj{"networkEndpoints": []any{ep}})
	if h := health(); len(h) != 1 || h[0] != "DRAINING" {
		t.Fatalf("the detached endpoint's health: %v", h)
	}
	s.mu.Lock()
	set, _ := s.backendSetLocked("projects/p/global/backendServices/bs")
	s.mu.Unlock()
	if _, ok := s.lb.bal.pick(set, "", nil, false); ok {
		t.Fatal("a new request went to the draining endpoint")
	}
	clock.add(21 * time.Second)
	waitFor(t, "the endpoint gone", func() bool {
		return len(listOf(call("POST", neg+"/listNetworkEndpoints", obj{})["items"])) == 0
	})
}

// TestSameFingerprint: a fingerprint sent base64url-encoded, as gcloud
// sends bytes fields, matches the standard encoding the server wrote.
func TestSameFingerprint(t *testing.T) {
	if !sameFingerprint("a-_bcdefgh8=", "a+/bcdefgh8=") || !sameFingerprint("abc=", "abc=") || sameFingerprint("a-_bcdefgh8=", "a+/bcdefgh9=") {
		t.Fatal("sameFingerprint")
	}
}
