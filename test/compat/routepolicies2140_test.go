//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// TestPrivateNATAndRoutePoliciesThroughTheGoClient (#2140), the official
// compute/apiv1 REST client: a Private NAT gateway (type PRIVATE) of a
// subnet, translating to a PRIVATE_NAT subnet by a nexthop.is_hybrid rule,
// beside a public gateway of the same subnet, with dynamic ports by
// default and NAT IPs refused; route policies created by
// updateRoutePolicy, read by getRoutePolicy and listRoutePolicies, patched
// under their fingerprint (a stale one 412), used by a BGP peer's
// importPolicies (an export policy refused), not deletable while used,
// then deleted.
// covers: compute.routers.updateRoutePolicy, compute.routers.getRoutePolicy, compute.routers.listRoutePolicies, compute.routers.patchRoutePolicy, compute.routers.deleteRoutePolicy
func TestPrivateNATAndRoutePoliciesThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	routers, _ := computeapi.NewRoutersRESTClient(ctx, opts...)
	p := h.Project()
	const region = "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	netName, rName := "ct-pnat-"+sfx, "ct-rp-"+sfx
	wait := func(op *computeapi.Operation, err error) error {
		t.Helper()
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		if op, err := routers.Delete(c, &computepb.DeleteRouterRequest{Project: p, Region: region, Router: rName}); err == nil {
			_ = op.Wait(c)
		}
		for _, s := range []string{"-a", "-nat"} {
			if op, err := subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: netName + s}); err == nil {
				_ = op.Wait(c)
			}
		}
		_, _ = nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: netName})
	})
	must(wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(netName), AutoCreateSubnetworks: proto.Bool(false)}})))
	network := "global/networks/" + netName
	must(wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(netName + "-a"), Network: proto.String(network), IpCidrRange: proto.String("10.81.0.0/24")}})))
	must(wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(netName + "-nat"), Network: proto.String(network), IpCidrRange: proto.String("100.81.0.0/24"),
		Purpose: proto.String("PRIVATE_NAT")}})))
	subA := "regions/" + region + "/subnetworks/" + netName + "-a"
	natSub := "regions/" + region + "/subnetworks/" + netName + "-nat"
	private := func(withIPs bool) *computepb.RouterNat {
		n := &computepb.RouterNat{Name: proto.String("pn"), Type: proto.String("PRIVATE"),
			SourceSubnetworkIpRangesToNat: proto.String("LIST_OF_SUBNETWORKS"),
			Subnetworks:                   []*computepb.RouterNatSubnetworkToNat{{Name: proto.String(subA), SourceIpRangesToNat: []string{"ALL_IP_RANGES"}}},
			Rules: []*computepb.RouterNatRule{{RuleNumber: proto.Uint32(100), Match: proto.String("nexthop.is_hybrid"),
				Action: &computepb.RouterNatRuleAction{SourceNatActiveRanges: []string{natSub}}}}}
		if withIPs {
			n.NatIpAllocateOption = proto.String("AUTO_ONLY")
		}
		return n
	}
	public := &computepb.RouterNat{Name: proto.String("pub"), SourceSubnetworkIpRangesToNat: proto.String("LIST_OF_SUBNETWORKS"),
		Subnetworks:         []*computepb.RouterNatSubnetworkToNat{{Name: proto.String(subA)}},
		NatIpAllocateOption: proto.String("AUTO_ONLY")}
	router := func(n *computepb.RouterNat) *computepb.Router {
		return &computepb.Router{Name: proto.String(rName), Network: proto.String(network), Bgp: &computepb.RouterBgp{Asn: proto.Uint32(64512)},
			Interfaces: []*computepb.RouterInterface{{Name: proto.String("if0"), IpRange: proto.String("169.254.81.1/30")}},
			BgpPeers: []*computepb.RouterBgpPeer{{Name: proto.String("peer0"), InterfaceName: proto.String("if0"), PeerAsn: proto.Uint32(65001),
				PeerIpAddress: proto.String("169.254.81.2")}},
			Nats: []*computepb.RouterNat{public, n}}
	}
	if err := wait(routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: router(private(true))})); err == nil {
		t.Fatal("a Private NAT gateway with NAT IP allocation was accepted")
	}
	must(wait(routers.Insert(ctx, &computepb.InsertRouterRequest{Project: p, Region: region, RouterResource: router(private(false))})))
	r, err := routers.Get(ctx, &computepb.GetRouterRequest{Project: p, Region: region, Router: rName})
	must(err)
	var pn *computepb.RouterNat
	for _, n := range r.GetNats() {
		if n.GetName() == "pn" {
			pn = n
		}
	}
	if pn == nil || pn.GetType() != "PRIVATE" || !pn.GetEnableDynamicPortAllocation() || len(pn.GetRules()) != 1 || len(pn.GetRules()[0].GetAction().GetSourceNatActiveRanges()) != 1 {
		t.Fatalf("private gateway = %v", pn)
	}

	// Route policies.
	imp, exp := computepb.RoutePolicy_ROUTE_POLICY_TYPE_IMPORT.String(), computepb.RoutePolicy_ROUTE_POLICY_TYPE_EXPORT.String()
	upd := func(rp *computepb.RoutePolicy) error {
		return wait(routers.UpdateRoutePolicy(ctx, &computepb.UpdateRoutePolicyRouterRequest{Project: p, Region: region, Router: rName, RoutePolicyResource: rp}))
	}
	if err := upd(&computepb.RoutePolicy{Name: proto.String("bad"), Type: proto.String(imp), Terms: []*computepb.RoutePolicyPolicyTerm{
		{Priority: proto.Int32(1), Actions: []*computepb.Expr{{Expression: proto.String("accept(")}}}}}); err == nil {
		t.Error("a malformed CEL action was accepted")
	}
	must(upd(&computepb.RoutePolicy{Name: proto.String("imp"), Type: proto.String(imp), Terms: []*computepb.RoutePolicyPolicyTerm{
		{Priority: proto.Int32(100), Match: &computepb.Expr{Expression: proto.String("destination.inAnyRange(prefix('10.0.0.0/8'))")},
			Actions: []*computepb.Expr{{Expression: proto.String("accept()")}}}}}))
	must(upd(&computepb.RoutePolicy{Name: proto.String("exp"), Type: proto.String(exp)}))
	got, err := routers.GetRoutePolicy(ctx, &computepb.GetRoutePolicyRouterRequest{Project: p, Region: region, Router: rName, Policy: proto.String("imp")})
	must(err)
	fp := got.GetResource().GetFingerprint()
	if got.GetResource().GetType() != imp || len(got.GetResource().GetTerms()) != 1 || fp == "" {
		t.Fatalf("policy = %v", got.GetResource())
	}
	patch := func(rp *computepb.RoutePolicy) error {
		return wait(routers.PatchRoutePolicy(ctx, &computepb.PatchRoutePolicyRouterRequest{Project: p, Region: region, Router: rName, RoutePolicyResource: rp}))
	}
	if err := patch(&computepb.RoutePolicy{Name: proto.String("imp"), Fingerprint: proto.String("c3RhbGU="), Description: proto.String("x")}); err == nil {
		t.Error("a stale fingerprint was accepted")
	}
	must(patch(&computepb.RoutePolicy{Name: proto.String("imp"), Fingerprint: proto.String(fp), Description: proto.String("inbound")}))
	var names []string
	it := routers.ListRoutePolicies(ctx, &computepb.ListRoutePoliciesRoutersRequest{Project: p, Region: region, Router: rName})
	for {
		rp, err := it.Next()
		if err == iterator.Done {
			break
		}
		must(err)
		names = append(names, rp.GetName())
		if rp.GetName() == "imp" && rp.GetDescription() != "inbound" {
			t.Errorf("patched policy = %v", rp)
		}
	}
	if len(names) != 2 {
		t.Errorf("listRoutePolicies = %v", names)
	}
	r, _ = routers.Get(ctx, &computepb.GetRouterRequest{Project: p, Region: region, Router: rName})
	r.BgpPeers[0].ImportPolicies = []string{"exp"}
	if err := wait(routers.Patch(ctx, &computepb.PatchRouterRequest{Project: p, Region: region, Router: rName, RouterResource: r})); err == nil {
		t.Error("an export policy as an import policy was accepted")
	}
	r.BgpPeers[0].ImportPolicies = []string{"imp"}
	must(wait(routers.Patch(ctx, &computepb.PatchRouterRequest{Project: p, Region: region, Router: rName, RouterResource: r})))
	del := func(name string) error {
		return wait(routers.DeleteRoutePolicy(ctx, &computepb.DeleteRoutePolicyRouterRequest{Project: p, Region: region, Router: rName, Policy: proto.String(name)}))
	}
	if err := del("imp"); err == nil {
		t.Error("a policy in use was deleted")
	}
	must(del("exp"))
	if _, err := routers.GetRoutePolicy(ctx, &computepb.GetRoutePolicyRouterRequest{Project: p, Region: region, Router: rName, Policy: proto.String("exp")}); err == nil {
		t.Error("a deleted policy is still read")
	}
}

// TestPSCEndpointPatchAndSetTargetThroughTheGoClient (#2140), the official
// compute/apiv1 REST client: an endpoint for a published service turns
// global access on by forwardingRules.patch under its fingerprint (a
// stale one refused, a description refused), setTarget keeps its
// attachment and refuses another; an endpoint for Google APIs refuses
// global access and a new bundle by globalForwardingRules.patch and
// setTarget.
// covers: compute.forwardingRules.patch, compute.forwardingRules.setTarget, compute.globalForwardingRules.patch, compute.globalForwardingRules.setTarget
func TestPSCEndpointPatchAndSetTargetThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	gaddrs, _ := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	gfr, _ := computeapi.NewGlobalForwardingRulesRESTClient(ctx, opts...)
	fr, _ := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	sas, _ := computeapi.NewServiceAttachmentsRESTClient(ctx, opts...)
	p := h.Project()
	const region = "us-central1"
	n := time.Now().UnixNano() % 1e6
	sfx := fmt.Sprint(n)
	vpc := "ct-pscu-" + sfx
	ep, apis, ip := "ct-pscu-ep-"+sfx, fmt.Sprintf("pscu%d", n), "ct-pscu-ip-"+sfx
	wait := func(op *computeapi.Operation, err error) error {
		t.Helper()
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		w := func(op *computeapi.Operation, err error) {
			if err == nil {
				_ = op.Wait(c)
			}
		}
		w(fr.Delete(c, &computepb.DeleteForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep}))
		w(gfr.Delete(c, &computepb.DeleteGlobalForwardingRuleRequest{Project: p, ForwardingRule: apis}))
		w(gaddrs.Delete(c, &computepb.DeleteGlobalAddressRequest{Project: p, Address: ip}))
		for _, a := range []string{"svc", "svc2"} {
			w(sas.Delete(c, &computepb.DeleteServiceAttachmentRequest{Project: p, Region: region, ServiceAttachment: vpc + "-" + a}))
		}
		for _, s := range []string{"-nat", "-a"} {
			w(subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: vpc + s}))
		}
		_, _ = nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: vpc})
	})
	must(wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(vpc), AutoCreateSubnetworks: proto.Bool(false)}})))
	netLink := "projects/" + p + "/global/networks/" + vpc
	must(wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(vpc + "-a"), Network: proto.String(netLink), IpCidrRange: proto.String("10.82.0.0/24")}})))
	must(wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: &computepb.Subnetwork{
		Name: proto.String(vpc + "-nat"), Network: proto.String(netLink), IpCidrRange: proto.String("10.82.1.0/24"),
		Purpose: proto.String("PRIVATE_SERVICE_CONNECT")}})))
	att := func(a string) string {
		return "projects/" + p + "/regions/" + region + "/serviceAttachments/" + vpc + "-" + a
	}
	for _, a := range []string{"svc", "svc2"} {
		must(wait(sas.Insert(ctx, &computepb.InsertServiceAttachmentRequest{Project: p, Region: region, ServiceAttachmentResource: &computepb.ServiceAttachment{
			Name: proto.String(vpc + "-" + a), TargetService: proto.String("projects/" + p + "/regions/" + region + "/forwardingRules/ilb"),
			ConnectionPreference: proto.String("ACCEPT_AUTOMATIC"), NatSubnets: []string{"regions/" + region + "/subnetworks/" + vpc + "-nat"}}})))
	}
	must(wait(fr.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(ep), Network: proto.String(netLink), Subnetwork: proto.String("regions/" + region + "/subnetworks/" + vpc + "-a"),
		Target: proto.String(att("svc"))}})))
	r, err := fr.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep})
	must(err)
	patch := func(b *computepb.ForwardingRule) error {
		return wait(fr.Patch(ctx, &computepb.PatchForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep, ForwardingRuleResource: b}))
	}
	if err := patch(&computepb.ForwardingRule{AllowPscGlobalAccess: proto.Bool(true), Fingerprint: proto.String("c3RhbGU=")}); err == nil {
		t.Error("a stale fingerprint was accepted")
	}
	if err := patch(&computepb.ForwardingRule{Description: proto.String("new")}); err == nil {
		t.Error("a description patch was accepted")
	}
	must(patch(&computepb.ForwardingRule{AllowPscGlobalAccess: proto.Bool(true), Fingerprint: r.Fingerprint}))
	if r2, _ := fr.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep}); !r2.GetAllowPscGlobalAccess() || r2.GetFingerprint() == r.GetFingerprint() {
		t.Errorf("after patch = %v", r2)
	}
	setTarget := func(target string) error {
		return wait(fr.SetTarget(ctx, &computepb.SetTargetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep,
			TargetReferenceResource: &computepb.TargetReference{Target: proto.String(target)}}))
	}
	if err := setTarget(att("svc2")); err == nil {
		t.Error("a new service attachment target was accepted")
	}
	must(setTarget(att("svc")))

	must(wait(gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: p, AddressResource: &computepb.Address{
		Name: proto.String(ip), AddressType: proto.String("INTERNAL"), Purpose: proto.String("PRIVATE_SERVICE_CONNECT"),
		Address: proto.String("10.255.82.254"), Network: proto.String(netLink)}})))
	must(wait(gfr.Insert(ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: p, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(apis), Target: proto.String("all-apis"), Network: proto.String(netLink), IPAddress: proto.String("global/addresses/" + ip)}})))
	if err := wait(gfr.Patch(ctx, &computepb.PatchGlobalForwardingRuleRequest{Project: p, ForwardingRule: apis,
		ForwardingRuleResource: &computepb.ForwardingRule{AllowPscGlobalAccess: proto.Bool(true)}})); err == nil {
		t.Error("global access on an endpoint for Google APIs was accepted")
	}
	if err := wait(gfr.SetTarget(ctx, &computepb.SetTargetGlobalForwardingRuleRequest{Project: p, ForwardingRule: apis,
		TargetReferenceResource: &computepb.TargetReference{Target: proto.String("vpc-sc")}})); err == nil {
		t.Error("a new bundle was accepted")
	}
	must(wait(gfr.SetTarget(ctx, &computepb.SetTargetGlobalForwardingRuleRequest{Project: p, ForwardingRule: apis,
		TargetReferenceResource: &computepb.TargetReference{Target: proto.String("all-apis")}})))
}
