package compute

import (
	"strings"
	"testing"
)

// TestInternalRuleSubnetDefault: an internal forwarding rule without a
// subnetwork takes its auto mode network's own subnet in the region, and
// is refused in a custom mode network, as Google documents the field
// ("optional" in auto mode, "must be specified" in custom mode).
func TestInternalRuleSubnetDefault(t *testing.T) {
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	const p = "p"
	for _, n := range []obj{{"name": "auto1", "autoCreateSubnetworks": true}, {"name": "cust", "autoCreateSubnetworks": false}} {
		if _, err := s.Call("POST", p, "global/networks", nil, n); err != nil {
			t.Fatal(err)
		}
	}
	rule := func(name, network string) (obj, error) {
		if _, err := s.Call("POST", p, "regions/us-central1/backendServices", nil, obj{"name": name, "protocol": "TCP",
			"loadBalancingScheme": "INTERNAL", "network": "global/networks/" + network}); err != nil {
			return nil, err
		}
		if _, err := s.Call("POST", p, "regions/us-central1/forwardingRules", nil, obj{"name": name, "loadBalancingScheme": "INTERNAL",
			"IPProtocol": "TCP", "ports": []any{"80"}, "network": "global/networks/" + network,
			"backendService": "projects/p/regions/us-central1/backendServices/" + name}); err != nil {
			return nil, err
		}
		return s.Call("GET", p, "regions/us-central1/forwardingRules/"+name, nil, nil)
	}
	o, err := rule("a", "auto1")
	if err != nil || !strings.HasSuffix(str(o, "subnetwork"), "/regions/us-central1/subnetworks/auto1") {
		t.Fatalf("auto mode: %v, %v", o["subnetwork"], err)
	}
	if _, err := rule("c", "cust"); err == nil || !strings.Contains(err.Error(), "Subnetwork should be specified for custom subnetmode network") {
		t.Fatalf("custom mode: %v", err)
	}
}
