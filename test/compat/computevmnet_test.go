//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
)

// TestComputeVMNetworkingThroughTheGoClient (#1968), the official
// compute/apiv1 REST client: two VMs on one custom network reach each
// other at their internal IPs and internal DNS names, with the network's
// firewall rules enforced as on Google: an allowed port connects, a port no
// rule allows is refused by the implied deny-ingress rule, a deny of a
// higher priority (a lower number) beats an allow, a rule with source and
// target tags admits only the tagged VM (and stops admitting it once
// setTags removes the tag), an egress deny on the client blocks what the
// server's ingress allows. Two VMs on the default network reach each other
// through default-allow-internal, and neither reaches the custom network's
// VMs, nor resolves their names.
// covers: compute.firewalls.insert, compute.firewalls.delete, compute.networks.insert, compute.subnetworks.insert, compute.instances.insert, compute.instances.get, compute.instances.setTags, compute.instances.delete
func TestComputeVMNetworkingThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	addr := h.Endpoint(EnvCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	netA := "ct-net-" + sfx
	server, client, defA, defB := "ct-srv-"+sfx, "ct-cli-"+sfx, "ct-def1-"+sfx, "ct-def2-"+sfx
	fwNames := []string{netA + "-allow", netA + "-deny", netA + "-tags", netA + "-egress"}
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
		var wg sync.WaitGroup
		for _, n := range []string{server, client, defA, defB} {
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: n}); err == nil {
					_ = op.Wait(c)
				}
			}(n)
		}
		wg.Wait()
		for _, f := range fwNames {
			if op, err := fws.Delete(c, &computepb.DeleteFirewallRequest{Project: p, Firewall: f}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: netA}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: netA}); err == nil {
			_ = op.Wait(c)
		}
	})

	wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(netA), AutoCreateSubnetworks: proto.Bool(false)}}))
	wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(netA), Network: proto.String("global/networks/" + netA), IpCidrRange: proto.String("10.66.0.0/24")}}))
	tcp := func(ports ...string) []*computepb.Allowed {
		return []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: ports}}
	}
	netLink := "global/networks/" + netA
	// 8080 and 8081 from anywhere at 1000; 8081 denied at 900; 9090 from
	// the client tag to the server tag; the client may not send to 7070.
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(fwNames[0]), Network: proto.String(netLink), Priority: proto.Int32(1000),
		SourceRanges: []string{"0.0.0.0/0"}, Allowed: tcp("8080", "8081", "7070")}}))
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(fwNames[1]), Network: proto.String(netLink), Priority: proto.Int32(900),
		SourceRanges: []string{"10.66.0.0/24"}, Denied: []*computepb.Denied{{IPProtocol: proto.String("tcp"), Ports: []string{"8081"}}}}}))
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(fwNames[2]), Network: proto.String(netLink), SourceTags: []string{"client"},
		TargetTags: []string{"server"}, Allowed: tcp("9090")}}))
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(fwNames[3]), Network: proto.String(netLink), Direction: proto.String("EGRESS"),
		DestinationRanges: []string{"10.66.0.0/24"}, TargetTags: []string{"client"},
		Denied: []*computepb.Denied{{IPProtocol: proto.String("tcp"), Ports: []string{"7070"}}}}}))

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	keyLine := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"
	// Every VM listens on the ports the test probes.
	script := "#!/bin/bash\nfor p in 7070 8080 8081 8082 9090; do systemd-run --unit=listen-$p nc -lk $p; done\n"
	insert := func(name string, nic *computepb.NetworkInterface, tags ...string) {
		wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
			Name:        proto.String(name),
			MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
			Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
			NetworkInterfaces: []*computepb.NetworkInterface{nic},
			Tags:              &computepb.Tags{Items: tags},
			Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("startup-script"), Value: proto.String(script)},
				{Key: proto.String("ssh-keys"), Value: proto.String(keyLine)}}},
		}}))
	}
	var wg sync.WaitGroup
	for _, v := range []struct {
		name string
		nic  *computepb.NetworkInterface
		tags []string
	}{
		{server, &computepb.NetworkInterface{Subnetwork: proto.String("regions/" + region + "/subnetworks/" + netA)}, []string{"server"}},
		{client, &computepb.NetworkInterface{Subnetwork: proto.String("regions/" + region + "/subnetworks/" + netA)}, []string{"client"}},
		{defA, &computepb.NetworkInterface{Network: proto.String("global/networks/default")}, nil},
		{defB, &computepb.NetworkInterface{Network: proto.String("global/networks/default")}, nil},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			insert(v.name, v.nic, v.tags...)
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	ip := map[string]string{}
	for _, n := range []string{server, client, defA, defB} {
		i, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: n})
		if err != nil {
			t.Fatal(err)
		}
		ip[n] = i.GetNetworkInterfaces()[0].GetNetworkIP()
	}
	if !strings.HasPrefix(ip[server], "10.66.0.") || !strings.HasPrefix(ip[defA], "10.128.") {
		t.Fatalf("internal IPs: %v", ip)
	}

	run := func(vm, cmd string) string {
		t.Helper()
		sshAddr := vmSSHAddress(t, addr, p, zone, vm)
		var out string
		deadline := time.Now().Add(3 * time.Minute)
		for {
			c, err := ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
			if err == nil {
				s, err2 := c.NewSession()
				if err2 == nil {
					b, err3 := s.CombinedOutput(cmd)
					out, err = string(b), err3
				} else {
					err = err2
				}
				c.Close()
				if err == nil {
					return out
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("ssh %s %q: %v: %s", vm, cmd, err, out)
			}
			time.Sleep(2 * time.Second)
		}
	}
	probe := func(from, host, port string) bool {
		t.Helper()
		out := run(from, "nc -z -w 3 "+host+" "+port+" && echo OPEN || echo CLOSED")
		return strings.Contains(out, "OPEN")
	}
	// eventually waits until a probe gives want (the guests apply their
	// network every 2 seconds), and fails after two minutes.
	eventually := func(what, from, host, port string, want bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for probe(from, host, port) != want {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %s -> %s:%s open=%v, want %v; %s", what, from, host, port, !want, want,
					run(from, "ip -4 addr show vpc0; ip route; sudo nft list ruleset"))
			}
			time.Sleep(3 * time.Second)
		}
	}
	// Each listener is up before the blocked ports are probed.
	for _, vm := range []string{server, defB} {
		deadline := time.Now().Add(3 * time.Minute)
		for !strings.Contains(run(vm, "ss -ltn"), ":9090") {
			if time.Now().After(deadline) {
				t.Fatalf("%s never listened: %s", vm, run(vm, "ss -ltn"))
			}
			time.Sleep(3 * time.Second)
		}
	}

	if out := run(client, "ip -4 addr show vpc0"); !strings.Contains(out, ip[client]) {
		t.Fatalf("the client's internal IP is not on vpc0: %s", out)
	}
	eventually("an allowed port", client, ip[server], "8080", true)
	if probe(client, ip[server], "8082") {
		t.Error("a port no rule allows connected: the implied deny-ingress rule is not enforced")
	}
	if probe(client, ip[server], "8081") {
		t.Error("a deny at priority 900 did not beat an allow at 1000")
	}
	if !probe(client, ip[server], "9090") {
		t.Error("a source-tag rule did not admit the tagged client")
	}
	if probe(server, ip[client], "9090") {
		t.Error("a target-tag rule applied to an instance without the tag")
	}
	if probe(client, ip[server], "7070") {
		t.Error("the client's egress deny did not block 7070")
	}
	if !probe(server, ip[client], "7070") {
		t.Error("the allow on 7070 did not admit the server (its egress is not denied)")
	}
	fqdn := func(vm string) string { return vm + "." + zone + ".c." + p + ".internal" }
	if out := run(client, "getent hosts "+fqdn(server)); !strings.Contains(out, ip[server]) {
		t.Errorf("internal DNS: %s does not resolve to %s on its network: %q", fqdn(server), ip[server], out)
	}
	if !probe(client, fqdn(server), "8080") {
		t.Error("the server's internal DNS name did not connect")
	}

	// The default network: default-allow-internal admits any port from
	// 10.128.0.0/9; the custom network is not reachable from it.
	eventually("default-allow-internal", defA, ip[defB], "8082", true)
	if !probe(defA, fqdn(defB), "8080") {
		t.Error("a default network VM's DNS name did not connect")
	}
	if probe(defA, ip[server], "8080") {
		t.Error("a VM on another network reached the custom network's VM, whose firewall admits 0.0.0.0/0 on 8080")
	}
	if out := run(defA, "getent hosts "+fqdn(server)+" || echo NONE"); !strings.Contains(out, "NONE") {
		t.Errorf("another network resolved %s: %q", fqdn(server), out)
	}

	// setTags: without its tag the client is no longer a source of the tag
	// rule, nor a target of the egress deny.
	ci, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: client})
	if err != nil {
		t.Fatal(err)
	}
	wait(insts.SetTags(ctx, &computepb.SetTagsInstanceRequest{Project: p, Zone: zone, Instance: client,
		TagsResource: &computepb.Tags{Fingerprint: ci.GetTags().Fingerprint, Items: []string{"other"}}}))
	eventually("after setTags removed the source tag", client, ip[server], "9090", false)
	eventually("after setTags removed the egress deny's target tag", client, ip[server], "7070", true)

	// Deleting the deny rule opens 8081.
	wait(fws.Delete(ctx, &computepb.DeleteFirewallRequest{Project: p, Firewall: fwNames[1]}))
	eventually("after the deny rule was deleted", client, ip[server], "8081", true)
}
