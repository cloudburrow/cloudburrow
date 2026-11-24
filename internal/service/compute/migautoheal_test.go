package compute

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// healLB reaches every VM at a server that answers /healthz with 200, or,
// for the VMs named in bad, at one that answers 500.
type healLB struct {
	mu        sync.Mutex
	bad       map[string]bool
	good, ill string
}

func (h *healLB) InstanceAddr(_ context.Context, vm VMSpec, port int) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if port != 80 {
		return "", fmt.Errorf("no %s:%d", vm.Name, port)
	}
	if h.bad[vm.Name] || h.bad["*"] {
		return h.ill, nil
	}
	return h.good, nil
}
func (h *healLB) Ingress() string           { return "" }
func (h *healLB) LocalAPI(api string) string { return "" }

func (h *healLB) setBad(name string, bad bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bad[name] = bad
}

type healClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *healClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *healClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func autohealFixture(t *testing.T) (*Server, *healLB, *healClock, func(method, path string, body obj) obj) {
	t.Helper()
	srv := func(code int) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(s.Close)
		return s.Listener.Addr().String()
	}
	lb := &healLB{bad: map[string]bool{}, good: srv(200), ill: srv(500)}
	clock := &healClock{now: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)}
	s, err := New(Options{VMs: fakeVMs{}, LB: lb, Now: clock.Now, UpdateEvery: 100 * time.Millisecond, MIGRetryBase: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	call := func(method, path string, body obj) obj { t.Helper(); return lbCall(t, s, method, path, body) }
	call("POST", "global/firewalls", obj{"name": "allow-hc", "network": "global/networks/default",
		"sourceRanges": []any{"35.191.0.0/16", "130.211.0.0/22"}, "allowed": []any{obj{"IPProtocol": "tcp", "ports": []any{"80"}}}})
	call("POST", "global/healthChecks", obj{"name": "hc", "type": "HTTP", "httpHealthCheck": obj{"port": 80, "requestPath": "/healthz"},
		"checkIntervalSec": 1, "timeoutSec": 1, "healthyThreshold": 1, "unhealthyThreshold": 1})
	call("POST", "global/instanceTemplates", obj{"name": "t", "properties": obj{"machineType": "e2-small",
		"disks":             []any{obj{"boot": true, "autoDelete": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}}}})
	return s, lb, clock, call
}

func managedOf(call func(method, path string, body obj) obj, igm string) []obj {
	var out []obj
	for _, e := range listOf(call("POST", igm+"/listManagedInstances", nil)["managedInstances"]) {
		out = append(out, objOf(e))
	}
	return out
}

func healthOf(mi obj) string {
	ih := listOf(mi["instanceHealth"])
	if len(ih) == 0 {
		return ""
	}
	return str(objOf(ih[0]), "detailedHealthState")
}

// TestMIGAutohealingPolicyValidation: one policy, an existing health check
// of the group's region, initialDelaySec 0-3600; patch replaces or clears
// it; a health check a group autoheals with cannot be deleted.
func TestMIGAutohealingPolicyValidation(t *testing.T) {
	s, _, _, call := autohealFixture(t)
	z := "zones/us-central1-a/instanceGroupManagers"
	bad := []struct {
		policies []any
		want     string
	}{
		{[]any{obj{"healthCheck": "global/healthChecks/hc"}, obj{"healthCheck": "global/healthChecks/hc"}}, "only one autohealing policy"},
		{[]any{obj{"healthCheck": "global/healthChecks/nope"}}, "not found"},
		{[]any{obj{"initialDelaySec": 10}}, "healthCheck"},
		{[]any{obj{"healthCheck": "global/healthChecks/hc", "initialDelaySec": 3601}}, "initialDelaySec"},
		{[]any{obj{"healthCheck": "global/healthChecks/hc", "initialDelaySec": -1}}, "initialDelaySec"},
	}
	for _, c := range bad {
		err := tryCall(t, s, "POST", z, obj{"name": "g", "instanceTemplate": "global/instanceTemplates/t", "targetSize": 0, "autoHealingPolicies": c.policies})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want an error with %q", c.policies, err, c.want)
		}
	}
	call("POST", "regions/europe-west1/healthChecks", obj{"name": "far", "type": "TCP", "tcpHealthCheck": obj{"port": 80}})
	if err := tryCall(t, s, "POST", z, obj{"name": "g", "instanceTemplate": "global/instanceTemplates/t",
		"autoHealingPolicies": []any{obj{"healthCheck": "regions/europe-west1/healthChecks/far"}}}); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("a health check of another region: %v", err)
	}
	call("POST", z, obj{"name": "g", "instanceTemplate": "global/instanceTemplates/t", "targetSize": 0,
		"autoHealingPolicies": []any{obj{"healthCheck": "global/healthChecks/hc", "initialDelaySec": 120}}})
	g := call("GET", z+"/g", nil)
	p := objOf(listOf(g["autoHealingPolicies"])[0])
	if !strings.HasSuffix(str(p, "healthCheck"), "projects/p/global/healthChecks/hc") || p["initialDelaySec"] != 120.0 {
		t.Fatalf("autoHealingPolicies: %v", g["autoHealingPolicies"])
	}
	if err := tryCall(t, s, "DELETE", "global/healthChecks/hc", nil); err == nil || !strings.Contains(err.Error(), "instanceGroupManagers/g") {
		t.Fatalf("deleting a health check in use: %v", err)
	}
	call("PATCH", z+"/g", obj{"autoHealingPolicies": []any{obj{"healthCheck": "global/healthChecks/hc", "initialDelaySec": 0}}})
	if p := objOf(listOf(call("GET", z+"/g", nil)["autoHealingPolicies"])[0]); p["initialDelaySec"] != 0.0 {
		t.Fatalf("patched: %v", p)
	}
	call("PATCH", z+"/g", obj{"autoHealingPolicies": []any{}})
	if g := call("GET", z+"/g", nil); g["autoHealingPolicies"] != nil {
		t.Fatalf("cleared: %v", g["autoHealingPolicies"])
	}
	call("DELETE", "global/healthChecks/hc", nil)
}

func tryCall(t *testing.T, s *Server, method, path string, body obj) error {
	_, err := vmCall(t, s, method, path, jsonBody(body))
	return err
}

// TestMIGAutohealingRecreatesUnhealthyVMs: a VM failing its health check
// is VERIFYING through its initial delay, then recreated (RECREATING, by
// the same name), within maxUnavailable; HEALTHY ones are left alone, and
// the group is stable once every VM is HEALTHY.
func TestMIGAutohealingRecreatesUnhealthyVMs(t *testing.T) {
	_, lb, clock, call := autohealFixture(t)
	igm := "zones/us-central1-a/instanceGroupManagers/web"
	lb.setBad("*", true)
	call("POST", "zones/us-central1-a/instanceGroupManagers", obj{"name": "web", "instanceTemplate": "global/instanceTemplates/t", "targetSize": 2,
		"autoHealingPolicies": []any{obj{"healthCheck": "global/healthChecks/hc", "initialDelaySec": 60}}})
	waitFor(t, "both VMs verifying and unhealthy", func() bool {
		mis := managedOf(call, igm)
		return len(mis) == 2 && str(mis[0], "currentAction") == "VERIFYING" && str(mis[1], "currentAction") == "VERIFYING" &&
			healthOf(mis[0]) == "UNHEALTHY" && healthOf(mis[1]) == "UNHEALTHY"
	})
	g := call("GET", igm, nil)
	if objOf(g["currentActions"])["verifying"] != 2.0 || objOf(g["status"])["isStable"] != false {
		t.Fatalf("verifying group: %v %v", g["currentActions"], g["status"])
	}
	// No recreation within the initial delay.
	time.Sleep(500 * time.Millisecond)
	ids := map[string]any{}
	for _, mi := range managedOf(call, igm) {
		if str(mi, "currentAction") != "VERIFYING" {
			t.Fatalf("recreated within the initial delay: %v", mi)
		}
		ids[str(mi, "name")] = mi["id"]
	}
	// The delay over, one VM at a time (maxUnavailable 1, one zone) is
	// recreated by the same name; the one recreated is VERIFYING again,
	// so unavailable, and the other waits.
	clock.add(61 * time.Second)
	changed := func() int {
		n := 0
		for _, mi := range managedOf(call, igm) {
			if mi["id"] != ids[str(mi, "name")] {
				n++
			}
		}
		return n
	}
	waitFor(t, "one VM recreated", func() bool { return changed() == 1 })
	time.Sleep(time.Second)
	if n := changed(); n != 1 {
		t.Fatalf("%d VMs recreated at once, beyond maxUnavailable 1", n)
	}
	lb.setBad("*", false)
	waitFor(t, "every VM healthy and the group stable", func() bool {
		mis := managedOf(call, igm)
		if len(mis) != 2 {
			return false
		}
		for _, mi := range mis {
			if str(mi, "currentAction") != "NONE" || healthOf(mi) != "HEALTHY" || ids[str(mi, "name")] == nil {
				return false
			}
		}
		return objOf(call("GET", igm, nil)["status"])["isStable"] == true
	})
	recreated := 0
	for _, mi := range managedOf(call, igm) {
		if mi["id"] != ids[str(mi, "name")] {
			recreated++
		}
	}
	if recreated == 0 {
		t.Fatal("no VM was recreated")
	}
	// A healthy VM that turns unhealthy, its delay passed, is recreated
	// at once.
	name := str(managedOf(call, igm)[0], "name")
	before := managedOf(call, igm)[0]["id"]
	lb.setBad(name, true)
	waitFor(t, "the ill VM recreated", func() bool {
		for _, mi := range managedOf(call, igm) {
			if str(mi, "name") == name && mi["id"] != before && mi["id"] != nil {
				return true
			}
		}
		return false
	})
	lb.setBad(name, false)
}
