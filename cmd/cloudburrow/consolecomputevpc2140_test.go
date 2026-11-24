package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func vpcFieldNames(fields []console.Field) string {
	var out []string
	for _, f := range fields {
		out = append(out, f.Name)
	}
	return strings.Join(out, ",")
}

// The VPC networking parity round (#2140): Create VPC network with Set MTU
// automatically, a first subnet with Flow logs and its firewall rules; the
// Subnets in current project tab with Add subnet and Edit; VPC network
// peering, INACTIVE until both sides peer, its page's Edit and Delete; the
// Effective routes tab with the peer's subnet route; the firewall rule
// form's targets, source filters, protocols and enforcement; Create route
// to an instance; promoting a VM's ephemeral internal address.
func TestConsoleVPCParity2140(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	const project = "vpc-2140"
	nets := computeNetworksProvider{screen}

	_, form := nets.CreateForm()
	if got := vpcFieldNames(form); !strings.HasPrefix(got, "name,description,autoMtu,mtu,ulaIpv6,subnetMode,subnetName") ||
		!strings.Contains(got, "subnetStackType,subnetRange,subnetSecondaryRanges,privateGoogleAccess,subnetFlowLogs,rule-custom,rule-icmp,rule-rdp,rule-ssh,routingMode") {
		t.Fatalf("Create VPC network fields = %s", got)
	}
	if _, err := nets.Create(ctx, project, map[string]string{"name": "blue", "subnetMode": "Custom", "autoMtu": "true", "mtu": "1300",
		"subnetName": "blue-sub", "subnetRegion": "us-central1", "subnetRange": "10.10.0.0/24", "subnetFlowLogs": "On",
		"privateGoogleAccess": "On", "rule-custom": "true", "rule-ssh": "true", "routingMode": "Regional"}); err != nil {
		t.Fatal(err)
	}
	l, _ := nets.List(ctx, project)
	if r := vpcRow(l, "blue"); r == nil || r.Fields["MTU"] != "1460" || r.Fields["Firewall rules"] != "2" {
		t.Fatalf("blue row (Set MTU automatically, two rules) = %+v", r)
	}
	fwp := computeFirewallsProvider{screen}
	fl, _ := fwp.List(ctx, project)
	if r := vpcRow(fl, "blue-allow-custom"); r == nil || r.Fields["Filters"] != "IP ranges: 10.10.0.0/24" || r.Fields["Protocols / ports"] != "all" || r.Fields["Priority"] != "65534" {
		t.Errorf("blue-allow-custom = %+v", r)
	}
	if r := vpcRow(fl, "blue-allow-ssh"); r == nil || r.Fields["Protocols / ports"] != "tcp:22" {
		t.Errorf("blue-allow-ssh = %+v", r)
	}
	if fl.TableTitle != "VPC firewall rules" || fwp.Title() != "Firewall policies" {
		t.Errorf("firewall page title %q, table %q", fwp.Title(), fl.TableTitle)
	}

	// Subnets in current project.
	subs := computeSubnetsProvider{screen}
	if got := vpcActionIDs(subs.ListActions(ctx, project)); got != "addsubnet" {
		t.Fatalf("subnets tab actions = %s", got)
	}
	if err := subs.ActAt(ctx, project, nil, "addsubnet", map[string]string{"network": "blue", "name": "blue-east", "region": "us-east1",
		"ipCidrRange": "10.11.0.0/24", "privateGoogleAccess": "Off", "flowLogs": "Off"}); err != nil {
		t.Fatal(err)
	}
	sl, _ := subs.List(ctx, project)
	r := vpcRow(sl, "blue-sub")
	if r == nil || r.Fields["Network"] != "blue" || r.Fields["Flow logs"] != "On" || r.Fields["Private Google Access"] != "On" ||
		strings.Join(r.Opens, "/") != "us-central1/blue-sub" || vpcRow(sl, "blue-east") == nil {
		t.Fatalf("subnets tab = %+v", sl.Items)
	}
	if err := subs.Edit(ctx, project, []string{"us-east1", "blue-east"}, map[string]string{"ipCidrRange": "10.11.0.0/23", "flowLogs": "On"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := subs.Detail(ctx, project, []string{"us-east1", "blue-east"}); d.Edit == nil || d.Summary[2].Value != "10.11.0.0/23" {
		t.Fatalf("subnet page from the tab = %+v", d.Summary)
	}
	if got := vpcActionIDs(nets.DetailActions(ctx, project, []string{"default", "subnets", "us-central1", "default"})); got != "delete" {
		t.Fatalf("auto subnet actions = %s", got)
	} else if a := nets.DetailActions(ctx, project, []string{"default", "subnets", "us-central1", "default"})[0]; a.Unavailable == "" {
		t.Error("an auto mode network's own subnet offers Delete")
	}

	// VPC network peering.
	if _, err := nets.Create(ctx, project, map[string]string{"name": "green", "subnetMode": "Custom", "autoMtu": "true",
		"subnetName": "green-sub", "subnetRegion": "us-central1", "subnetRange": "10.20.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	peer := computePeeringProvider{screen}
	if got := vpcFieldNames(peer.CreateFormFor(ctx, project)); got != "name,network,peerWhere,peerNetwork,peerProject,peerNetworkName,stackType,importCustomRoutes,exportCustomRoutes,importSubnetRoutesWithPublicIp,updateStrategy" {
		t.Fatalf("Create peering connection fields = %s", got)
	}
	if _, err := peer.Create(ctx, project, map[string]string{"name": "blue-green", "network": "blue", "peerWhere": "here", "peerNetwork": "green",
		"importCustomRoutes": "true"}); err != nil {
		t.Fatal(err)
	}
	pl, _ := peer.List(ctx, project)
	if r := vpcRow(pl, "blue-green"); r == nil || r.Status != "Inactive" || r.Fields["Peered VPC network"] != "green" || r.Fields["Peered project ID"] != project ||
		r.Fields["Import custom routes"] != "On" {
		t.Fatalf("one-sided peering = %+v", pl.Items)
	}
	if _, err := peer.Create(ctx, project, map[string]string{"name": "green-blue", "network": "green", "peerWhere": "other", "peerProject": project,
		"peerNetworkName": "blue"}); err != nil {
		t.Fatal(err)
	}
	pl, _ = peer.List(ctx, project)
	if pl.Total != 2 || vpcRow(pl, "blue-green").Status != "Active" || vpcRow(pl, "green-blue").Status != "Active" {
		t.Fatalf("both sides peered = %+v", pl.Items)
	}
	if _, err := peer.Create(ctx, project, map[string]string{"name": "to-default", "network": "blue", "peerWhere": "here", "peerNetwork": "blue"}); err == nil {
		t.Error("a network was peered with itself")
	}
	d, _ := peer.Detail(ctx, project, []string{"blue", "blue-green"})
	if d.Edit == nil || d.Heading != "Peering connection details" {
		t.Fatalf("peering page = %+v", d)
	}
	if err := peer.Edit(ctx, project, []string{"blue", "blue-green"}, map[string]string{"exportCustomRoutes": "true", "updateStrategy": "CONSENSUS"}); err != nil {
		t.Fatal(err)
	}
	if r := vpcRow(func() console.Listing { l, _ := peer.List(ctx, project); return l }(), "blue-green"); r.Fields["Export custom routes"] != "On" || r.Fields["Import custom routes"] != "Off" {
		t.Errorf("after Edit = %+v", r)
	}
	if nd, _ := nets.Detail(ctx, project, []string{"blue"}); nd.Sections[4].Listing.Total != 1 {
		t.Errorf("blue's VPC network peering tab = %+v", nd.Sections[4].Listing)
	}

	// Effective routes: green's subnet, exchanged by the ACTIVE peering.
	eff := computeEffectiveRoutesProvider{screen}
	el, err := eff.List(console.WithScopeValue(ctx, "network", "blue"), project)
	if err != nil || len(el.Filters) != 2 {
		t.Fatalf("effective routes = %+v, %v", el, err)
	}
	var peering, local bool
	for _, it := range el.Items {
		peering = peering || (it.Fields["Destination IP range"] == "10.20.0.0/24" && it.Fields["Route type"] == "Peering subnet" && it.Status == "Active")
		local = local || (it.Fields["Destination IP range"] == "10.10.0.0/24" && it.Fields["Route type"] == "Subnet")
	}
	if !peering || !local {
		t.Fatalf("effective routes of blue = %+v", el.Items)
	}

	// The firewall rule form.
	fields := fwp.CreateFormFor(ctx, project)
	if got := vpcFieldNames(fields); got != "name,description,logs,network,priority,direction,action,targets,targetTags,targetServiceAccount,"+
		"sourceFilter,sourceRanges,sourceTags,sourceServiceAccount,secondSourceFilter,secondSourceTags,secondSourceServiceAccount,"+
		"destinationFilter,destinationRanges,protocolsMode,tcp,tcpPorts,udp,udpPorts,other,otherProtocols,enforcement" {
		t.Fatalf("Create firewall rule fields = %s", got)
	}
	if _, err := fwp.Create(ctx, project, map[string]string{"name": "web", "network": "blue", "logs": "On", "priority": "900", "direction": "Ingress",
		"action": "Allow", "targets": fwTargetSA, "targetServiceAccount": "web@vpc-2140.iam.gserviceaccount.com",
		"sourceFilter": fwIPv4, "sourceRanges": "0.0.0.0/0", "secondSourceFilter": fwSourceTags, "secondSourceTags": "lb",
		"protocolsMode": fwSpecified, "tcp": "true", "tcpPorts": "80, 443", "udp": "true", "other": "true", "otherProtocols": "icmp",
		"enforcement": "Disabled"}); err != nil {
		t.Fatal(err)
	}
	fl, _ = fwp.List(ctx, project)
	w := vpcRow(fl, "web")
	if w == nil || w.Fields["Targets"] != "web@vpc-2140.iam.gserviceaccount.com" || w.Fields["Filters"] != "IP ranges: 0.0.0.0/0; Tags: lb" ||
		w.Fields["Protocols / ports"] != "tcp:80,443 udp icmp" || w.Fields["Logs"] != "On" || w.Status != "Disabled" {
		t.Fatalf("web rule = %+v", w)
	}
	// Its Edit's form holds what it was made with; direction and action
	// are fixed.
	wd, _ := fwp.Detail(ctx, project, []string{"web"})
	got := map[string]console.Field{}
	for _, f := range wd.Edit.Fields {
		got[f.Name] = f
	}
	if got["targets"].Default != fwTargetSA || got["secondSourceFilter"].Default != fwSourceTags || got["protocolsMode"].Default != fwSpecified ||
		got["tcpPorts"].Default != "80,443" || got["otherProtocols"].Default != "icmp" || !got["direction"].Immutable || !got["action"].Immutable ||
		got["enforcement"].Default != "Disabled" {
		t.Fatalf("web's Edit form = %+v", got)
	}
	if err := fwp.Edit(ctx, project, []string{"web"}, map[string]string{"priority": "800", "logs": "Off", "targets": fwAllTargets,
		"sourceFilter": fwSourceTags, "sourceTags": "lb", "protocolsMode": fwAll, "enforcement": "Enabled"}); err != nil {
		t.Fatal(err)
	}
	fl, _ = fwp.List(ctx, project)
	if w := vpcRow(fl, "web"); w.Fields["Type"] != "Ingress" || w.Fields["Action"] != "Allow" || w.Fields["Targets"] != "Apply to all" ||
		w.Fields["Filters"] != "Tags: lb" || w.Fields["Protocols / ports"] != "all" || w.Status != "" || w.Fields["Priority"] != "800" {
		t.Fatalf("web after Edit = %+v", w)
	}
	if _, err := fwp.Create(ctx, project, map[string]string{"name": "out", "network": "blue", "priority": "1000", "direction": "Egress", "action": "Deny",
		"targets": fwAllTargets, "destinationFilter": fwIPv4, "destinationRanges": "10.20.0.0/24", "protocolsMode": fwAll}); err != nil {
		t.Fatal(err)
	}
	if _, err := fwp.Create(ctx, project, map[string]string{"name": "none", "network": "blue", "protocolsMode": fwSpecified}); err == nil {
		t.Error("Specified protocols and ports with none chosen was accepted")
	}

	// Create route to an instance, and promoting its ephemeral address.
	vms := computeInstancesProvider{screen}
	if _, err := vms.Create(ctx, project, map[string]string{"name": "hop", "zone": "us-central1-a", "network": "blue", "subnetwork": "blue-sub"}); err != nil {
		t.Fatal(err)
	}
	settle(t, screen, project)
	routes := computeRoutesProvider{screen}
	var hopField console.Field
	for _, f := range routes.CreateFormFor(ctx, project) {
		if f.Name == "nextHopInstance" {
			hopField = f
		}
	}
	if strings.Join(hopField.Options, ",") != "us-central1-a/hop" {
		t.Fatalf("next hop instances = %v", hopField.Options)
	}
	if _, err := routes.Create(ctx, project, map[string]string{"name": "via-hop", "network": "blue", "destRange": "192.168.0.0/16", "priority": "1000",
		"nextHop": hopInstance, "nextHopInstance": "us-central1-a/hop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := routes.Create(ctx, project, map[string]string{"name": "via-vpn", "network": "blue", "destRange": "192.169.0.0/16", "nextHop": hopVPN}); err == nil {
		t.Error("a VPN tunnel next hop was accepted")
	}
	rl, _ := routes.List(ctx, project)
	if r := vpcRow(rl, "via-hop"); r == nil || r.Fields["Next hop"] != "Instance hop (zone us-central1-a)" {
		t.Fatalf("via-hop = %+v", r)
	}
	addrs := computeAddressesProvider{screen}
	internal := computeInternalAddressesProvider{addrs}
	il, _ := internal.List(ctx, project)
	var eph *console.Resource
	for i := range il.Items {
		if il.Items[i].Fields["Type"] == "Ephemeral" {
			eph = &il.Items[i]
		}
	}
	if eph == nil || !strings.HasPrefix(eph.Fields["IP address"], "10.10.0.") || eph.Fields["In use by"] != "VM instance hop (us-central1-a)" ||
		vpcActionIDs(eph.Actions) != "promote" || eph.Actions[0].Unavailable != "" {
		t.Fatalf("hop's ephemeral internal address = %+v", eph)
	}
	if got := vpcActionIDs(addrs.DetailActions(ctx, project, eph.Target)); got != "promote" {
		t.Fatalf("the ephemeral row's actions at its target = %s", got)
	}
	if err := addrs.ActAt(ctx, project, eph.Target, "promote", map[string]string{"name": "hop-ip"}); err != nil {
		t.Fatal(err)
	}
	il, _ = internal.List(ctx, project)
	if r := vpcRow(il, "hop-ip"); r == nil || r.Status != "In_use" && r.Status != "In use" || r.Fields["In use by"] != "hop" || r.Fields["Type"] != "Static" {
		t.Fatalf("promoted = %+v (all %+v)", r, il.Items)
	}
	for _, it := range il.Items {
		if it.Fields["Type"] == "Ephemeral" {
			t.Errorf("the promoted address is still listed ephemeral: %+v", it)
		}
	}
	el2, _ := addrs.List(ctx, project)
	for _, it := range el2.Items {
		if it.Fields["Type"] == "Ephemeral" && (len(it.Actions) != 1 || it.Actions[0].Unavailable == "") {
			t.Errorf("an external ephemeral address offers Promote: %+v", it)
		}
	}
}
