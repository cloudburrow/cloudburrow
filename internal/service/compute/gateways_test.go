package compute

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRunGateways (#2140): a connector's gateway takes its range's second
// address and a Direct VPC egress one an address of its subnet that no VM
// has; both are members of the network, so a VM's guest gets each as a
// peer and the connector's range on-link, and the gateway's own poll
// returns its forwarding ruleset with its address as the source.
func TestRunGateways(t *testing.T) {
	s, _ := newTestServer(t, Options{VMs: fakeVMs{}})
	if _, err := s.Call("POST", "gw", "zones/us-central1-a/instances", nil, obj{"name": "vm", "machineType": "zones/us-central1-a/machineTypes/e2-small",
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}}}); err != nil {
		t.Fatal(err)
	}
	states := s.SetGateways([]Gateway{
		{Key: "connector:c", Connector: true, Network: "projects/gw/global/networks/default", Range: "10.8.0.0/28", Region: "us-central1"},
		{Key: "direct:d", Subnetwork: "projects/gw/regions/us-central1/subnetworks/default", Region: "us-central1", Tags: []string{"run"}},
		{Key: "missing", Network: "projects/gw/global/networks/none", Range: "10.9.0.0/28", Connector: true},
	})
	if len(states) != 2 || states[0].IP != "10.8.0.2" || !strings.HasPrefix(states[1].IP, "10.128.") || states[1].Network != "projects/gw/global/networks/default" {
		t.Fatalf("states = %+v", states)
	}
	// The same set again keeps the addresses.
	if again := s.SetGateways([]Gateway{{Key: "direct:d", Subnetwork: "projects/gw/regions/us-central1/subnetworks/default", Region: "us-central1"}}); again[0].IP != states[1].IP {
		t.Fatalf("a kept gateway changed address: %v", again)
	}
	s.SetGateways([]Gateway{
		{Key: "connector:c", Connector: true, Network: "projects/gw/global/networks/default", Range: "10.8.0.0/28", Region: "us-central1"},
		{Key: "direct:d", Subnetwork: "projects/gw/regions/us-central1/subnetworks/default", Region: "us-central1", Tags: []string{"run"}},
	})
	// The gateways poll, which makes them peers.
	for _, g := range s.Gateways() {
		r := httptest.NewRequest("GET", GuestPrefix+g.Token+"/cloudburrow/vpc?pod=10.244.0.4"+g.IP[len(g.IP)-1:], nil)
		r.Header.Set("Metadata-Flavor", "Google")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "forward\n") || !strings.Contains(w.Body.String(), "snat ip to "+g.IP) {
			t.Fatalf("%s's poll: %d %s", g.Key, w.Code, w.Body.String())
		}
	}
	s.mu.Lock()
	inst := s.res["projects/gw/zones/us-central1-a/instances/vm"]
	inst["status"] = stRunning
	conf, _ := s.vpcConfigLocked(inst, "10.244.0.9")
	s.mu.Unlock()
	if !strings.Contains(conf, "route 10.8.0.0/28\n") || !strings.Contains(conf, "peer 10.8.0.2 ") || !strings.Contains(conf, "peer "+states[1].IP+" ") {
		t.Fatalf("the VM's configuration lacks the gateways:\n%s", conf)
	}
	if strings.Contains(conf, "host 10.8.0.2 ") {
		t.Errorf("a gateway has a DNS name:\n%s", conf)
	}
	if r := s.GatewayRanges("connector:c"); len(r) == 0 || !strings.Contains(strings.Join(r, " "), "10.128.0.0/20") || !strings.Contains(strings.Join(r, " "), "10.8.0.0/28") {
		t.Errorf("GatewayRanges = %v", r)
	}
}
