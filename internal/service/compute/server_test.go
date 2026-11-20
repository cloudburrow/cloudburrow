package compute

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

func newTestServer(t *testing.T, opts Options) (*Server, []option.ClientOption) {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, []option.ClientOption{option.WithEndpoint(ts.URL), option.WithoutAuthentication()}
}

// code is err's HTTP status.
func code(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

func TestNetworksSubnetsFirewallsAddressesThroughTheGoClient(t *testing.T) {
	ctx := context.Background()
	_, opts := newTestServer(t, Options{})
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	addrs, _ := computeapi.NewAddressesRESTClient(ctx, opts...)
	gaddrs, _ := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	routes, _ := computeapi.NewRoutesRESTClient(ctx, opts...)
	regions, _ := computeapi.NewRegionsRESTClient(ctx, opts...)
	const p = "proj"

	// The default network is there, auto mode, with a subnet per region.
	def, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: p, Network: "default"})
	if err != nil {
		t.Fatalf("default network: %v", err)
	}
	if !def.GetAutoCreateSubnetworks() || len(def.GetSubnetworks()) != len(Regions()) {
		t.Fatalf("default network: auto %v, %d subnets", def.GetAutoCreateSubnetworks(), len(def.GetSubnetworks()))
	}
	us, err := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: "default"})
	if err != nil || us.GetIpCidrRange() != "10.128.0.0/20" || us.GetGatewayAddress() != "10.128.0.1" {
		t.Fatalf("default us-central1 subnet: %v %v", us, err)
	}

	op, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String("vpc"), AutoCreateSubnetworks: proto.Bool(false), RoutingConfig: &computepb.NetworkRoutingConfig{RoutingMode: proto.String("GLOBAL")}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil || !op.Done() {
		t.Fatalf("wait: %v", err)
	}
	if _, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String("vpc"), AutoCreateSubnetworks: proto.Bool(false)}}); code(err) != http.StatusConflict {
		t.Fatalf("duplicate network: %v", err)
	}
	if _, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String("Bad_Name"), AutoCreateSubnetworks: proto.Bool(false)}}); code(err) != 400 || !strings.Contains(err.Error(), "Must be a match of regex") {
		t.Fatalf("bad name: %v", err)
	}
	vpc, _ := nets.Get(ctx, &computepb.GetNetworkRequest{Project: p, Network: "vpc"})
	if vpc.GetRoutingConfig().GetRoutingMode() != "GLOBAL" || vpc.GetMtu() != 1460 {
		t.Fatalf("vpc: %v", vpc)
	}

	insSub := func(name, cidr string, sec ...*computepb.SubnetworkSecondaryRange) error {
		op, err := subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: "us-central1", SubnetworkResource: &computepb.Subnetwork{
			Name: proto.String(name), Network: vpc.SelfLink, IpCidrRange: proto.String(cidr), SecondaryIpRanges: sec}})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	if err := insSub("a", "10.0.0.0/24", &computepb.SubnetworkSecondaryRange{RangeName: proto.String("pods"), IpCidrRange: proto.String("10.4.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	if err := insSub("b", "10.0.0.128/25"); code(err) != 400 || !strings.Contains(err.Error(), "conflicts with existing subnetwork 'a'") {
		t.Fatalf("overlap: %v", err)
	}
	if err := insSub("c", "10.4.1.0/24"); code(err) != 400 {
		t.Fatalf("overlaps a secondary range: %v", err)
	}
	if err := insSub("d", "169.254.0.0/24"); code(err) != 400 {
		t.Fatalf("restricted range: %v", err)
	}
	if err := insSub("e", "10.1.0.0/30"); code(err) != 400 {
		t.Fatalf("a /30: %v", err)
	}

	// expandIpCidrRange: larger only.
	eop, err := subs.ExpandIpCidrRange(ctx, &computepb.ExpandIpCidrRangeSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: "a",
		SubnetworksExpandIpCidrRangeRequestResource: &computepb.SubnetworksExpandIpCidrRangeRequest{IpCidrRange: proto.String("10.0.0.0/22")}})
	if err != nil {
		t.Fatal(err)
	}
	_ = eop.Wait(ctx)
	if _, err := subs.ExpandIpCidrRange(ctx, &computepb.ExpandIpCidrRangeSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: "a",
		SubnetworksExpandIpCidrRangeRequestResource: &computepb.SubnetworksExpandIpCidrRangeRequest{IpCidrRange: proto.String("10.0.0.0/24")}}); code(err) != 400 {
		t.Fatalf("shrink: %v", err)
	}
	a, _ := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: "a"})
	if a.GetIpCidrRange() != "10.0.0.0/22" {
		t.Fatalf("expanded: %v", a.GetIpCidrRange())
	}
	// Patch secondary ranges under the fingerprint.
	pop, err := subs.Patch(ctx, &computepb.PatchSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: "a", SubnetworkResource: &computepb.Subnetwork{
		Fingerprint: a.Fingerprint, SecondaryIpRanges: []*computepb.SubnetworkSecondaryRange{
			{RangeName: proto.String("pods"), IpCidrRange: proto.String("10.4.0.0/16")},
			{RangeName: proto.String("svc"), IpCidrRange: proto.String("10.5.0.0/20")}}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = pop.Wait(ctx)
	if _, err := subs.Patch(ctx, &computepb.PatchSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: "a", SubnetworkResource: &computepb.Subnetwork{
		Fingerprint: a.Fingerprint, Description: proto.String("x")}}); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("stale fingerprint: %v", err)
	}

	// Firewall rules.
	fop, err := fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String("allow-ssh"), Network: vpc.SelfLink, Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"22"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = fop.Wait(ctx)
	fw, _ := fws.Get(ctx, &computepb.GetFirewallRequest{Project: p, Firewall: "allow-ssh"})
	if fw.GetDirection() != "INGRESS" || fw.GetPriority() != 1000 || len(fw.GetSourceRanges()) != 1 || fw.GetSourceRanges()[0] != "0.0.0.0/0" {
		t.Fatalf("firewall defaults: %v", fw)
	}
	if _, err := fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String("bad"), Network: vpc.SelfLink, Allowed: []*computepb.Allowed{{IPProtocol: proto.String("icmp"), Ports: []string{"22"}}}}}); code(err) != 400 {
		t.Fatalf("ports with icmp: %v", err)
	}
	uop, err := fws.Patch(ctx, &computepb.PatchFirewallRequest{Project: p, Firewall: "allow-ssh", FirewallResource: &computepb.Firewall{Priority: proto.Int32(900)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = uop.Wait(ctx)

	// Addresses: an internal one in subnet a, chosen and given.
	aop, err := addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: "us-central1", AddressResource: &computepb.Address{
		Name: proto.String("int1"), AddressType: proto.String("INTERNAL"), Subnetwork: a.SelfLink}})
	if err != nil {
		t.Fatal(err)
	}
	_ = aop.Wait(ctx)
	int1, _ := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: "us-central1", Address: "int1"})
	if int1.GetAddress() != "10.0.0.2" || int1.GetStatus() != "RESERVED" {
		t.Fatalf("int1: %v", int1)
	}
	if _, err := addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: "us-central1", AddressResource: &computepb.Address{
		Name: proto.String("int2"), AddressType: proto.String("INTERNAL"), Subnetwork: a.SelfLink, Address: proto.String("10.0.0.2")}}); code(err) != 400 {
		t.Fatalf("address in use: %v", err)
	}
	if _, err := addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: "us-central1", AddressResource: &computepb.Address{
		Name: proto.String("int3"), AddressType: proto.String("INTERNAL"), Subnetwork: a.SelfLink, Address: proto.String("10.0.0.1")}}); code(err) != 400 {
		t.Fatalf("gateway address: %v", err)
	}
	ext, err := addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: "us-central1", AddressResource: &computepb.Address{Name: proto.String("ext")}})
	if err != nil {
		t.Fatal(err)
	}
	_ = ext.Wait(ctx)
	// An allocated range for private services access.
	gop, err := gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: p, AddressResource: &computepb.Address{
		Name: proto.String("psa"), AddressType: proto.String("INTERNAL"), Purpose: proto.String("VPC_PEERING"), PrefixLength: proto.Int32(16), Network: vpc.SelfLink}})
	if err != nil {
		t.Fatal(err)
	}
	_ = gop.Wait(ctx)
	psa, _ := gaddrs.Get(ctx, &computepb.GetGlobalAddressRequest{Project: p, Address: "psa"})
	if psa.GetAddress() == "" || strings.HasPrefix(psa.GetAddress(), "10.0.") || strings.HasPrefix(psa.GetAddress(), "10.4.") {
		t.Fatalf("allocated range overlaps a subnet: %v", psa.GetAddress())
	}
	if err := insSub("f", psa.GetAddress()+"/24"); code(err) != 400 {
		t.Fatalf("subnet in allocated range: %v", err)
	}

	// A static route; one inside a subnet range is refused.
	rop, err := routes.Insert(ctx, &computepb.InsertRouteRequest{Project: p, RouteResource: &computepb.Route{
		Name: proto.String("to-onprem"), Network: vpc.SelfLink, DestRange: proto.String("192.168.0.0/16"), NextHopIp: proto.String("10.0.0.9")}})
	if err != nil {
		t.Fatal(err)
	}
	_ = rop.Wait(ctx)
	if _, err := routes.Insert(ctx, &computepb.InsertRouteRequest{Project: p, RouteResource: &computepb.Route{
		Name: proto.String("inside"), Network: vpc.SelfLink, DestRange: proto.String("10.0.1.0/24"),
		NextHopGateway: proto.String("global/gateways/default-internet-gateway")}}); code(err) != 400 {
		t.Fatalf("route inside a subnet: %v", err)
	}

	// Deletion is refused while the network or subnet is in use.
	if _, err := subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: "a"}); code(err) != 400 || !strings.Contains(err.Error(), "already being used by") {
		t.Fatalf("subnet in use: %v", err)
	}
	if _, err := nets.Delete(ctx, &computepb.DeleteNetworkRequest{Project: p, Network: "vpc"}); code(err) != 400 || !strings.Contains(err.Error(), "already being used by") {
		t.Fatalf("network in use: %v", err)
	}
	if _, err := subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: "default"}); code(err) != 400 {
		t.Fatalf("auto subnet delete: %v", err)
	}

	// Lists: by region, and a filter.
	it := subs.List(ctx, &computepb.ListSubnetworksRequest{Project: p, Region: "us-central1", Filter: proto.String(`name = "a"`)})
	n := 0
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("filtered list: %d", n)
	}
	agg := addrs.AggregatedList(ctx, &computepb.AggregatedListAddressesRequest{Project: p})
	scopes := 0
	for {
		pair, err := agg.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(pair.Value.GetAddresses()) > 0 {
			scopes++
		}
	}
	if scopes != 2 {
		t.Fatalf("aggregated scopes: %d", scopes)
	}
	r, err := regions.Get(ctx, &computepb.GetRegionRequest{Project: p, Region: "us-central1"})
	if err != nil || len(r.GetZones()) != 4 {
		t.Fatalf("region: %v %v", r, err)
	}

	// Tear down in order.
	for _, del := range []func() (*computeapi.Operation, error){
		func() (*computeapi.Operation, error) {
			return routes.Delete(ctx, &computepb.DeleteRouteRequest{Project: p, Route: "to-onprem"})
		},
		func() (*computeapi.Operation, error) {
			return addrs.Delete(ctx, &computepb.DeleteAddressRequest{Project: p, Region: "us-central1", Address: "int1"})
		},
		func() (*computeapi.Operation, error) {
			return gaddrs.Delete(ctx, &computepb.DeleteGlobalAddressRequest{Project: p, Address: "psa"})
		},
		func() (*computeapi.Operation, error) {
			return fws.Delete(ctx, &computepb.DeleteFirewallRequest{Project: p, Firewall: "allow-ssh"})
		},
		func() (*computeapi.Operation, error) {
			return subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: p, Region: "us-central1", Subnetwork: "a"})
		},
		func() (*computeapi.Operation, error) {
			return nets.Delete(ctx, &computepb.DeleteNetworkRequest{Project: p, Network: "vpc"})
		},
	} {
		op, err := del()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: p, Network: "vpc"}); code(err) != 404 {
		t.Fatalf("deleted network: %v", err)
	}
}

func TestStatePersistsAndUnservedPathsAre501(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compute.json")
	s, _ := newTestServer(t, Options{State: path})
	if _, err := s.Call("POST", "p", "global/networks", nil, obj{"name": "keep", "autoCreateSubnetworks": false}); err != nil {
		t.Fatal(err)
	}
	s2, err := New(Options{State: path})
	if err != nil {
		t.Fatal(err)
	}
	if !s2.HasNetwork("keep", "p") || !s2.HasNetwork("projects/p/global/networks/default", "") {
		t.Fatal("state not restored")
	}
	if _, err := s2.Call("GET", "p", "zones/us-central1-a/instances", nil, nil); ErrorStatus(err) != 501 {
		t.Fatalf("instances: %v", err)
	}
	if _, err := s2.Call("POST", "p", "global/networks", nil, obj{"name": "legacy"}); ErrorStatus(err) != 501 {
		t.Fatalf("legacy: %v", err)
	}
}

// TestImplementedMethodsInProcess covers the methods no compat test
// reaches: switchToCustomMode, firewalls.update, setLabels, the operation
// lists and deletes, and zones.get.
func TestImplementedMethodsInProcess(t *testing.T) {
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	must := func(method, path string, body obj) obj {
		t.Helper()
		o, err := s.Call(method, "p", path, nil, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return o
	}
	must("POST", "global/networks", obj{"name": "auto", "autoCreateSubnetworks": true})
	must("POST", "global/networks/auto/switchToCustomMode", nil)
	if n := must("GET", "global/networks/auto", nil); n["autoCreateSubnetworks"] != false {
		t.Errorf("switchToCustomMode: %v", n["autoCreateSubnetworks"])
	}
	if _, err := s.Call("POST", "p", "global/networks/auto/switchToCustomMode", nil, nil); ErrorStatus(err) != 400 {
		t.Errorf("switching a custom network: %v", err)
	}
	// Its subnets are custom now, so they can be deleted.
	must("DELETE", "regions/us-east1/subnetworks/auto", nil)
	must("POST", "global/firewalls", obj{"name": "fw", "network": "global/networks/auto", "allowed": []any{obj{"IPProtocol": "tcp"}}})
	must("PUT", "global/firewalls/fw", obj{"name": "fw", "network": "global/networks/auto", "denied": []any{obj{"IPProtocol": "udp", "ports": []any{"53"}}}, "priority": 10.0})
	if fw := must("GET", "global/firewalls/fw", nil); fw["allowed"] != nil || fw["priority"] != 10.0 {
		t.Errorf("update: %v", fw)
	}
	must("POST", "global/addresses", obj{"name": "g"})
	must("POST", "regions/us-central1/addresses", obj{"name": "r"})
	for _, path := range []string{"global/addresses/g", "regions/us-central1/addresses/r"} {
		a := must("GET", path, nil)
		if _, err := s.Call("POST", "p", path+"/setLabels", nil, obj{"labels": obj{"env": "dev"}, "labelFingerprint": "stale"}); ErrorStatus(err) != 412 {
			t.Errorf("setLabels with a stale fingerprint: %v", err)
		}
		must("POST", path+"/setLabels", obj{"labels": obj{"env": "dev"}, "labelFingerprint": a["labelFingerprint"]})
		if l, _ := must("GET", path, nil)["labels"].(map[string]any); l["env"] != "dev" {
			t.Errorf("%s labels: %v", path, l)
		}
	}
	if l := must("GET", "global/addresses", nil); len(l["items"].([]any)) != 1 {
		t.Errorf("globalAddresses.list: %v", l)
	}
	ops := must("GET", "global/operations", nil)["items"].([]any)
	if len(ops) == 0 {
		t.Fatal("no global operations")
	}
	if agg := must("GET", "aggregated/operations", nil)["items"].(map[string]any); agg["global"] == nil || agg["regions/us-central1"] == nil {
		t.Errorf("aggregated operations: %v", agg)
	}
	op := ops[0].(map[string]any)["name"].(string)
	must("DELETE", "global/operations/"+op, nil)
	if _, err := s.Call("GET", "p", "global/operations/"+op, nil, nil); ErrorStatus(err) != 404 {
		t.Errorf("a deleted operation: %v", err)
	}
	rops := must("GET", "regions/us-central1/operations", nil)["items"].([]any)
	must("DELETE", "regions/us-central1/operations/"+rops[0].(map[string]any)["name"].(string), nil)
	if l := must("GET", "zones/us-central1-a/operations", nil); len(l["items"].([]any)) != 0 {
		t.Errorf("zone operations: %v", l)
	}
	for _, m := range []string{"GET", "POST"} {
		path := "zones/us-central1-a/operations/none"
		if m == "POST" {
			path += "/wait"
		}
		if _, err := s.Call(m, "p", path, nil, nil); ErrorStatus(err) != 404 {
			t.Errorf("%s %s: %v", m, path, err)
		}
	}
	if _, err := s.Call("DELETE", "p", "zones/us-central1-a/operations/none", nil, nil); ErrorStatus(err) != 404 {
		t.Errorf("deleting a missing zone operation: %v", err)
	}
	if z := must("GET", "zones/us-central1-f", nil); z["region"] != "projects/p/regions/us-central1" {
		t.Errorf("zones.get: %v", z)
	}
	if _, err := s.Call("GET", "p", "zones/us-central1-z", nil, nil); ErrorStatus(err) != 404 {
		t.Errorf("an unknown zone: %v", err)
	}
}
