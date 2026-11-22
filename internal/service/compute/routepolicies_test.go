package compute

import (
	"context"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// rpFixture is a server with network "net", subnets "a" (10.1.0.0/24)
// and "pnat" (100.64.0.0/24, purpose PRIVATE_NAT) in us-central1, and a
// router "r" with a BGP interface and peer.
func rpFixture(t *testing.T) (*Server, func(method, path string, q map[string][]string, b obj) (obj, error)) {
	t.Helper()
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, q map[string][]string, b obj) (obj, error) {
		return s.Call(method, "rp", path, q, b)
	}
	must := func(method, path string, b obj) {
		t.Helper()
		if _, err := call(method, path, nil, b); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	must("POST", "global/networks", obj{"name": "net", "autoCreateSubnetworks": false})
	must("POST", "regions/us-central1/subnetworks", obj{"name": "a", "network": "global/networks/net", "ipCidrRange": "10.1.0.0/24"})
	must("POST", "regions/us-central1/subnetworks", obj{"name": "pnat", "network": "global/networks/net", "ipCidrRange": "100.64.0.0/24", "purpose": "PRIVATE_NAT"})
	must("POST", "regions/us-central1/routers", obj{"name": "r", "network": "global/networks/net", "bgp": obj{"asn": 64512.0},
		"interfaces": []any{obj{"name": "if0", "ipRange": "169.254.1.1/30"}},
		"bgpPeers":   []any{obj{"name": "peer0", "interfaceName": "if0", "peerAsn": 65001.0, "peerIpAddress": "169.254.1.2"}}})
	return s, call
}

// TestPrivateNAT (#2140): a PRIVATE gateway translating to a PRIVATE_NAT
// subnet by next-hop rules, beside a public gateway of the same subnet;
// its refusals (NAT IPs, Endpoint-Independent Mapping, a non-PRIVATE_NAT
// range, IP actions, a destination match, two next hops); dynamic ports
// by default; and no internet egress or mapping info from it.
func TestPrivateNAT(t *testing.T) {
	_, call := rpFixture(t)
	hub := `nexthop.hub == "//networkconnectivity.googleapis.com/projects/rp/locations/global/hubs/h1"`
	rule := func(num float64, match string, ranges ...any) obj {
		return obj{"ruleNumber": num, "match": match, "action": obj{"sourceNatActiveRanges": ranges}}
	}
	nat := func(extra obj, rules ...any) obj {
		n := obj{"name": "pn", "type": "PRIVATE", "sourceSubnetworkIpRangesToNat": natList,
			"subnetworks": []any{obj{"name": "regions/us-central1/subnetworks/a", "sourceIpRangesToNat": []any{"ALL_IP_RANGES"}}}}
		for k, v := range extra {
			n[k] = v
		}
		if len(rules) > 0 {
			n["rules"] = rules
		}
		return n
	}
	patch := func(nats ...any) error {
		_, err := call("PATCH", "regions/us-central1/routers/r", nil, obj{"nats": nats})
		return err
	}
	pnat := "regions/us-central1/subnetworks/pnat"
	for name, n := range map[string]obj{
		"nat ip option":  nat(obj{"natIpAllocateOption": natAutoOnly}),
		"eim":            nat(obj{"enableEndpointIndependentMapping": true}),
		"plain range":    nat(nil, rule(1, "nexthop.is_hybrid", "regions/us-central1/subnetworks/a")),
		"ip action":      nat(nil, obj{"ruleNumber": 1.0, "match": "nexthop.is_hybrid", "action": obj{"sourceNatActiveRanges": []any{pnat}, "sourceNatActiveIps": []any{"x"}}}),
		"dest match":     nat(nil, rule(1, "inIpRange(destination.ip, '1.1.1.0/24')", pnat)),
		"two next hops":  nat(nil, rule(1, "nexthop.is_hybrid", pnat), rule(2, hub, pnat)),
		"bad hub":        nat(nil, rule(1, `nexthop.hub == "hubs/h1"`, pnat)),
		"no ranges":      nat(nil, obj{"ruleNumber": 1.0, "match": "nexthop.is_hybrid"}),
		"unknown type":   nat(obj{"type": "SHARED"}),
		"duplicate rule": nat(nil, rule(1, "nexthop.is_hybrid", pnat), rule(1, "nexthop.is_hybrid", pnat)),
	} {
		if err := patch(n); ErrorStatus(err) != 400 {
			t.Errorf("%s: err = %v, want 400", name, err)
		}
	}
	both := hub + " || nexthop.is_hybrid"
	public := obj{"name": "pub", "sourceSubnetworkIpRangesToNat": natList, "natIpAllocateOption": natAutoOnly,
		"subnetworks": []any{obj{"name": "regions/us-central1/subnetworks/a"}}}
	if err := patch(public, nat(nil, rule(10, both, pnat), rule(20, `inIpRange(source.ip, "10.1.0.0/25") && (`+both+`)`, pnat))); err != nil {
		t.Fatal(err)
	}
	r, err := call("GET", "regions/us-central1/routers/r", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pn map[string]any
	for _, e := range sliceOf(r["nats"]) {
		if m, _ := e.(map[string]any); str(m, "name") == "pn" {
			pn = m
		}
	}
	if pn == nil || str(pn, "type") != "PRIVATE" || pn["enableDynamicPortAllocation"] != true || pn["natIpAllocateOption"] != nil || len(sliceOf(pn["rules"])) != 2 {
		t.Fatalf("private gateway = %v", pn)
	}
	if mi, err := call("GET", "regions/us-central1/routers/r/getNatMappingInfo", map[string][]string{"natName": {"pn"}}, nil); err != nil || len(sliceOf(mi["result"])) != 0 {
		t.Errorf("mapping info of a private gateway = %v, %v", mi, err)
	}
	// A second private gateway on the same subnet is refused.
	second := nat(nil)
	second["name"] = "pn2"
	if err := patch(public, pn, second); ErrorStatus(err) != 400 {
		t.Errorf("two private gateways on one subnet: %v", err)
	}
}

// TestRoutePoliciesThroughTheGoClient (#2140): updateRoutePolicy creates,
// getRoutePolicy and listRoutePolicies read, patchRoutePolicy merges under
// the fingerprint, the type and term rules, CEL syntax, a BGP peer's
// importPolicies, deleteRoutePolicy refused while a peer uses the policy.
func TestRoutePoliciesThroughTheGoClient(t *testing.T) {
	ctx := context.Background()
	s, _ := rpFixture(t)
	_, opts := newTestServerFrom(t, s)
	routers, err := computeapi.NewRoutersRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	const project, region = "rp", "us-central1"
	wait := func(op *computeapi.Operation, err error) error {
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	upd := func(p *computepb.RoutePolicy) error {
		return wait(routers.UpdateRoutePolicy(ctx, &computepb.UpdateRoutePolicyRouterRequest{Project: project, Region: region, Router: "r", RoutePolicyResource: p}))
	}
	term := func(pr int32, match string, acts ...string) *computepb.RoutePolicyPolicyTerm {
		t := &computepb.RoutePolicyPolicyTerm{Priority: proto.Int32(pr)}
		if match != "" {
			t.Match = &computepb.Expr{Expression: proto.String(match)}
		}
		for _, a := range acts {
			t.Actions = append(t.Actions, &computepb.Expr{Expression: proto.String(a)})
		}
		return t
	}
	imp := computepb.RoutePolicy_ROUTE_POLICY_TYPE_IMPORT.String()
	for name, p := range map[string]*computepb.RoutePolicy{
		"no type":      {Name: proto.String("p")},
		"bad name":     {Name: proto.String("P!"), Type: proto.String(imp)},
		"dup priority": {Name: proto.String("p"), Type: proto.String(imp), Terms: []*computepb.RoutePolicyPolicyTerm{term(1, "", "accept()"), term(1, "", "drop()")}},
		"neg priority": {Name: proto.String("p"), Type: proto.String(imp), Terms: []*computepb.RoutePolicyPolicyTerm{term(-1, "", "accept()")}},
		"unbalanced":   {Name: proto.String("p"), Type: proto.String(imp), Terms: []*computepb.RoutePolicyPolicyTerm{term(1, "destination.inAnyRange(prefix('10.0.0.0/8')", "accept()")}},
		"assignment":   {Name: proto.String("p"), Type: proto.String(imp), Terms: []*computepb.RoutePolicyPolicyTerm{term(1, "", "med = 5")}},
		"open string":  {Name: proto.String("p"), Type: proto.String(imp), Terms: []*computepb.RoutePolicyPolicyTerm{term(1, "destination == prefix('10.0.0.0/8)", "accept()")}},
		"empty expr":   {Name: proto.String("p"), Type: proto.String(imp), Terms: []*computepb.RoutePolicyPolicyTerm{{Priority: proto.Int32(1), Actions: []*computepb.Expr{{}}}}},
	} {
		if err := upd(p); code(err) != 400 {
			t.Errorf("%s: err = %v, want 400", name, err)
		}
	}
	if err := upd(&computepb.RoutePolicy{Name: proto.String("imp"), Type: proto.String(imp), Description: proto.String("in"),
		Terms: []*computepb.RoutePolicyPolicyTerm{term(200, "", "drop()"), term(100, "destination.inAnyRange(prefix('10.0.0.0/8'))", "med.set(100)", "accept()")}}); err != nil {
		t.Fatal(err)
	}
	got, err := routers.GetRoutePolicy(ctx, &computepb.GetRoutePolicyRouterRequest{Project: project, Region: region, Router: "r", Policy: proto.String("imp")})
	if err != nil {
		t.Fatal(err)
	}
	rp := got.GetResource()
	if rp.GetType() != imp || len(rp.GetTerms()) != 2 || rp.GetTerms()[0].GetPriority() != 100 || len(rp.GetTerms()[0].GetActions()) != 2 || rp.GetFingerprint() == "" {
		t.Fatalf("policy = %v", rp)
	}
	if _, err := routers.GetRoutePolicy(ctx, &computepb.GetRoutePolicyRouterRequest{Project: project, Region: region, Router: "r", Policy: proto.String("none")}); code(err) != 404 {
		t.Errorf("missing policy: %v", err)
	}
	// patch: a stale fingerprint, a type change, then a description.
	patch := func(p *computepb.RoutePolicy) error {
		return wait(routers.PatchRoutePolicy(ctx, &computepb.PatchRoutePolicyRouterRequest{Project: project, Region: region, Router: "r", RoutePolicyResource: p}))
	}
	if err := patch(&computepb.RoutePolicy{Name: proto.String("imp"), Fingerprint: proto.String("c3RhbGU="), Description: proto.String("x")}); code(err) != 412 {
		t.Errorf("stale fingerprint: %v", err)
	}
	if err := patch(&computepb.RoutePolicy{Name: proto.String("imp"), Type: proto.String(computepb.RoutePolicy_ROUTE_POLICY_TYPE_EXPORT.String())}); code(err) != 400 {
		t.Errorf("type change: %v", err)
	}
	if err := patch(&computepb.RoutePolicy{Name: proto.String("imp"), Fingerprint: proto.String(rp.GetFingerprint()), Description: proto.String("inbound")}); err != nil {
		t.Fatal(err)
	}
	got, _ = routers.GetRoutePolicy(ctx, &computepb.GetRoutePolicyRouterRequest{Project: project, Region: region, Router: "r", Policy: proto.String("imp")})
	if got.GetResource().GetDescription() != "inbound" || len(got.GetResource().GetTerms()) != 2 || got.GetResource().GetFingerprint() == rp.GetFingerprint() {
		t.Errorf("patched policy = %v", got.GetResource())
	}
	if err := upd(&computepb.RoutePolicy{Name: proto.String("exp"), Type: proto.String(computepb.RoutePolicy_ROUTE_POLICY_TYPE_EXPORT.String())}); err != nil {
		t.Fatal(err)
	}
	var names []string
	it := routers.ListRoutePolicies(ctx, &computepb.ListRoutePoliciesRoutersRequest{Project: project, Region: region, Router: "r"})
	for {
		p, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, p.GetName())
	}
	if len(names) != 2 || names[0] != "exp" || names[1] != "imp" {
		t.Errorf("listRoutePolicies = %v", names)
	}
	// A peer's importPolicies: the type must match.
	r, _ := routers.Get(ctx, &computepb.GetRouterRequest{Project: project, Region: region, Router: "r"})
	r.BgpPeers[0].ImportPolicies = []string{"exp"}
	if err := wait(routers.Patch(ctx, &computepb.PatchRouterRequest{Project: project, Region: region, Router: "r", RouterResource: r})); code(err) != 400 {
		t.Errorf("an export policy as import: %v", err)
	}
	r.BgpPeers[0].ImportPolicies = []string{"imp"}
	if err := wait(routers.Patch(ctx, &computepb.PatchRouterRequest{Project: project, Region: region, Router: "r", RouterResource: r})); err != nil {
		t.Fatal(err)
	}
	if r, _ = routers.Get(ctx, &computepb.GetRouterRequest{Project: project, Region: region, Router: "r"}); len(r.GetBgpPeers()[0].GetImportPolicies()) != 1 {
		t.Errorf("peer importPolicies = %v", r.GetBgpPeers()[0])
	}
	del := func(name string) error {
		return wait(routers.DeleteRoutePolicy(ctx, &computepb.DeleteRoutePolicyRouterRequest{Project: project, Region: region, Router: "r", Policy: proto.String(name)}))
	}
	if err := del("imp"); code(err) != 400 {
		t.Errorf("deleting a policy in use: %v", err)
	}
	if err := del("exp"); err != nil {
		t.Fatal(err)
	}
	if _, err := routers.GetRoutePolicy(ctx, &computepb.GetRoutePolicyRouterRequest{Project: project, Region: region, Router: "r", Policy: proto.String("exp")}); code(err) != 404 {
		t.Errorf("deleted policy: %v", err)
	}
}

// TestNamedSets (#2140): updateNamedSet, getNamedSet, listNamedSets,
// patchNamedSet, deleteNamedSet and their refusals (the Go client of this
// release has no named-set methods, so they are called over REST).
func TestNamedSets(t *testing.T) {
	_, call := rpFixture(t)
	base := "regions/us-central1/routers/r/"
	for name, b := range map[string]obj{
		"no type":  {"name": "s"},
		"bad type": {"name": "s", "type": "NAMED_SET_TYPE_ASPATH"},
		"bad expr": {"name": "s", "type": "NAMED_SET_TYPE_PREFIX", "elements": []any{obj{"expression": "prefix('10.0.0.0/8'"}}},
		"no expr":  {"name": "s", "type": "NAMED_SET_TYPE_PREFIX", "elements": []any{obj{"title": "x"}}},
		"bad name": {"name": "S", "type": "NAMED_SET_TYPE_PREFIX"},
	} {
		if _, err := call("POST", base+"updateNamedSet", nil, b); ErrorStatus(err) != 400 {
			t.Errorf("%s: err = %v, want 400", name, err)
		}
	}
	if _, err := call("POST", base+"updateNamedSet", nil, obj{"name": "pfx", "type": "NAMED_SET_TYPE_PREFIX",
		"elements": []any{obj{"expression": "prefix('10.0.0.0/8').orLonger()"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("GET", base+"getNamedSet", nil, nil); ErrorStatus(err) != 400 {
		t.Errorf("getNamedSet without namedSet: %v", err)
	}
	g, err := call("GET", base+"getNamedSet", map[string][]string{"namedSet": {"pfx"}}, nil)
	res, _ := g["resource"].(map[string]any)
	if err != nil || str(res, "type") != "NAMED_SET_TYPE_PREFIX" || len(sliceOf(res["elements"])) != 1 {
		t.Fatalf("getNamedSet = %v, %v", g, err)
	}
	if _, err := call("POST", base+"patchNamedSet", nil, obj{"name": "pfx", "fingerprint": "c3RhbGU=", "description": "x"}); ErrorStatus(err) != 412 {
		t.Errorf("stale fingerprint: %v", err)
	}
	if _, err := call("POST", base+"patchNamedSet", nil, obj{"name": "pfx", "description": "nets"}); err != nil {
		t.Fatal(err)
	}
	l, err := call("GET", base+"listNamedSets", nil, nil)
	if err != nil || str(l, "kind") != "compute#routersListNamedSets" || len(sliceOf(l["result"])) != 1 {
		t.Fatalf("listNamedSets = %v, %v", l, err)
	}
	if m, _ := sliceOf(l["result"])[0].(map[string]any); str(m, "description") != "nets" || len(sliceOf(m["elements"])) != 1 {
		t.Errorf("patched set = %v", m)
	}
	// A route policy naming the set keeps it ("Manage named sets").
	if _, err := call("POST", base+"updateRoutePolicy", nil, obj{"name": "uses-pfx", "type": "ROUTE_POLICY_TYPE_IMPORT",
		"terms": []any{obj{"priority": 1.0, "match": obj{"expression": "destination.inAnyRange(prefixSets('pfx'))"},
			"actions": []any{obj{"expression": "accept()"}}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("POST", base+"deleteNamedSet", map[string][]string{"namedSet": {"pfx"}}, nil); ErrorStatus(err) != 400 {
		t.Errorf("deleting a set a policy uses: %v", err)
	}
	if _, err := call("POST", base+"deleteRoutePolicy", map[string][]string{"policy": {"uses-pfx"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := call("POST", base+"deleteNamedSet", map[string][]string{"namedSet": {"pfx"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := call("POST", base+"deleteNamedSet", map[string][]string{"namedSet": {"pfx"}}, nil); ErrorStatus(err) != 404 {
		t.Errorf("deleting twice: %v", err)
	}
	if _, err := call("GET", base+"getNamedSet", map[string][]string{"namedSet": {"pfx"}}, nil); ErrorStatus(err) != 404 {
		t.Errorf("deleted set: %v", err)
	}
	// Route policies and named sets survive a router patch.
	call("POST", base+"updateRoutePolicy", nil, obj{"name": "keep", "type": "ROUTE_POLICY_TYPE_EXPORT"})
	if _, err := call("PATCH", "regions/us-central1/routers/r", nil, obj{"description": "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("GET", base+"getRoutePolicy", map[string][]string{"policy": {"keep"}}, nil); err != nil {
		t.Errorf("policy after router patch: %v", err)
	}
	if r, _ := call("GET", "regions/us-central1/routers/r", nil, nil); r["_routePolicies"] != nil {
		t.Errorf("router shows its policies: %v", r)
	}
}
