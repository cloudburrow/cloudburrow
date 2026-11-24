package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Private NAT and Route policies (#2140): Create Cloud NAT gateway with NAT
// type Private (public-only fields hidden, the rule's next hop and Private
// NAT subnet), the gateway's Edit without NAT IP settings, and a Cloud
// Router's Route policies tab.
func TestConsolePrivateNATAndRoutePolicies2140(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	const project = "pnat-2140"
	nat := computeNATProvider{screen}
	routers := computeRoutersProvider{screen}
	if _, err := screen.call("POST", project, "regions/us-central1/subnetworks", nil, map[string]any{"name": "pnat",
		"network": "global/networks/default", "ipCidrRange": "100.64.0.0/24", "purpose": "PRIVATE_NAT"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range nat.CreateFormFor(ctx, project) {
		switch f.Name {
		case "natIpAllocateOption", "enableEndpointIndependentMapping":
			if f.VisibleWhen["type"] != "PUBLIC" {
				t.Errorf("%s visible for Private NAT: %v", f.Name, f.VisibleWhen)
			}
		case "natSubnet":
			if f.VisibleWhen["type"] != "PRIVATE" || !strings.Contains(strings.Join(f.Options, ","), "pnat") || strings.Contains(strings.Join(f.Options, ","), "default") {
				t.Errorf("Private NAT subnet field = %+v", f)
			}
		case "type":
			if len(f.UnavailableOptions) != 0 {
				t.Errorf("NAT type Private unavailable: %v", f.UnavailableOptions)
			}
		}
	}
	if _, err := nat.Create(ctx, project, map[string]string{"name": "pn", "type": "PRIVATE", "network": "default", "region": "us-central1",
		"router": natNewRouter, "routerName": "pr", "source": "ALL_SUBNETWORKS_ALL_IP_RANGES", "ruleNumber": "100", "nextHop": "HUB",
		"hub": "projects/pnat-2140/locations/global/hubs/h1", "natSubnet": "pnat"}); err != nil {
		t.Fatal(err)
	}
	r, err := screen.call("GET", project, "regions/us-central1/routers/pr", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := vmMaps(r, "nats")[0]
	if vpcStr(n, "type") != "PRIVATE" || n["natIpAllocateOption"] != nil || len(vmMaps(n, "rules")) != 1 ||
		!strings.Contains(vpcStr(vmMaps(n, "rules")[0], "match"), "hubs/h1") {
		t.Fatalf("private gateway = %v", n)
	}
	l, _ := nat.List(ctx, project)
	if row := vpcRow(l, "pn"); row == nil || row.Fields["NAT type"] != "Private" {
		t.Errorf("NAT row = %+v", row)
	}
	d, err := nat.Detail(ctx, project, []string{"us-central1", "pr", "pn"})
	if err != nil || d.Edit == nil {
		t.Fatalf("detail = %+v, %v", d, err)
	}
	names := vpcFieldNames(d.Edit.Fields)
	if strings.Contains(names, "natIpAllocateOption") || !strings.Contains(names, "natSubnet") {
		t.Errorf("Private NAT Edit fields = %s", names)
	}
	values := map[string]string{"source": "ALL_SUBNETWORKS_ALL_IP_RANGES", "ruleNumber": "200", "nextHop": "HYBRID", "natSubnet": "pnat"}
	if err := nat.Edit(ctx, project, []string{"us-central1", "pr", "pn"}, values); err != nil {
		t.Fatal(err)
	}
	r, _ = screen.call("GET", project, "regions/us-central1/routers/pr", nil, nil)
	if rl := vmMaps(vmMaps(r, "nats")[0], "rules"); len(rl) != 1 || vpcStr(rl[0], "match") != "nexthop.is_hybrid" || vpcStr(rl[0], "ruleNumber") != "200" {
		t.Errorf("edited rules = %v", rl)
	}

	rpath := []string{"us-central1", "pr"}
	if acts := routers.DetailActions(ctx, project, rpath); len(acts) != 3 || acts[0].ID != "createRoutePolicy" || acts[1].ID != "createNamedSet" {
		t.Fatalf("router actions = %+v", acts)
	}
	if err := routers.ActAt(ctx, project, rpath, "createRoutePolicy", map[string]string{"policyName": "bad", "policyType": "ROUTE_POLICY_TYPE_IMPORT",
		"terms": console.FormatGroup([]map[string]string{{"priority": "1", "actions": "accept("}})}); err == nil {
		t.Error("a malformed CEL action was accepted")
	}
	if err := routers.ActAt(ctx, project, rpath, "createRoutePolicy", map[string]string{"policyName": "imp", "policyType": "ROUTE_POLICY_TYPE_IMPORT",
		"terms": console.FormatGroup([]map[string]string{{"priority": "10", "match": "destination.inAnyRange(prefix('10.0.0.0/8'))", "actions": "med.set(5); accept()"}, {"priority": "20", "actions": "drop()"}})}); err != nil {
		t.Fatal(err)
	}
	if acts := routers.DetailActions(ctx, project, rpath); len(acts) != 4 || acts[1].ID != "deleteRoutePolicy" || acts[1].Fields[0].Options[0] != "imp" {
		t.Fatalf("router actions with a policy = %+v", acts)
	}
	rd, err := routers.Detail(ctx, project, []string{"us-central1", "pr"})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range rd.Sections {
		if s.ID == "routepolicies" {
			row := vpcRow(s.Listing, "imp")
			found = row != nil && row.Fields["Type"] == "Import" && strings.Contains(row.Fields["Terms"], "drop()")
		}
	}
	if !found {
		t.Errorf("Route policies tab = %+v", rd.Sections)
	}
	if err := routers.ActAt(ctx, project, rpath, "deleteRoutePolicy", map[string]string{"policyName": "imp"}); err != nil {
		t.Fatal(err)
	}
	if names := routers.routePolicyNames(project, rpath); len(names) != 0 {
		t.Errorf("after Delete route policy: %v", names)
	}
}
