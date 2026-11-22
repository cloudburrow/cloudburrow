package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Cloud Router and Cloud NAT (#2140), over the Compute Engine API's
// routers: Network Connectivity's Cloud Routers page (Create a router, a
// router's details with its BGP sessions and NAT gateways), and Network
// services' Cloud NAT page (Create Cloud NAT gateway, a gateway's details
// with its mappings), as network-connectivity/docs/router/how-to/create-router-vpc-network,
// viewing-router-details and nat/docs/set-up-manage-network-address-translation
// (read 2026-11-22) describe them.

const routerNote = "BGP sessions never come up locally: no VPN tunnel, Interconnect attachment or peer router exists, so every BGP session is Down. " +
	"A Cloud NAT gateway is what gives a VM without an external IP its internet egress, as on Google."

// --- Cloud Routers ------------------------------------------------------------

type computeRoutersProvider struct{ computeScreen }

func (computeRoutersProvider) ID() string    { return "hybrid-routers" }
func (computeRoutersProvider) Title() string { return "Cloud Routers" }

// routerColumns are the list's columns: "BGP sessions" is in Google's docs
// (viewing-router-details); the rest are UNVERIFIED (re-read 2026-11-22).
func routerColumns() []string {
	return []string{"Network", "Region", "Google ASN", "Interconnect / VPN gateway", "BGP sessions", "Cloud NAT gateways"}
}

func (p computeRoutersProvider) routers(project string) ([]map[string]any, error) {
	m, err := p.call("GET", project, "aggregated/routers", nil, nil)
	if err != nil {
		return nil, err
	}
	rs := aggregatedItems(m, "routers")
	sort.Slice(rs, func(i, j int) bool {
		a := vpcLast(vpcStr(rs[i], "region")) + "/" + vpcStr(rs[i], "name")
		b := vpcLast(vpcStr(rs[j], "region")) + "/" + vpcStr(rs[j], "name")
		return a < b
	})
	return rs, nil
}

func routerASN(r map[string]any) string {
	bgp, _ := r["bgp"].(map[string]any)
	return vpcDash(vpcStr(bgp, "asn"))
}

func routerSessions(r map[string]any) string {
	n := len(vmMaps(r, "bgpPeers"))
	if n == 0 {
		return "—"
	}
	return strconv.Itoa(n) + " down"
}

func routerNATNames(r map[string]any) string {
	var names []string
	for _, n := range vmMaps(r, "nats") {
		names = append(names, vpcStr(n, "name"))
	}
	return vpcDash(strings.Join(names, ", "))
}

func routerRow(r map[string]any) console.Resource {
	region := vpcLast(vpcStr(r, "region"))
	target := []string{region, vpcStr(r, "name")}
	return console.Resource{Name: vpcStr(r, "name"), Opens: target, Target: target,
		Actions: []console.Action{routerDelete(target)},
		Fields: map[string]string{"Network": vpcLast(vpcStr(r, "network")), "Region": region, "Google ASN": routerASN(r),
			"Interconnect / VPN gateway": "—", "BGP sessions": routerSessions(r), "Cloud NAT gateways": routerNATNames(r)}}
}

func routerDelete(path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete Cloud Router " + path[1] + "? Its BGP sessions and Cloud NAT gateways are deleted with it."}
}

func (p computeRoutersProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: routerColumns(), Noun: "Cloud Routers", Note: routerNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Cloud Routers are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	rs, err := p.routers(project)
	if err != nil {
		return listing, err
	}
	for _, r := range rs {
		listing.Items = append(listing.Items, routerRow(r))
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeRoutersProvider) CreateOnPage() bool  { return true }
func (computeRoutersProvider) CreateTitle() string { return "Create a router" }

func (computeRoutersProvider) CreateSubmit() string { return "Create" }

func (computeRoutersProvider) CreateForm() (string, []console.Field) {
	return "Create router", routerFields(nil, nil)
}

func (p computeRoutersProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return routerFields(p.networkNames(ctx, project), nil)
}

// routerFields are Create a router's: Name, Description, Network, Region,
// Google ASN, BGP peer keepalive interval, BGP identifier, and Advertised
// routes (all subnets, or custom routes with Add a custom route); cur, an
// existing router, makes them its Edit's (name, network and region fixed).
func routerFields(networks []string, cur map[string]any) []console.Field {
	bgp, _ := cur["bgp"].(map[string]any)
	mode, subnets, ranges := "DEFAULT", "true", ""
	asn, keep, ident := "", "20", ""
	if cur != nil {
		asn, ident = vpcStr(bgp, "asn"), vpcStr(bgp, "identifierRange")
		if k := vpcStr(bgp, "keepaliveInterval"); k != "" {
			keep = k
		}
		if vpcStr(bgp, "advertiseMode") == "CUSTOM" {
			mode = "CUSTOM"
			subnets = strconv.FormatBool(hasString(vpcStrs(bgp, "advertisedGroups"), "ALL_SUBNETS"))
			var rows []map[string]string
			for _, r := range vmMaps(bgp, "advertisedIpRanges") {
				rows = append(rows, map[string]string{"range": vpcStr(r, "range"), "description": vpcStr(r, "description")})
			}
			ranges = console.FormatGroup(rows)
		}
	}
	custom := map[string]string{"advertiseMode": "CUSTOM"}
	tail := []console.Field{
		{Name: "asn", Label: "Google ASN", Type: "text", Required: true, Default: asn, Pattern: `^[0-9]+$`,
			Help: "Any private ASN (64512-65534, 4200000000-4294967294) that you are not already using in the on-premises network, or 16550 for Partner Interconnect."},
		{Name: "keepaliveInterval", Label: "BGP peer keepalive interval", Type: "text", Default: keep, Pattern: `^[0-9]+$`,
			Help: "Seconds, from 20 to 60. The default is 20."},
		{Name: "identifierRange", Label: "BGP identifier", Type: "text", Default: ident,
			Help: "Optional. A link-local IPv4 range in 169.254.0.0/16, /30 or larger."},
		{Name: "advertiseMode", Label: "Routes", Type: "radio", Section: "Advertised routes", Default: mode, Options: []string{"DEFAULT", "CUSTOM"},
			OptionLabels: map[string]string{"DEFAULT": "Advertise all subnets visible to the Cloud Router (Default)", "CUSTOM": "Create custom routes"}},
		{Name: "advertiseSubnets", Label: "Advertise all subnets visible to the Cloud Router", Type: "checkbox", Section: "Advertised routes",
			Default: subnets, VisibleWhen: custom},
		{Name: "customRanges", Label: "Custom routes", Type: console.GroupFieldType, Section: "Advertised routes", AddLabel: "Add a custom route",
			Default: ranges, VisibleWhen: custom, Group: []console.Field{
				{Name: "range", Label: "IP range", Type: "text", Required: true, Help: "For example, 192.168.10.0/24"},
				{Name: "description", Label: "Description", Type: "text"},
			}},
	}
	if cur != nil {
		return append([]console.Field{{Name: "description", Label: "Description", Type: "text", Default: vpcStr(cur, "description")}}, tail...)
	}
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Options: networks}
	if networks == nil {
		netField.Type = "text"
	}
	return append([]console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text"},
		netField,
		computeRegionField("region"),
	}, tail...)
}

func hasString(l []string, v string) bool {
	for _, e := range l {
		if e == v {
			return true
		}
	}
	return false
}

// routerBGP is the bgp the form's values make.
func routerBGP(values map[string]string) (map[string]any, error) {
	asn, err := strconv.ParseUint(strings.TrimSpace(values["asn"]), 10, 32)
	if err != nil {
		return nil, errors.New("Google ASN: enter a private ASN (64512-65534 or 4200000000-4294967294) or 16550")
	}
	bgp := map[string]any{"asn": float64(asn), "advertiseMode": vpcOr(values["advertiseMode"], "DEFAULT")}
	if k := strings.TrimSpace(values["keepaliveInterval"]); k != "" {
		v, err := strconv.Atoi(k)
		if err != nil {
			return nil, errors.New("BGP peer keepalive interval: enter a number of seconds from 20 to 60")
		}
		bgp["keepaliveInterval"] = float64(v)
	}
	if id := strings.TrimSpace(values["identifierRange"]); id != "" {
		bgp["identifierRange"] = id
	}
	if bgp["advertiseMode"] == "CUSTOM" {
		groups := []any{}
		if values["advertiseSubnets"] == "true" {
			groups = append(groups, "ALL_SUBNETS")
		}
		bgp["advertisedGroups"] = groups
		rows, err := console.ParseGroup(values["customRanges"])
		if err != nil {
			return nil, err
		}
		ranges := []any{}
		for _, r := range rows {
			e := map[string]any{"range": strings.TrimSpace(r["range"])}
			if d := strings.TrimSpace(r["description"]); d != "" {
				e["description"] = d
			}
			ranges = append(ranges, e)
		}
		bgp["advertisedIpRanges"] = ranges
	}
	return bgp, nil
}

func (p computeRoutersProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	bgp, err := routerBGP(values)
	if err != nil {
		return "", err
	}
	body := map[string]any{"name": name, "network": "global/networks/" + strings.TrimSpace(values["network"]), "bgp": bgp}
	if d := strings.TrimSpace(values["description"]); d != "" {
		body["description"] = d
	}
	region := vpcOr(values["region"], "us-central1")
	if _, err := p.call("POST", project, "regions/"+region+"/routers", nil, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeRoutersProvider) router(project string, path []string) (map[string]any, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("a Cloud Router is addressed by its region and name, not %v", path)
	}
	return p.call("GET", project, "regions/"+path[0]+"/routers/"+path[1], nil, nil)
}

func advertisedText(bgp map[string]any) string {
	if vpcStr(bgp, "advertiseMode") != "CUSTOM" {
		return "Advertise all subnets visible to the Cloud Router (Default)"
	}
	var parts []string
	if hasString(vpcStrs(bgp, "advertisedGroups"), "ALL_SUBNETS") {
		parts = append(parts, "All subnets")
	}
	for _, r := range vmMaps(bgp, "advertisedIpRanges") {
		parts = append(parts, vpcStr(r, "range"))
	}
	return "Custom: " + vpcDash(strings.Join(parts, ", "))
}

func (p computeRoutersProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	r, err := p.router(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	bgp, _ := r["bgp"].(map[string]any)
	status, _ := p.call("GET", project, "regions/"+path[0]+"/routers/"+path[1]+"/getRouterStatus", nil, nil)
	result, _ := status["result"].(map[string]any)
	peerStatus := map[string]map[string]any{}
	for _, ps := range vmMaps(result, "bgpPeerStatus") {
		peerStatus[vpcStr(ps, "name")] = ps
	}
	sessions := console.Listing{Columns: []string{"Peer ASN", "Cloud Router BGP IP", "BGP peer IP", "Status", "Advertised routes", "BFD", "MD5 authentication"},
		Noun: "BGP sessions", Note: "No peer router exists locally, so each session stays Down; Advertised routes are what the router would advertise once it is up."}
	for _, peer := range vmMaps(r, "bgpPeers") {
		ps := peerStatus[vpcStr(peer, "name")]
		bfd, _ := peer["bfd"].(map[string]any)
		status := "Down"
		if vpcStr(peer, "enable") == "FALSE" {
			status = "Disabled"
		}
		sessions.Items = append(sessions.Items, console.Resource{Name: vpcStr(peer, "name"), Status: status, Fields: map[string]string{
			"Peer ASN": vpcStr(peer, "peerAsn"), "Cloud Router BGP IP": vpcDash(vpcStr(peer, "ipAddress")), "BGP peer IP": vpcDash(vpcStr(peer, "peerIpAddress")),
			"Status": status, "Advertised routes": strconv.Itoa(len(vmMaps(ps, "advertisedRoutes"))),
			"BFD": vpcTitle(strings.ToLower(vpcOr(vpcStr(bfd, "sessionInitializationMode"), "DISABLED"))), "MD5 authentication": vpcOnOff(vpcStr(peer, "md5AuthenticationKeyName") != "")}})
	}
	sessions.Total = len(sessions.Items)
	nats := console.Listing{Columns: natColumns(), Noun: "Cloud NAT gateways"}
	for _, n := range vmMaps(r, "nats") {
		nats.Items = append(nats.Items, natRow(r, n))
	}
	nats.Total = len(nats.Items)
	ifaces := console.Listing{Columns: []string{"IP range", "Linked to", "Private IP address"}, Noun: "interfaces"}
	for _, in := range vmMaps(r, "interfaces") {
		linked := "—"
		if s := vpcStr(in, "subnetwork"); s != "" {
			linked = "Subnetwork " + vpcLast(s)
		}
		ifaces.Items = append(ifaces.Items, console.Resource{Name: vpcStr(in, "name"), Fields: map[string]string{"IP range": vpcDash(vpcStr(in, "ipRange")),
			"Linked to": linked, "Private IP address": vpcDash(vpcStr(in, "privateIpAddress"))}})
	}
	ifaces.Total = len(ifaces.Items)
	props := []console.Property{
		{Label: "Name", Value: vpcStr(r, "name")},
		{Label: "Description", Value: vpcDash(vpcStr(r, "description"))},
		{Label: "Network", Value: vpcLast(vpcStr(r, "network"))},
		{Label: "Region", Value: path[0]},
		{Label: "Google ASN", Value: routerASN(r)},
		{Label: "BGP peer keepalive interval", Value: vpcDash(vpcStr(bgp, "keepaliveInterval")) + " seconds"},
		{Label: "BGP identifier", Value: vpcDash(vpcStr(bgp, "identifierRange"))},
		{Label: "Advertised routes", Value: advertisedText(bgp)},
	}
	if bgp == nil {
		props[5].Value = "—"
	}
	return console.Detail{Title: path[1], Heading: "Router details",
		Sections: []console.Section{
			{ID: "details", Label: "Router details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "bgp", Label: "BGP sessions", Listing: sessions,
				Unavailable: "Add BGP session needs a Cloud VPN tunnel or a Cloud Interconnect VLAN attachment, which are not served; a router appliance session (an interface on a subnetwork and a VM as peer) is made through the API."},
			{ID: "nat", Label: "Cloud NAT gateways", Listing: nats},
			{ID: "interfaces", Label: "Interfaces", Listing: ifaces},
			p.routePoliciesSection(project, path),
		},
		Edit: &console.EditForm{Label: "Edit", Note: "A router's name, network and region are fixed.", Fields: routerFields(nil, r)}}, nil
}

func (p computeRoutersProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	if _, err := p.router(project, path); err != nil {
		return nil
	}
	a := routerDelete(path)
	a.Leaves = true
	return append(routePolicyActions(p.routePolicyNames(project, path)), a)
}

func (p computeRoutersProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 2 && (action == "createRoutePolicy" || action == "deleteRoutePolicy") {
		return p.actRoutePolicy(project, path, action, values)
	}
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("DELETE", project, "regions/"+path[0]+"/routers/"+path[1], nil, nil)
	return err
}

func (p computeRoutersProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 2 {
		return errors.New("no Cloud Router")
	}
	bgp, err := routerBGP(values)
	if err != nil {
		return err
	}
	_, err = p.call("PATCH", project, "regions/"+path[0]+"/routers/"+path[1], nil,
		map[string]any{"description": strings.TrimSpace(values["description"]), "bgp": bgp})
	return err
}

// --- Cloud NAT ------------------------------------------------------------------

type computeNATProvider struct{ computeScreen }

func (computeNATProvider) ID() string    { return "netservices-nat" }
func (computeNATProvider) Title() string { return "Cloud NAT" }

// natColumns: Status is in Google's docs (set-up-manage-network-address-translation);
// the rest are UNVERIFIED (re-read 2026-11-22).
func natColumns() []string {
	return []string{"Status", "NAT type", "Network", "Region", "Cloud Router", "NAT IP addresses"}
}

func natAllocText(n map[string]any) string {
	if vpcStr(n, "natIpAllocateOption") == "MANUAL_ONLY" {
		return "Manual"
	}
	return "Automatic"
}

func natRow(r, n map[string]any) console.Resource {
	region := vpcLast(vpcStr(r, "region"))
	target := []string{region, vpcStr(r, "name"), vpcStr(n, "name")}
	return console.Resource{Name: vpcStr(n, "name"), Status: "Running", Opens: target, Target: target,
		Actions: []console.Action{natDelete(target)},
		Fields: map[string]string{"Status": "Running", "NAT type": vpcTitle(strings.ToLower(vpcOr(vpcStr(n, "type"), "PUBLIC"))),
			"Network": vpcLast(vpcStr(r, "network")), "Region": region, "Cloud Router": vpcStr(r, "name"), "NAT IP addresses": natAllocText(n)}}
}

func natDelete(path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Delete Cloud NAT gateway " + path[2] + "? Its Cloud Router " + path[1] + " is kept."}
}

func (p computeNATProvider) routers() computeRoutersProvider { return computeRoutersProvider(p) }

func (p computeNATProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: natColumns(), Noun: "Cloud NAT gateways", AlwaysStatus: true,
		Note: "A VM with no external IP reaches the internet only through a Cloud NAT gateway of its subnet, as on Google; NAT IP addresses are local documentation addresses, and logs are records only."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Cloud NAT gateways are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	rs, err := p.routers().routers(project)
	if err != nil {
		return listing, err
	}
	for _, r := range rs {
		for _, n := range vmMaps(r, "nats") {
			listing.Items = append(listing.Items, natRow(r, n))
		}
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeNATProvider) CreateOnPage() bool  { return true }
func (computeNATProvider) CreateTitle() string { return "Create Cloud NAT gateway" }

func (computeNATProvider) CreateSubmit() string { return "Create" }

// CreateForm's label is the list's button: Google's first gateway has
// "Get started", later ones "Create Cloud NAT gateway".
func (computeNATProvider) CreateForm() (string, []console.Field) {
	return "Create Cloud NAT gateway", natFields(natChoices{}, nil)
}

// natChoices are what Create Cloud NAT gateway's selects offer.
type natChoices struct {
	networks []string
	// routers are the routers' names by "network=N&region=R".
	routers map[string][]string
	// subnets are the subnets by "network=N&region=R".
	subnets map[string][]string
	// addresses are the reserved regional external addresses by "region=R".
	addresses map[string][]string
	// natSubnets are the PRIVATE_NAT subnets (Private NAT's ranges) by
	// "network=N&region=R".
	natSubnets map[string][]string
}

const natNewRouter = "__new__"

func (p computeNATProvider) choices(ctx context.Context, project string) natChoices {
	c := natChoices{networks: p.networkNames(ctx, project), routers: map[string][]string{}, subnets: map[string][]string{}, addresses: map[string][]string{},
		natSubnets: map[string][]string{}}
	if rs, err := p.routers().routers(project); err == nil {
		for _, r := range rs {
			k := "network=" + vpcLast(vpcStr(r, "network")) + "&region=" + vpcLast(vpcStr(r, "region"))
			c.routers[k] = append(c.routers[k], vpcStr(r, "name"))
		}
	}
	if m, err := p.call("GET", project, "aggregated/subnetworks", nil, nil); err == nil {
		for _, s := range aggregatedItems(m, "subnetworks") {
			k := "network=" + vpcLast(vpcStr(s, "network")) + "&region=" + vpcLast(vpcStr(s, "region"))
			if vpcStr(s, "purpose") == "PRIVATE_NAT" {
				c.natSubnets[k] = append(c.natSubnets[k], vpcStr(s, "name"))
				continue
			}
			c.subnets[k] = append(c.subnets[k], vpcStr(s, "name"))
		}
	}
	if m, err := p.call("GET", project, "aggregated/addresses", nil, nil); err == nil {
		for _, a := range aggregatedItems(m, "addresses") {
			if vpcStr(a, "addressType") == "EXTERNAL" && vpcStr(a, "region") != "" {
				k := "region=" + vpcLast(vpcStr(a, "region"))
				c.addresses[k] = append(c.addresses[k], vpcStr(a, "name"))
			}
		}
	}
	return c
}

func (p computeNATProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return natFields(p.choices(ctx, project), nil)
}

func conditional(m map[string][]string, extra ...string) []console.ConditionalOptions {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []console.ConditionalOptions
	for _, k := range keys {
		opts := append(append([]string{}, m[k]...), extra...)
		sort.Strings(opts[:len(m[k])])
		out = append(out, console.ConditionalOptions{When: k, Options: opts})
	}
	return out
}

// union is every option of m, sorted, once each.
func union(m map[string][]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range m {
		for _, v := range l {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	sort.Strings(out)
	return out
}

// natFields are Create Cloud NAT gateway's: Gateway name, NAT type, Select
// Cloud Router (Network, Region, Cloud Router or Create new router), Cloud
// NAT mapping (Source endpoint type, Source subnets: all ranges, primary
// ranges or Custom), Cloud NAT IP addresses (Automatic or Manual), Network
// service tier, and Advanced configurations (logging, port allocation,
// timeouts). cur, an existing gateway, makes them its Edit's.
func natFields(c natChoices, cur map[string]any) []console.Field {
	const mapping, adv, sel = "Cloud NAT mapping", "Advanced configurations", "Select Cloud Router"
	def := func(k, d string) string {
		if cur == nil || vpcStr(cur, k) == "" {
			return d
		}
		return vpcStr(cur, k)
	}
	custom := map[string]string{"source": "LIST_OF_SUBNETWORKS"}
	manual := map[string]string{"natIpAllocateOption": "MANUAL_ONLY"}
	dyn := map[string]string{"enableDynamicPortAllocation": "true"}
	var subRows, ipRows []map[string]string
	logging := "none"
	if cur != nil {
		for _, s := range vmMaps(cur, "subnetworks") {
			rg := "ALL_IP_RANGES"
			if hasString(vpcStrs(s, "sourceIpRangesToNat"), "PRIMARY_IP_RANGE") {
				rg = "PRIMARY_IP_RANGE"
			}
			subRows = append(subRows, map[string]string{"subnet": vpcLast(vpcStr(s, "name")), "ranges": rg})
		}
		for _, a := range vpcStrs(cur, "natIps") {
			ipRows = append(ipRows, map[string]string{"address": vpcLast(a)})
		}
		if lc, _ := cur["logConfig"].(map[string]any); lc["enable"] == true {
			logging = vpcOr(vpcStr(lc, "filter"), "ALL")
		}
	}
	subnetField := console.Field{Name: "subnet", Label: "Subnet", Type: "select", Required: true, Options: union(c.subnets), OptionsWhen: conditional(c.subnets)}
	addrField := console.Field{Name: "address", Label: "IP address", Type: "select", Required: true, Options: union(c.addresses), OptionsWhen: conditional(c.addresses)}
	if c.networks == nil {
		subnetField.Type, addrField.Type = "text", "text"
	}
	minPorts := def("minPortsPerVm", "64")
	fields := []console.Field{
		{Name: "endpointType", Label: "Source endpoint type", Type: "radio", Section: mapping, Default: "VM",
			Options: []string{"VM", "LB"}, OptionLabels: map[string]string{"VM": "VM instances, GKE nodes, Serverless", "LB": "Managed proxy load balancers"},
			UnavailableOptions: map[string]string{"LB": "Managed proxy load balancers are not served."}},
		{Name: "ipVersion", Label: "Source IP version", Type: "radio", Section: mapping, Default: "IPV4", Options: []string{"IPV4", "IPV6"},
			OptionLabels:       map[string]string{"IPV4": "IPv4 subnet ranges", "IPV6": "IPv6 subnet ranges"},
			UnavailableOptions: map[string]string{"IPV6": "IPv6 (NAT64) is not served: networks are IPv4 only."}},
		{Name: "source", Label: "Source subnets", Type: "radio", Section: mapping, Default: def("sourceSubnetworkIpRangesToNat", "ALL_SUBNETWORKS_ALL_IP_RANGES"),
			Options: []string{"ALL_SUBNETWORKS_ALL_IP_RANGES", "ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES", "LIST_OF_SUBNETWORKS"},
			OptionLabels: map[string]string{"ALL_SUBNETWORKS_ALL_IP_RANGES": "Primary and secondary IP ranges for all subnets",
				"ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES": "Primary IP ranges for all subnets", "LIST_OF_SUBNETWORKS": "Custom"}},
		{Name: "subnets", Label: "Subnets", Type: console.GroupFieldType, Section: mapping, AddLabel: "Add subnet and IP range", VisibleWhen: custom,
			Default: console.FormatGroup(subRows), Group: []console.Field{subnetField,
				{Name: "ranges", Label: "IP ranges", Type: "select", Default: "ALL_IP_RANGES", Options: []string{"ALL_IP_RANGES", "PRIMARY_IP_RANGE"},
					OptionLabels: map[string]string{"ALL_IP_RANGES": "Primary and secondary IP ranges", "PRIMARY_IP_RANGE": "Primary IP range"}}}},
		{Name: "natIpAllocateOption", Label: "Cloud NAT IP addresses", Type: "radio", Section: mapping, Default: def("natIpAllocateOption", "AUTO_ONLY"),
			Options: []string{"AUTO_ONLY", "MANUAL_ONLY"}, OptionLabels: map[string]string{"AUTO_ONLY": "Automatic (recommended)", "MANUAL_ONLY": "Manual"}},
		{Name: "natIps", Label: "IP addresses", Type: console.GroupFieldType, Section: mapping, AddLabel: "Add IP address", VisibleWhen: manual,
			Default: console.FormatGroup(ipRows), Group: []console.Field{addrField},
			Help: "Static external IP addresses reserved in the gateway's region; each becomes In use by the Cloud Router."},
		{Name: "autoNetworkTier", Label: "Network service tier", Type: "radio", Section: mapping, Default: def("autoNetworkTier", "PREMIUM"),
			Options: []string{"PREMIUM", "STANDARD"}, OptionLabels: map[string]string{"PREMIUM": "Premium", "STANDARD": "Standard"},
			VisibleWhen: map[string]string{"natIpAllocateOption": "AUTO_ONLY"}},
		{Name: "logging", Label: "Cloud NAT logging", Type: "radio", Section: adv, Default: logging,
			Options:      []string{"none", "ALL", "TRANSLATIONS_ONLY", "ERRORS_ONLY"},
			OptionLabels: map[string]string{"none": "No logging", "ALL": "Translation and errors", "TRANSLATIONS_ONLY": "Translation only", "ERRORS_ONLY": "Errors only"}},
		{Name: "enableEndpointIndependentMapping", Label: "Enable Endpoint-Independent Mapping", Type: "checkbox", Section: adv,
			Default: def("enableEndpointIndependentMapping", "false")},
		{Name: "enableDynamicPortAllocation", Label: "Enable Dynamic Port Allocation", Type: "checkbox", Section: adv,
			Default: def("enableDynamicPortAllocation", "false")},
		{Name: "minPortsPerVm", Label: "Minimum ports per VM instance", Type: "text", Section: adv, Default: minPorts, Pattern: `^[0-9]+$`,
			Help: "2 to 57344 with static port allocation (default 64); a power of 2 from 32 with dynamic port allocation (default 32)."},
		{Name: "maxPortsPerVm", Label: "Maximum ports per VM instance", Type: "text", Section: adv, Default: def("maxPortsPerVm", "65536"), Pattern: `^[0-9]+$`,
			VisibleWhen: dyn, Help: "A power of 2 greater than the minimum, at most 65536."},
	}
	for _, t := range []struct{ name, label string }{
		{"udpIdleTimeoutSec", "UDP Mapping Idle Timeout"}, {"icmpIdleTimeoutSec", "ICMP Mapping Idle Timeout"},
		{"tcpEstablishedIdleTimeoutSec", "TCP Established Connection Idle Timeout"},
		{"tcpTransitoryIdleTimeoutSec", "TCP Transitory Connection Idle Timeout"}, {"tcpTimeWaitTimeoutSec", "TCP TIME_WAIT Timeout"},
	} {
		fields = append(fields, console.Field{Name: t.name, Label: t.label, Type: "text", Section: adv, Pattern: `^[0-9]+$`,
			Default: def(t.name, strconv.Itoa(compute.NATTimeoutDefaults[t.name])), Help: "Seconds."})
	}
	fields = natTypeFields(c, cur, fields)
	if cur != nil {
		return fields
	}
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Section: sel, Options: c.networks}
	routerField := console.Field{Name: "router", Label: "Cloud Router", Type: "select", Required: true, Section: sel,
		Options: []string{natNewRouter}, OptionsWhen: conditional(c.routers, natNewRouter), OptionLabels: map[string]string{natNewRouter: "Create new router"}}
	if c.networks == nil {
		netField.Type = "text"
	}
	region := computeRegionField("region")
	region.Section = sel
	return append([]console.Field{
		{Name: "name", Label: "Gateway name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "type", Label: "NAT type", Type: "radio", Default: "PUBLIC", Options: []string{"PUBLIC", "PRIVATE"},
			OptionLabels: map[string]string{"PUBLIC": "Public", "PRIVATE": "Private"},
			Help:         "Private NAT translates between Network Connectivity Center spokes and hybrid networks through PRIVATE_NAT subnets; it gives no internet access."},
		netField, region, routerField,
		{Name: "routerName", Label: "Name of the new router", Type: "text", Required: true, Section: sel, Pattern: computeNamePattern,
			VisibleWhen: map[string]string{"router": natNewRouter}, Help: "A Cloud Router made in the network and region for this gateway."},
	}, fields...)
}

// natBody is the gateway the form's values make, named name.
func natBody(project, region, name string, values map[string]string) (map[string]any, error) {
	if values["type"] == "PRIVATE" {
		return privateNatBody(project, region, name, values)
	}
	n := map[string]any{"name": name, "type": "PUBLIC", "sourceSubnetworkIpRangesToNat": vpcOr(values["source"], "ALL_SUBNETWORKS_ALL_IP_RANGES"),
		"natIpAllocateOption":              vpcOr(values["natIpAllocateOption"], "AUTO_ONLY"),
		"enableEndpointIndependentMapping": values["enableEndpointIndependentMapping"] == "true",
		"enableDynamicPortAllocation":      values["enableDynamicPortAllocation"] == "true"}
	if n["sourceSubnetworkIpRangesToNat"] == "LIST_OF_SUBNETWORKS" {
		rows, err := console.ParseGroup(values["subnets"])
		if err != nil {
			return nil, err
		}
		var subs []any
		for _, r := range rows {
			subs = append(subs, map[string]any{"name": "regions/" + region + "/subnetworks/" + strings.TrimSpace(r["subnet"]),
				"sourceIpRangesToNat": []any{vpcOr(r["ranges"], "ALL_IP_RANGES")}})
		}
		n["subnetworks"] = subs
	}
	if n["natIpAllocateOption"] == "MANUAL_ONLY" {
		rows, err := console.ParseGroup(values["natIps"])
		if err != nil {
			return nil, err
		}
		var ips []any
		for _, r := range rows {
			ips = append(ips, "projects/"+project+"/regions/"+region+"/addresses/"+strings.TrimSpace(r["address"]))
		}
		n["natIps"] = ips
	} else if t := values["autoNetworkTier"]; t != "" {
		n["autoNetworkTier"] = t
	}
	if l := values["logging"]; l != "" && l != "none" {
		n["logConfig"] = map[string]any{"enable": true, "filter": l}
	} else {
		n["logConfig"] = map[string]any{"enable": false, "filter": "ALL"}
	}
	dyn := n["enableDynamicPortAllocation"] == true
	for _, k := range []string{"minPortsPerVm", "maxPortsPerVm", "udpIdleTimeoutSec", "icmpIdleTimeoutSec", "tcpEstablishedIdleTimeoutSec",
		"tcpTransitoryIdleTimeoutSec", "tcpTimeWaitTimeoutSec"} {
		v := strings.TrimSpace(values[k])
		if v == "" || (k == "maxPortsPerVm" && !dyn) {
			continue
		}
		if k == "minPortsPerVm" && dyn && v == "64" {
			v = "32" // the form's static default, under dynamic allocation
		}
		i, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%s: enter a number", k)
		}
		n[k] = float64(i)
	}
	return n, nil
}

func (p computeNATProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	region := vpcOr(values["region"], "us-central1")
	network := strings.TrimSpace(values["network"])
	n, err := natBody(project, region, name, values)
	if err != nil {
		return "", err
	}
	router := strings.TrimSpace(values["router"])
	if router == natNewRouter || router == "" {
		router = strings.TrimSpace(values["routerName"])
		body := map[string]any{"name": router, "network": "global/networks/" + network, "nats": []any{n}}
		if _, err := p.call("POST", project, "regions/"+region+"/routers", nil, body); err != nil {
			return "", err
		}
		return name, nil
	}
	r, err := p.call("GET", project, "regions/"+region+"/routers/"+router, nil, nil)
	if err != nil {
		return "", err
	}
	if vpcLast(vpcStr(r, "network")) != network {
		return "", fmt.Errorf("Cloud Router %s is in network %s, not %s", router, vpcLast(vpcStr(r, "network")), network)
	}
	nats := []any{}
	for _, e := range vmMaps(r, "nats") {
		nats = append(nats, e)
	}
	nats = append(nats, n)
	if _, err := p.call("PATCH", project, "regions/"+region+"/routers/"+router, nil, map[string]any{"nats": nats}); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeNATProvider) gateway(project string, path []string) (map[string]any, map[string]any, error) {
	if len(path) != 3 {
		return nil, nil, fmt.Errorf("a Cloud NAT gateway is addressed by its region, router and name, not %v", path)
	}
	r, err := p.call("GET", project, "regions/"+path[0]+"/routers/"+path[1], nil, nil)
	if err != nil {
		return nil, nil, err
	}
	for _, n := range vmMaps(r, "nats") {
		if vpcStr(n, "name") == path[2] {
			return r, n, nil
		}
	}
	return nil, nil, fmt.Errorf("Cloud Router %s has no Cloud NAT gateway %s", path[1], path[2])
}

func natSourceText(n map[string]any) string {
	switch vpcStr(n, "sourceSubnetworkIpRangesToNat") {
	case "ALL_SUBNETWORKS_ALL_IP_RANGES":
		return "Primary and secondary IP ranges for all subnets"
	case "ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES":
		return "Primary IP ranges for all subnets"
	}
	var names []string
	for _, s := range vmMaps(n, "subnetworks") {
		names = append(names, vpcLast(vpcStr(s, "name")))
	}
	return "Custom: " + strings.Join(names, ", ")
}

func natSeconds(n map[string]any, k string) string {
	if v := vpcStr(n, k); v != "" {
		return v + " seconds"
	}
	return strconv.Itoa(compute.NATTimeoutDefaults[k]) + " seconds (default)"
}

func (p computeNATProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	r, n, err := p.gateway(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	base := "regions/" + path[0] + "/routers/" + path[1]
	status, _ := p.call("GET", project, base+"/getRouterStatus", nil, nil)
	result, _ := status["result"].(map[string]any)
	var ns map[string]any
	for _, s := range vmMaps(result, "natStatus") {
		if vpcStr(s, "name") == path[2] {
			ns = s
		}
	}
	ips := console.Listing{Columns: []string{"IP address", "Allocation"}, Noun: "allocated external IP addresses"}
	for _, a := range vpcStrs(ns, "autoAllocatedNatIps") {
		ips.Items = append(ips.Items, console.Resource{Name: a, Fields: map[string]string{"IP address": a, "Allocation": "Automatic"}})
	}
	res := vpcStrs(ns, "userAllocatedNatIpResources")
	for i, a := range vpcStrs(ns, "userAllocatedNatIps") {
		name := a
		if i < len(res) {
			name = vpcLast(res[i])
		}
		ips.Items = append(ips.Items, console.Resource{Name: name, Fields: map[string]string{"IP address": a, "Allocation": "Manual"}})
	}
	ips.Total = len(ips.Items)
	mappings := console.Listing{Columns: []string{"Internal IP address", "NAT IP address and ports", "Ports"}, Noun: "VM instances"}
	if m, err := p.call("GET", project, base+"/getNatMappingInfo", map[string][]string{"natName": {path[2]}}, nil); err == nil {
		for _, vm := range vmMaps(m, "result") {
			for _, im := range vmMaps(vm, "interfaceNatMappings") {
				mappings.Items = append(mappings.Items, console.Resource{Name: vpcStr(vm, "instanceName"), Fields: map[string]string{
					"Internal IP address": vpcStr(im, "sourceVirtualIp"), "NAT IP address and ports": vpcDash(strings.Join(vpcStrs(im, "natIpPortRanges"), ", ")),
					"Ports": vpcStr(im, "numTotalNatPorts")}})
			}
		}
	}
	mappings.Total = len(mappings.Items)
	lc, _ := n["logConfig"].(map[string]any)
	logging := "No logging"
	if lc["enable"] == true {
		logging = map[string]string{"ALL": "Translation and errors", "TRANSLATIONS_ONLY": "Translation only", "ERRORS_ONLY": "Errors only"}[vpcOr(vpcStr(lc, "filter"), "ALL")]
	}
	minPorts := vpcStr(n, "minPortsPerVm")
	if minPorts == "" {
		minPorts = "64 (default)"
		if n["enableDynamicPortAllocation"] == true {
			minPorts = "32 (default)"
		}
	}
	props := []console.Property{
		{Label: "Gateway name", Value: path[2]},
		{Label: "NAT type", Value: vpcTitle(strings.ToLower(vpcOr(vpcStr(n, "type"), "PUBLIC")))},
		{Label: "Network", Value: vpcLast(vpcStr(r, "network"))},
		{Label: "Region", Value: path[0]},
		{Label: "Cloud Router", Value: path[1]},
		{Label: "Source subnets", Value: natSourceText(n)},
		{Label: "Cloud NAT IP addresses", Value: natAllocText(n)},
		{Label: "VM instances with NAT mappings", Value: vpcOr(vpcStr(ns, "numVmEndpointsWithNatMappings"), "0")},
		{Label: "Cloud NAT logging", Value: logging},
		{Label: "Endpoint-Independent Mapping", Value: vpcOnOff(n["enableEndpointIndependentMapping"] == true)},
		{Label: "Dynamic Port Allocation", Value: vpcOnOff(n["enableDynamicPortAllocation"] == true)},
		{Label: "Minimum ports per VM instance", Value: minPorts},
	}
	if n["enableDynamicPortAllocation"] == true {
		props = append(props, console.Property{Label: "Maximum ports per VM instance", Value: vpcOr(vpcStr(n, "maxPortsPerVm"), "65536 (default)")})
	}
	for _, t := range []struct{ k, label string }{{"udpIdleTimeoutSec", "UDP Mapping Idle Timeout"}, {"icmpIdleTimeoutSec", "ICMP Mapping Idle Timeout"},
		{"tcpEstablishedIdleTimeoutSec", "TCP Established Connection Idle Timeout"}, {"tcpTransitoryIdleTimeoutSec", "TCP Transitory Connection Idle Timeout"},
		{"tcpTimeWaitTimeoutSec", "TCP TIME_WAIT Timeout"}} {
		props = append(props, console.Property{Label: t.label, Value: natSeconds(n, t.k)})
	}
	return console.Detail{Title: path[2], Heading: "NAT gateway details",
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "ips", Label: "Allocated external IP addresses", Listing: ips},
			{ID: "mappings", Label: "Mapping information", Listing: mappings,
				Note: "Each VM with no external IP on a subnet the gateway translates, with the NAT IP address and ports it is given."},
		},
		Edit: &console.EditForm{Label: "Edit", Note: "A gateway's name, network, region and Cloud Router are fixed.", Fields: natFields(p.choices(ctx, project), n)}}, nil
}

func (p computeNATProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 3 {
		return nil
	}
	if _, _, err := p.gateway(project, path); err != nil {
		return nil
	}
	a := natDelete(path)
	a.Leaves = true
	return []console.Action{a}
}

// replaceNat sets the router's gateways to its own with path[2] replaced by
// with (removed when nil).
func (p computeNATProvider) replaceNat(project string, path []string, with map[string]any) error {
	r, _, err := p.gateway(project, path)
	if err != nil {
		return err
	}
	nats := []any{}
	for _, e := range vmMaps(r, "nats") {
		if vpcStr(e, "name") == path[2] {
			if with != nil {
				nats = append(nats, with)
			}
			continue
		}
		nats = append(nats, e)
	}
	_, err = p.call("PATCH", project, "regions/"+path[0]+"/routers/"+path[1], nil, map[string]any{"nats": nats})
	return err
}

func (p computeNATProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 3 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	return p.replaceNat(project, path, nil)
}

func (p computeNATProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 3 {
		return errors.New("no Cloud NAT gateway")
	}
	if r, err := p.call("GET", project, "regions/"+path[0]+"/routers/"+path[1], nil, nil); err == nil {
		for _, e := range vmMaps(r, "nats") {
			if vpcStr(e, "name") == path[2] && vpcStr(e, "type") == "PRIVATE" {
				values["type"] = "PRIVATE"
			}
		}
	}
	n, err := natBody(project, path[0], path[2], values)
	if err != nil {
		return err
	}
	return p.replaceNat(project, path, n)
}
