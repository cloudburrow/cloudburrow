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

// covers: compute.forwardingRules.insert, compute.forwardingRules.get, compute.forwardingRules.delete, compute.regionBackendServices.insert, compute.regionHealthChecks.insert
//
// TestRegionalAndInternalIPv6ForwardingRules2140 (#2140), the official
// compute/apiv1 REST clients, as load-balancing/docs/forwarding-rule-concepts
// describes IPv6 rules: in a custom mode network with ULA internal IPv6, a
// dual-stack INTERNAL subnet and a dual-stack EXTERNAL one, a web VM in an
// unmanaged instance group behind an internal passthrough Network Load
// Balancer whose rule has ipVersion IPV6 (a /96 of the internal subnet's
// range) and a backend service-based external passthrough one whose rule
// has ipVersion IPV6 (a /96 of the external subnet's range, Premium Tier).
// A dual-stack client VM reaches the web VM at the internal rule's IPv6
// address and port; an egress rule denying that address stops it; the
// external rule's listener answers from the web VM.
func TestRegionalAndInternalIPv6ForwardingRules2140(t *testing.T) {
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
	igs, _ := computeapi.NewInstanceGroupsRESTClient(ctx, opts...)
	hcs, _ := computeapi.NewRegionHealthChecksRESTClient(ctx, opts...)
	bss, _ := computeapi.NewRegionBackendServicesRESTClient(ctx, opts...)
	frs, _ := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
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
	nw := "ct-lb6-" + sfx
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
	in6, ex6 := nw+"-in6", nw+"-ex6"
	for _, s := range []struct{ name, cidr, access string }{{in6, "10.93.0.0/24", "INTERNAL"}, {ex6, "10.94.0.0/24", "EXTERNAL"}} {
		wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
			Name: proto.String(s.name), Network: proto.String(nwRel), IpCidrRange: proto.String(s.cidr),
			StackType: proto.String("IPV4_IPV6"), Ipv6AccessType: proto.String(s.access)}}))
		later(func(c context.Context) (*computeapi.Operation, error) {
			return subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: s.name})
		})
	}
	fw := nw + "-in"
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(fw), Network: proto.String(nwRel), SourceRanges: []string{n.GetInternalIpv6Range(), "10.93.0.0/24", "35.191.0.0/16", "130.211.0.0/22"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"80"}}}}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return fws.Delete(c, &computepb.DeleteFirewallRequest{Project: p, Firewall: fw})
	})

	vms := newVPCVMs(ctx, t, h, zone)
	web, client := "ct-lb6web-"+sfx, "ct-lb6cli-"+sfx
	dualNIC := func() *computepb.NetworkInterface {
		return &computepb.NetworkInterface{Subnetwork: proto.String("regions/" + region + "/subnetworks/" + in6), StackType: proto.String("IPV4_IPV6")}
	}
	vms.make(map[string]*computepb.NetworkInterface{web: dualNIC(), client: dualNIC()})
	group := nw + "-ig"
	wait(igs.Insert(ctx, &computepb.InsertInstanceGroupRequest{Project: p, Zone: zone, InstanceGroupResource: &computepb.InstanceGroup{
		Name: proto.String(group), Network: proto.String(nwRel)}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return igs.Delete(c, &computepb.DeleteInstanceGroupRequest{Project: p, Zone: zone, InstanceGroup: group})
	})
	wait(igs.AddInstances(ctx, &computepb.AddInstancesInstanceGroupRequest{Project: p, Zone: zone, InstanceGroup: group,
		InstanceGroupsAddInstancesRequestResource: &computepb.InstanceGroupsAddInstancesRequest{
			Instances: []*computepb.InstanceReference{{Instance: proto.String("zones/" + zone + "/instances/" + web)}}}}))
	scope := "projects/" + p + "/regions/" + region + "/"
	hc := nw + "-hc"
	wait(hcs.Insert(ctx, &computepb.InsertRegionHealthCheckRequest{Project: p, Region: region, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(hc), Type: proto.String("TCP"), TcpHealthCheck: &computepb.TCPHealthCheck{Port: proto.Int32(80)},
		CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2)}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(c, &computepb.DeleteRegionHealthCheckRequest{Project: p, Region: region, HealthCheck: hc})
	})
	backends := []*computepb.Backend{{Group: proto.String("projects/" + p + "/zones/" + zone + "/instanceGroups/" + group), BalancingMode: proto.String("CONNECTION")}}
	for _, b := range []struct{ name, scheme string }{{nw + "-ilb", "INTERNAL"}, {nw + "-xlb", "EXTERNAL"}} {
		bs := &computepb.BackendService{Name: proto.String(b.name), Protocol: proto.String("TCP"), LoadBalancingScheme: proto.String(b.scheme),
			HealthChecks: []string{scope + "healthChecks/" + hc}, Backends: backends}
		if b.scheme == "INTERNAL" {
			bs.Network = proto.String(nwRel)
		}
		wait(bss.Insert(ctx, &computepb.InsertRegionBackendServiceRequest{Project: p, Region: region, BackendServiceResource: bs}))
		later(func(c context.Context) (*computeapi.Operation, error) {
			return bss.Delete(c, &computepb.DeleteRegionBackendServiceRequest{Project: p, Region: region, BackendService: b.name})
		})
	}
	ilb, xlb := nw+"-ilb", nw+"-xlb"
	wait(frs.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(ilb), LoadBalancingScheme: proto.String("INTERNAL"), IPProtocol: proto.String("TCP"), Ports: []string{"80"},
		IpVersion: proto.String("IPV6"), Network: proto.String(nwRel), Subnetwork: proto.String("regions/" + region + "/subnetworks/" + in6),
		BackendService: proto.String(scope + "backendServices/" + ilb)}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return frs.Delete(c, &computepb.DeleteForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ilb})
	})
	wait(frs.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(xlb), LoadBalancingScheme: proto.String("EXTERNAL"), IPProtocol: proto.String("TCP"), PortRange: proto.String("80"),
		IpVersion: proto.String("IPV6"), Subnetwork: proto.String("regions/" + region + "/subnetworks/" + ex6),
		BackendService: proto.String(scope + "backendServices/" + xlb)}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return frs.Delete(c, &computepb.DeleteForwardingRuleRequest{Project: p, Region: region, ForwardingRule: xlb})
	})
	ir, err := frs.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ilb})
	if err != nil || ir.GetIpVersion() != "IPV6" || !strings.HasSuffix(ir.GetIPAddress(), "/96") || !strings.HasSuffix(ir.GetSubnetwork(), "/subnetworks/"+in6) {
		t.Fatalf("the internal IPv6 rule: %v %v", ir, err)
	}
	xr, err := frs.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: xlb})
	if err != nil || xr.GetIpVersion() != "IPV6" || !strings.HasSuffix(xr.GetIPAddress(), "/96") || xr.GetNetworkTier() != "PREMIUM" {
		t.Fatalf("the external IPv6 rule: %v %v", xr, err)
	}
	vip := strings.TrimSuffix(ir.GetIPAddress(), "/96")
	curl := "curl -s -m 5 -g http://[" + vip + "]:80/ || echo UNREACHABLE"
	vms.eventually("the client reaches the internal IPv6 load balancer", client, curl, "VM-"+web)

	// The external rule, on its listener.
	resp, err := http.Get("http://" + computeAddr + "/compute/v1/projects/" + p + "/regions/" + region + "/forwardingRules/" + xlb)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	resp.Body.Close()
	local, _ := raw["cloudburrowAddress"].(string)
	if local == "" {
		t.Fatalf("the external rule's listener: %v", raw)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		r, err := http.Get("http://" + local + "/")
		var body []byte
		if err == nil {
			body, _ = io.ReadAll(r.Body)
			r.Body.Close()
		}
		if err == nil && strings.Contains(string(body), "VM-"+web) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the external IPv6 load balancer: %v %q", err, body)
		}
		time.Sleep(3 * time.Second)
	}

	// An egress rule denying the internal rule's address applies to it.
	deny := nw + "-no-ilb6"
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(deny), Network: proto.String(nwRel), Direction: proto.String("EGRESS"), Priority: proto.Int32(100),
		DestinationRanges: []string{vip + "/128"}, Denied: []*computepb.Denied{{IPProtocol: proto.String("tcp")}}}}))
	later(func(c context.Context) (*computeapi.Operation, error) {
		return fws.Delete(c, &computepb.DeleteFirewallRequest{Project: p, Firewall: deny})
	})
	vms.eventually("an egress deny of the internal IPv6 rule", client, curl, "UNREACHABLE")
}
