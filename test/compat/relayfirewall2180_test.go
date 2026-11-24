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
	"cloud.google.com/go/logging/logadmin"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestRelayedConnectionsFollowTheEgressFirewall (#2180) runs two VMs
// (web: network tag web, service account web@; batch: batch@) behind
// internal passthrough Network Load Balancers whose one backend is web:
// a UDP one on port 5353 (a UDP echo on web) and a TCP one on port 22
// (web's sshd). Each VM's connections to the rules' VPC addresses go
// through the identity relay, so its egress firewall rules apply there,
// as Google applies them to a VM's traffic to a load balancer:
//   - a UDP egress deny targeting batch@ refuses batch's datagrams and not
//     web's (the relay carries UDP with the VM's identity);
//   - a TCP egress deny for every instance refuses both, and an allow of
//     a higher priority on the web tag lets web through;
//   - each rule's Firewall Rules Logging records appear in Cloud Logging
//     (compute.googleapis.com%2Ffirewall, gce_subnetwork), DENIED and
//     ALLOWED, read with the official Logging client.
//
// covers: compute.firewalls.insert, compute.firewalls.get, compute.firewalls.delete, google.logging.v2.LoggingServiceV2/ListLogEntries
func TestRelayedConnectionsFollowTheEgressFirewall(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvCompute) // skips without it
	logAddr := h.Endpoint(EnvLogging)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	sfx := time.Now().UnixNano() % 1e6
	n := func(s string) string { return fmt.Sprintf("rfw-%s-%d", s, sfx) }
	webSA, batchSA := "web@"+p+".iam.gserviceaccount.com", "batch@"+p+".iam.gserviceaccount.com"
	start := time.Now()
	wait := func(what string, op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nets.Close() })
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	t.Cleanup(func() { _ = subs.Close() })
	o, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(n("net")), AutoCreateSubnetworks: proto.Bool(false)}})
	wait("the network", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: n("net")})
	})
	netRel := "projects/" + p + "/global/networks/" + n("net")
	subRel := "projects/" + p + "/regions/" + region + "/subnetworks/" + n("sub")
	o, err = subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(n("sub")), IpCidrRange: proto.String("10.83.0.0/24"), Network: proto.String(netRel)}})
	wait("the subnetwork", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: n("sub")})
	})
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	t.Cleanup(func() { _ = fws.Close() })
	firewall := func(f *computepb.Firewall) {
		t.Helper()
		f.Network = proto.String(netRel)
		o, err := fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: f})
		wait("the firewall rule "+f.GetName(), o, err)
		name := f.GetName()
		later(t, func(c context.Context) (*computeapi.Operation, error) {
			return fws.Delete(c, &computepb.DeleteFirewallRequest{Project: p, Firewall: name})
		})
	}
	// The network's ingress: the VMs' own traffic and the health checks.
	firewall(&computepb.Firewall{Name: proto.String(n("in")), Direction: proto.String("INGRESS"), SourceRanges: []string{"0.0.0.0/0"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("all")}}})

	// The VMs.
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	t.Cleanup(func() { _ = insts.Close() })
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	keyLine := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"
	type vmSpec struct {
		sa   string
		tags []string
	}
	vms := map[string]vmSpec{n("web"): {webSA, []string{"web"}}, n("batch"): {batchSA, nil}}
	var wg sync.WaitGroup
	for name, spec := range vms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
				Name:        proto.String(name),
				MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
				Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
					InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
				NetworkInterfaces: []*computepb.NetworkInterface{{Subnetwork: proto.String(subRel)}},
				Tags:              &computepb.Tags{Items: spec.tags},
				ServiceAccounts:   []*computepb.ServiceAccount{{Email: proto.String(spec.sa), Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}}},
				Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(keyLine)}}},
			}})
			if err == nil {
				err = o.Wait(ctx)
			}
			if err != nil {
				t.Errorf("the instance %s: %v", name, err)
			}
		}()
	}
	t.Cleanup(func() {
		var wg sync.WaitGroup
		for name := range vms {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if o, err := insts.Delete(context.Background(), &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: name}); err == nil {
					_ = o.Wait(context.Background())
				}
			}()
		}
		wg.Wait()
	})
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	// web's instance group, the health check and the two internal
	// passthrough load balancers.
	igs, _ := computeapi.NewInstanceGroupsRESTClient(ctx, opts...)
	t.Cleanup(func() { _ = igs.Close() })
	rhcs, _ := computeapi.NewRegionHealthChecksRESTClient(ctx, opts...)
	t.Cleanup(func() { _ = rhcs.Close() })
	rbss, _ := computeapi.NewRegionBackendServicesRESTClient(ctx, opts...)
	t.Cleanup(func() { _ = rbss.Close() })
	frs, _ := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	t.Cleanup(func() { _ = frs.Close() })
	o, err = igs.Insert(ctx, &computepb.InsertInstanceGroupRequest{Project: p, Zone: zone, InstanceGroupResource: &computepb.InstanceGroup{
		Name: proto.String(n("ig")), Network: proto.String(netRel)}})
	wait("the instance group", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return igs.Delete(c, &computepb.DeleteInstanceGroupRequest{Project: p, Zone: zone, InstanceGroup: n("ig")})
	})
	o, err = igs.AddInstances(ctx, &computepb.AddInstancesInstanceGroupRequest{Project: p, Zone: zone, InstanceGroup: n("ig"),
		InstanceGroupsAddInstancesRequestResource: &computepb.InstanceGroupsAddInstancesRequest{
			Instances: []*computepb.InstanceReference{{Instance: proto.String("zones/" + zone + "/instances/" + n("web"))}}}})
	wait("web in the instance group", o, err)
	o, err = rhcs.Insert(ctx, &computepb.InsertRegionHealthCheckRequest{Project: p, Region: region, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(n("hc")), Type: proto.String("TCP"), CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2),
		TcpHealthCheck: &computepb.TCPHealthCheck{Port: proto.Int32(22)}}})
	wait("the health check", o, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return rhcs.Delete(c, &computepb.DeleteRegionHealthCheckRequest{Project: p, Region: region, HealthCheck: n("hc")})
	})
	vips := map[string]string{}
	for _, lb := range []struct{ name, proto, port string }{{"udp", "UDP", "5353"}, {"tcp", "TCP", "22"}} {
		o, err = rbss.Insert(ctx, &computepb.InsertRegionBackendServiceRequest{Project: p, Region: region, BackendServiceResource: &computepb.BackendService{
			Name: proto.String(n(lb.name)), Protocol: proto.String(lb.proto), LoadBalancingScheme: proto.String("INTERNAL"), Network: proto.String(netRel),
			HealthChecks: []string{"regions/" + region + "/healthChecks/" + n("hc")},
			Backends:     []*computepb.Backend{{Group: proto.String("zones/" + zone + "/instanceGroups/" + n("ig")), BalancingMode: proto.String("CONNECTION")}}}})
		wait("the backend service "+lb.name, o, err)
		bs := n(lb.name)
		later(t, func(c context.Context) (*computeapi.Operation, error) {
			return rbss.Delete(c, &computepb.DeleteRegionBackendServiceRequest{Project: p, Region: region, BackendService: bs})
		})
		o, err = frs.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
			Name: proto.String(n(lb.name)), LoadBalancingScheme: proto.String("INTERNAL"), IPProtocol: proto.String(lb.proto), Ports: []string{lb.port},
			Network: proto.String(netRel), Subnetwork: proto.String(subRel), BackendService: proto.String("regions/" + region + "/backendServices/" + bs)}})
		wait("the forwarding rule "+lb.name, o, err)
		later(t, func(c context.Context) (*computeapi.Operation, error) {
			return frs.Delete(c, &computepb.DeleteForwardingRuleRequest{Project: p, Region: region, ForwardingRule: bs})
		})
		r, err := frs.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: bs})
		if err != nil {
			t.Fatal(err)
		}
		vips[lb.name] = r.GetIPAddress()
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
	// The UDP echo on web.
	run(n("web"), "sudo systemd-run --unit udpecho socat UDP4-RECVFROM:5353,fork EXEC:cat >/dev/null 2>&1 || true")
	udp := func(vm string) string {
		return strings.TrimSpace(run(vm, "printf ping | timeout 6 socat -t 4 - UDP4:"+vips["udp"]+":5353 2>/dev/null || true"))
	}
	tcp := func(vm string) string {
		out := run(vm, "timeout 6 socat -t 3 - TCP4:"+vips["tcp"]+":22 </dev/null 2>/dev/null | head -c 7 || true")
		if strings.HasPrefix(out, "SSH-2.0") {
			return "open"
		}
		return "refused"
	}
	expect := func(what string, probe func(string) string, want map[string]string) {
		t.Helper()
		got := map[string]string{}
		same := func() bool {
			ok := true
			for vm, w := range want {
				got[vm] = probe(vm)
				if got[vm] != w {
					ok = false
				}
			}
			return ok
		}
		deadline := time.Now().Add(3 * time.Minute)
		for !same() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)
		}
		if !same() {
			t.Fatalf("%s: got %v, want %v; batch's relay: %s", what, got, want,
				run(n("batch"), "sudo nft list chain inet cloudburrow relay; ss -lun | grep ':470'; ps -eo args | grep [u]dprelay; "+
					"sudo journalctl -u cloudburrow-vpc --no-pager | tail -5"))
		}
	}
	const echoed = "ping"
	expect("both VMs reach the UDP load balancer", udp, map[string]string{n("web"): echoed, n("batch"): echoed})
	expect("both VMs reach the TCP load balancer", tcp, map[string]string{n("web"): "open", n("batch"): "open"})

	// A UDP egress deny by service account: batch's flow is refused at the
	// relay, web's still carried.
	firewall(&computepb.Firewall{Name: proto.String(n("deny-udp")), Direction: proto.String("EGRESS"), Priority: proto.Int32(1000),
		TargetServiceAccounts: []string{batchSA}, DestinationRanges: []string{vips["udp"] + "/32"},
		Denied:    []*computepb.Denied{{IPProtocol: proto.String("udp"), Ports: []string{"5353"}}},
		LogConfig: &computepb.FirewallLogConfig{Enable: proto.Bool(true)}})
	got, err := fws.Get(ctx, &computepb.GetFirewallRequest{Project: p, Firewall: n("deny-udp")})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetLogConfig().GetMetadata() != "INCLUDE_ALL_METADATA" {
		t.Errorf("logConfig: %v", got.GetLogConfig())
	}
	expect("the UDP deny for batch@", udp, map[string]string{n("web"): echoed, n("batch"): ""})

	// A TCP egress deny for every instance, then a higher-priority allow
	// on web's tag.
	firewall(&computepb.Firewall{Name: proto.String(n("deny-tcp")), Direction: proto.String("EGRESS"), Priority: proto.Int32(1000),
		DestinationRanges: []string{vips["tcp"] + "/32"},
		Denied:            []*computepb.Denied{{IPProtocol: proto.String("tcp"), Ports: []string{"22"}}},
		LogConfig:         &computepb.FirewallLogConfig{Enable: proto.Bool(true)}})
	expect("the TCP deny", tcp, map[string]string{n("web"): "refused", n("batch"): "refused"})
	firewall(&computepb.Firewall{Name: proto.String(n("allow-web")), Direction: proto.String("EGRESS"), Priority: proto.Int32(900),
		TargetTags: []string{"web"}, DestinationRanges: []string{vips["tcp"] + "/32"},
		Allowed:   []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"22"}}},
		LogConfig: &computepb.FirewallLogConfig{Enable: proto.Bool(true), Metadata: proto.String("EXCLUDE_ALL_METADATA")}})
	expect("the higher-priority allow for web", tcp, map[string]string{n("web"): "open", n("batch"): "refused"})

	// The firewall logs.
	ac, err := logadmin.NewClient(ctx, "projects/"+p, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	read := func(rule, disposition string) []map[string]any {
		filter := `logName = "projects/` + p + `/logs/compute.googleapis.com%2Ffirewall" AND resource.type = "gce_subnetwork" AND ` +
			`jsonPayload.rule_details.reference = "network:` + n("net") + `/firewall:` + rule + `" AND jsonPayload.disposition = "` + disposition +
			`" AND timestamp >= "` + start.Add(-time.Minute).UTC().Format(time.RFC3339) + `"`
		var out []map[string]any
		it := ac.Entries(ctx, logadmin.Filter(filter))
		for {
			e, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("the firewall log: %v", err)
			}
			var pl map[string]any
			switch v := e.Payload.(type) {
			case map[string]any:
				pl = v
			case *structpb.Struct:
				pl = v.AsMap()
			}
			if e.Resource.GetLabels()["subnetwork_name"] != n("sub") || e.Resource.GetLabels()["location"] != region {
				t.Errorf("the record's resource: %v", e.Resource)
			}
			out = append(out, pl)
		}
	}
	denied := read(n("deny-udp"), "DENIED")
	if len(denied) == 0 {
		t.Fatal("no DENIED record of the UDP deny rule")
	}
	c := denied[0]["connection"].(map[string]any)
	inst, _ := denied[0]["instance"].(map[string]any)
	if c["dest_ip"] != vips["udp"] || c["dest_port"] != float64(5353) || c["protocol"] != float64(17) || inst["vm_name"] != n("batch") {
		t.Errorf("the UDP deny's record: %v", denied[0])
	}
	if len(read(n("deny-tcp"), "DENIED")) == 0 {
		t.Error("no DENIED record of the TCP deny rule")
	}
	allowed := read(n("allow-web"), "ALLOWED")
	if len(allowed) == 0 {
		t.Fatal("no ALLOWED record of the allow rule")
	}
	if _, ok := allowed[0]["instance"]; ok {
		t.Errorf("EXCLUDE_ALL_METADATA's record has the instance: %v", allowed[0])
	}
	if c := allowed[0]["connection"].(map[string]any); c["dest_port"] != float64(22) || c["protocol"] != float64(6) {
		t.Errorf("the allow's record: %v", allowed[0])
	}
}
