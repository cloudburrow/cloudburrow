package redisadmin

import (
	"strings"
	"testing"

	redispb "cloud.google.com/go/redis/apiv1/redispb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// TestNetworksAreCheckedAgainstComputeEngine (#1936): with Compute Engine
// served, an authorized network must exist, a reserved_ip_range may not
// overlap its subnets, a chosen range is moved off them, a PSA range name
// must be an allocated range, and a PSC network must exist; without it
// nothing is checked.
func TestNetworksAreCheckedAgainstComputeEngine(t *testing.T) {
	cs, err := compute.New(compute.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Call("POST", "p", "global/networks", nil, map[string]any{"name": "vpc", "autoCreateSubnetworks": false}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Call("POST", "p", "global/addresses", nil, map[string]any{"name": "psa", "addressType": "INTERNAL",
		"purpose": "VPC_PEERING", "prefixLength": 24.0, "network": "global/networks/vpc"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	rec := func(network, mode, rng string) *record {
		return &record{Name: "projects/p/locations/us-central1/instances/i1", AuthorizedNetwork: network, ConnectMode: mode, ReservedIPRange: rng}
	}
	// Without Compute, anything goes.
	if err := s.checkInstanceVPC(&redispb.Instance{}, rec("projects/p/global/networks/nope", "DIRECT_PEERING", "10.0.0.0/29")); err != nil {
		t.Fatalf("without compute: %v", err)
	}
	s.SetNetworks(func() Networks { return cs })
	if err := s.checkInstanceVPC(&redispb.Instance{}, rec("projects/p/global/networks/nope", "DIRECT_PEERING", "10.0.0.0/29")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a missing network: %v", err)
	}
	given := &redispb.Instance{ReservedIpRange: "10.128.0.0/29"}
	if err := s.checkInstanceVPC(given, rec("projects/p/global/networks/default", "DIRECT_PEERING", "10.128.0.0/29")); err == nil || !strings.Contains(err.Error(), "non-overlapping") {
		t.Errorf("a range on the default network's subnet: %v", err)
	}
	chosen := rec("projects/p/global/networks/default", "DIRECT_PEERING", "10.128.0.0/29")
	if err := s.checkInstanceVPC(&redispb.Instance{}, chosen); err != nil || chosen.ReservedIPRange == "10.128.0.0/29" {
		t.Errorf("a chosen range is not moved: %v %s", err, chosen.ReservedIPRange)
	}
	if err := s.checkInstanceVPC(&redispb.Instance{}, rec("projects/p/global/networks/vpc", "PRIVATE_SERVICE_ACCESS", "psa")); err != nil {
		t.Errorf("an allocated range by name: %v", err)
	}
	if err := s.checkInstanceVPC(&redispb.Instance{}, rec("projects/p/global/networks/vpc", "PRIVATE_SERVICE_ACCESS", "other")); err == nil {
		t.Error("an unknown allocated range was accepted")
	}
	if err := s.checkPSCNetworks("psc_configs.network", "projects/p/global/networks/vpc"); err != nil {
		t.Errorf("an existing PSC network: %v", err)
	}
	if err := s.checkPSCNetworks("psc_configs.network", "projects/p/global/networks/gone"); err == nil {
		t.Error("a missing PSC network was accepted")
	}
}
