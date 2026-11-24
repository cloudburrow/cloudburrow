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
	if got := vpcFieldNames(eps.CreateFormFor(ctx, project)); got != "target,targetService,name,network,region,subnetwork,ipAddress,globalAccess,serviceDirectoryRegion" {
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
	if got := vpcFieldNames(pub.CreateFormFor(ctx, project)); got != "targetService,name,natSubnets,connectionPreference,acceptedProjects,rejectedProjects,enableProxyProtocol,domainName" {
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
