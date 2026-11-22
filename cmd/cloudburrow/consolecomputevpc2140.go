package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The VPC network pages of the 2026-11-22 parity round (#2140): the VPC
// networks page's Subnets in current project tab, and VPC network peering,
// over the Compute Engine API's networks and subnetworks and its
// addPeering, removePeering and updatePeering.

// --- Subnets in current project ---------------------------------------------

// computeSubnetsProvider is the VPC networks page's Subnets in current
// project tab (vpc/docs/create-modify-vpc-networks, read 2026-11-22):
// every subnet of the project, each opening its page at [region, name],
// with Add subnet in a network chosen.
type computeSubnetsProvider struct{ computeScreen }

func (computeSubnetsProvider) ID() string    { return "vpc-subnets" }
func (computeSubnetsProvider) Title() string { return "Subnets in current project" }

func (p computeSubnetsProvider) networks() computeNetworksProvider {
	return computeNetworksProvider(p)
}

func (p computeSubnetsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "Network", "IP stack type", "Internal IP ranges", "Secondary IPv4 ranges",
		"Gateway", "Private Google Access", "Flow logs"}, Noun: "subnets", Note: computeNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Subnets are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	m, err := p.call("GET", project, "aggregated/subnetworks", nil, nil)
	if err != nil {
		return listing, err
	}
	subs := aggregatedItems(m, "subnetworks")
	sort.Slice(subs, func(i, j int) bool {
		a, b := vpcLast(vpcStr(subs[i], "network"))+"/"+vpcStr(subs[i], "name"), vpcLast(vpcStr(subs[j], "network"))+"/"+vpcStr(subs[j], "name")
		if a != b {
			return a < b
		}
		return vpcLast(vpcStr(subs[i], "region")) < vpcLast(vpcStr(subs[j], "region"))
	})
	for _, s := range subs {
		region := vpcLast(vpcStr(s, "region"))
		target := []string{region, vpcStr(s, "name")}
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(s, "name"), Opens: target, Target: target,
			Fields: map[string]string{"Region": region, "Network": vpcLast(vpcStr(s, "network")), "IP stack type": "IPv4 (single-stack)",
				"Internal IP ranges": vpcStr(s, "ipCidrRange"), "Secondary IPv4 ranges": vpcDash(secondaryText(s)),
				"Gateway": vpcStr(s, "gatewayAddress"), "Private Google Access": vpcOnOff(s["privateIpGoogleAccess"] == true),
				"Flow logs": vpcOnOff(flowLogsOn(s))}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p computeSubnetsProvider) ListActions(ctx context.Context, project string) []console.Action {
	if p.api() == nil || project == "" {
		return nil
	}
	return []console.Action{addSubnetAction(p.networkNames(ctx, project))}
}

// netPath is the network page's path of the subnet at [region, name].
func (p computeSubnetsProvider) netPath(project string, path []string) ([]string, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("a subnet is addressed by its region and name, not %v", path)
	}
	s, err := p.call("GET", project, "regions/"+path[0]+"/subnetworks/"+path[1], nil, nil)
	if err != nil {
		return nil, err
	}
	return []string{vpcLast(vpcStr(s, "network")), "subnets", path[0], path[1]}, nil
}

func (p computeSubnetsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	np, err := p.netPath(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	d, err := p.networks().subnetDetail(project, np)
	d.Trail = nil
	return d, err
}

func (p computeSubnetsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil {
		return nil
	}
	if len(path) == 0 {
		return p.ListActions(ctx, project)
	}
	np, err := p.netPath(project, path)
	if err != nil {
		return nil
	}
	return p.networks().DetailActions(ctx, project, np)
}

func (p computeSubnetsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 0 && action == "addsubnet" {
		network := strings.TrimSpace(values["network"])
		if network == "" {
			return errors.New("VPC network: choose the network to add the subnet to")
		}
		return p.networks().addSubnet(project, network, values)
	}
	np, err := p.netPath(project, path)
	if err != nil {
		return err
	}
	return p.networks().ActAt(ctx, project, np, action, values)
}

func (p computeSubnetsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	np, err := p.netPath(project, path)
	if err != nil {
		return err
	}
	return p.networks().subnetEdit(project, np, values)
}

// --- VPC network peering ----------------------------------------------------

// peeringListing is a network page's VPC network peering tab.
func peeringListing(n map[string]any) console.Listing {
	l := console.Listing{Columns: peeringColumns(), Noun: "peering connections"}
	for _, e := range vmMaps(n, "peerings") {
		l.Items = append(l.Items, peeringRow(vpcStr(n, "name"), e))
	}
	l.Total = len(l.Items)
	return l
}

func peeringColumns() []string {
	return []string{"Your VPC network", "Peered VPC network", "Peered project ID", "Status", "Import custom routes", "Export custom routes"}
}

// peeringProject is the project a peer network link names.
func peeringProject(link string) string {
	if i := strings.Index(link, "projects/"); i >= 0 {
		return strings.SplitN(link[i+len("projects/"):], "/", 2)[0]
	}
	return ""
}

func peeringState(p map[string]any) string {
	if vpcStr(p, "state") == "ACTIVE" {
		return "Active"
	}
	return "Inactive"
}

func peeringRow(network string, p map[string]any) console.Resource {
	target := []string{network, vpcStr(p, "name")}
	return console.Resource{Name: vpcStr(p, "name"), Status: peeringState(p), Target: target, Opens: target,
		Fields: map[string]string{"Your VPC network": network, "Peered VPC network": vpcLast(vpcStr(p, "network")),
			"Peered project ID": peeringProject(vpcStr(p, "network")), "Status": peeringState(p),
			"Import custom routes": vpcOnOff(p["importCustomRoutes"] == true), "Export custom routes": vpcOnOff(p["exportCustomRoutes"] == true)}}
}

// computePeeringProvider is VPC network peering (vpc/docs/using-vpc-peering,
// read 2026-11-22): every peering of the project's networks, Create
// connection, a connection's page with Edit, and Delete.
type computePeeringProvider struct{ computeScreen }

func (computePeeringProvider) ID() string    { return "vpc-peering" }
func (computePeeringProvider) Title() string { return "VPC network peering" }

func (p computePeeringProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: peeringColumns(), Noun: "peering connections", AlwaysStatus: true,
		Note: "A peering is ACTIVE once the peer network has a peering back to this one; then the two networks' VMs reach each other at their internal IPs over the exchanged subnet routes, each network's firewall rules deciding. Peering is not transitive. Custom routes are records only."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Peering connections are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	nets, err := p.list(project, "global/networks")
	if err != nil {
		return listing, err
	}
	for _, n := range nets {
		for _, e := range vmMaps(n, "peerings") {
			r := peeringRow(vpcStr(n, "name"), e)
			r.Actions = []console.Action{peeringDelete(r.Target)}
			listing.Items = append(listing.Items, r)
		}
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func peeringDelete(path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, Confirm: "Delete peering connection " + path[1] + "?"}
}

func (computePeeringProvider) CreateOnPage() bool  { return true }
func (computePeeringProvider) CreateTitle() string { return "Create peering connection" }

func (computePeeringProvider) CreateForm() (string, []console.Field) {
	return "Create connection", peeringFields("", nil, nil)
}

func (p computePeeringProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return peeringFields(project, p.networkNames(ctx, project), nil)
}

// peeringFields are Google's Create peering connection's: Peering
// connection name, Your VPC network, Peered VPC network (in this project or
// another), the IP stack type, Exchange IPv4 custom routes, Import subnet
// routes with public IP, and Advanced options' Update strategy; cur, an
// existing peering, makes them its Edit's.
func peeringFields(project string, networks []string, cur map[string]any) []console.Field {
	here := map[string]string{"peerWhere": "here"}
	other := map[string]string{"peerWhere": "other"}
	def := func(k string) string {
		if cur == nil {
			return "false"
		}
		return vpcStr(cur, k)
	}
	strategy := "INDEPENDENT"
	if cur != nil && vpcStr(cur, "updateStrategy") != "" {
		strategy = vpcStr(cur, "updateStrategy")
	}
	routes := []console.Field{
		{Name: "importCustomRoutes", Label: "Import custom routes", Type: "checkbox", Section: "Exchange IPv4 custom routes", Default: def("importCustomRoutes")},
		{Name: "exportCustomRoutes", Label: "Export custom routes", Type: "checkbox", Section: "Exchange IPv4 custom routes", Default: def("exportCustomRoutes")},
		{Name: "importSubnetRoutesWithPublicIp", Label: "Import subnet routes with public IP", Type: "checkbox",
			Section: "Exchange subnet routes with privately used public IPv4 addresses", Default: def("importSubnetRoutesWithPublicIp")},
		{Name: "updateStrategy", Label: "Update strategy", Type: "radio", Section: "Advanced options", Default: strategy,
			Options: []string{"INDEPENDENT", "CONSENSUS"}, OptionLabels: map[string]string{"INDEPENDENT": "Independent", "CONSENSUS": "Consensus"}},
	}
	if cur != nil {
		return routes
	}
	netField := console.Field{Name: "network", Label: "Your VPC network", Type: "select", Required: true, Options: networks}
	peerField := console.Field{Name: "peerNetwork", Label: "VPC network name", Type: "select", Required: true, Options: networks, VisibleWhen: here}
	if networks == nil {
		netField.Type, peerField.Type = "text", "text"
	}
	inProject := "In project " + project
	if project == "" {
		inProject = "In this project"
	}
	return append([]console.Field{
		{Name: "name", Label: "Peering connection name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		netField,
		{Name: "peerWhere", Label: "Peered VPC network", Type: "radio", Default: "here", Options: []string{"here", "other"},
			OptionLabels: map[string]string{"here": inProject, "other": "In another project"}},
		peerField,
		{Name: "peerProject", Label: "Project ID", Type: "text", Required: true, VisibleWhen: other,
			Help: "A project this instance keeps networks for."},
		{Name: "peerNetworkName", Label: "VPC network name", Type: "text", Required: true, Pattern: computeNamePattern, VisibleWhen: other},
		{Name: "stackType", Label: "IP stack type", Type: "radio", Default: "IPV4_ONLY", Options: []string{"IPV4_ONLY", "IPV4_IPV6"},
			OptionLabels:       map[string]string{"IPV4_ONLY": "IPv4 (single-stack)", "IPV4_IPV6": "IPv4 and IPv6 (dual-stack)"},
			UnavailableOptions: map[string]string{"IPV4_IPV6": "IPv6 is not served: networks are IPv4 only."}},
	}, routes...)
}

func peeringBool(values map[string]string, k string) bool { return values[k] == "true" }

func (p computePeeringProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	network := strings.TrimSpace(values["network"])
	peer := "projects/" + project + "/global/networks/" + strings.TrimSpace(values["peerNetwork"])
	if values["peerWhere"] == "other" {
		peer = "projects/" + strings.TrimSpace(values["peerProject"]) + "/global/networks/" + strings.TrimSpace(values["peerNetworkName"])
	}
	np := map[string]any{"name": name, "network": peer, "exchangeSubnetRoutes": true,
		"importCustomRoutes": peeringBool(values, "importCustomRoutes"), "exportCustomRoutes": peeringBool(values, "exportCustomRoutes"),
		"importSubnetRoutesWithPublicIp": peeringBool(values, "importSubnetRoutesWithPublicIp"),
		"updateStrategy":                 vpcOr(values["updateStrategy"], "INDEPENDENT")}
	if st := values["stackType"]; st != "" {
		np["stackType"] = st
	}
	if _, err := p.call("POST", project, "global/networks/"+network+"/addPeering", nil, map[string]any{"networkPeering": np}); err != nil {
		return "", err
	}
	return name, nil
}

func (p computePeeringProvider) peering(project string, path []string) (map[string]any, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("a peering connection is addressed by its network and name, not %v", path)
	}
	n, err := p.call("GET", project, "global/networks/"+path[0], nil, nil)
	if err != nil {
		return nil, err
	}
	for _, e := range vmMaps(n, "peerings") {
		if vpcStr(e, "name") == path[1] {
			return e, nil
		}
	}
	return nil, fmt.Errorf("network %s has no peering connection %s", path[0], path[1])
}

func (p computePeeringProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	e, err := p.peering(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	row := peeringRow(path[0], e)
	var props []console.Property
	for _, c := range peeringColumns() {
		props = append(props, console.Property{Label: c, Value: row.Fields[c]})
	}
	props = append(props,
		console.Property{Label: "Exchange subnet routes with public IP", Value: "Export " + vpcOnOff(e["exportSubnetRoutesWithPublicIp"] == true) +
			", import " + vpcOnOff(e["importSubnetRoutesWithPublicIp"] == true)},
		console.Property{Label: "IP stack type", Value: "IPv4 (single-stack)"},
		console.Property{Label: "Update strategy", Value: map[string]string{"CONSENSUS": "Consensus"}[vpcStr(e, "updateStrategy")]},
		console.Property{Label: "Peer MTU", Value: vpcDash(vpcStr(e, "peerMtu"))},
		console.Property{Label: "State details", Value: vpcDash(vpcStr(e, "stateDetails"))})
	if props[len(props)-3].Value == "" {
		props[len(props)-3].Value = "Independent"
	}
	return console.Detail{Title: path[1], Heading: "Peering connection details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Properties: props}}}},
		Edit: &console.EditForm{Label: "Edit", Fields: peeringFields(project, nil, e)}}, nil
}

func (p computePeeringProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	if _, err := p.peering(project, path); err != nil {
		return nil
	}
	a := peeringDelete(path)
	a.Leaves = true
	return []console.Action{a}
}

func (p computePeeringProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("POST", project, "global/networks/"+path[0]+"/removePeering", nil, map[string]any{"name": path[1]})
	return err
}

func (p computePeeringProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 2 {
		return errors.New("no peering connection")
	}
	np := map[string]any{"name": path[1], "importCustomRoutes": peeringBool(values, "importCustomRoutes"),
		"exportCustomRoutes": peeringBool(values, "exportCustomRoutes"), "importSubnetRoutesWithPublicIp": peeringBool(values, "importSubnetRoutesWithPublicIp")}
	if us := values["updateStrategy"]; us != "" {
		np["updateStrategy"] = us
	}
	_, err := p.call("PATCH", project, "global/networks/"+path[0]+"/updatePeering", nil, map[string]any{"networkPeering": np})
	return err
}

func (p computePeeringProvider) RowEditLabel() string { return "Edit" }

// --- Effective routes ---------------------------------------------------------

// computeEffectiveRoutesProvider is the Routes page's Effective routes tab
// (vpc/docs/using-routes, read 2026-11-22: "select a network and region,
// then View"): every route that applies to the network's VMs in a region,
// the network's subnet, default and static routes and the subnet routes an
// ACTIVE peering exchanges, each with its Status.
type computeEffectiveRoutesProvider struct{ computeScreen }

func (computeEffectiveRoutesProvider) ID() string    { return "vpc-routes-effective" }
func (computeEffectiveRoutesProvider) Title() string { return "Routes" }

const (
	effNetworkKey = "network"
	effRegionKey  = "region"
)

func (computeEffectiveRoutesProvider) FilterKeys() []string {
	return []string{effNetworkKey, effRegionKey}
}

var _ console.Filterer = computeEffectiveRoutesProvider{}

func (p computeEffectiveRoutesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Status", "Destination IP range", "Priority", "Route type", "Next hop", "Instance tags"},
		Noun: "routes", Note: computeNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Effective routes are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	nets := p.networkNames(ctx, project)
	network := console.ScopeValue(ctx, effNetworkKey)
	if network == "" && len(nets) > 0 {
		network = nets[0]
		for _, n := range nets {
			if n == "default" {
				network = n
			}
		}
	}
	region := vpcOr(console.ScopeValue(ctx, effRegionKey), "us-central1")
	netScope := &console.Scope{Key: effNetworkKey, Label: "Network", Selected: network}
	for _, n := range nets {
		netScope.Options = append(netScope.Options, console.ScopeOption{ID: n})
	}
	regScope := &console.Scope{Key: effRegionKey, Label: "Region", Selected: region, Default: "us-central1"}
	for _, r := range computeRegionField("").Options {
		regScope.Options = append(regScope.Options, console.ScopeOption{ID: r})
	}
	listing.Filters = []*console.Scope{netScope, regScope}
	if network == "" {
		listing.Prompt = "The project has no VPC network."
		return listing, nil
	}
	filter := url.Values{"filter": {`network = "projects/` + project + `/global/networks/` + network + `"`}}
	l, err := p.call("GET", project, "global/routes", filter, nil)
	if err != nil {
		return listing, err
	}
	insts := map[string]bool{}
	for _, i := range p.instanceNames(project) {
		insts[i] = true
	}
	for _, r := range computeItems(l) {
		row := routeRow(r)
		status := "Active"
		if hi := vpcStr(r, "nextHopInstance"); hi != "" && !insts[zoneOf(hi)+"/"+vpcLast(hi)] {
			status = "Inactive"
		}
		row.Status = status
		row.Fields["Status"] = status
		listing.Items = append(listing.Items, row)
	}
	// The subnet routes each ACTIVE peering exchanges.
	if n, err := p.call("GET", project, "global/networks/"+network, nil, nil); err == nil {
		for _, pe := range vmMaps(n, "peerings") {
			if vpcStr(pe, "state") != "ACTIVE" {
				continue
			}
			peerProject := peeringProject(vpcStr(pe, "network"))
			peer, err := p.call("GET", peerProject, "global/networks/"+vpcLast(vpcStr(pe, "network")), nil, nil)
			if err != nil {
				continue
			}
			for _, sl := range vpcStrs(peer, "subnetworks") {
				parts := strings.Split(sl, "/")
				if len(parts) < 6 {
					continue
				}
				s, err := p.call("GET", peerProject, "regions/"+parts[3]+"/subnetworks/"+parts[5], nil, nil)
				if err != nil {
					continue
				}
				listing.Items = append(listing.Items, console.Resource{Name: "peering-route-" + vpcStr(pe, "name") + "-" + parts[5] + "-" + parts[3], Status: "Active",
					Fields: map[string]string{"Status": "Active", "Destination IP range": vpcStr(s, "ipCidrRange"), "Priority": "0",
						"Route type": "Peering subnet", "Next hop": "Peering " + vpcStr(pe, "name"), "Instance tags": "—",
						"Network": network, "Description": "—"}})
			}
		}
	}
	sort.SliceStable(listing.Items, func(i, j int) bool {
		return listing.Items[i].Fields["Destination IP range"] < listing.Items[j].Fields["Destination IP range"]
	})
	listing.Total = len(listing.Items)
	return listing, nil
}
