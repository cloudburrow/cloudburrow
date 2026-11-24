package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleIPv6LoadBalancerFrontend2140 (#2140): Create load balancer's
// Frontend configuration has Google's IP version (IPv4, IPv6) and, for a
// regional IPv6 frontend, its subnetwork; an internal passthrough Network
// Load Balancer made with IPv6 has an IPV6 forwarding rule on a /96 of the
// subnetwork, which the Frontends tab lists; without a subnetwork it is
// refused.
func TestConsoleIPv6LoadBalancerFrontend2140(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	const project = "lb6-console"
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"global/networks", map[string]any{"name": "v6", "autoCreateSubnetworks": false, "enableUlaInternalIpv6": true}},
		{"regions/us-central1/subnetworks", map[string]any{"name": "in6", "network": "global/networks/v6", "ipCidrRange": "10.6.0.0/24",
			"stackType": "IPV4_IPV6", "ipv6AccessType": "INTERNAL"}},
		{"zones/us-central1-a/instanceGroups", map[string]any{"name": "web", "network": "global/networks/v6"}},
	} {
		if _, err := screen.call("POST", project, c.path, nil, c.body); err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
	}
	lb := computeLBProvider{screen}
	fields := lb.CreateFormFor(ctx, project)
	if f := vpcField(fields, "ipVersion"); f == nil || f.Label != "IP version" || f.Section != "Frontend configuration" || strings.Join(f.Options, ",") != "IPV4,IPV6" ||
		f.OptionLabels["IPV6"] != "IPv6" || f.Default != "IPV4" {
		t.Fatalf("IP version field = %+v", f)
	}
	if f := vpcField(fields, "subnetwork"); f == nil || f.VisibleWhen["ipVersion"] != "IPV6" {
		t.Fatalf("subnetwork field = %+v", f)
	}
	v := map[string]string{"lbType": "network-passthrough", "facing": "internal", "region": "us-central1", "network": "v6",
		"name": "ilb6", "protocol": "TCP", "port": "80", "ipVersion": "IPV6", "backendType": "instanceGroup", "instanceGroup": "us-central1-a/web"}
	if _, err := lb.Create(ctx, project, v); err == nil || !strings.Contains(err.Error(), "Subnetwork") {
		t.Fatalf("a regional IPv6 frontend without a subnetwork: %v", err)
	}
	v["name"], v["subnetwork"] = "ilb6b", "in6"
	if _, err := lb.Create(ctx, project, v); err != nil {
		t.Fatal(err)
	}
	r, err := screen.call("GET", project, "regions/us-central1/forwardingRules/ilb6b-forwarding-rule", nil, nil)
	if err != nil || r["ipVersion"] != "IPV6" || !strings.HasSuffix(vpcStr(r, "IPAddress"), "/96") || !strings.HasSuffix(vpcStr(r, "subnetwork"), "/subnetworks/in6") {
		t.Fatalf("the rule = %v %v", r, err)
	}
	if fl, _ := (computeLBFrontendsProvider{screen}).List(ctx, project); vpcRow(fl, "ilb6b-forwarding-rule") == nil {
		t.Fatalf("frontends = %+v", fl.Items)
	}
}
