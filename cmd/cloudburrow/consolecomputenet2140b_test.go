package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Cloud Routers and Cloud NAT (#2140): Create a router with custom
// advertisements, its details and Edit; Create Cloud NAT gateway on a new
// router and on an existing one, Manual IP addresses In use by the router,
// the gateway's details with its timeouts, Edit and Delete.
func TestConsoleCloudRouterAndNAT2140(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	const project = "router-2140"
	routers := computeRoutersProvider{screen}
	if got := vpcFieldNames(routers.CreateFormFor(ctx, project)); got != "name,description,network,region,asn,keepaliveInterval,identifierRange,advertiseMode,advertiseSubnets,customRanges" {
		t.Fatalf("Create a router fields = %s", got)
	}
	if _, err := routers.Create(ctx, project, map[string]string{"name": "r1", "network": "default", "region": "us-central1", "asn": "1234"}); err == nil ||
		!strings.Contains(err.Error(), "private ASN") {
		t.Errorf("a public ASN: %v", err)
	}
	if _, err := routers.Create(ctx, project, map[string]string{"name": "r1", "network": "default", "region": "us-central1", "asn": "64512",
		"keepaliveInterval": "30", "advertiseMode": "CUSTOM", "advertiseSubnets": "true",
		"customRanges": console.FormatGroup([]map[string]string{{"range": "192.168.0.0/24", "description": "on-prem"}})}); err != nil {
		t.Fatal(err)
	}
	l, _ := routers.List(ctx, project)
	r := vpcRow(l, "r1")
	if r == nil || r.Fields["Google ASN"] != "64512" || r.Fields["Network"] != "default" || strings.Join(r.Opens, "/") != "us-central1/r1" {
		t.Fatalf("router row = %+v", r)
	}
	d, _ := routers.Detail(ctx, project, []string{"us-central1", "r1"})
	if d.Unavailable != "" || d.Heading != "Router details" || len(d.Sections) != 4 || d.Edit == nil ||
		!strings.Contains(d.Sections[0].Groups[0].Properties[7].Value, "192.168.0.0/24") || d.Sections[0].Groups[0].Properties[5].Value != "30 seconds" {
		t.Fatalf("router details = %+v", d)
	}
	if err := routers.Edit(ctx, project, []string{"us-central1", "r1"}, map[string]string{"asn": "65000", "advertiseMode": "DEFAULT", "description": "edge"}); err != nil {
		t.Fatal(err)
	}
	if l, _ = routers.List(ctx, project); vpcRow(l, "r1").Fields["Google ASN"] != "65000" {
		t.Errorf("after Edit: %+v", vpcRow(l, "r1"))
	}

	// Cloud NAT on a new router, Automatic.
	nat := computeNATProvider{screen}
	form := nat.CreateFormFor(ctx, project)
	if got := vpcFieldNames(form); !strings.HasPrefix(got, "name,type,network,region,router,routerName,endpointType,ipVersion,source,subnets,natIpAllocateOption,natIps,autoNetworkTier,logging,enableEndpointIndependentMapping,enableDynamicPortAllocation,minPortsPerVm,maxPortsPerVm,udpIdleTimeoutSec") {
		t.Fatalf("Create Cloud NAT gateway fields = %s", got)
	}
	if f := vpcField(form, "router"); f == nil || len(f.OptionsWhen) != 1 || strings.Join(f.OptionsWhen[0].Options, ",") != "r1,"+natNewRouter {
		t.Fatalf("Cloud Router choices = %+v", f)
	}
	if f := vpcField(form, "minPortsPerVm"); f.Default != "64" {
		t.Errorf("Minimum ports per VM instance default = %q", f.Default)
	}
	if _, err := nat.Create(ctx, project, map[string]string{"name": "nat-east", "network": "default", "region": "us-east1", "router": natNewRouter,
		"routerName": "r-east", "source": "ALL_SUBNETWORKS_ALL_IP_RANGES", "natIpAllocateOption": "AUTO_ONLY", "logging": "ERRORS_ONLY", "minPortsPerVm": "64"}); err != nil {
		t.Fatal(err)
	}
	// Manual, on r1, of subnet default.
	if _, err := screen.call("POST", project, "regions/us-central1/addresses", nil, map[string]any{"name": "nat-ip"}); err != nil {
		t.Fatal(err)
	}
	if _, err := nat.Create(ctx, project, map[string]string{"name": "nat-c", "network": "default", "region": "us-central1", "router": "r1",
		"source": "LIST_OF_SUBNETWORKS", "subnets": console.FormatGroup([]map[string]string{{"subnet": "default", "ranges": "PRIMARY_IP_RANGE"}}),
		"natIpAllocateOption": "MANUAL_ONLY", "natIps": console.FormatGroup([]map[string]string{{"address": "nat-ip"}}), "udpIdleTimeoutSec": "45"}); err != nil {
		t.Fatal(err)
	}
	nl, _ := nat.List(ctx, project)
	if a, b := vpcRow(nl, "nat-east"), vpcRow(nl, "nat-c"); a == nil || b == nil || a.Fields["Cloud Router"] != "r-east" || b.Fields["NAT IP addresses"] != "Manual" ||
		b.Status != "Running" || strings.Join(b.Opens, "/") != "us-central1/r1/nat-c" {
		t.Fatalf("Cloud NAT list = %+v", nl.Items)
	}
	ip, _ := screen.call("GET", project, "regions/us-central1/addresses/nat-ip", nil, nil)
	if vpcStr(ip, "status") != "IN_USE" {
		t.Errorf("NAT IP status = %s", vpcStr(ip, "status"))
	}
	nd, _ := nat.Detail(ctx, project, []string{"us-central1", "r1", "nat-c"})
	props := map[string]string{}
	for _, p := range nd.Sections[0].Groups[0].Properties {
		props[p.Label] = p.Value
	}
	if props["Source subnets"] != "Custom: default" || props["UDP Mapping Idle Timeout"] != "45 seconds" ||
		props["TCP Established Connection Idle Timeout"] != "1200 seconds (default)" || props["Minimum ports per VM instance"] != "64 (default)" ||
		len(nd.Sections[1].Listing.Items) != 1 || nd.Edit == nil {
		t.Fatalf("NAT details = %v, ips %+v", props, nd.Sections[1].Listing.Items)
	}
	if err := nat.Edit(ctx, project, []string{"us-central1", "r1", "nat-c"}, map[string]string{"source": "ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES",
		"natIpAllocateOption": "AUTO_ONLY", "logging": "none", "minPortsPerVm": "128"}); err != nil {
		t.Fatal(err)
	}
	if ip, _ = screen.call("GET", project, "regions/us-central1/addresses/nat-ip", nil, nil); vpcStr(ip, "status") != "RESERVED" {
		t.Errorf("after switching to Automatic, NAT IP = %s", vpcStr(ip, "status"))
	}
	if err := nat.ActAt(ctx, project, []string{"us-central1", "r1", "nat-c"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if nl, _ = nat.List(ctx, project); vpcRow(nl, "nat-c") != nil {
		t.Error("nat-c still listed after Delete")
	}
	if l, _ = routers.List(ctx, project); vpcRow(l, "r1") == nil || vpcRow(l, "r-east").Fields["Cloud NAT gateways"] != "nat-east" {
		t.Errorf("routers after the NAT's Delete = %+v", l.Items)
	}
	if err := routers.ActAt(ctx, project, []string{"us-east1", "r-east"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
}
