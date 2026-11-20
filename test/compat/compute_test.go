//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

// EnvCompute is the host address of the Compute Engine API's VPC
// networking (compute.googleapis.com, #1936), JSON under /compute/v1/.
const EnvCompute = "CLOUDBURROW_TEST_COMPUTE"

// computeOpts are the official REST clients' options: the endpoint, never
// Google's, and no credentials.
func computeOpts(t *testing.T, h *Harness) []option.ClientOption {
	t.Helper()
	addr := h.Endpoint(EnvCompute) // skips without it
	if addr == "" {
		t.Fatal("no Compute Engine endpoint")
	}
	return []option.ClientOption{option.WithEndpoint("http://" + addr), option.WithoutAuthentication()}
}

func computeCode(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

// TestComputeVPCNetworksThroughTheGoClient (#1936), the official
// compute/apiv1 REST client: a custom mode network with GLOBAL routing, a
// subnet with a secondary range, the documented refusals (an overlapping
// range, a restricted range, a bad name, a duplicate), expandIpCidrRange
// and a patch of the secondary ranges under the fingerprint, a firewall
// rule with Google's defaults, internal (chosen and given), external and
// private-services-access addresses, a static route, deletion refused while
// the network and subnet are in use, the default network's auto subnets,
// the regions and zones, and every operation waited on in Compute's shape.
// covers: compute.networks.insert, compute.networks.get, compute.networks.list, compute.networks.delete, compute.networks.patch, compute.subnetworks.insert, compute.subnetworks.get, compute.subnetworks.list, compute.subnetworks.delete, compute.subnetworks.patch, compute.subnetworks.expandIpCidrRange, compute.subnetworks.setPrivateIpGoogleAccess, compute.subnetworks.aggregatedList, compute.firewalls.insert, compute.firewalls.get, compute.firewalls.list, compute.firewalls.patch, compute.firewalls.delete, compute.routes.insert, compute.routes.get, compute.routes.list, compute.routes.delete, compute.addresses.insert, compute.addresses.get, compute.addresses.list, compute.addresses.delete, compute.addresses.aggregatedList, compute.globalAddresses.insert, compute.globalAddresses.get, compute.globalAddresses.delete, compute.globalOperations.get, compute.globalOperations.wait, compute.regionOperations.get, compute.regionOperations.wait, compute.regions.get, compute.regions.list, compute.zones.list
func TestComputeVPCNetworksThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
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
	zones, _ := computeapi.NewZonesRESTClient(ctx, opts...)
	p := h.Project()
	const region = "us-central1"
	vpcName := fmt.Sprintf("ct-vpc-%d", time.Now().UnixNano()%1e6)
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		if !op.Done() {
			t.Fatal("the operation is not done after Wait")
		}
	}
	// Whatever the test reached, by name, in dependency order.
	t.Cleanup(func() {
		c := context.Background()
		_, _ = routes.Delete(c, &computepb.DeleteRouteRequest{Project: p, Route: vpcName + "-r"})
		_, _ = addrs.Delete(c, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: vpcName + "-int"})
		_, _ = addrs.Delete(c, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: vpcName + "-int2"})
		_, _ = addrs.Delete(c, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: vpcName + "-ext"})
		_, _ = gaddrs.Delete(c, &computepb.DeleteGlobalAddressRequest{Project: p, Address: vpcName + "-psa"})
		_, _ = fws.Delete(c, &computepb.DeleteFirewallRequest{Project: p, Firewall: vpcName + "-ssh"})
		_, _ = subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a"})
		_, _ = nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: vpcName})
	})

	def, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: p, Network: "default"})
	if err != nil {
		t.Fatalf("the default network: %v", err)
	}
	if !def.GetAutoCreateSubnetworks() || len(def.GetSubnetworks()) < 40 {
		t.Fatalf("the default network: auto %v with %d subnets", def.GetAutoCreateSubnetworks(), len(def.GetSubnetworks()))
	}
	ds, err := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: region, Subnetwork: "default"})
	if err != nil || ds.GetIpCidrRange() != "10.128.0.0/20" || ds.GetGatewayAddress() != "10.128.0.1" {
		t.Fatalf("default's us-central1 subnet: %v, %v", ds, err)
	}
	if _, err := subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: "default"}); computeCode(err) != 400 {
		t.Errorf("deleting an auto mode network's subnet: %v", err)
	}

	wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(vpcName), AutoCreateSubnetworks: proto.Bool(false), Mtu: proto.Int32(1500),
		RoutingConfig: &computepb.NetworkRoutingConfig{RoutingMode: proto.String("GLOBAL")}}}))
	vpc, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: p, Network: vpcName})
	if err != nil || vpc.GetAutoCreateSubnetworks() || vpc.GetMtu() != 1500 || vpc.GetRoutingConfig().GetRoutingMode() != "GLOBAL" {
		t.Fatalf("the network: %v, %v", vpc, err)
	}
	if _, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(vpcName), AutoCreateSubnetworks: proto.Bool(false)}}); computeCode(err) != 409 {
		t.Errorf("a duplicate network: %v", err)
	}
	if _, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String("Not_Valid"), AutoCreateSubnetworks: proto.Bool(false)}}); computeCode(err) != 400 || !strings.Contains(err.Error(), "Must be a match of regex") {
		t.Errorf("an invalid name: %v", err)
	}
	wait(nets.Patch(ctx, &computepb.PatchNetworkRequest{Project: p, Network: vpcName, NetworkResource: &computepb.Network{
		RoutingConfig: &computepb.NetworkRoutingConfig{RoutingMode: proto.String("REGIONAL")}}}))

	sub := func(name, cidr string, sec ...*computepb.SubnetworkSecondaryRange) (*computeapi.Operation, error) {
		return subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
			Name: proto.String(name), Network: vpc.SelfLink, IpCidrRange: proto.String(cidr), SecondaryIpRanges: sec}})
	}
	wait(sub(vpcName+"-a", "10.10.0.0/24", &computepb.SubnetworkSecondaryRange{RangeName: proto.String("pods"), IpCidrRange: proto.String("10.20.0.0/16")}))
	if _, err := sub(vpcName+"-b", "10.10.0.128/25"); computeCode(err) != 400 || !strings.Contains(err.Error(), "conflicts with existing subnetwork") {
		t.Errorf("an overlapping primary range: %v", err)
	}
	if _, err := sub(vpcName+"-b", "10.20.5.0/24"); computeCode(err) != 400 {
		t.Errorf("a range overlapping a secondary range: %v", err)
	}
	if _, err := sub(vpcName+"-b", "127.0.0.0/24"); computeCode(err) != 400 {
		t.Errorf("a restricted range: %v", err)
	}
	wait(subs.ExpandIpCidrRange(ctx, &computepb.ExpandIpCidrRangeSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a",
		SubnetworksExpandIpCidrRangeRequestResource: &computepb.SubnetworksExpandIpCidrRangeRequest{IpCidrRange: proto.String("10.10.0.0/22")}}))
	if _, err := subs.ExpandIpCidrRange(ctx, &computepb.ExpandIpCidrRangeSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a",
		SubnetworksExpandIpCidrRangeRequestResource: &computepb.SubnetworksExpandIpCidrRangeRequest{IpCidrRange: proto.String("10.10.0.0/23")}}); computeCode(err) != 400 {
		t.Errorf("shrinking the range: %v", err)
	}
	a, err := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a"})
	if err != nil || a.GetIpCidrRange() != "10.10.0.0/22" {
		t.Fatalf("the expanded subnet: %v, %v", a, err)
	}
	wait(subs.Patch(ctx, &computepb.PatchSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a", SubnetworkResource: &computepb.Subnetwork{
		Fingerprint: a.Fingerprint, SecondaryIpRanges: []*computepb.SubnetworkSecondaryRange{
			{RangeName: proto.String("pods"), IpCidrRange: proto.String("10.20.0.0/16")},
			{RangeName: proto.String("services"), IpCidrRange: proto.String("10.30.0.0/20")}}}}))
	if _, err := subs.Patch(ctx, &computepb.PatchSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a", SubnetworkResource: &computepb.Subnetwork{
		Fingerprint: a.Fingerprint, Description: proto.String("stale")}}); computeCode(err) != 412 {
		t.Errorf("a patch under a stale fingerprint: %v", err)
	}
	wait(subs.SetPrivateIpGoogleAccess(ctx, &computepb.SetPrivateIpGoogleAccessSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a",
		SubnetworksSetPrivateIpGoogleAccessRequestResource: &computepb.SubnetworksSetPrivateIpGoogleAccessRequest{PrivateIpGoogleAccess: proto.Bool(true)}}))
	if a, _ = subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a"}); len(a.GetSecondaryIpRanges()) != 2 || !a.GetPrivateIpGoogleAccess() {
		t.Errorf("after the patch: %v", a)
	}

	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(vpcName + "-ssh"), Network: vpc.SelfLink, TargetTags: []string{"ssh"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"22"}}}}}))
	fw, err := fws.Get(ctx, &computepb.GetFirewallRequest{Project: p, Firewall: vpcName + "-ssh"})
	if err != nil || fw.GetDirection() != "INGRESS" || fw.GetPriority() != 1000 || strings.Join(fw.GetSourceRanges(), ",") != "0.0.0.0/0" {
		t.Errorf("the firewall rule's defaults: %v, %v", fw, err)
	}
	if _, err := fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(vpcName + "-bad"), Network: vpc.SelfLink, Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"99999"}}}}}); computeCode(err) != 400 {
		t.Errorf("an invalid port: %v", err)
	}
	wait(fws.Patch(ctx, &computepb.PatchFirewallRequest{Project: p, Firewall: vpcName + "-ssh", FirewallResource: &computepb.Firewall{
		SourceRanges: []string{"35.235.240.0/20"}}}))

	wait(addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: region, AddressResource: &computepb.Address{
		Name: proto.String(vpcName + "-int"), AddressType: proto.String("INTERNAL"), Subnetwork: a.SelfLink}}))
	in, err := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: region, Address: vpcName + "-int"})
	if err != nil || in.GetAddress() != "10.10.0.2" || in.GetStatus() != "RESERVED" || in.GetPurpose() != "GCE_ENDPOINT" {
		t.Errorf("a chosen internal address: %v, %v", in, err)
	}
	wait(addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: region, AddressResource: &computepb.Address{
		Name: proto.String(vpcName + "-int2"), AddressType: proto.String("INTERNAL"), Subnetwork: a.SelfLink, Address: proto.String("10.10.1.7")}}))
	if _, err := addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: region, AddressResource: &computepb.Address{
		Name: proto.String(vpcName + "-dup"), AddressType: proto.String("INTERNAL"), Subnetwork: a.SelfLink, Address: proto.String("10.10.1.7")}}); computeCode(err) != 400 {
		t.Errorf("an address already reserved: %v", err)
	}
	if _, err := addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: region, AddressResource: &computepb.Address{
		Name: proto.String(vpcName + "-out"), AddressType: proto.String("INTERNAL"), Subnetwork: a.SelfLink, Address: proto.String("10.99.0.5")}}); computeCode(err) != 400 {
		t.Errorf("an address outside the subnet: %v", err)
	}
	wait(addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: region, AddressResource: &computepb.Address{Name: proto.String(vpcName + "-ext")}}))
	if ext, err := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: region, Address: vpcName + "-ext"}); err != nil || ext.GetAddressType() != "EXTERNAL" || ext.GetAddress() == "" || ext.GetNetworkTier() != "PREMIUM" {
		t.Errorf("an external address: %v, %v", ext, err)
	}
	wait(gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: p, AddressResource: &computepb.Address{
		Name: proto.String(vpcName + "-psa"), AddressType: proto.String("INTERNAL"), Purpose: proto.String("VPC_PEERING"),
		PrefixLength: proto.Int32(20), Network: vpc.SelfLink}}))
	psa, err := gaddrs.Get(ctx, &computepb.GetGlobalAddressRequest{Project: p, Address: vpcName + "-psa"})
	if err != nil || psa.GetPrefixLength() != 20 || psa.GetAddress() == "" {
		t.Fatalf("an allocated range: %v, %v", psa, err)
	}
	if _, err := sub(vpcName+"-c", psa.GetAddress()+"/24"); computeCode(err) != 400 {
		t.Errorf("a subnet inside an allocated range: %v", err)
	}

	wait(routes.Insert(ctx, &computepb.InsertRouteRequest{Project: p, RouteResource: &computepb.Route{
		Name: proto.String(vpcName + "-r"), Network: vpc.SelfLink, DestRange: proto.String("192.168.50.0/24"), NextHopIp: proto.String("10.10.0.9")}}))
	if r, err := routes.Get(ctx, &computepb.GetRouteRequest{Project: p, Route: vpcName + "-r"}); err != nil || r.GetPriority() != 1000 {
		t.Errorf("the route: %v, %v", r, err)
	}

	if _, err := subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a"}); computeCode(err) != 400 || !strings.Contains(err.Error(), "already being used by") {
		t.Errorf("deleting a subnet with addresses: %v", err)
	}
	if _, err := nets.Delete(ctx, &computepb.DeleteNetworkRequest{Project: p, Network: vpcName}); computeCode(err) != 400 || !strings.Contains(err.Error(), "already being used by") {
		t.Errorf("deleting a network in use: %v", err)
	}

	it := nets.List(ctx, &computepb.ListNetworksRequest{Project: p, Filter: proto.String(fmt.Sprintf("name = %q", vpcName))})
	n := 0
	for {
		_, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("networks.list by name: %d", n)
	}
	for _, l := range []func() error{
		func() error {
			_, err := subs.List(ctx, &computepb.ListSubnetworksRequest{Project: p, Region: region}).Next()
			return err
		},
		func() error { _, err := fws.List(ctx, &computepb.ListFirewallsRequest{Project: p}).Next(); return err },
		func() error { _, err := routes.List(ctx, &computepb.ListRoutesRequest{Project: p}).Next(); return err },
		func() error {
			_, err := addrs.List(ctx, &computepb.ListAddressesRequest{Project: p, Region: region}).Next()
			return err
		},
		func() error {
			_, err := subs.AggregatedList(ctx, &computepb.AggregatedListSubnetworksRequest{Project: p}).Next()
			return err
		},
		func() error {
			_, err := addrs.AggregatedList(ctx, &computepb.AggregatedListAddressesRequest{Project: p}).Next()
			return err
		},
		func() error {
			_, err := regions.List(ctx, &computepb.ListRegionsRequest{Project: p}).Next()
			return err
		},
		func() error { _, err := zones.List(ctx, &computepb.ListZonesRequest{Project: p}).Next(); return err },
	} {
		if err := l(); err != nil {
			t.Errorf("a list: %v", err)
		}
	}
	if r, err := regions.Get(ctx, &computepb.GetRegionRequest{Project: p, Region: region}); err != nil || len(r.GetZones()) == 0 {
		t.Errorf("regions.get: %v, %v", r, err)
	}

	wait(routes.Delete(ctx, &computepb.DeleteRouteRequest{Project: p, Route: vpcName + "-r"}))
	wait(addrs.Delete(ctx, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: vpcName + "-int"}))
	wait(addrs.Delete(ctx, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: vpcName + "-int2"}))
	wait(addrs.Delete(ctx, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: vpcName + "-ext"}))
	wait(gaddrs.Delete(ctx, &computepb.DeleteGlobalAddressRequest{Project: p, Address: vpcName + "-psa"}))
	wait(fws.Delete(ctx, &computepb.DeleteFirewallRequest{Project: p, Firewall: vpcName + "-ssh"}))
	wait(subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: vpcName + "-a"}))
	wait(nets.Delete(ctx, &computepb.DeleteNetworkRequest{Project: p, Network: vpcName}))
	if _, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: p, Network: vpcName}); computeCode(err) != 404 {
		t.Errorf("the deleted network: %v", err)
	}
}

// TestGcloudComputeNetworks (#1936): the pinned gcloud through
// gcloud-setup, egress to Google blocked, drives networks create (custom
// mode), describe and list, subnets create (with a secondary range),
// expand-ip-range, describe and list, an overlapping subnet's refusal,
// firewall-rules create, list and delete, addresses create (regional
// internal and external, and global), list and delete, and the deletes.
// gcloud compute sends most of these as batch requests.
// covers: compute.networks.list, compute.subnetworks.expandIpCidrRange, compute.firewalls.insert, compute.firewalls.list, compute.firewalls.delete, compute.globalAddresses.insert, compute.globalAddresses.delete, compute.addresses.aggregatedList
func TestGcloudComputeNetworks(t *testing.T) {
	h := New(t)
	computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	if conf, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name)); !strings.Contains(string(conf), "compute = http://") {
		t.Fatalf("the configuration has no compute override:\n%s", conf)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command(gcloud, append(args, "--project", h.Project(), "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	net := fmt.Sprintf("gc-vpc-%d", time.Now().UnixNano()%1e6)
	const region = "us-east1"
	t.Cleanup(func() {
		_, _ = run("compute", "addresses", "delete", net+"-int", "--region", region)
		_, _ = run("compute", "addresses", "delete", net+"-ext", "--region", region)
		_, _ = run("compute", "addresses", "delete", net+"-g", "--global")
		_, _ = run("compute", "firewall-rules", "delete", net+"-fw")
		_, _ = run("compute", "networks", "subnets", "delete", net+"-s", "--region", region)
		_, _ = run("compute", "networks", "delete", net)
	})
	gc("compute", "networks", "create", net, "--subnet-mode", "custom", "--bgp-routing-mode", "global")
	if got := gc("compute", "networks", "describe", net, "--format=value(routingConfig.routingMode,autoCreateSubnetworks)"); !strings.Contains(got, "GLOBAL") {
		t.Errorf("networks describe: %q", got)
	}
	if got := gc("compute", "networks", "list", "--format=value(name)"); !strings.Contains(got, net) || !strings.Contains(got, "default") {
		t.Errorf("networks list:\n%s", got)
	}
	gc("compute", "networks", "subnets", "create", net+"-s", "--network", net, "--region", region, "--range", "10.50.0.0/24",
		"--secondary-range", "pods=10.60.0.0/16")
	if out, err := run("compute", "networks", "subnets", "create", net+"-x", "--network", net, "--region", region, "--range", "10.50.0.0/25"); err == nil || !strings.Contains(out, "conflicts with existing subnetwork") {
		t.Errorf("an overlapping subnet: %v\n%s", err, out)
	}
	gc("compute", "networks", "subnets", "expand-ip-range", net+"-s", "--region", region, "--prefix-length", "23")
	if got := gc("compute", "networks", "subnets", "describe", net+"-s", "--region", region, "--format=value(ipCidrRange,secondaryIpRanges[0].rangeName)"); !strings.Contains(got, "10.50.0.0/23") || !strings.Contains(got, "pods") {
		t.Errorf("subnets describe: %q", got)
	}
	if got := gc("compute", "networks", "subnets", "list", "--network", net, "--format=value(name)"); !strings.Contains(got, net+"-s") {
		t.Errorf("subnets list:\n%s", got)
	}
	gc("compute", "firewall-rules", "create", net+"-fw", "--network", net, "--allow", "tcp:22,tcp:80-90,icmp", "--source-ranges", "10.0.0.0/8")
	if got := gc("compute", "firewall-rules", "list", "--format=value(name,direction,priority)"); !strings.Contains(got, net+"-fw") || !strings.Contains(got, "INGRESS") {
		t.Errorf("firewall-rules list:\n%s", got)
	}
	gc("compute", "addresses", "create", net+"-int", "--region", region, "--subnet", net+"-s")
	gc("compute", "addresses", "create", net+"-ext", "--region", region)
	gc("compute", "addresses", "create", net+"-g", "--global")
	if got := gc("compute", "addresses", "list", "--format=value(name,address,addressType)"); !strings.Contains(got, net+"-int") || !strings.Contains(got, "10.50.0.2") ||
		!strings.Contains(got, net+"-ext") || !strings.Contains(got, net+"-g") {
		t.Errorf("addresses list:\n%s", got)
	}
	gc("compute", "addresses", "delete", net+"-int", "--region", region)
	gc("compute", "addresses", "delete", net+"-ext", "--region", region)
	gc("compute", "addresses", "delete", net+"-g", "--global")
	gc("compute", "firewall-rules", "delete", net+"-fw")
	gc("compute", "networks", "subnets", "delete", net+"-s", "--region", region)
	gc("compute", "networks", "delete", net)
	if got := gc("compute", "networks", "list", "--format=value(name)"); strings.Contains(got, net+"\n") {
		t.Errorf("networks list after delete:\n%s", got)
	}
}

// TestTerraformComputeNetworkAndSubnetwork (#1936): google_compute_network
// (custom mode) and google_compute_subnetwork with a secondary range apply
// through `cloudburrow terraform` (compute_custom_endpoint) with egress to
// Google blocked, as the official client reads them back; the plan is
// clean; a second secondary range and Private Google Access apply in
// place; the plan is clean again; destroy removes both.
// covers: compute.networks.insert, compute.networks.get, compute.networks.delete, compute.subnetworks.insert, compute.subnetworks.get, compute.subnetworks.patch, compute.subnetworks.setPrivateIpGoogleAccess, compute.subnetworks.delete, compute.globalOperations.get, compute.regionOperations.get
func TestTerraformComputeNetworkAndSubnetwork(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	subs, err := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	nets, _ := computeapi.NewNetworksRESTClient(ctx, opts...)
	m := newTFModule(t)
	name := fmt.Sprintf("tfvpc-%d", time.Now().UnixNano()%1e6)
	const region = "europe-west1"
	t.Cleanup(func() {
		c := context.Background()
		_, _ = subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: m.project, Region: region, Subnetwork: name + "-s"})
		_, _ = nets.Delete(c, &computepb.DeleteNetworkRequest{Project: m.project, Network: name})
	})
	module := func(extra string, pga bool) string {
		return fmt.Sprintf(`
resource "google_compute_network" "n" {
  project                 = %[1]q
  name                    = %[2]q
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"
}
resource "google_compute_subnetwork" "s" {
  project                  = %[1]q
  name                     = "%[2]s-s"
  region                   = %[3]q
  network                  = google_compute_network.n.id
  ip_cidr_range            = "10.70.0.0/24"
  private_ip_google_access = %[5]v
  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.71.0.0/16"
  }
  %[4]s
}
`, m.project, name, region, extra, pga)
	}
	m.write(module("", false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	s, err := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: m.project, Region: region, Subnetwork: name + "-s"})
	if err != nil || s.GetIpCidrRange() != "10.70.0.0/24" || len(s.GetSecondaryIpRanges()) != 1 {
		t.Fatalf("the subnet Terraform made: %v, %v", s, err)
	}
	m.planClean()
	m.write(module(`secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.72.0.0/20"
  }`, true))
	m.apply()
	s, err = subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: m.project, Region: region, Subnetwork: name + "-s"})
	if err != nil || len(s.GetSecondaryIpRanges()) != 2 || !s.GetPrivateIpGoogleAccess() {
		t.Fatalf("after the update: %v, %v", s, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: m.project, Network: name}); computeCode(err) != 404 {
		t.Errorf("the network after destroy: %v", err)
	}
}
