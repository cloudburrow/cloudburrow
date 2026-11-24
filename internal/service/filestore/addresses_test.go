package filestore

import (
	"context"
	"net"
	"testing"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"google.golang.org/grpc/codes"
)

// fakeNetworks has one subnetwork and one allocated range on default.
type fakeNetworks struct{}

func (fakeNetworks) HasNetwork(string, string) bool { return true }

func (fakeNetworks) Conflict(_ string, n *net.IPNet) string {
	_, sub, _ := net.ParseCIDR("10.128.0.0/20")
	if overlaps(n, sub) {
		return "subnetwork default in region us-central1 (10.128.0.0/20)"
	}
	_, ar, _ := net.ParseCIDR("10.60.0.0/24")
	if overlaps(n, ar) {
		return "allocated range psa (10.60.0.0/24)"
	}
	return ""
}

func (fakeNetworks) AllocatedRange(_, name string) (*net.IPNet, bool) {
	if name != "psa" {
		return nil, false
	}
	_, ar, _ := net.ParseCIDR("10.60.0.0/24")
	return ar, true
}

func (f fakeNetworks) AllocatedRanges(network string) map[string]*net.IPNet {
	ar, _ := f.AllocatedRange(network, "psa")
	return map[string]*net.IPNet{"psa": ar}
}

// TestInstancesServeInTheirReservedRange: an instance answers the second
// address of its block; blocks may not overlap subnetworks, other
// instances or 172.17.0.0/16; private services access takes a free block
// of the allocated range and keeps its name.
func TestInstancesServeInTheirReservedRange(t *testing.T) {
	ctx := context.Background()
	be := &MemoryBackend{}
	s, err := NewServer("", Options{Backend: be, Networks: func() Networks { return fakeNetworks{} }})
	if err != nil {
		t.Fatal(err)
	}
	zone := "projects/p/locations/us-central1-a"
	mk := func(id string, tier filestorepb.Instance_Tier, nc *filestorepb.NetworkConfig) (*filestorepb.Instance, error) {
		in := &filestorepb.Instance{Tier: tier, FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024}},
			Networks: []*filestorepb.NetworkConfig{nc}}
		op, err := s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: id, Instance: in})
		if err != nil {
			return nil, err
		}
		wait(t, s, op, nil)
		return s.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: zone + "/instances/" + id})
	}
	a, err := mk("a", filestorepb.Instance_ZONAL, &filestorepb.NetworkConfig{Network: "default", ReservedIpRange: "10.9.0.64/26"})
	if err != nil || a.GetNetworks()[0].GetIpAddresses()[0] != "10.9.0.66" {
		t.Fatalf("a = %v, %v; want 10.9.0.66", a.GetNetworks(), err)
	}
	if be.servers[s.ServerKey(a.GetName())].Address != "10.9.0.66" {
		t.Fatalf("the backend was not given the address: %+v", be.servers)
	}
	_, err = mk("b", filestorepb.Instance_BASIC_HDD, &filestorepb.NetworkConfig{Network: "default", ReservedIpRange: "10.9.0.72/29"})
	wantCode(t, "a range inside another instance's", err, codes.InvalidArgument)
	_, err = mk("c", filestorepb.Instance_BASIC_HDD, &filestorepb.NetworkConfig{Network: "default", ReservedIpRange: "10.128.1.0/29"})
	wantCode(t, "a range inside a subnetwork", err, codes.InvalidArgument)
	_, err = mk("d", filestorepb.Instance_BASIC_HDD, &filestorepb.NetworkConfig{Network: "default", ReservedIpRange: "172.17.3.0/29"})
	wantCode(t, "a range in 172.17.0.0/16", err, codes.InvalidArgument)
	// Unset, a free block is chosen.
	e, err := mk("e", filestorepb.Instance_BASIC_HDD, &filestorepb.NetworkConfig{Network: "default"})
	if err != nil {
		t.Fatal(err)
	}
	_, block, err := net.ParseCIDR(e.GetNetworks()[0].GetReservedIpRange())
	if err != nil || !block.Contains(net.ParseIP(e.GetNetworks()[0].GetIpAddresses()[0])) {
		t.Fatalf("e = %v", e.GetNetworks())
	}
	// Private services access: blocks of the allocated range, in order.
	psa := filestorepb.NetworkConfig_PRIVATE_SERVICE_ACCESS
	f, err := mk("f", filestorepb.Instance_BASIC_HDD, &filestorepb.NetworkConfig{Network: "default", ConnectMode: psa, ReservedIpRange: "psa"})
	if err != nil || f.GetNetworks()[0].GetIpAddresses()[0] != "10.60.0.2" || f.GetNetworks()[0].GetReservedIpRange() != "psa" {
		t.Fatalf("f = %v, %v", f.GetNetworks(), err)
	}
	g, err := mk("g", filestorepb.Instance_BASIC_HDD, &filestorepb.NetworkConfig{Network: "default", ConnectMode: psa})
	if err != nil || g.GetNetworks()[0].GetIpAddresses()[0] != "10.60.0.10" {
		t.Fatalf("g = %v, %v", g.GetNetworks(), err)
	}
	_, err = mk("h", filestorepb.Instance_BASIC_HDD, &filestorepb.NetworkConfig{Network: "default", ConnectMode: psa, ReservedIpRange: "nope"})
	wantCode(t, "an unallocated range name", err, codes.InvalidArgument)
}
