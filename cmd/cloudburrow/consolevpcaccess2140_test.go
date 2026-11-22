package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleServerlessVPCAccess2140 (#2140): Serverless VPC access's
// Create connector (Name, Region, Network, Subnet with Custom IP range and
// the project's /28 subnets, Scaling settings), the list with Google's
// defaults, the API's refusals shown as they are, the connector's page and
// Delete.
func TestConsoleServerlessVPCAccess2140(t *testing.T) {
	ctx := context.Background()
	screen := computeConsoleFixture(t)
	const project = "sva-2140"
	p := vpcAccessProvider{screen}
	if got := vpcFieldNames(p.CreateFormFor(ctx, project)); got != "name,region,network,subnet,ipCidrRange,minInstances,maxInstances,machineType" {
		t.Fatalf("Create connector fields = %s", got)
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "web", "region": "us-central1", "network": "default",
		"subnet": "custom", "ipCidrRange": "10.8.0.0/28", "minInstances": "2", "maxInstances": "10", "machineType": "e2-micro"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "web2", "region": "us-central1", "network": "default",
		"subnet": "custom", "ipCidrRange": "10.8.0.0/28"}); err == nil || !strings.Contains(err.Error(), "conflicts with connector 'web'") {
		t.Fatalf("an overlapping range: %v", err)
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "web3", "region": "us-central1", "network": "default",
		"subnet": "custom", "ipCidrRange": "10.8.1.0/28", "minInstances": "7", "maxInstances": "4"}); err == nil || !strings.Contains(err.Error(), "must be lower") {
		t.Fatalf("min above max: %v", err)
	}
	// A /28 subnet is offered in Subnet.
	if _, err := screen.call("POST", project, "regions/us-central1/subnetworks", nil, map[string]any{"name": "conn-sub",
		"network": "projects/" + project + "/global/networks/default", "ipCidrRange": "10.9.0.0/28"}); err != nil {
		t.Fatal(err)
	}
	var subField []string
	for _, f := range p.CreateFormFor(ctx, project) {
		if f.Name == "subnet" {
			subField = f.Options
		}
	}
	if strings.Join(subField, ",") != "custom,us-central1/conn-sub" {
		t.Fatalf("Subnet options = %v", subField)
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "sub", "region": "us-central1", "subnet": "us-central1/conn-sub",
		"minInstances": "3", "maxInstances": "4", "machineType": "f1-micro"}); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	web, sub := vpcRow(l, "web"), vpcRow(l, "sub")
	if web == nil || web.Status != "Ready" || web.Fields["IP address range"] != "10.8.0.0/28" || web.Fields["Min instances"] != "2" ||
		web.Fields["Max instances"] != "10" || web.Fields["Instance type"] != "e2-micro" || web.Fields["Network"] != "default" {
		t.Fatalf("web row = %+v", web)
	}
	if sub == nil || sub.Fields["Subnet"] != "conn-sub" || sub.Fields["Network"] != "default" || sub.Fields["Instance type"] != "f1-micro" {
		t.Fatalf("sub row = %+v", sub)
	}
	d, err := p.Detail(ctx, project, []string{"us-central1", "web"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("detail: %v %q", err, d.Unavailable)
	}
	var props []string
	for _, pr := range d.Sections[0].Groups[0].Properties {
		props = append(props, pr.Label+"="+pr.Value)
	}
	if got := strings.Join(props, ";"); !strings.Contains(got, "Minimum throughput=200 Mbps") || !strings.Contains(got, "Maximum throughput=1000 Mbps") {
		t.Errorf("detail = %s", got)
	}
	if a := p.DetailActions(ctx, project, []string{"us-central1", "web"}); len(a) != 1 || a[0].ID != "delete" {
		t.Fatalf("actions = %+v", a)
	}
	if err := p.ActAt(ctx, project, []string{"us-central1", "web"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, project); vpcRow(l, "web") != nil || l.Total != 1 {
		t.Errorf("after delete: %+v", l.Items)
	}
}
