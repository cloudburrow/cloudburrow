//go:build compat

package compat

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

// covers: compute.routers.insert, compute.routers.getRouterStatus, compute.routers.delete, compute.instances.insert, compute.instances.delete
//
// TestCloudRouterBGPWithARouterApplianceVM2140 (#2140), the official
// compute/apiv1 REST clients, as network-connectivity/docs/router-appliance
// sets one up: a network and subnet; a VM with canIpForward (the router
// appliance) and a client VM; firewall rules letting the Cloud Router
// interface's address reach the appliance on TCP 179; a Cloud Router with
// an interface on the subnet at a chosen privateIpAddress and a BGP peer
// with the appliance. The appliance runs GoBGP's gobgpd (copied from the
// image CloudBurrow runs Cloud Router's speaker from) and advertises
// 192.168.77.0/24 with MED 50, an address of which it holds on its
// loopback. getRouterStatus reports the session UP and Established with the
// route learned as the best route; the appliance learns the subnet's range
// from Cloud Router with MED 100; the client VM routes 192.168.77.0/24
// through the appliance and reaches it; once withdrawn, the route goes.
func TestCloudRouterBGPWithARouterApplianceVM2140(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	p := h.Project()
	const zone, region, cidr, ifaceIP = "us-central1-a", "us-central1", "10.77.0.0/24", "10.77.0.250"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	name := "ct-bgp-" + sfx
	nets, _ := computeapi.NewNetworksRESTClient(ctx, opts...)
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	routers, _ := computeapi.NewRoutersRESTClient(ctx, opts...)
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	network := "projects/" + p + "/global/networks/" + name
	subnet := "projects/" + p + "/regions/" + region + "/subnetworks/" + name
	wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(name), AutoCreateSubnetworks: proto.Bool(false)}}))
	t.Cleanup(func() {
		c := context.Background()
		if op, err := routers.Delete(c, &computepb.DeleteRouterRequest{Project: p, Region: region, Router: name}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := fws.Delete(c, &computepb.DeleteFirewallRequest{Project: p, Firewall: name}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: name}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: name}); err == nil {
			_ = op.Wait(c)
		}
	})
	wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(name), Network: proto.String(network), IpCidrRange: proto.String(cidr)}}))
	// BGP from the Cloud Router interface's address, and the client's HTTP
	// and ping.
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(name), Network: proto.String(network), SourceRanges: []string{cidr},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"80", "179"}}, {IPProtocol: proto.String("icmp")}}}}))

	v := newVPCVMs(ctx, t, h, zone)
	v.make(map[string]*computepb.NetworkInterface{"client-" + sfx: {Network: proto.String(network), Subnetwork: proto.String(subnet)}})
	client := "client-" + sfx
	appliance := "appliance-" + sfx
	wait(v.insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
		Name: proto.String(appliance), MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"), CanIpForward: proto.Bool(true),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String(network), Subnetwork: proto.String(subnet)}},
		Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("startup-script"), Value: proto.String(lbWebScript)},
			{Key: proto.String("ssh-keys"), Value: proto.String(v.key)}}}}}))
	t.Cleanup(func() {
		if op, err := v.insts.Delete(context.Background(), &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: appliance}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	ai, err := v.insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: appliance})
	if err != nil {
		t.Fatal(err)
	}
	aip := ai.GetNetworkInterfaces()[0].GetNetworkIP()

	wait(routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: &computepb.Router{
		Name: proto.String(name), Network: proto.String(network), Bgp: &computepb.RouterBgp{Asn: proto.Uint32(64514)},
		Interfaces: []*computepb.RouterInterface{{Name: proto.String("if0"), Subnetwork: proto.String(subnet), PrivateIpAddress: proto.String(ifaceIP)}},
		BgpPeers: []*computepb.RouterBgpPeer{{Name: proto.String("appliance"), InterfaceName: proto.String("if0"), PeerAsn: proto.Uint32(65001),
			PeerIpAddress: proto.String(aip), RouterApplianceInstance: proto.String("projects/" + p + "/zones/" + zone + "/instances/" + appliance)}}}}))

	// The appliance's gobgpd: GoBGP from the image the router's speaker
	// runs, which CloudBurrow builds once a router needs one.
	dir := t.TempDir()
	var img string
	deadline := time.Now().Add(10 * time.Minute)
	for img == "" {
		out, _ := exec.CommandContext(ctx, "docker", "images", "--filter", "label=cloudburrow.dev/gobgp", "--format", "{{.Repository}}:{{.Tag}}").Output()
		if f := strings.Fields(string(out)); len(f) > 0 {
			img = f[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CloudBurrow built no GoBGP image for the router's speaker")
		}
		time.Sleep(5 * time.Second)
	}
	cid, err := exec.CommandContext(ctx, "docker", "create", img).Output()
	if err != nil {
		t.Fatalf("docker create %s: %v", img, err)
	}
	id := strings.TrimSpace(string(cid))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", id).Run() })
	for _, b := range []string{"gobgpd", "gobgp"} {
		if out, err := exec.CommandContext(ctx, "docker", "cp", id+":/usr/bin/"+b, filepath.Join(dir, b)).CombinedOutput(); err != nil {
			t.Fatalf("docker cp %s: %v: %s", b, err, out)
		}
		raw, err := os.ReadFile(filepath.Join(dir, b))
		if err != nil {
			t.Fatal(err)
		}
		v.run(appliance, "true") // booted
		upload(t, v, appliance, raw, "/usr/local/bin/"+b)
	}
	conf := fmt.Sprintf("[global.config]\n  as = 65001\n  router-id = %q\n[[neighbors]]\n  [neighbors.config]\n    neighbor-address = %q\n    peer-as = 64514\n", aip, ifaceIP)
	upload(t, v, appliance, []byte(conf), "/etc/gobgpd.toml")
	v.run(appliance, "sudo ip addr add 192.168.77.1/32 dev lo; sudo systemd-run --unit=gobgpd /usr/local/bin/gobgpd -f /etc/gobgpd.toml -t toml")
	v.eventually("gobgpd's API", appliance, "sudo /usr/local/bin/gobgp global rib add -a ipv4 192.168.77.0/24 med 50 && echo ADDED", "ADDED")

	status := func() *computepb.RouterStatus {
		t.Helper()
		st, err := routers.GetRouterStatus(ctx, &computepb.GetRouterStatusRouterRequest{Project: p, Region: region, Router: name})
		if err != nil {
			t.Fatal(err)
		}
		return st.GetResult()
	}
	deadline = time.Now().Add(5 * time.Minute)
	for {
		st := status()
		ps := st.GetBgpPeerStatus()
		if len(ps) == 1 && ps[0].GetStatus() == "UP" && ps[0].GetState() == "Established" && ps[0].GetNumLearnedRoutes() == 1 &&
			len(st.GetBestRoutes()) == 1 && len(st.GetBestRoutesForRouter()) == 1 {
			b := st.GetBestRoutes()[0]
			if b.GetDestRange() != "192.168.77.0/24" || b.GetNextHopIp() != aip || b.GetPriority() != 50 || b.GetRouteType() != "BGP" {
				t.Fatalf("best route: %v", b)
			}
			if ps[0].GetUptimeSeconds() == "" || len(ps[0].GetAdvertisedRoutes()) != 1 || ps[0].GetAdvertisedRoutes()[0].GetDestRange() != cidr {
				t.Fatalf("peer status: %v", ps[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session did not come up: %v\ngobgpd on the appliance: %s", st, v.run(appliance, "sudo /usr/local/bin/gobgp neighbor; sudo journalctl -u gobgpd --no-pager | tail -20"))
		}
		time.Sleep(3 * time.Second)
	}
	// The appliance learned the subnet from Cloud Router, its MED the
	// peer's advertised route priority.
	v.eventually("the appliance's RIB", appliance, "sudo /usr/local/bin/gobgp global rib -a ipv4", "Med: 100")
	if out := v.run(appliance, "sudo /usr/local/bin/gobgp global rib -a ipv4"); !strings.Contains(out, cidr) || !strings.Contains(out, "64514") {
		t.Fatalf("the appliance's RIB lacks %s from AS 64514:\n%s", cidr, out)
	}
	// The client routes the learned prefix through the appliance.
	v.eventually("the client's dynamic route", client, "ip route show 192.168.77.0/24", "via "+aip)
	v.eventually("the appliance through the dynamic route", client, "curl -s -m 5 http://192.168.77.1/", "VM-"+appliance)
	// Withdrawn, the route goes.
	v.run(appliance, "sudo /usr/local/bin/gobgp global rib del -a ipv4 192.168.77.0/24")
	deadline = time.Now().Add(2 * time.Minute)
	for {
		st := status()
		if ps := st.GetBgpPeerStatus(); len(ps) == 1 && ps[0].GetNumLearnedRoutes() == 0 && len(st.GetBestRoutes()) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the withdrawn route stays: %v", st)
		}
		time.Sleep(3 * time.Second)
	}
	v.eventually("the withdrawn route gone", client, "ip route show 192.168.77.0/24 | wc -l", "0")
}

// upload writes data to path on vm over SSH, as root, executable.
func upload(t *testing.T, v *vpcVMs, vm string, data []byte, path string) {
	t.Helper()
	c, err := ssh.Dial("tcp", vmSSHAddress(t, v.addr, v.p, v.zone, vm), &ssh.ClientConfig{User: "tester",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(v.signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	s.Stdin = bytes.NewReader(data)
	if out, err := s.CombinedOutput("sudo sh -c 'cat > " + path + " && chmod 755 " + path + "'"); err != nil {
		t.Fatalf("upload %s to %s: %v: %s", path, vm, err, out)
	}
}
