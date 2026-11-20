package sqladmin

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// TestPrivateNetworkIsCheckedAgainstComputeEngine (#1936).
func TestPrivateNetworkIsCheckedAgainstComputeEngine(t *testing.T) {
	cs, err := compute.New(compute.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Call("POST", "p", "global/addresses", nil, map[string]any{"name": "sql-range", "addressType": "INTERNAL",
		"purpose": "VPC_PEERING", "prefixLength": 20.0, "network": "global/networks/default"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	ip := func(network, rng string) map[string]any {
		ic := map[string]any{"privateNetwork": network}
		if rng != "" {
			ic["allocatedIpRange"] = rng
		}
		return map[string]any{"ipConfiguration": ic}
	}
	if err := s.checkPrivateNetwork("p", ip("projects/p/global/networks/nope", "")); err != nil {
		t.Fatalf("without compute: %v", err)
	}
	s.SetNetworks(func() Networks { return cs })
	for _, tc := range []struct {
		settings map[string]any
		ok       bool
	}{
		{map[string]any{}, true},
		{ip("projects/p/global/networks/default", ""), true},
		{ip("https://www.googleapis.com/compute/v1/projects/p/global/networks/default", "sql-range"), true},
		{ip("projects/p/global/networks/nope", ""), false},
		{ip("projects/p/global/networks/default", "other"), false},
	} {
		if err := s.checkPrivateNetwork("p", tc.settings); (err == nil) != tc.ok {
			t.Errorf("%v: %v", tc.settings, err)
		}
	}
}
