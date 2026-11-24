package compute

import (
	"context"
	"net/http"
	"strings"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// TestCloudRouterAndNATThroughTheGoClient (#2140): routers with BGP,
// Google's ASN and advertisement rules, NAT gateways with their subnets,
// manual addresses (IN_USE by the router, released on delete) and
// defaults, one gateway per subnet, getRouterStatus (peers DOWN, natStatus)
// and getNatMappingInfo, aggregatedList, and the network kept while a
// router uses it.
func TestCloudRouterAndNATThroughTheGoClient(t *testing.T) {
	ctx := context.Background()
	_, opts := newTestServer(t, Options{})
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	addrs, _ := computeapi.NewAddressesRESTClient(ctx, opts...)
	routers, _ := computeapi.NewRoutersRESTClient(ctx, opts...)
	const project, region = "router-p", "us-central1"
	if _, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: project, NetworkResource: &computepb.Network{
		Name: proto.String("net"), AutoCreateSubnetworks: proto.Bool(false)}}); err != nil {
		t.Fatal(err)
	}
	for _, sn := range []struct{ name, cidr string }{{"a", "10.1.0.0/24"}, {"b", "10.2.0.0/24"}} {
		if _, err := subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: project, Region: region, SubnetworkResource: &computepb.Subnetwork{
			Name: proto.String(sn.name), Network: proto.String("global/networks/net"), IpCidrRange: proto.String(sn.cidr)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: project, Region: region, AddressResource: &computepb.Address{Name: proto.String("nat-ip")}}); err != nil {
		t.Fatal(err)
	}
	insert := func(r *computepb.Router) error {
		_, err := routers.Insert(ctx, &computepb.InsertRouterRequest{Project: project, Region: region, RouterResource: r})
		return err
	}
	// Refusals.
	for name, r := range map[string]*computepb.Router{
		"public ASN":      {Name: proto.String("r"), Network: proto.String("global/networks/net"), Bgp: &computepb.RouterBgp{Asn: proto.Uint32(1234)}},
		"no network":      {Name: proto.String("r")},
		"custom groups":   {Name: proto.String("r"), Network: proto.String("global/networks/net"), Bgp: &computepb.RouterBgp{Asn: proto.Uint32(64512), AdvertisedGroups: []string{"ALL_SUBNETS"}}},
		"keepalive":       {Name: proto.String("r"), Network: proto.String("global/networks/net"), Bgp: &computepb.RouterBgp{Asn: proto.Uint32(64512), KeepaliveInterval: proto.Uint32(10)}},
		"peer, no iface":  {Name: proto.String("r"), Network: proto.String("global/networks/net"), Bgp: &computepb.RouterBgp{Asn: proto.Uint32(64512)}, BgpPeers: []*computepb.RouterBgpPeer{{Name: proto.String("p"), InterfaceName: proto.String("x"), PeerAsn: proto.Uint32(65001)}}},
		"vpn interface":   {Name: proto.String("r"), Network: proto.String("global/networks/net"), Interfaces: []*computepb.RouterInterface{{Name: proto.String("i"), LinkedVpnTunnel: proto.String("regions/us-central1/vpnTunnels/t")}}},
		"nat without ips": {Name: proto.String("r"), Network: proto.String("global/networks/net"), Nats: []*computepb.RouterNat{{Name: proto.String("n"), SourceSubnetworkIpRangesToNat: proto.String(natAllAll), NatIpAllocateOption: proto.String(natManualOnly)}}},
		"nat dyn + eim":   {Name: proto.String("r"), Network: proto.String("global/networks/net"), Nats: []*computepb.RouterNat{{Name: proto.String("n"), SourceSubnetworkIpRangesToNat: proto.String(natAllAll), NatIpAllocateOption: proto.String(natAutoOnly), EnableDynamicPortAllocation: proto.Bool(true), EnableEndpointIndependentMapping: proto.Bool(true)}}},
	} {
		want := http.StatusBadRequest
		if name == "vpn interface" {
			want = http.StatusNotImplemented
		}
		if err := insert(r); code(err) != want {
			t.Errorf("%s: err = %v, want %d", name, err, want)
		}
	}
	// A router with BGP and a manual NAT of subnet a.
	if err := insert(&computepb.Router{Name: proto.String("r1"), Network: proto.String("global/networks/net"),
		Bgp: &computepb.RouterBgp{Asn: proto.Uint32(64512), AdvertiseMode: proto.String("CUSTOM"), AdvertisedGroups: []string{"ALL_SUBNETS"},
			AdvertisedIpRanges: []*computepb.RouterAdvertisedIpRange{{Range: proto.String("192.168.10.0/24")}}},
		Interfaces: []*computepb.RouterInterface{{Name: proto.String("if0"), IpRange: proto.String("169.254.1.1/30")}},
		BgpPeers: []*computepb.RouterBgpPeer{{Name: proto.String("peer0"), InterfaceName: proto.String("if0"), PeerAsn: proto.Uint32(65001),
			PeerIpAddress: proto.String("169.254.1.2")}},
		Nats: []*computepb.RouterNat{{Name: proto.String("nat-a"), SourceSubnetworkIpRangesToNat: proto.String(natList),
			Subnetworks:         []*computepb.RouterNatSubnetworkToNat{{Name: proto.String("regions/us-central1/subnetworks/a")}},
			NatIpAllocateOption: proto.String(natManualOnly), NatIps: []string{"regions/us-central1/addresses/nat-ip"},
			LogConfig: &computepb.RouterNatLogConfig{Enable: proto.Bool(true), Filter: proto.String("ERRORS_ONLY")}}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := routers.Get(ctx, &computepb.GetRouterRequest{Project: project, Region: region, Router: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetBgp().GetKeepaliveInterval() != 20 || got.GetBgpPeers()[0].GetEnable() != "TRUE" || got.GetBgpPeers()[0].GetAdvertisedRoutePriority() != 100 ||
		got.GetBgpPeers()[0].GetIpAddress() != "169.254.1.1" || got.GetBgpPeers()[0].GetBfd().GetSessionInitializationMode() != "DISABLED" ||
		!strings.HasSuffix(got.GetNetwork(), "/projects/router-p/global/networks/net") {
		t.Errorf("router = %+v", got)
	}
	nat := got.GetNats()[0]
	if nat.GetType() != "PUBLIC" || !strings.HasSuffix(nat.GetSubnetworks()[0].GetName(), "/subnetworks/a") ||
		nat.GetSubnetworks()[0].GetSourceIpRangesToNat()[0] != "ALL_IP_RANGES" || nat.GetLogConfig().GetFilter() != "ERRORS_ONLY" {
		t.Errorf("nat = %+v", nat)
	}
	ip, _ := addrs.Get(ctx, &computepb.GetAddressRequest{Project: project, Region: region, Address: "nat-ip"})
	if ip.GetStatus() != "IN_USE" || len(ip.GetUsers()) != 1 || !strings.HasSuffix(ip.GetUsers()[0], "/routers/r1") {
		t.Errorf("NAT IP = %s %v", ip.GetStatus(), ip.GetUsers())
	}
	// Subnet a has its gateway; a second router's ALL_SUBNETWORKS one is
	// refused, a gateway of subnet b is not.
	if err := insert(&computepb.Router{Name: proto.String("r2"), Network: proto.String("global/networks/net"),
		Nats: []*computepb.RouterNat{{Name: proto.String("all"), SourceSubnetworkIpRangesToNat: proto.String(natAllAll), NatIpAllocateOption: proto.String(natAutoOnly)}}}); code(err) != http.StatusBadRequest {
		t.Errorf("a second gateway of every subnet: %v", err)
	}
	if err := insert(&computepb.Router{Name: proto.String("r2"), Network: proto.String("global/networks/net"),
		Nats: []*computepb.RouterNat{{Name: proto.String("nat-b"), SourceSubnetworkIpRangesToNat: proto.String(natList), NatIpAllocateOption: proto.String(natAutoOnly),
			Subnetworks: []*computepb.RouterNatSubnetworkToNat{{Name: proto.String("b")}}}}}); err != nil {
		t.Fatal(err)
	}
	// Status: the peer is DOWN, advertised the custom groups and range.
	st, err := routers.GetRouterStatus(ctx, &computepb.GetRouterStatusRouterRequest{Project: project, Region: region, Router: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	ps := st.GetResult().GetBgpPeerStatus()[0]
	if ps.GetStatus() != "DOWN" || len(ps.GetAdvertisedRoutes()) != 3 {
		t.Errorf("peer status = %+v", ps)
	}
	ns := st.GetResult().GetNatStatus()[0]
	if len(ns.GetUserAllocatedNatIps()) != 1 || ns.GetUserAllocatedNatIps()[0] != ip.GetAddress() {
		t.Errorf("nat status = %+v", ns)
	}
	st2, _ := routers.GetRouterStatus(ctx, &computepb.GetRouterStatusRouterRequest{Project: project, Region: region, Router: "r2"})
	if auto := st2.GetResult().GetNatStatus()[0].GetAutoAllocatedNatIps(); len(auto) != 1 || auto[0] == ip.GetAddress() {
		t.Errorf("auto NAT IPs = %v", auto)
	}
	// The mapping info is empty without VMs.
	it := routers.GetNatMappingInfo(ctx, &computepb.GetNatMappingInfoRoutersRequest{Project: project, Region: region, Router: "r1"})
	if _, err := it.Next(); err != iterator.Done {
		t.Errorf("mapping info without VMs: %v", err)
	}
	// Patch: the NAT's ports and timeouts.
	got.Nats[0].MinPortsPerVm = proto.Int32(128)
	got.Nats[0].UdpIdleTimeoutSec = proto.Int32(60)
	if _, err := routers.Patch(ctx, &computepb.PatchRouterRequest{Project: project, Region: region, Router: "r1", RouterResource: &computepb.Router{Nats: got.Nats}}); err != nil {
		t.Fatal(err)
	}
	got, _ = routers.Get(ctx, &computepb.GetRouterRequest{Project: project, Region: region, Router: "r1"})
	if got.GetNats()[0].GetMinPortsPerVm() != 128 || got.GetNats()[0].GetUdpIdleTimeoutSec() != 60 || len(got.GetBgpPeers()) != 1 {
		t.Errorf("patched = %+v", got)
	}
	// aggregatedList.
	agg := routers.AggregatedList(ctx, &computepb.AggregatedListRoutersRequest{Project: project})
	n := 0
	for {
		pair, err := agg.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n += len(pair.Value.GetRouters())
	}
	if n != 2 {
		t.Errorf("aggregated routers = %d", n)
	}
	// The network and subnet are kept while routers use them.
	if _, err := nets.Delete(ctx, &computepb.DeleteNetworkRequest{Project: project, Network: "net"}); code(err) != http.StatusBadRequest {
		t.Errorf("network deleted under a router: %v", err)
	}
	if _, err := subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: project, Region: region, Subnetwork: "b"}); code(err) != http.StatusBadRequest {
		t.Errorf("subnet deleted under a NAT: %v", err)
	}
	// The address can't be released while in use; deleting the router
	// releases it.
	if _, err := addrs.Delete(ctx, &computepb.DeleteAddressRequest{Project: project, Region: region, Address: "nat-ip"}); code(err) != http.StatusBadRequest {
		t.Errorf("NAT IP released in use: %v", err)
	}
	if _, err := routers.Delete(ctx, &computepb.DeleteRouterRequest{Project: project, Region: region, Router: "r1"}); err != nil {
		t.Fatal(err)
	}
	ip, _ = addrs.Get(ctx, &computepb.GetAddressRequest{Project: project, Region: region, Address: "nat-ip"})
	if ip.GetStatus() != "RESERVED" || len(ip.GetUsers()) != 0 {
		t.Errorf("after delete: %s %v", ip.GetStatus(), ip.GetUsers())
	}
}

// TestNATGatesVMEgress (#2140): a VM with no external IP is refused the
// internet in its guest's ruleset until a NAT gateway translates its
// subnet; NAT mapping info gives it its ports.
func TestNATGatesVMEgress(t *testing.T) {
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	const project = "nat-p"
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureProjectLocked(project); err != nil {
		t.Fatal(err)
	}
	sub := subnetRel(project, "us-central1", "default")
	vm := func(name string, external bool) obj {
		nic := obj{"network": networkRel(project, "default"), "subnetwork": sub, "networkIP": "10.128.0." + map[bool]string{true: "3", false: "2"}[external]}
		if external {
			nic["accessConfigs"] = []any{obj{"natIP": "127.0.0.1"}}
		}
		rel := "projects/" + project + "/zones/us-central1-a/instances/" + name
		o := norm(obj{"name": name, "selfLink": rel, "zone": "projects/" + project + "/zones/us-central1-a", "status": stRunning,
			"networkInterfaces": []any{nic}})
		s.res[rel] = o
		return o
	}
	internal, external := vm("internal", false), vm("external", true)
	const drop = `no external IP and no Cloud NAT`
	cfg := func(o obj) string {
		text, ok := s.vpcConfigLocked(o, "10.244.0.9")
		if !ok {
			t.Fatal("no config")
		}
		return text
	}
	if !strings.Contains(cfg(internal), drop) || strings.Contains(cfg(external), drop) {
		t.Fatalf("before NAT: internal has the drop %v, external %v", strings.Contains(cfg(internal), drop), strings.Contains(cfg(external), drop))
	}
	if _, err := s.insertRouter(project, "us-central1", obj{"name": "r", "network": "global/networks/default",
		"nats": []any{map[string]any{"name": "n", "sourceSubnetworkIpRangesToNat": natAllPrimary, "natIpAllocateOption": natAutoOnly}}}); err != nil {
		t.Fatal(err)
	}
	// Until the gateway's pod runs (natgateways.go) the VM has no route.
	if !strings.Contains(cfg(internal), drop) {
		t.Errorf("a NAT'd VM reaches the internet before its gateway's pod runs:\n%s", cfg(internal))
	}
	for _, g := range s.natGatewaysLocked() {
		s.vpcConfigLocked(obj{"selfLink": natRel(g.Key), "zone": "us-central1-a", "status": stRunning, "_natGateway": g.Key,
			"networkInterfaces": []any{obj{"network": g.Network, "networkIP": g.IP}}}, "10.244.0.30")
	}
	if strings.Contains(cfg(internal), drop) {
		t.Errorf("a NAT'd VM still has no internet:\n%s", cfg(internal))
	}
	out, err := s.natMappingInfo(call{project: project, query: map[string][]string{}}, "us-central1", "r")
	if err != nil {
		t.Fatal(err)
	}
	res := out["result"].([]obj)
	if len(res) != 1 || res[0]["instanceName"] != "internal" {
		t.Fatalf("mappings = %v", res)
	}
	m := res[0]["interfaceNatMappings"].([]any)[0].(obj)
	if r := m["natIpPortRanges"].([]any); len(r) != 1 || !strings.HasSuffix(r[0].(string), ":1024-1087") || m["numTotalNatPorts"] != 64.0 {
		t.Errorf("mapping = %v", m)
	}
}
