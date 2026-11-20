package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

func computeConsoleFixture(t *testing.T) computeScreen {
	t.Helper()
	svc := &computeService{cfg: config.Config{BindAddress: "127.0.0.1", Mode: config.ModeEphemeral}}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	return computeScreen{svc: svc}
}

func vpcRow(l console.Listing, name string) *console.Resource {
	for i := range l.Items {
		if l.Items[i].Name == name {
			return &l.Items[i]
		}
	}
	return nil
}

func vpcActionIDs(acts []console.Action) string {
	var ids []string
	for _, a := range acts {
		ids = append(ids, a.ID)
	}
	return strings.Join(ids, ",")
}

// The VPC networks page (#1936): Create VPC network with a first subnet,
// the network's page and its Subnets tab, Add subnet with a secondary
// range, Expand IP range, Edit secondary ranges, Private Google Access,
// Edit, a refused overlap with the API's message, Delete subnet and Delete
// VPC network; without the API the list says it has not started.
func TestConsoleVPCNetworksAndSubnets(t *testing.T) {
	ctx := context.Background()
	screen := computeConsoleFixture(t)
	p := computeNetworksProvider{screen}
	const project = "vpc-console"

	l, err := p.List(ctx, project)
	if err != nil || vpcRow(l, "default") == nil || vpcRow(l, "default").Fields["Mode"] != "Auto" {
		t.Fatalf("List = %+v, %v: the default network is not listed as auto", l, err)
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "vpc1", "subnetMode": "Custom", "mtu": "1500", "routingMode": "Global",
		"subnetName": "sub1", "subnetRegion": "us-east1", "subnetRange": "10.10.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	l, _ = p.List(ctx, project)
	r := vpcRow(l, "vpc1")
	if r == nil || r.Fields["Mode"] != "Custom" || r.Fields["Subnets"] != "1" || r.Fields["MTU"] != "1500" || r.Fields["Dynamic routing mode"] != "Global" {
		t.Fatalf("vpc1 row = %+v", r)
	}
	if got := vpcActionIDs(p.DetailActions(ctx, project, []string{"vpc1"})); got != "addsubnet,delete" {
		t.Errorf("custom network actions = %s", got)
	}
	if got := vpcActionIDs(p.DetailActions(ctx, project, []string{"default"})); got != "addsubnet,custommode,delete" {
		t.Errorf("auto network actions = %s", got)
	}
	if err := p.ActAt(ctx, project, []string{"vpc1"}, "addsubnet", map[string]string{"name": "sub2", "region": "us-central1",
		"ipCidrRange": "10.20.0.0/24", "secondaryRanges": `[{"rangeName":"pods","ipCidrRange":"10.30.0.0/20"}]`}); err != nil {
		t.Fatal(err)
	}
	// An overlap is refused with the API's own words.
	err = p.ActAt(ctx, project, []string{"vpc1"}, "addsubnet", map[string]string{"name": "sub3", "region": "us-central1", "ipCidrRange": "10.20.0.128/25"})
	if err == nil || !strings.Contains(err.Error(), "conflicts with existing subnetwork 'sub2'") {
		t.Fatalf("overlap: %v", err)
	}
	d, err := p.Detail(ctx, project, []string{"vpc1"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 3 || d.Edit == nil {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	subs := d.Sections[0].Listing
	s2 := vpcRow(subs, "sub2")
	if subs.Total != 2 || s2 == nil || s2.Fields["Secondary IPv4 ranges"] != "pods: 10.30.0.0/20" || s2.Fields["Gateway"] != "10.20.0.1" {
		t.Fatalf("subnets = %+v", subs)
	}
	sp := []string{"vpc1", "subnets", "us-central1", "sub2"}
	if got := vpcActionIDs(p.DetailActions(ctx, project, sp)); got != "expand,secondary,privateaccess,delete" {
		t.Errorf("subnet actions = %s", got)
	}
	if err := p.ActAt(ctx, project, sp, "expand", map[string]string{"ipCidrRange": "10.20.0.0/24"}); err == nil {
		t.Error("expanding to the same range was accepted")
	}
	if err := p.ActAt(ctx, project, sp, "expand", map[string]string{"ipCidrRange": "10.20.0.0/22"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, sp, "secondary", map[string]string{"secondaryRanges": `[{"rangeName":"svc","ipCidrRange":"10.40.0.0/24"}]`}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, sp, "privateaccess", map[string]string{"privateIpGoogleAccess": "true"}); err != nil {
		t.Fatal(err)
	}
	sd, _ := p.Detail(ctx, project, sp)
	props := map[string]string{}
	for _, pr := range sd.Summary {
		props[pr.Label] = pr.Value
	}
	if props["Primary IPv4 range"] != "10.20.0.0/22" || props["Secondary IPv4 ranges"] != "svc: 10.40.0.0/24" || props["Private Google Access"] != "On" {
		t.Fatalf("subnet page = %+v", props)
	}
	if err := p.Edit(ctx, project, []string{"vpc1"}, map[string]string{"mtu": "8896", "routingMode": "Regional"}); err != nil {
		t.Fatal(err)
	}
	l, _ = p.List(ctx, project)
	if r := vpcRow(l, "vpc1"); r.Fields["MTU"] != "8896" || r.Fields["Dynamic routing mode"] != "Regional" {
		t.Fatalf("after Edit: %+v", r)
	}
	// Deleting a network with subnets is refused; with none it goes.
	if err := p.Delete(ctx, project, "vpc1"); err == nil || !strings.Contains(err.Error(), "already being used by") {
		t.Fatalf("delete in use: %v", err)
	}
	for _, sub := range [][]string{sp, {"vpc1", "subnets", "us-east1", "sub1"}} {
		if err := p.ActAt(ctx, project, sub, "delete", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.ActAt(ctx, project, []string{"vpc1"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, project); vpcRow(l, "vpc1") != nil {
		t.Error("vpc1 is still listed")
	}
	if err := p.ActAt(ctx, project, []string{"default"}, "custommode", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, project); vpcRow(l, "default").Fields["Mode"] != "Custom" {
		t.Error("default is not custom mode after Switch to custom mode")
	}

	none := computeNetworksProvider{computeScreen{svc: &computeService{}}}
	if _, err := none.List(ctx, project); err == nil || !strings.Contains(err.Error(), "has not started") {
		t.Errorf("unstarted List: %v", err)
	}
}

// Firewall, Routes and IP addresses (#1936): Create firewall rule, Edit,
// Delete; Create route to the internet gateway and to an IP, Delete; Reserve
// external, internal and private services access addresses, Edit labels and
// Release.
func TestConsoleVPCFirewallsRoutesAndAddresses(t *testing.T) {
	ctx := context.Background()
	screen := computeConsoleFixture(t)
	const project = "vpc-console2"
	nets := computeNetworksProvider{screen}
	if _, err := nets.Create(ctx, project, map[string]string{"name": "app", "subnetMode": "Custom",
		"subnetName": "app-sub", "subnetRegion": "us-central1", "subnetRange": "10.1.0.0/24"}); err != nil {
		t.Fatal(err)
	}

	fw := computeFirewallsProvider{screen}
	found := false
	for _, f := range fw.CreateFormFor(ctx, project) {
		if f.Name == "network" && strings.Contains(strings.Join(f.Options, ","), "app") {
			found = true
		}
	}
	if !found {
		t.Error("the create form's network field does not offer app")
	}
	if _, err := fw.Create(ctx, project, map[string]string{"name": "allow-web", "network": "app", "priority": "900", "direction": "Ingress",
		"action": "Allow", "targetTags": "web", "sourceRanges": "0.0.0.0/0", "protocols": "tcp:80,443 icmp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Create(ctx, project, map[string]string{"name": "bad", "network": "app", "protocols": "icmp:22"}); err == nil {
		t.Error("ports with icmp were accepted")
	}
	l, err := fw.List(ctx, project)
	r := vpcRow(l, "allow-web")
	if err != nil || r == nil || r.Fields["Protocols / ports"] != "tcp:80,443 icmp" || r.Fields["Targets"] != "web" || r.Fields["Priority"] != "900" || r.Fields["Network"] != "app" {
		t.Fatalf("firewall row = %+v, %v", r, err)
	}
	if d, _ := fw.Detail(ctx, project, []string{"allow-web"}); d.Edit == nil {
		t.Fatal("a rule's page has no Edit")
	}
	if err := fw.Edit(ctx, project, []string{"allow-web"}, map[string]string{"priority": "100", "direction": "Ingress", "action": "Deny",
		"sourceRanges": "192.168.0.0/16", "protocols": "all"}); err != nil {
		t.Fatal(err)
	}
	l, _ = fw.List(ctx, project)
	if r := vpcRow(l, "allow-web"); r.Fields["Action"] != "Deny" || r.Fields["Priority"] != "100" || r.Fields["Protocols / ports"] != "all" {
		t.Fatalf("after Edit: %+v", r)
	}
	if err := fw.Delete(ctx, project, "allow-web"); err != nil {
		t.Fatal(err)
	}

	routes := computeRoutesProvider{screen}
	if _, err := routes.Create(ctx, project, map[string]string{"name": "to-net", "network": "app", "destRange": "192.168.0.0/16",
		"nextHop": "Specify an IP address", "nextHopIp": "10.1.0.5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := routes.Create(ctx, project, map[string]string{"name": "inside", "network": "app", "destRange": "10.1.0.0/25"}); err == nil {
		t.Error("a route inside a subnet range was accepted")
	}
	l, _ = routes.List(ctx, project)
	if r := vpcRow(l, "to-net"); r == nil || r.Fields["Next hop"] != "10.1.0.5" || r.Fields["Route type"] != "Static" {
		t.Fatalf("route row = %+v", r)
	}
	if err := routes.Delete(ctx, project, "to-net"); err != nil {
		t.Fatal(err)
	}

	addrs := computeAddressesProvider{screen}
	if got := vpcActionIDs(addrs.ListActions(ctx, project)); got != "reserve-external,reserve-internal,allocate-psa" {
		t.Errorf("list actions = %s", got)
	}
	for action, values := range map[string]map[string]string{
		"reserve-external": {"name": "ext", "ipType": "Regional", "region": "us-central1", "networkTier": "Premium"},
		"reserve-internal": {"name": "int", "network": "app", "region": "us-central1", "subnetwork": "app-sub", "address": "10.1.0.10"},
		"allocate-psa":     {"name": "psa", "network": "app", "prefixLength": "20"},
	} {
		if err := addrs.ActAt(ctx, project, nil, action, values); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if err := addrs.ActAt(ctx, project, nil, "reserve-internal", map[string]string{"name": "dup", "region": "us-central1",
		"subnetwork": "app-sub", "address": "10.1.0.10"}); err == nil {
		t.Error("an address in use was reserved again")
	}
	l, err = addrs.List(ctx, project)
	if err != nil || l.Total != 3 {
		t.Fatalf("addresses = %+v, %v", l, err)
	}
	if r := vpcRow(l, "int"); r.Fields["IP address"] != "10.1.0.10" || r.Fields["Access type"] != "Internal" || r.Fields["Subnetwork"] != "app-sub" {
		t.Errorf("int row = %+v", r)
	}
	if r := vpcRow(l, "psa"); r.Fields["Purpose"] != "VPC_PEERING" || !strings.HasSuffix(r.Fields["IP address"], "/20") || r.Fields["Region"] != "global" {
		t.Errorf("psa row = %+v", r)
	}
	ext := vpcRow(l, "ext")
	if got := vpcActionIDs(ext.Actions); got != "labels,release" || strings.Join(ext.Target, "/") != "us-central1/ext" {
		t.Fatalf("ext actions %s at %v", got, ext.Target)
	}
	if err := addrs.ActAt(ctx, project, ext.Target, "labels", map[string]string{"labels": `{"env":"dev"}`}); err != nil {
		t.Fatal(err)
	}
	// Releasing the subnet's address lets the subnet go.
	for _, r := range l.Items {
		if err := addrs.ActAt(ctx, project, r.Target, "release", nil); err != nil {
			t.Fatalf("release %s: %v", r.Name, err)
		}
	}
	if l, _ := addrs.List(ctx, project); l.Total != 0 {
		t.Errorf("%d addresses after Release", l.Total)
	}
}
