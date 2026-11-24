//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// TestCloudRouterAndNATThroughTheGoClient (#2140), the official
// compute/apiv1 REST client: a Cloud Router with a private ASN, custom
// advertisements, an interface and a BGP peer (Google's defaults: keepalive
// 20, priority 100, enable TRUE, BFD DISABLED); a public ASN, custom groups
// without CUSTOM mode and a VPN tunnel interface refused; a NAT gateway of
// one subnet with a manual address that becomes IN_USE by the router, a
// second ALL_SUBNETWORKS gateway refused, an AUTO_ONLY one of another
// subnet on a second router; getRouterStatus (the peer DOWN with its
// advertised routes, each gateway's addresses), getNatIpInfo,
// getNatMappingInfo, preview, patch and update, aggregatedList and list;
// the network and subnet kept while routers use them; delete releasing the
// address.
// covers: compute.routers.insert, compute.routers.get, compute.routers.list, compute.routers.aggregatedList, compute.routers.patch, compute.routers.update, compute.routers.preview, compute.routers.delete, compute.routers.getRouterStatus, compute.routers.getNatMappingInfo, compute.routers.getNatIpInfo
func TestCloudRouterAndNATThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	addrs, _ := computeapi.NewAddressesRESTClient(ctx, opts...)
	routers, _ := computeapi.NewRoutersRESTClient(ctx, opts...)
	p := h.Project()
	const region = "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	netName, r1, r2, ipName := "ct-rtr-"+sfx, "ct-r1-"+sfx, "ct-r2-"+sfx, "ct-natip-"+sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = routers.Delete(c, &computepb.DeleteRouterRequest{Project: p, Region: region, Router: r1})
		_, _ = routers.Delete(c, &computepb.DeleteRouterRequest{Project: p, Region: region, Router: r2})
		_, _ = addrs.Delete(c, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: ipName})
		_, _ = subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: netName + "-a"})
		_, _ = subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: netName + "-b"})
		_, _ = nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: netName})
	})
	wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(netName), AutoCreateSubnetworks: proto.Bool(false)}}))
	for i, s := range []string{"-a", "-b"} {
		wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
			Name: proto.String(netName + s), Network: proto.String("global/networks/" + netName), IpCidrRange: proto.String(fmt.Sprintf("10.%d.0.0/24", 71+i))}}))
	}
	wait(addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: region, AddressResource: &computepb.Address{Name: proto.String(ipName)}}))
	network := "global/networks/" + netName

	for name, r := range map[string]*computepb.Router{
		"a public ASN": {Name: proto.String(r1), Network: proto.String(network), Bgp: &computepb.RouterBgp{Asn: proto.Uint32(15169)}},
		"custom groups in DEFAULT mode": {Name: proto.String(r1), Network: proto.String(network),
			Bgp: &computepb.RouterBgp{Asn: proto.Uint32(64512), AdvertisedGroups: []string{"ALL_SUBNETS"}}},
		"a VPN tunnel interface": {Name: proto.String(r1), Network: proto.String(network),
			Interfaces: []*computepb.RouterInterface{{Name: proto.String("if0"), LinkedVpnTunnel: proto.String("regions/" + region + "/vpnTunnels/none")}}},
	} {
		if _, err := routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: r}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	router := &computepb.Router{Name: proto.String(r1), Network: proto.String(network), Description: proto.String("edge"),
		Bgp: &computepb.RouterBgp{Asn: proto.Uint32(64512), AdvertiseMode: proto.String("CUSTOM"), AdvertisedGroups: []string{"ALL_SUBNETS"},
			AdvertisedIpRanges: []*computepb.RouterAdvertisedIpRange{{Range: proto.String("192.168.50.0/24"), Description: proto.String("on-prem")}}},
		Interfaces: []*computepb.RouterInterface{{Name: proto.String("if0"), IpRange: proto.String("169.254.10.1/30")}},
		BgpPeers: []*computepb.RouterBgpPeer{{Name: proto.String("peer0"), InterfaceName: proto.String("if0"), PeerAsn: proto.Uint32(65010),
			PeerIpAddress: proto.String("169.254.10.2")}},
		Nats: []*computepb.RouterNat{{Name: proto.String("nat-a"), SourceSubnetworkIpRangesToNat: proto.String("LIST_OF_SUBNETWORKS"),
			Subnetworks: []*computepb.RouterNatSubnetworkToNat{{Name: proto.String("regions/" + region + "/subnetworks/" + netName + "-a"),
				SourceIpRangesToNat: []string{"PRIMARY_IP_RANGE"}}},
			NatIpAllocateOption: proto.String("MANUAL_ONLY"), NatIps: []string{"regions/" + region + "/addresses/" + ipName},
			LogConfig: &computepb.RouterNatLogConfig{Enable: proto.Bool(true), Filter: proto.String("TRANSLATIONS_ONLY")}}},
	}
	pv, err := routers.Preview(ctx, &computepb.PreviewRouterRequest{Project: p, Region: region, RouterResource: router})
	if err != nil || pv.GetResource().GetBgp().GetKeepaliveInterval() != 20 {
		t.Fatalf("preview: %v %v", pv, err)
	}
	wait(routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: router}))
	got, err := routers.Get(ctx, &computepb.GetRouterRequest{Project: p, Region: region, Router: r1})
	if err != nil {
		t.Fatal(err)
	}
	peer := got.GetBgpPeers()[0]
	if got.GetBgp().GetAsn() != 64512 || got.GetBgp().GetKeepaliveInterval() != 20 || peer.GetAdvertisedRoutePriority() != 100 ||
		peer.GetEnable() != "TRUE" || peer.GetBfd().GetSessionInitializationMode() != "DISABLED" || peer.GetIpAddress() != "169.254.10.1" ||
		got.GetNats()[0].GetLogConfig().GetFilter() != "TRANSLATIONS_ONLY" || !strings.HasSuffix(got.GetRegion(), "/regions/"+region) {
		t.Errorf("router = %v", got)
	}
	ip, _ := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: region, Address: ipName})
	if ip.GetStatus() != "IN_USE" || len(ip.GetUsers()) != 1 || !strings.HasSuffix(ip.GetUsers()[0], "/routers/"+r1) {
		t.Errorf("NAT IP = %s %v", ip.GetStatus(), ip.GetUsers())
	}
	if _, err := routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: &computepb.Router{
		Name: proto.String(r2), Network: proto.String(network), Nats: []*computepb.RouterNat{{Name: proto.String("all"),
			SourceSubnetworkIpRangesToNat: proto.String("ALL_SUBNETWORKS_ALL_IP_RANGES"), NatIpAllocateOption: proto.String("AUTO_ONLY")}}}}); err == nil {
		t.Error("a second gateway of all subnets was accepted")
	}
	wait(routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: &computepb.Router{
		Name: proto.String(r2), Network: proto.String(network), Nats: []*computepb.RouterNat{{Name: proto.String("nat-b"),
			SourceSubnetworkIpRangesToNat: proto.String("LIST_OF_SUBNETWORKS"), NatIpAllocateOption: proto.String("AUTO_ONLY"),
			Subnetworks: []*computepb.RouterNatSubnetworkToNat{{Name: proto.String(netName + "-b")}}}}}}))

	st, err := routers.GetRouterStatus(ctx, &computepb.GetRouterStatusRouterRequest{Project: p, Region: region, Router: r1})
	if err != nil {
		t.Fatal(err)
	}
	if ps := st.GetResult().GetBgpPeerStatus(); len(ps) != 1 || ps[0].GetStatus() != "DOWN" || len(ps[0].GetAdvertisedRoutes()) != 3 {
		t.Errorf("BGP peer status = %v", ps)
	}
	if ns := st.GetResult().GetNatStatus(); len(ns) != 1 || len(ns[0].GetUserAllocatedNatIps()) != 1 || ns[0].GetUserAllocatedNatIps()[0] != ip.GetAddress() {
		t.Errorf("NAT status = %v", ns)
	}
	st2, err := routers.GetRouterStatus(ctx, &computepb.GetRouterStatusRouterRequest{Project: p, Region: region, Router: r2})
	if err != nil || len(st2.GetResult().GetNatStatus()) != 1 || len(st2.GetResult().GetNatStatus()[0].GetAutoAllocatedNatIps()) != 1 {
		t.Errorf("AUTO_ONLY status = %v %v", st2, err)
	}
	info, err := routers.GetNatIpInfo(ctx, &computepb.GetNatIpInfoRouterRequest{Project: p, Region: region, Router: r1})
	if err != nil || len(info.GetResult()) != 1 || info.GetResult()[0].GetNatIpInfoMappings()[0].GetNatIp() != ip.GetAddress() {
		t.Errorf("NAT IP info = %v %v", info, err)
	}
	it := routers.GetNatMappingInfo(ctx, &computepb.GetNatMappingInfoRoutersRequest{Project: p, Region: region, Router: r1})
	if _, err := it.Next(); err != iterator.Done {
		t.Errorf("mapping info of a gateway without VMs: %v", err)
	}

	// patch keeps what it doesn't name; update replaces the router.
	nat := got.GetNats()[0]
	nat.MinPortsPerVm, nat.TcpEstablishedIdleTimeoutSec = proto.Int32(256), proto.Int32(600)
	wait(routers.Patch(ctx, &computepb.PatchRouterRequest{Project: p, Region: region, Router: r1, RouterResource: &computepb.Router{Nats: []*computepb.RouterNat{nat}}}))
	if got, _ = routers.Get(ctx, &computepb.GetRouterRequest{Project: p, Region: region, Router: r1}); got.GetNats()[0].GetMinPortsPerVm() != 256 ||
		len(got.GetBgpPeers()) != 1 || got.GetDescription() != "edge" {
		t.Errorf("after patch = %v", got)
	}
	wait(routers.Update(ctx, &computepb.UpdateRouterRequest{Project: p, Region: region, Router: r1, RouterResource: &computepb.Router{
		Name: proto.String(r1), Network: proto.String(network), Bgp: &computepb.RouterBgp{Asn: proto.Uint32(4200000001)}}}))
	if got, _ = routers.Get(ctx, &computepb.GetRouterRequest{Project: p, Region: region, Router: r1}); len(got.GetNats()) != 0 || got.GetBgp().GetAsn() != 4200000001 {
		t.Errorf("after update = %v", got)
	}
	if ip, _ = addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: region, Address: ipName}); ip.GetStatus() != "RESERVED" {
		t.Errorf("NAT IP after the gateway went = %s", ip.GetStatus())
	}

	n := 0
	for r, err := range routers.List(ctx, &computepb.ListRoutersRequest{Project: p, Region: region}).All() {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(r.GetName(), "ct-r") && strings.HasSuffix(r.GetName(), sfx) {
			n++
		}
	}
	agg := 0
	for pair, err := range routers.AggregatedList(ctx, &computepb.AggregatedListRoutersRequest{Project: p}).All() {
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pair.Value.GetRouters() {
			if strings.HasSuffix(r.GetName(), sfx) {
				agg++
			}
		}
	}
	if n != 2 || agg != 2 {
		t.Errorf("listed %d, aggregated %d routers", n, agg)
	}
	if _, err := nets.Delete(ctx, &computepb.DeleteNetworkRequest{Project: p, Network: netName}); computeCode(err) != 400 {
		t.Errorf("the network deleted under its routers: %v", err)
	}
	if _, err := subs.Delete(ctx, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: netName + "-b"}); computeCode(err) != 400 {
		t.Errorf("a NAT'd subnet deleted: %v", err)
	}
	wait(routers.Delete(ctx, &computepb.DeleteRouterRequest{Project: p, Region: region, Router: r1}))
	wait(routers.Delete(ctx, &computepb.DeleteRouterRequest{Project: p, Region: region, Router: r2}))
	if _, err := routers.Get(ctx, &computepb.GetRouterRequest{Project: p, Region: region, Router: r1}); computeCode(err) != 404 {
		t.Errorf("deleted router: %v", err)
	}
}

// TestCloudNATGivesVMsWithoutExternalIPsEgress (#2140): a VM with no
// external IP has no route to the internet (its guest's ruleset drops
// egress to public addresses) until a Cloud NAT gateway translates its
// subnet; getNatMappingInfo then gives it its NAT IP and ports. Nothing is
// sent to the internet: the test reads the VM's applied ruleset.
// covers: compute.routers.insert, compute.routers.getNatMappingInfo, compute.routers.getRouterStatus, compute.routers.delete, compute.instances.insert, compute.instances.delete
func TestCloudNATGivesVMsWithoutExternalIPsEgress(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	addr := h.Endpoint(EnvCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	routers, _ := computeapi.NewRoutersRESTClient(ctx, opts...)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	vm, router := "ct-natvm-"+sfx, "ct-natr-"+sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = routers.Delete(c, &computepb.DeleteRouterRequest{Project: p, Region: region, Router: router})
		if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vm}); err == nil {
			_ = op.Wait(c)
		}
	})
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	keyLine := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"
	wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
		Name:        proto.String(vm),
		MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		// No accessConfigs: no external IP.
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
		Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(keyLine)}}},
	}}))
	ruleset := func() string {
		t.Helper()
		sshAddr := vmSSHAddress(t, addr, p, zone, vm)
		var last error
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			c, err := ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
			if err != nil {
				last = err
				continue
			}
			s, err := c.NewSession()
			if err != nil {
				c.Close()
				last = err
				continue
			}
			b, err := s.CombinedOutput("sudo nft list ruleset")
			c.Close()
			if err == nil && strings.Contains(string(b), "implied allow egress") {
				return string(b)
			}
			last = fmt.Errorf("%v: %s", err, b)
		}
		t.Fatalf("no ruleset from %s: %v", vm, last)
		return ""
	}
	const drop = "no external IP and no Cloud NAT"
	eventually := func(want bool) {
		t.Helper()
		var rs string
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
			if rs = ruleset(); strings.Contains(rs, drop) == want {
				return
			}
		}
		t.Fatalf("the VM's ruleset has the no-NAT drop: %v, want %v:\n%s", !want, want, rs)
	}
	eventually(true)
	wait(routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: &computepb.Router{
		Name: proto.String(router), Network: proto.String("global/networks/default"), Nats: []*computepb.RouterNat{{Name: proto.String("nat"),
			SourceSubnetworkIpRangesToNat: proto.String("ALL_SUBNETWORKS_ALL_IP_RANGES"), NatIpAllocateOption: proto.String("AUTO_ONLY")}}}}))
	eventually(false)
	found := false
	for m, err := range routers.GetNatMappingInfo(ctx, &computepb.GetNatMappingInfoRoutersRequest{Project: p, Region: region, Router: router}).All() {
		if err != nil {
			t.Fatal(err)
		}
		if m.GetInstanceName() == vm && len(m.GetInterfaceNatMappings()) == 1 && m.GetInterfaceNatMappings()[0].GetNumTotalNatPorts() == 64 &&
			len(m.GetInterfaceNatMappings()[0].GetNatIpPortRanges()) == 1 {
			found = true
		}
	}
	if !found {
		t.Error("the VM has no NAT mapping")
	}
	st, err := routers.GetRouterStatus(ctx, &computepb.GetRouterStatusRouterRequest{Project: p, Region: region, Router: router})
	if err != nil || st.GetResult().GetNatStatus()[0].GetNumVmEndpointsWithNatMappings() < 1 {
		t.Errorf("NAT status = %v %v", st, err)
	}
	wait(routers.Delete(ctx, &computepb.DeleteRouterRequest{Project: p, Region: region, Router: router}))
	eventually(true)
}
