//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// ipv6Web serves "V6-<hostname>" on [::]:8080, IPv6 only. It is a script
// file, as lbWebScript is: systemd-run would expand ${...} in an inline
// command line itself.
const ipv6Web = `sudo tee /usr/local/bin/web6 >/dev/null <<'EOF'
#!/bin/bash
b="V6-$(hostname)"
while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' ${#b} "$b" | nc -6 -lN 8080 >/dev/null 2>&1; done
EOF
sudo chmod +x /usr/local/bin/web6 && sudo systemd-run --unit=web6 /usr/local/bin/web6`

// covers: compute.subnetworks.insert, compute.subnetworks.get, compute.subnetworks.patch, compute.subnetworks.delete, compute.instances.insert, compute.instances.get, compute.globalAddresses.insert, compute.globalAddresses.get, compute.globalAddresses.delete, compute.globalForwardingRules.insert, compute.globalForwardingRules.get, compute.globalForwardingRules.delete
//
// TestIPv6OnlyVMsAndGlobalIPv6LoadBalancing2140 (#2140), the official
// compute/apiv1 REST clients, as vpc/docs/subnets#ipv6-only,
// compute/docs/instances/create-ipv6-instance and
// vpc/docs/reserve-static-external-ip-address set them up: a custom mode
// network with ULA internal IPv6, an IPv6-only subnet (no ipCidrRange) and
// a dual-stack one, an IPv6-only VM on each and a dual-stack VM, and a
// firewall rule allowing the network's /48. The IPv6-only VMs have no IPv4
// address on vpc0, the metadata server lists their ipv6s, and they and the
// dual-stack VM reach each other over IPv6 by address and by internal DNS
// name; an IPv6-only subnet's stack type does not change. Then a global
// external IPv6 address and a global external Application Load Balancer
// of the dual-stack VM on it: the rule has ipVersion IPV6 and the address,
// which is IN_USE, and its listener answers from the VM.
func TestIPv6OnlyVMsAndGlobalIPv6LoadBalancing2140(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	computeAddr := h.Endpoint(EnvCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	nets, _ := computeapi.NewNetworksRESTClient(ctx, opts...)
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	gas, _ := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	gfrs, _ := computeapi.NewGlobalForwardingRulesRESTClient(ctx, opts...)
	igs, _ := computeapi.NewInstanceGroupsRESTClient(ctx, opts...)
	hcs, _ := computeapi.NewHealthChecksRESTClient(ctx, opts...)
	bss, _ := computeapi.NewBackendServicesRESTClient(ctx, opts...)
	ums, _ := computeapi.NewUrlMapsRESTClient(ctx, opts...)
	thps, _ := computeapi.NewTargetHttpProxiesRESTClient(ctx, opts...)
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	later := func(f func(c context.Context) (*computeapi.Operation, error)) {
		t.Cleanup(func() {
			if op, err := f(context.Background()); err == nil {
				_ = op.Wait(context.Background())
			}
		})
	}
	nw := "ct-v6-" + sfx
	nwRel := "projects/" + p + "/global/networks/" + nw
	wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(nw), AutoCreateSubnetworks: proto.Bool(false), EnableUlaInternalIpv6: proto.Bool(true)}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: nw})
	})
	n, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: p, Network: nw})
	if err != nil {
		t.Fatal(err)
	}
	only6, dual := nw+"-only6", nw+"-dual"
	wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(only6), Network: proto.String(nwRel), StackType: proto.String("IPV6_ONLY"), Ipv6AccessType: proto.String("INTERNAL")}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: only6})
	})
	wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(dual), Network: proto.String(nwRel), IpCidrRange: proto.String("10.96.0.0/24"),
		StackType: proto.String("IPV4_IPV6"), Ipv6AccessType: proto.String("INTERNAL")}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: dual})
	})
	s6, err := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: region, Subnetwork: only6})
	if err != nil || s6.GetStackType() != "IPV6_ONLY" || s6.GetIpCidrRange() != "" || !strings.HasSuffix(s6.GetIpv6CidrRange(), "/64") {
		t.Fatalf("the IPv6-only subnet: %v %v", s6, err)
	}
	if _, err := subs.Patch(ctx, &computepb.PatchSubnetworkRequest{Project: p, Region: region, Subnetwork: only6,
		SubnetworkResource: &computepb.Subnetwork{StackType: proto.String("IPV4_IPV6"), Fingerprint: s6.Fingerprint}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("changing an IPv6-only subnet's stack type: %v", err)
	}
	fw := nw + "-in"
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(fw), Network: proto.String(nwRel), SourceRanges: []string{n.GetInternalIpv6Range(), "10.96.0.0/24", "35.191.0.0/16", "130.211.0.0/22"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"80", "8080"}}}}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return fws.Delete(c, &computepb.DeleteFirewallRequest{Project: p, Firewall: fw})
	})

	vms := newVPCVMs(ctx, t, h, zone)
	a, b, c := "ct-v6a-"+sfx, "ct-v6b-"+sfx, "ct-v6c-"+sfx
	vms.make(map[string]*computepb.NetworkInterface{
		a: {Subnetwork: proto.String("regions/" + region + "/subnetworks/" + only6), StackType: proto.String("IPV6_ONLY")},
		b: {Subnetwork: proto.String("regions/" + region + "/subnetworks/" + dual), StackType: proto.String("IPV6_ONLY")},
		c: {Subnetwork: proto.String("regions/" + region + "/subnetworks/" + dual), StackType: proto.String("IPV4_IPV6")},
	})
	v6 := map[string]string{}
	for _, vm := range []string{a, b, c} {
		i, err := vms.insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: vm})
		if err != nil {
			t.Fatal(err)
		}
		nic := i.GetNetworkInterfaces()[0]
		if (vm != c && (nic.GetStackType() != "IPV6_ONLY" || nic.GetNetworkIP() != "")) || nic.GetIpv6Address() == "" {
			t.Fatalf("%s's interface: %v", vm, nic)
		}
		v6[vm] = nic.GetIpv6Address()
	}
	for _, vm := range []string{a, b, c} {
		vms.run(vm, ipv6Web)
	}
	if out := vms.eventually("a's IPv6 address on vpc0", a, "ip -6 -o addr show dev vpc0 scope global", v6[a]); strings.Contains(vms.run(a, "ip -4 -o addr show dev vpc0"), "inet ") {
		t.Errorf("the IPv6-only VM has an IPv4 address on vpc0: %s", out)
	}
	vms.eventually("the metadata server's ipv6s", a,
		// The instance's own metadata server, which GCE_METADATA_HOST
		// names in the guest (#2177).
		`curl -s -H 'Metadata-Flavor: Google' "http://$GCE_METADATA_HOST/computeMetadata/v1/instance/network-interfaces/0/ipv6s"`, v6[a])
	curl6 := func(ip string) string { return "curl -s -m 5 -g http://[" + ip + "]:8080/ || echo UNREACHABLE" }
	vms.eventually("the IPv6-only VMs reach each other", a, curl6(v6[b]), "V6-"+b)
	vms.eventually("the dual-stack VM reaches an IPv6-only one", c, curl6(v6[a]), "V6-"+a)
	vms.eventually("an IPv6-only VM reaches the dual-stack one", b, curl6(v6[c]), "V6-"+c)
	vms.eventually("internal DNS names an IPv6-only VM by its IPv6 address", c,
		"curl -s -m 5 http://"+a+"."+zone+".c."+p+".internal:8080/ || echo UNREACHABLE", "V6-"+a)

	// A global external IPv6 address and a global external Application
	// Load Balancer of c on it.
	addr := nw + "-addr"
	wait(gas.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: p, AddressResource: &computepb.Address{
		Name: proto.String(addr), IpVersion: proto.String("IPV6")}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return gas.Delete(c, &computepb.DeleteGlobalAddressRequest{Project: p, Address: addr})
	})
	ga, err := gas.Get(ctx, &computepb.GetGlobalAddressRequest{Project: p, Address: addr})
	if err != nil || ga.GetIpVersion() != "IPV6" || ga.GetAddressType() != "EXTERNAL" || !strings.Contains(ga.GetAddress(), ":") {
		t.Fatalf("the global IPv6 address: %v %v", ga, err)
	}
	group := nw + "-ig"
	wait(igs.Insert(ctx, &computepb.InsertInstanceGroupRequest{Project: p, Zone: zone, InstanceGroupResource: &computepb.InstanceGroup{
		Name: proto.String(group), Network: proto.String(nwRel), NamedPorts: []*computepb.NamedPort{{Name: proto.String("http"), Port: proto.Int32(80)}}}}))
	later(func(ctx context.Context) (*computeapi.Operation, error) {
		return igs.Delete(ctx, &computepb.DeleteInstanceGroupRequest{Project: p, Zone: zone, InstanceGroup: group})
	})
	wait(igs.AddInstances(ctx, &computepb.AddInstancesInstanceGroupRequest{Project: p, Zone: zone, InstanceGroup: group,
		InstanceGroupsAddInstancesRequestResource: &computepb.InstanceGroupsAddInstancesRequest{
			Instances: []*computepb.InstanceReference{{Instance: proto.String("zones/" + zone + "/instances/" + c)}}}}))
	hc := nw + "-hc"
	wait(hcs.Insert(ctx, &computepb.InsertHealthCheckRequest{Project: p, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(hc), Type: proto.String("HTTP"), CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2),
		HttpHealthCheck: &computepb.HTTPHealthCheck{PortSpecification: proto.String("USE_SERVING_PORT"), RequestPath: proto.String("/")}}}))
	later(func(ctx context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(ctx, &computepb.DeleteHealthCheckRequest{Project: p, HealthCheck: hc})
	})
	bs := nw + "-bs"
	wait(bss.Insert(ctx, &computepb.InsertBackendServiceRequest{Project: p, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(bs), Protocol: proto.String("HTTP"), PortName: proto.String("http"),
		LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"), HealthChecks: []string{"global/healthChecks/" + hc},
		Backends: []*computepb.Backend{{Group: proto.String("zones/" + zone + "/instanceGroups/" + group), BalancingMode: proto.String("UTILIZATION")}}}}))
	later(func(ctx context.Context) (*computeapi.Operation, error) {
		return bss.Delete(ctx, &computepb.DeleteBackendServiceRequest{Project: p, BackendService: bs})
	})
	um := nw + "-map"
	wait(ums.Insert(ctx, &computepb.InsertUrlMapRequest{Project: p, UrlMapResource: &computepb.UrlMap{
		Name: proto.String(um), DefaultService: proto.String("global/backendServices/" + bs)}}))
	later(func(ctx context.Context) (*computeapi.Operation, error) {
		return ums.Delete(ctx, &computepb.DeleteUrlMapRequest{Project: p, UrlMap: um})
	})
	px := nw + "-proxy"
	wait(thps.Insert(ctx, &computepb.InsertTargetHttpProxyRequest{Project: p, TargetHttpProxyResource: &computepb.TargetHttpProxy{
		Name: proto.String(px), UrlMap: proto.String("global/urlMaps/" + um)}}))
	later(func(ctx context.Context) (*computeapi.Operation, error) {
		return thps.Delete(ctx, &computepb.DeleteTargetHttpProxyRequest{Project: p, TargetHttpProxy: px})
	})
	rn := nw + "-rule"
	wait(gfrs.Insert(ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: p, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(rn), Target: proto.String("global/targetHttpProxies/" + px), PortRange: proto.String("80"),
		LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"), IPAddress: proto.String("global/addresses/" + addr), IpVersion: proto.String("IPV6")}}))
	later(func(ctx context.Context) (*computeapi.Operation, error) {
		return gfrs.Delete(ctx, &computepb.DeleteGlobalForwardingRuleRequest{Project: p, ForwardingRule: rn})
	})
	rule, err := gfrs.Get(ctx, &computepb.GetGlobalForwardingRuleRequest{Project: p, ForwardingRule: rn})
	if err != nil || rule.GetIpVersion() != "IPV6" || rule.GetIPAddress() != ga.GetAddress() {
		t.Fatalf("the IPv6 forwarding rule: %v %v", rule, err)
	}
	if ga, err := gas.Get(ctx, &computepb.GetGlobalAddressRequest{Project: p, Address: addr}); err != nil || ga.GetStatus() != "IN_USE" {
		t.Errorf("the address in use: %v %v", ga, err)
	}
	resp, err := http.Get("http://" + computeAddr + "/compute/v1/projects/" + p + "/global/forwardingRules/" + rn)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	resp.Body.Close()
	local, _ := raw["cloudburrowAddress"].(string)
	if local == "" {
		t.Fatalf("the rule's listener: %v", raw)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		r, err := http.Get("http://" + local + "/")
		var body []byte
		if err == nil {
			body, _ = io.ReadAll(r.Body)
			r.Body.Close()
		}
		if err == nil && strings.Contains(string(body), "VM-"+c) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the IPv6 load balancer: %v %q", err, body)
		}
		time.Sleep(3 * time.Second)
	}
}
