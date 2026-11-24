package compute

import (
	"strings"
	"testing"
)

// TestConnectorFirewalls (#2140): a connector's six aet- rules are made in
// its network, listed like any rule, let the connector's range and tag
// into the network's VMs, and go with it.
func TestConnectorFirewalls(t *testing.T) {
	s, _ := newTestServer(t, Options{VMs: fakeVMs{}})
	if _, err := s.Call("POST", "fw", "zones/us-central1-a/instances", nil, obj{"name": "vm", "machineType": "zones/us-central1-a/machineTypes/e2-small",
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConnectorFirewalls("projects/fw/global/networks/default", "us-central1", "c", "10.8.0.0/28", true); err != nil {
		t.Fatal(err)
	}
	l, err := s.Call("GET", "fw", "global/firewalls", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range sliceOf(l["items"]) {
		if n := str(objOf(e), "name"); strings.HasPrefix(n, "aet-") {
			names = append(names, n)
		}
	}
	if len(names) != 6 || !strings.Contains(strings.Join(names, " "), "aet-us-central1-c-sbntfw") {
		t.Fatalf("connector rules = %v", names)
	}
	s.mu.Lock()
	inst := s.res["projects/fw/zones/us-central1-a/instances/vm"]
	inst["status"] = stRunning
	conf, _ := s.vpcConfigLocked(inst, "10.244.0.9")
	s.mu.Unlock()
	if !strings.Contains(conf, "10.8.0.0/28") || !strings.Contains(conf, `ip saddr { 10.8.0.0/28 } meta l4proto tcp accept comment "aet-us-central1-c-sbntfw`) {
		t.Fatalf("the VM does not let the connector in:\n%s", conf)
	}
	if err := s.SetConnectorFirewalls("projects/fw/global/networks/default", "us-central1", "c", "10.8.0.0/28", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("GET", "fw", "global/firewalls/aet-us-central1-c-sbntfw", nil, nil); ErrorStatus(err) != 404 {
		t.Errorf("a deleted connector's rule: %v", err)
	}
}
