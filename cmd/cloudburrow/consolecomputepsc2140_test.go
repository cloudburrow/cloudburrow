package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsolePrivateServiceConnect2140: Connect endpoint for All Google
// APIs (its IP address reserved as Create IP address reserves it), the
// Connected endpoints tab and an endpoint's page; Publish service on a PSC
// subnet with Accept connections for selected projects; an endpoint to it
// Pending, then Accepted after Edit names the consumer project; the
// service's Connected endpoints; Delete on both tabs.
func TestConsolePrivateServiceConnect2140(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	const project = "psc-console"
	eps := computePSCEndpointsProvider{screen}
	pub := computePSCPublishedProvider{screen}
	if got := vpcFieldNames(eps.CreateFormFor(ctx, project)); got != "target,targetService,name,network,region,subnetwork,allowPscGlobalAccess,ipAddress,serviceDirectoryRegion" {
		t.Fatalf("Connect endpoint fields = %s", got)
	}
	if _, err := eps.Create(ctx, project, map[string]string{"target": "all-apis", "name": "googleapis1", "network": "default", "ipAddress": "10.255.255.254"}); err != nil {
		t.Fatal(err)
	}
	if _, err := eps.Create(ctx, project, map[string]string{"target": "all-apis", "name": "bad-name", "network": "default", "ipAddress": "10.255.255.250"}); err == nil {
		t.Fatal("an endpoint for Google APIs named with a hyphen")
	}
	l, err := eps.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	r := vpcRow(l, "googleapis1")
	if r == nil || r.Fields["Target"] != "All Google APIs (all-apis)" || r.Fields["Status"] != "Accepted" || r.Fields["IP address"] != "10.255.255.254" ||
		r.Fields["Scope"] != "Global" || strings.Join(r.Opens, "/") != "global/googleapis1" {
		t.Fatalf("endpoint row = %+v", r)
	}
	if d, _ := eps.Detail(ctx, project, []string{"global", "googleapis1"}); d.Unavailable != "" || d.Heading != "Endpoint details" {
		t.Fatalf("endpoint page = %+v", d)
	}

	// The producer publishes a service, on a PSC subnet.
	if _, err := screen.call("POST", project, "regions/us-central1/subnetworks", nil, map[string]any{"name": "psc-nat", "network": "global/networks/default",
		"ipCidrRange": "10.0.9.0/24", "purpose": "PRIVATE_SERVICE_CONNECT"}); err != nil {
		t.Fatal(err)
	}
	if got := pub.pscSubnets(project); strings.Join(got, ",") != "us-central1/psc-nat" {
		t.Fatalf("PSC subnets = %v", got)
	}
	// The internal load balancer the service publishes (#2153).
	const rg = "regions/us-central1/"
	for _, c := range []struct {
		coll string
		body map[string]any
	}{
		{rg + "healthChecks", map[string]any{"name": "ilb-hc", "type": "TCP", "tcpHealthCheck": map[string]any{"port": 80}}},
		{rg + "backendServices", map[string]any{"name": "ilb", "protocol": "TCP", "loadBalancingScheme": "INTERNAL",
			"healthChecks": []any{"projects/" + project + "/" + rg + "healthChecks/ilb-hc"}}},
		{rg + "forwardingRules", map[string]any{"name": "ilb", "loadBalancingScheme": "INTERNAL", "IPProtocol": "TCP", "ports": []any{"80"},
			"network": "global/networks/default", "backendService": "projects/" + project + "/" + rg + "backendServices/ilb"}},
	} {
		if _, err := screen.call("POST", project, c.coll, nil, c.body); err != nil {
			t.Fatalf("%s: %v", c.coll, err)
		}
	}
	form := pub.CreateFormFor(ctx, project)
	for _, f := range form {
		if f.Name == "targetService" && (f.Type != "select" || len(f.OptionsWhen) != 1 || f.OptionsWhen[0].When != "lbType=INTERNAL" ||
			strings.Join(f.OptionsWhen[0].Options, ",") != "projects/"+project+"/regions/us-central1/forwardingRules/ilb" ||
			f.OptionLabels["projects/"+project+"/regions/us-central1/forwardingRules/ilb"] != "ilb (us-central1)") {
			t.Errorf("Internal load balancer field = %+v", f)
		}
	}
	if got := vpcFieldNames(form); got != "lbType,targetService,name,natSubnets,connectionPreference,acceptedProjects,rejectedProjects,enableProxyProtocol,domainName" {
		t.Fatalf("Publish service fields = %s", got)
	}
	if _, err := pub.Create(ctx, project, map[string]string{"targetService": "projects/" + project + "/regions/us-central1/forwardingRules/ilb",
		"name": "svc", "natSubnets": "us-central1/psc-nat", "connectionPreference": "ACCEPT_MANUAL", "acceptedProjects": "someone:5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := eps.Create(ctx, project, map[string]string{"target": "published", "name": "to-svc", "network": "default", "region": "us-central1",
		"subnetwork": "default", "targetService": "projects/" + project + "/regions/us-central1/serviceAttachments/svc"}); err != nil {
		t.Fatal(err)
	}
	// Edit: Enable global access, then Save, and back (#2140); an
	// endpoint for Google APIs has no Edit.
	ep := []string{"us-central1", "to-svc"}
	if d, _ := eps.Detail(ctx, project, ep); d.Edit == nil || d.Edit.Fields[0].Name != "allowPscGlobalAccess" || d.Edit.Fields[0].Default != "false" {
		t.Fatalf("endpoint Edit = %+v", d.Edit)
	}
	if d, _ := eps.Detail(ctx, project, []string{"global", "googleapis1"}); d.Edit != nil {
		t.Errorf("an endpoint for Google APIs has an Edit: %+v", d.Edit)
	}
	for _, on := range []string{"true", "false"} {
		if err := eps.Edit(ctx, project, ep, map[string]string{"allowPscGlobalAccess": on}); err != nil {
			t.Fatal(err)
		}
		d, _ := eps.Detail(ctx, project, ep)
		want := map[string]string{"true": "Enabled", "false": "Disabled"}[on]
		var got string
		for _, pr := range d.Sections[0].Groups[0].Properties {
			if pr.Label == "Global access" {
				got = pr.Value
			}
		}
		if got != want || d.Edit.Fields[0].Default != on {
			t.Errorf("after Save with %s: Global access %q, Edit default %q", on, got, d.Edit.Fields[0].Default)
		}
	}
	if err := eps.Edit(ctx, project, []string{"global", "googleapis1"}, map[string]string{"allowPscGlobalAccess": "true"}); err == nil {
		t.Error("an endpoint for Google APIs took a global access edit")
	}
	l, _ = eps.List(ctx, project)
	if r := vpcRow(l, "to-svc"); r == nil || r.Status != "Pending" || r.Fields["Scope"] != "us-central1" {
		t.Fatalf("published endpoint row = %+v", r)
	}
	pl, _ := pub.List(ctx, project)
	if r := vpcRow(pl, "svc"); r == nil || r.Fields["Connected endpoints"] != "1" || r.Fields["Connection preference"] != "Accept connections for selected projects" {
		t.Fatalf("published service row = %+v", r)
	}
	d, _ := pub.Detail(ctx, project, []string{"us-central1", "svc"})
	if d.Edit == nil || len(d.Sections) != 2 || d.Sections[1].Listing.Items[0].Fields["Status"] != "Pending" {
		t.Fatalf("service page = %+v", d.Sections)
	}
	if err := pub.Edit(ctx, project, []string{"us-central1", "svc"}, map[string]string{"connectionPreference": "ACCEPT_MANUAL", "acceptedProjects": project + ":10"}); err != nil {
		t.Fatal(err)
	}
	l, _ = eps.List(ctx, project)
	if r := vpcRow(l, "to-svc"); r == nil || r.Status != "Accepted" {
		t.Fatalf("after Edit = %+v", r)
	}
	if err := eps.ActAt(ctx, project, []string{"us-central1", "to-svc"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if err := eps.ActAt(ctx, project, []string{"global", "googleapis1"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if err := pub.ActAt(ctx, project, []string{"us-central1", "svc"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := eps.List(ctx, project); len(l.Items) != 0 {
		t.Fatalf("endpoints after delete = %+v", l.Items)
	}
}
