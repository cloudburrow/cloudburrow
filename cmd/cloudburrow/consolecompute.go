package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// The VPC network pages (#1936), VPC network in Google's console menu:
// VPC networks (with a network's page and its Subnets, Firewall rules and
// Routes tabs), Firewall, Routes and IP addresses. Each read and change is
// a call on the compute.Server the Compute Engine JSON API serves, so what
// the pages make is what the Go client, gcloud compute and Terraform read,
// and a refusal is the API's own message.

// computeScreen is what the VPC network pages share: the in-process API.
type computeScreen struct{ svc *computeService }

const computeNotStarted = "the Compute Engine API has not started"

const computeNote = "Memorystore, Kubernetes Engine and Cloud SQL check their network fields against these networks, as Google does. " +
	"Between VM instances a network carries traffic, its subnet routes and an ACTIVE peering's exchanged, with its firewall rules enforced on each VM; " +
	"static routes and firewall logs are records only. Operations complete at once."

const computeNamePattern = `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`

const computeNameHelp = "Lowercase letters, numbers and hyphens, starting with a letter and not ending with a hyphen; at most 63 characters."

func (c computeScreen) api() *compute.Server {
	if c.svc == nil {
		return nil
	}
	return c.svc.API()
}

// call is one Compute call, path after projects/P/.
func (c computeScreen) call(method, project, path string, q url.Values, body map[string]any) (map[string]any, error) {
	api := c.api()
	if api == nil {
		return nil, errors.New(computeNotStarted)
	}
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	return api.Call(method, project, path, q, body)
}

// items is a list's items.
func computeItems(m map[string]any) []map[string]any {
	l, _ := m["items"].([]any)
	out := make([]map[string]any, 0, len(l))
	for _, e := range l {
		if o, ok := e.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

func (c computeScreen) list(project, path string) ([]map[string]any, error) {
	m, err := c.call("GET", project, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return computeItems(m), nil
}

func vpcStr(o map[string]any, k string) string {
	switch v := o[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func vpcStrs(o map[string]any, k string) []string {
	l, _ := o[k].([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// last is a link's last segment: a resource's name.
func vpcLast(link string) string { return link[strings.LastIndex(link, "/")+1:] }

func vpcDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func vpcList(s string) []any {
	var out []any
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		out = append(out, f)
	}
	return out
}

func (c computeScreen) networkNames(ctx context.Context, project string) []string {
	nets, err := c.list(project, "global/networks")
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range nets {
		out = append(out, vpcStr(n, "name"))
	}
	return out
}

func computeRegionField(name string) console.Field {
	return console.Field{Name: name, Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: compute.Regions()}
}

// secondaryRangesField is a subnet's secondary IPv4 ranges.
func secondaryRangesField(section string) console.Field {
	return console.Field{Name: "secondaryRanges", Label: "Secondary IPv4 ranges", Type: console.GroupFieldType, Section: section,
		AddLabel: "Add a secondary IPv4 range", Group: []console.Field{
			{Name: "rangeName", Label: "Secondary IPv4 range name", Type: "text", Required: true, Pattern: computeNamePattern},
			{Name: "ipCidrRange", Label: "Secondary IPv4 range", Type: "text", Required: true, Help: "For example, 192.168.0.0/24"},
		}}
}

func parseSecondaries(v string) ([]any, error) {
	rows, err := console.ParseGroup(v)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		out = append(out, map[string]any{"rangeName": strings.TrimSpace(r["rangeName"]), "ipCidrRange": strings.TrimSpace(r["ipCidrRange"])})
	}
	return out, nil
}

// --- VPC networks -----------------------------------------------------------

// computeNetworksProvider is VPC networks: the project's networks, Create
// VPC network (auto or custom subnet mode, with a first subnet), Delete,
// and a network's page with its subnets (Add subnet, Expand IP range,
// Edit secondary ranges, Private Google Access, Delete), its firewall
// rules and its routes. Paths are [network] and [network, "subnets",
// region, subnet].
type computeNetworksProvider struct{ computeScreen }

func (computeNetworksProvider) ID() string    { return "vpc-networks" }
func (computeNetworksProvider) Title() string { return "VPC networks" }

func (p computeNetworksProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Subnets", "MTU", "Mode", "Dynamic routing mode", "Firewall rules", "Description"},
		Noun: "VPC networks", Note: computeNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "VPC networks are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	nets, err := p.list(project, "global/networks")
	if err != nil {
		return listing, err
	}
	fws, _ := p.list(project, "global/firewalls")
	for _, n := range nets {
		rules := 0
		for _, f := range fws {
			if vpcLast(vpcStr(f, "network")) == vpcStr(n, "name") {
				rules++
			}
		}
		listing.Items = append(listing.Items, console.Resource{Name: vpcStr(n, "name"), Fields: map[string]string{
			"Subnets": strconv.Itoa(len(vpcStrs(n, "subnetworks"))), "MTU": vpcStr(n, "mtu"), "Mode": networkMode(n),
			"Dynamic routing mode": routingModeText(n), "Firewall rules": strconv.Itoa(rules), "Description": vpcDash(vpcStr(n, "description")),
		}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func networkMode(n map[string]any) string {
	if n["autoCreateSubnetworks"] == true {
		return "Auto"
	}
	return "Custom"
}

func routingModeText(n map[string]any) string {
	rc, _ := n["routingConfig"].(map[string]any)
	if vpcStr(rc, "routingMode") == "GLOBAL" {
		return "Global"
	}
	return "Regional"
}

func (computeNetworksProvider) CreateOnPage() bool  { return true }
func (computeNetworksProvider) CreateTitle() string { return "Create a VPC network" }

// CreateForm is Google's Create a VPC network (vpc/docs/create-modify-vpc-networks,
// read 2026-11-22): Name, Description, Set MTU automatically (1460) or a
// Maximum transmission unit, the Subnet creation mode with a New subnet
// (Name, Region, IP stack type, IPv4 range, secondary ranges, Private
// Google Access and Flow logs, each On or Off), the Firewall rules to make
// with it, and the Dynamic routing mode.
func (computeNetworksProvider) CreateForm() (string, []console.Field) {
	custom := map[string]string{"subnetMode": "Custom"}
	sub := func(f console.Field) console.Field {
		f.Section, f.VisibleWhen = "Subnets", custom
		return f
	}
	sec := secondaryRangesField("Subnets")
	sec.Name, sec.VisibleWhen = "subnetSecondaryRanges", custom
	return "Create VPC network", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "autoMtu", Label: "Set MTU automatically", Type: "checkbox", Default: "true", Help: "The MTU is 1460, Google's default."},
		{Name: "mtu", Label: "Maximum transmission unit (MTU)", Type: "text", Required: true, Default: "1460", Pattern: `^[0-9]{4}$`,
			VisibleWhen: map[string]string{"autoMtu": "false"}, Help: "1300 to 8896 bytes."},
		{Name: "ulaIpv6", Label: "Configure a ULA internal IPv6 range for this VPC Network", Type: "checkbox", Section: "Private IPv6 address settings",
			Help: "A /48 of fd20::/20, from which its dual-stack subnets with internal IPv6 take their /64s."},
		{Name: "ulaAllocation", Label: "Allocate internal IPv6 range", Type: "radio", Section: "Private IPv6 address settings", Default: "Automatically",
			Options: []string{"Automatically", "Manually"}, VisibleWhen: map[string]string{"ulaIpv6": "true"}},
		{Name: "internalIpv6Range", Label: "Internal IPv6 range", Type: "text", Section: "Private IPv6 address settings",
			VisibleWhen: map[string]string{"ulaAllocation": "Manually"}, Help: "A /48 of fd20::/20, for example fd20:a:b::/48."},
		{Name: "subnetMode", Label: "Subnet creation mode", Type: "radio", Section: "Subnets", Default: "Custom", Options: []string{"Custom", "Automatic"},
			Help: "Automatic makes one subnet in each region, in 10.128.0.0/9; Custom makes only the subnets you add."},
		sub(console.Field{Name: "subnetName", Label: "Name", Type: "text", Pattern: `^$|` + computeNamePattern,
			Help: "New subnet: optional, a first subnet made with the network."}),
		sub(console.Field{Name: "subnetDescription", Label: "Description", Type: "text"}),
		sub(console.Field{Name: "subnetRegion", Label: "Region", Type: "select", Default: "us-central1", Options: compute.Regions()}),
		sub(stackTypeField("subnetStackType")),
		sub(ipv6AccessTypeField("subnetIpv6AccessType", "subnetStackType")),
		sub(withIPv4(console.Field{Name: "subnetRange", Label: "IPv4 range", Type: "text", Help: "For example, 10.0.0.0/24"}, "subnetStackType")),
		withIPv4(sec, "subnetStackType"),
		sub(console.Field{Name: "privateGoogleAccess", Label: "Private Google Access", Type: "radio", Default: "Off", Options: []string{"On", "Off"}}),
		sub(flowLogsField("subnetFlowLogs", "Off")),
		presetRuleField("custom", "Each rule ticked is made with the network, at priority 65534. allow-custom admits the network's own subnet ranges (10.128.0.0/9 in automatic mode)."),
		presetRuleField("icmp", ""), presetRuleField("rdp", ""), presetRuleField("ssh", ""),
		{Name: "routingMode", Label: "Dynamic routing mode", Type: "radio", Section: "Dynamic routing mode", Default: "Regional", Options: []string{"Regional", "Global"}},
	}
}

// presetRuleKinds are the firewall rules Create VPC network offers to make
// with the network, as Google's form lists them under IPv4 firewall rules.
var presetRuleKinds = []string{"custom", "icmp", "rdp", "ssh"}

var presetRuleLabels = map[string]string{
	"custom": "NETWORK-allow-custom: ingress from the network's subnet ranges, all protocols",
	"icmp":   "NETWORK-allow-icmp: ingress from 0.0.0.0/0, icmp",
	"rdp":    "NETWORK-allow-rdp: ingress from 0.0.0.0/0, tcp:3389",
	"ssh":    "NETWORK-allow-ssh: ingress from 0.0.0.0/0, tcp:22",
}

// presetRuleField is a checkbox of Create VPC network's IPv4 firewall rules.
func presetRuleField(kind, help string) console.Field {
	return console.Field{Name: "rule-" + kind, Label: presetRuleLabels[kind], Type: "checkbox", Section: "Firewall rules", Help: help}
}

// presetRule is the body of the preset rule kind for network.
func presetRule(network, kind string, ranges []any) map[string]any {
	b := map[string]any{"name": network + "-allow-" + kind, "network": "global/networks/" + network, "direction": "INGRESS",
		"priority": float64(65534), "sourceRanges": []any{"0.0.0.0/0"}}
	switch kind {
	case "custom":
		b["sourceRanges"], b["allowed"] = ranges, []any{map[string]any{"IPProtocol": "all"}}
	case "icmp":
		b["allowed"] = []any{map[string]any{"IPProtocol": "icmp"}}
	case "rdp":
		b["allowed"] = []any{map[string]any{"IPProtocol": "tcp", "ports": []any{"3389"}}}
	case "ssh":
		b["allowed"] = []any{map[string]any{"IPProtocol": "tcp", "ports": []any{"22"}}}
	}
	return b
}

// stackTypeField is a subnet's IP stack type: IPv4, dual-stack
// (consolecomputea1.go) or IPv6 (single-stack), whose subnet has no IPv4
// range ("Add an IPv6-only subnet", vpc/docs/create-modify-vpc-networks,
// read 2026-11-22).
func stackTypeField(name string) console.Field {
	return console.Field{Name: name, Label: "IP stack type", Type: "radio", Default: "IPV4_ONLY",
		Options:      []string{"IPV4_ONLY", "IPV4_IPV6", "IPV6_ONLY"},
		OptionLabels: map[string]string{"IPV4_ONLY": "IPv4 (single-stack)", "IPV4_IPV6": "IPv4 and IPv6 (dual-stack)", "IPV6_ONLY": "IPv6 (single-stack)"}}
}

// withIPv4 shows f, an IPv4 range field, only while the stack type field
// stack has IPv4: an IPv6-only subnet is given none, as Google's form asks
// for none.
func withIPv4(f console.Field, stack string) console.Field {
	f.ShowWhen = stack + "=IPV4_ONLY|IPV4_IPV6"
	return f
}

// flowLogsField is a subnet's Flow logs, On or Off: the subnet's logConfig,
// kept and returned; nothing is logged, as no traffic crosses a network.
func flowLogsField(name, def string) console.Field {
	return console.Field{Name: name, Label: "Flow logs", Type: "radio", Default: def, Options: []string{"On", "Off"},
		Help: "Kept as the subnet's logConfig; flow logs are not written locally."}
}

func (p computeNetworksProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	body := map[string]any{"name": name, "autoCreateSubnetworks": values["subnetMode"] == "Automatic",
		"routingConfig": map[string]any{"routingMode": strings.ToUpper(vpcOr(values["routingMode"], "Regional"))}}
	if d := strings.TrimSpace(values["description"]); d != "" {
		body["description"] = d
	}
	if values["ulaIpv6"] == "true" {
		body["enableUlaInternalIpv6"] = true
		if r := strings.TrimSpace(values["internalIpv6Range"]); r != "" && values["ulaAllocation"] == "Manually" {
			body["internalIpv6Range"] = r
		}
	}
	if m := strings.TrimSpace(values["mtu"]); m != "" && values["autoMtu"] != "true" {
		n, err := strconv.Atoi(m)
		if err != nil {
			return "", fmt.Errorf("MTU: %q is not a number", m)
		}
		body["mtu"] = float64(n)
	}
	sub := strings.TrimSpace(values["subnetName"])
	if values["subnetMode"] != "Automatic" && sub != "" && values["subnetStackType"] != "IPV6_ONLY" && strings.TrimSpace(values["subnetRange"]) == "" {
		return "", errors.New("the new subnet needs an IPv4 range")
	}
	if _, err := p.call("POST", project, "global/networks", nil, body); err != nil {
		return "", err
	}
	ranges := []any{"10.128.0.0/9"}
	if values["subnetMode"] != "Automatic" {
		ranges = nil
		if sub != "" {
			pga := values["privateGoogleAccess"]
			if pga == "On" {
				pga = "true"
			}
			if err := p.addSubnet(project, name, map[string]string{"name": sub, "description": values["subnetDescription"], "region": values["subnetRegion"],
				"ipCidrRange": values["subnetRange"], "privateGoogleAccess": pga, "secondaryRanges": values["subnetSecondaryRanges"],
				"stackType": values["subnetStackType"], "ipv6AccessType": values["subnetIpv6AccessType"], "flowLogs": values["subnetFlowLogs"]}); err != nil {
				return name, fmt.Errorf("the network %s was created, but not its subnet: %w", name, err)
			}
			if values["subnetStackType"] != "IPV6_ONLY" {
				ranges = []any{strings.TrimSpace(values["subnetRange"])}
			}
		}
	}
	for _, kind := range presetRuleKinds {
		if values["rule-"+kind] != "true" {
			continue
		}
		if kind == "custom" && len(ranges) == 0 {
			return name, fmt.Errorf("the network %s was created, but not %s-allow-custom: it has no subnet ranges to admit", name, name)
		}
		if _, err := p.call("POST", project, "global/firewalls", nil, presetRule(name, kind, ranges)); err != nil {
			return name, fmt.Errorf("the network %s was created, but not its firewall rule %s-allow-%s: %w", name, name, kind, err)
		}
	}
	return name, nil
}

func vpcOr(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func (p computeNetworksProvider) addSubnet(project, network string, values map[string]string) error {
	region := strings.TrimSpace(values["region"])
	if !compute.IsRegion(region) {
		return fmt.Errorf("region: %q is not a region", region)
	}
	pga := values["privateGoogleAccess"] == "true" || values["privateGoogleAccess"] == "On"
	body := map[string]any{"name": strings.TrimSpace(values["name"]), "network": "global/networks/" + network,
		"ipCidrRange": strings.TrimSpace(values["ipCidrRange"]), "privateIpGoogleAccess": pga}
	if st := values["stackType"]; st != "" {
		body["stackType"] = st
		if st == "IPV4_IPV6" || st == "IPV6_ONLY" {
			body["ipv6AccessType"] = strings.ToUpper(vpcOr(values["ipv6AccessType"], "Internal"))
		}
		if st == "IPV6_ONLY" {
			// Only the /64: no IPv4 range, primary or secondary.
			delete(body, "ipCidrRange")
		}
	}
	if values["flowLogs"] == "On" {
		body["logConfig"] = map[string]any{"enable": true, "aggregationInterval": "INTERVAL_5_SEC", "flowSampling": 0.5, "metadata": "INCLUDE_ALL_METADATA"}
	}
	if d := strings.TrimSpace(values["description"]); d != "" {
		body["description"] = d
	}
	sec, err := parseSecondaries(values["secondaryRanges"])
	if err != nil {
		return err
	}
	if len(sec) > 0 && values["stackType"] != "IPV6_ONLY" {
		body["secondaryIpRanges"] = sec
	}
	_, err = p.call("POST", project, "regions/"+region+"/subnetworks", nil, body)
	return err
}

func (p computeNetworksProvider) DeleteLabel() string { return "Delete VPC network" }

func (p computeNetworksProvider) Delete(ctx context.Context, project, name string) error {
	_, err := p.call("DELETE", project, "global/networks/"+name, nil, nil)
	return err
}

func (p computeNetworksProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	switch {
	case len(path) == 1:
		return p.networkDetail(project, path[0])
	case len(path) == 4 && path[1] == "subnets":
		return p.subnetDetail(project, path)
	}
	return console.DeeperThan(1, path), nil
}

func (p computeNetworksProvider) networkDetail(project, name string) (console.Detail, error) {
	n, err := p.call("GET", project, "global/networks/"+name, nil, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	subs := console.Listing{Columns: []string{"Region", "Internal IP ranges", "Secondary IPv4 ranges", "Gateway", "Private Google Access"},
		Noun: "subnets"}
	var refs []string
	for _, l := range vpcStrs(n, "subnetworks") {
		refs = append(refs, l)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		parts := strings.Split(ref, "/")
		if len(parts) < 6 {
			continue
		}
		region, sn := parts[3], parts[5]
		s, err := p.call("GET", project, "regions/"+region+"/subnetworks/"+sn, nil, nil)
		if err != nil {
			continue
		}
		target := []string{name, "subnets", region, sn}
		subs.Items = append(subs.Items, console.Resource{Name: sn, Opens: target, Target: target,
			Actions: p.subnetActions(project, target, s),
			Fields: map[string]string{"Region": region, "Internal IP ranges": vpcStr(s, "ipCidrRange"),
				"Secondary IPv4 ranges": vpcDash(secondaryText(s)), "Gateway": vpcStr(s, "gatewayAddress"),
				"Private Google Access": vpcOnOff(s["privateIpGoogleAccess"] == true)}})
	}
	subs.Total = len(subs.Items)
	filter := url.Values{"filter": {`network = "projects/` + project + `/global/networks/` + name + `"`}}
	fws := console.Listing{Columns: firewallColumns(), Noun: "firewall rules"}
	if l, err := p.call("GET", project, "global/firewalls", filter, nil); err == nil {
		for _, f := range computeItems(l) {
			fws.Items = append(fws.Items, firewallRow(f))
		}
	}
	fws.Total = len(fws.Items)
	routes := console.Listing{Columns: routeColumns(), Noun: "routes"}
	if l, err := p.call("GET", project, "global/routes", filter, nil); err == nil {
		for _, r := range computeItems(l) {
			routes.Items = append(routes.Items, routeRow(r))
		}
	}
	routes.Total = len(routes.Items)
	return console.Detail{
		Summary: []console.Property{
			{Label: "Description", Value: vpcDash(vpcStr(n, "description"))},
			{Label: "Subnet creation mode", Value: map[string]string{"Auto": "Auto subnets", "Custom": "Custom subnets"}[networkMode(n)]},
			{Label: "Maximum transmission unit", Value: vpcStr(n, "mtu")},
			{Label: "Dynamic routing mode", Value: routingModeText(n)},
			{Label: "Best path selection mode", Value: bestPathText(n)},
		},
		// Google's tabs (vpc/docs/create-modify-vpc-networks,
		// configure-private-services-access, using-vpc-peering, read
		// 2026-11-22); their order beyond Subnets first is not in the docs
		// (UNVERIFIED, re-read 2026-11-22:
		// https://docs.cloud.google.com/vpc/docs/create-modify-vpc-networks).
		Sections: []console.Section{
			{ID: "subnets", Label: "Subnets", Listing: subs, Note: computeNote},
			{ID: "static-internal", Label: "Static internal IP addresses", Listing: p.staticInternal(project, name)},
			{ID: "firewalls", Label: "Firewalls", Listing: fws},
			{ID: "routes", Label: "Routes", Listing: routes},
			{ID: "peering", Label: "VPC network peering", Listing: peeringListing(n)},
			{ID: "psa", Label: "Private services access", Listing: p.allocatedRanges(project, name)},
		},
		// "Updating network MTU is not supported in the Google Cloud
		// console" (vpc/docs/change-mtu-vpc-network): Edit has the routing
		// modes only, as Google's.
		Edit: &console.EditForm{Label: "Edit", Note: "A network's name is fixed; Switch to custom mode changes an auto mode network once. Its MTU changes through the API only, as on Google.",
			Fields: []console.Field{
				{Name: "routingMode", Label: "Dynamic routing mode", Type: "radio", Default: routingModeText(n), Options: []string{"Regional", "Global"}},
				{Name: "bestPath", Label: "Best path selection mode", Type: "radio", Default: bestPathText(n), Options: []string{"Legacy", "Standard"}},
			}},
	}, nil
}

func bestPathText(n map[string]any) string {
	rc, _ := n["routingConfig"].(map[string]any)
	if vpcStr(rc, "bgpBestPathSelectionMode") == "STANDARD" {
		return "Standard"
	}
	return "Legacy"
}

// staticInternal is a network's Static internal IP addresses tab: its
// reserved internal addresses.
func (p computeNetworksProvider) staticInternal(project, network string) console.Listing {
	l := console.Listing{Columns: []string{"Internal IP address", "Region", "Subnetwork", "Purpose", "In use by"}, Noun: "static internal IP addresses"}
	m, err := p.call("GET", project, "aggregated/addresses", nil, nil)
	if err != nil {
		return l
	}
	for _, a := range aggregatedItems(m, "addresses") {
		if vpcStr(a, "addressType") != "INTERNAL" || vpcLast(vpcStr(a, "network")) != network || vpcStr(a, "purpose") == "VPC_PEERING" {
			continue
		}
		var users []string
		for _, u := range vpcStrs(a, "users") {
			users = append(users, vpcLast(u))
		}
		l.Items = append(l.Items, console.Resource{Name: vpcStr(a, "name"), Fields: map[string]string{"Internal IP address": vpcStr(a, "address"),
			"Region": vpcLast(vpcStr(a, "region")), "Subnetwork": vpcDash(vpcLast(vpcStr(a, "subnetwork"))),
			"Purpose":   map[bool]string{true: "Shared", false: "Non-shared"}[vpcStr(a, "purpose") == "SHARED_LOADBALANCER_VIP"],
			"In use by": vpcDash(strings.Join(users, ", "))}})
	}
	l.Total = len(l.Items)
	return l
}

// allocatedRanges is a network's Private services access tab: its
// Allocated IP ranges for services.
func (p computeNetworksProvider) allocatedRanges(project, network string) console.Listing {
	l := console.Listing{Columns: []string{"Internal IP range", "Description"}, Noun: "allocated IP ranges for services",
		TableTitle: "Allocated IP ranges for services",
		Note:       "Allocate an IP range on the IP addresses page (Allocate IP range for private services access); Cloud SQL and Memorystore check their private service ranges against these."}
	for _, a := range func() []map[string]any { r, _ := p.list(project, "global/addresses"); return r }() {
		if vpcStr(a, "purpose") != "VPC_PEERING" || vpcLast(vpcStr(a, "network")) != network {
			continue
		}
		l.Items = append(l.Items, console.Resource{Name: vpcStr(a, "name"), Fields: map[string]string{
			"Internal IP range": vpcStr(a, "address") + "/" + vpcStr(a, "prefixLength"), "Description": vpcDash(vpcStr(a, "description"))}})
	}
	l.Total = len(l.Items)
	return l
}

// Edit is the network page's Edit (networks.patch): its dynamic routing
// mode and best path selection mode; a subnet page's Edit is the
// subnet's (subnetEdit).
func (p computeNetworksProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) == 4 && path[1] == "subnets" {
		return p.subnetEdit(project, path, values)
	}
	if len(path) != 1 {
		return errors.New("only a network's own page has Edit")
	}
	rc := map[string]any{"routingMode": strings.ToUpper(vpcOr(values["routingMode"], "Regional"))}
	if bp := values["bestPath"]; bp != "" {
		rc["bgpBestPathSelectionMode"] = strings.ToUpper(bp)
	}
	b := map[string]any{"routingConfig": rc}
	if m := strings.TrimSpace(values["mtu"]); m != "" {
		n, err := strconv.Atoi(m)
		if err != nil {
			return fmt.Errorf("MTU: %q is not a number", m)
		}
		b["mtu"] = float64(n)
	}
	_, err := p.call("PATCH", project, "global/networks/"+path[0], nil, b)
	return err
}

// subnetEdit is Google's Edit subnet: a wider IP address range
// (expandIpCidrRange), the secondary ranges, Private Google Access and
// Flow logs (subnetworks.patch), each sent only when it changed.
func (p computeNetworksProvider) subnetEdit(project string, path []string, values map[string]string) error {
	base := "regions/" + path[2] + "/subnetworks/" + path[3]
	s, err := p.call("GET", project, base, nil, nil)
	if err != nil {
		return err
	}
	if r := strings.TrimSpace(values["ipCidrRange"]); r != "" && r != vpcStr(s, "ipCidrRange") {
		if _, err := p.call("POST", project, base+"/expandIpCidrRange", nil, map[string]any{"ipCidrRange": r}); err != nil {
			return err
		}
	}
	if err := p.subnetStackEdit(project, base, s, values); err != nil {
		return err
	}
	if v, ok := values["privateGoogleAccess"]; ok && (v == "On") != (s["privateIpGoogleAccess"] == true) {
		if _, err := p.call("POST", project, base+"/setPrivateIpGoogleAccess", nil, map[string]any{"privateIpGoogleAccess": v == "On"}); err != nil {
			return err
		}
	}
	patch := map[string]any{}
	if v, ok := values["flowLogs"]; ok && (v == "On") != flowLogsOn(s) {
		lc := map[string]any{"enable": v == "On"}
		if v == "On" {
			lc = map[string]any{"enable": true, "aggregationInterval": "INTERVAL_5_SEC", "flowSampling": 0.5, "metadata": "INCLUDE_ALL_METADATA"}
		}
		patch["logConfig"] = lc
	}
	if v, ok := values["secondaryRanges"]; ok && v != secondaryRows(s) {
		sec, err := parseSecondaries(v)
		if err != nil {
			return err
		}
		patch["secondaryIpRanges"] = sec
	}
	if len(patch) == 0 {
		return nil
	}
	s, err = p.call("GET", project, base, nil, nil)
	if err != nil {
		return err
	}
	patch["fingerprint"] = vpcStr(s, "fingerprint")
	_, err = p.call("PATCH", project, base, nil, patch)
	return err
}

func flowLogsOn(s map[string]any) bool {
	lc, _ := s["logConfig"].(map[string]any)
	return lc["enable"] == true
}

func secondaryText(s map[string]any) string {
	l, _ := s["secondaryIpRanges"].([]any)
	var parts []string
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			parts = append(parts, vpcStr(m, "rangeName")+": "+vpcStr(m, "ipCidrRange"))
		}
	}
	return strings.Join(parts, ", ")
}

func secondaryRows(s map[string]any) string {
	l, _ := s["secondaryIpRanges"].([]any)
	var rows []map[string]string
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			rows = append(rows, map[string]string{"rangeName": vpcStr(m, "rangeName"), "ipCidrRange": vpcStr(m, "ipCidrRange")})
		}
	}
	return console.FormatGroup(rows)
}

func vpcOnOff(b bool) string {
	if b {
		return "On"
	}
	return "Off"
}

func (p computeNetworksProvider) subnetDetail(project string, path []string) (console.Detail, error) {
	s, err := p.call("GET", project, "regions/"+path[2]+"/subnetworks/"+path[3], nil, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	if vpcLast(vpcStr(s, "network")) != path[0] {
		return console.Detail{Unavailable: fmt.Sprintf("subnet %s is not in network %s", path[3], path[0])}, nil
	}
	return console.Detail{
		Trail: []console.Crumb{{Label: path[0], Path: path[:1]}, {Label: path[3]}},
		Summary: []console.Property{
			{Label: "VPC network", Value: path[0]},
			{Label: "Region", Value: path[2]},
			{Label: "Primary IPv4 range", Value: vpcStr(s, "ipCidrRange")},
			{Label: "Gateway", Value: vpcStr(s, "gatewayAddress")},
			{Label: "Secondary IPv4 ranges", Value: vpcDash(secondaryText(s))},
			{Label: "Private Google Access", Value: vpcOnOff(s["privateIpGoogleAccess"] == true)},
			{Label: "Flow logs", Value: vpcOnOff(flowLogsOn(s))},
			{Label: "Purpose", Value: vpcStr(s, "purpose")},
			{Label: "IP stack type", Value: stackTypeText(vpcStr(s, "stackType"))},
			{Label: "IPv6 access type", Value: vpcDash(vpcTitle(vpcStr(s, "ipv6AccessType")))},
			{Label: "IPv6 range", Value: vpcDash(vpcStr(s, "ipv6CidrRange"))},
			{Label: "Description", Value: vpcDash(vpcStr(s, "description"))},
		},
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Note: computeNote,
			Groups: []console.PropertyGroup{{Properties: []console.Property{{Label: "Fingerprint", Value: vpcStr(s, "fingerprint")}}}}}},
		// Google's Edit subnet (vpc/docs/create-modify-vpc-networks, read
		// 2026-11-22): the IP address range widened (never narrowed), the
		// IP stack type, Private Google Access and Flow logs.
		Edit: &console.EditForm{Label: "Edit", Note: "An IP address range can be widened, never narrowed or replaced; the change cannot be undone.",
			Fields: subnetEditFields(s)},
	}, nil
}

// subnetEditFields are Edit subnet's fields for s. An IPv6-only subnet has
// no IPv4 range to widen and keeps its stack type, and no subnet becomes
// IPv6-only: "You can't change a dual-stack or IPv4-only subnet to
// IPv6-only", nor back (vpc/docs/create-modify-vpc-networks, read
// 2026-11-22).
func subnetEditFields(s map[string]any) []console.Field {
	st := stackTypeField("stackType")
	st.Default = vpcOr(vpcStr(s, "stackType"), "IPV4_ONLY")
	tail := []console.Field{
		{Name: "privateGoogleAccess", Label: "Private Google Access", Type: "radio", Default: vpcOnOff(s["privateIpGoogleAccess"] == true), Options: []string{"On", "Off"}},
		flowLogsField("flowLogs", vpcOnOff(flowLogsOn(s))),
	}
	if st.Default == "IPV6_ONLY" {
		st.Immutable = true
		return append([]console.Field{st, subnetIpv6AccessEditField(s)}, tail...)
	}
	st.Options = []string{"IPV4_ONLY", "IPV4_IPV6"}
	return append([]console.Field{
		{Name: "ipCidrRange", Label: "IP address range", Type: "text", Required: true, Default: vpcStr(s, "ipCidrRange"),
			Help: "A broader range containing the current one, for example 10.0.0.0/20 for 10.0.0.0/24."},
		st,
		subnetIpv6AccessEditField(s),
		func() console.Field { f := secondaryRangesField(""); f.Default = secondaryRows(s); return f }(),
	}, tail...)
}

// addSubnetAction is Google's Add subnet (vpc/docs/create-modify-vpc-networks,
// read 2026-11-22): Name, Description, Region, IP stack type, IPv4 range,
// the secondary ranges, Private Google Access and Flow logs; networks, when
// given, adds the network to put it in (the Subnets in current project
// tab's).
func addSubnetAction(networks []string) console.Action {
	var fields []console.Field
	if networks != nil {
		fields = append(fields, console.Field{Name: "network", Label: "VPC network", Type: "select", Required: true, Options: networks})
	}
	fields = append(fields,
		console.Field{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		console.Field{Name: "description", Label: "Description", Type: "text"},
		computeRegionField("region"),
		stackTypeField("stackType"),
		ipv6AccessTypeField("ipv6AccessType", "stackType"),
		withIPv4(console.Field{Name: "ipCidrRange", Label: "IPv4 range", Type: "text", Required: true, Help: "For example, 10.0.0.0/24"}, "stackType"),
		withIPv4(secondaryRangesField(""), "stackType"),
		console.Field{Name: "privateGoogleAccess", Label: "Private Google Access", Type: "radio", Default: "Off", Options: []string{"On", "Off"}},
		flowLogsField("flowLogs", "Off"))
	return console.Action{ID: "addsubnet", Label: "Add subnet", Submit: "Add", Fields: fields}
}

// subnetActions are a subnet's own: Delete subnet, refused, as Google's
// is, on an auto mode network's own subnets. Its range, secondary ranges,
// Private Google Access and Flow logs are changed by Edit.
func (p computeNetworksProvider) subnetActions(project string, path []string, s map[string]any) []console.Action {
	del := console.Action{ID: "delete", Label: "Delete subnet", Destructive: true, Confirm: "Delete subnet " + path[3] + "?"}
	if n, err := p.call("GET", project, "global/networks/"+path[0], nil, nil); err == nil && n["autoCreateSubnetworks"] == true &&
		vpcStr(s, "name") == path[0] {
		del.Unavailable = "An auto mode network's own subnets cannot be deleted: switch the network to custom mode first."
	}
	return []console.Action{del}
}

func (p computeNetworksProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil {
		return nil
	}
	switch {
	case len(path) == 1:
		n, err := p.call("GET", project, "global/networks/"+path[0], nil, nil)
		if err != nil {
			return nil
		}
		acts := []console.Action{addSubnetAction(nil)}
		if n["autoCreateSubnetworks"] == true {
			acts = append(acts, console.Action{ID: "custommode", Label: "Switch to custom mode",
				Confirm: "Switch " + path[0] + " to custom subnet mode? Its subnets stay; new regions get none. This cannot be undone."})
		}
		return append(acts, console.Action{ID: "delete", Label: "Delete VPC network", Destructive: true, Leaves: true,
			Confirm: "Delete VPC network " + path[0] + "?"})
	case len(path) == 4 && path[1] == "subnets":
		s, err := p.call("GET", project, "regions/"+path[2]+"/subnetworks/"+path[3], nil, nil)
		if err != nil {
			return nil
		}
		acts := p.subnetActions(project, path, s)
		for i := range acts {
			if acts[i].ID == "delete" {
				acts[i].Leaves = true
			}
		}
		return acts
	}
	return nil
}

func (p computeNetworksProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	switch {
	case len(path) == 1:
		switch action {
		case "addsubnet":
			return p.addSubnet(project, path[0], values)
		case "custommode":
			_, err := p.call("POST", project, "global/networks/"+path[0]+"/switchToCustomMode", nil, nil)
			return err
		case "delete":
			return p.Delete(ctx, project, path[0])
		}
	case len(path) == 4 && path[1] == "subnets":
		base := "regions/" + path[2] + "/subnetworks/" + path[3]
		switch action {
		case "expand":
			_, err := p.call("POST", project, base+"/expandIpCidrRange", nil, map[string]any{"ipCidrRange": strings.TrimSpace(values["ipCidrRange"])})
			return err
		case "secondary":
			sec, err := parseSecondaries(values["secondaryRanges"])
			if err != nil {
				return err
			}
			fp := values["fingerprint"]
			if fp == "" {
				s, err := p.call("GET", project, base, nil, nil)
				if err != nil {
					return err
				}
				fp = vpcStr(s, "fingerprint")
			}
			_, err = p.call("PATCH", project, base, nil, map[string]any{"fingerprint": fp, "secondaryIpRanges": sec})
			return err
		case "privateaccess":
			_, err := p.call("POST", project, base+"/setPrivateIpGoogleAccess", nil, map[string]any{"privateIpGoogleAccess": values["privateIpGoogleAccess"] == "true"})
			return err
		case "delete":
			_, err := p.call("DELETE", project, base, nil, nil)
			return err
		}
	}
	return fmt.Errorf("%s is not available on this resource", action)
}

// --- Firewall ---------------------------------------------------------------

// computeFirewallsProvider is Firewall: the project's VPC firewall rules,
// Create firewall rule, a rule's page with Edit, and Delete.
type computeFirewallsProvider struct{ computeScreen }

func (computeFirewallsProvider) ID() string { return "vpc-firewalls" }

// Title is Google's page name: "go to the Firewall policies page", the
// VPC firewall rules a table on it (firewall/docs/using-firewalls, read
// 2026-11-22; #2140).
func (computeFirewallsProvider) Title() string { return "Firewall policies" }

func firewallColumns() []string {
	return []string{"Type", "Targets", "Filters", "Protocols / ports", "Action", "Priority", "Network", "Logs"}
}

func firewallRow(f map[string]any) console.Resource {
	typ := "Ingress"
	if vpcStr(f, "direction") == "EGRESS" {
		typ = "Egress"
	}
	targets := "Apply to all"
	if t := vpcStrs(f, "targetTags"); len(t) > 0 {
		targets = strings.Join(t, ", ")
	} else if t := vpcStrs(f, "targetServiceAccounts"); len(t) > 0 {
		targets = strings.Join(t, ", ")
	}
	var filters []string
	if r := vpcStrs(f, "sourceRanges"); len(r) > 0 {
		filters = append(filters, "IP ranges: "+strings.Join(r, ", "))
	}
	if r := vpcStrs(f, "sourceTags"); len(r) > 0 {
		filters = append(filters, "Tags: "+strings.Join(r, ", "))
	}
	if r := vpcStrs(f, "destinationRanges"); len(r) > 0 {
		filters = append(filters, "IP ranges: "+strings.Join(r, ", "))
	}
	action, entries := "Allow", f["allowed"]
	if _, ok := f["denied"]; ok {
		action, entries = "Deny", f["denied"]
	}
	logs := "Off"
	if lc, _ := f["logConfig"].(map[string]any); lc["enable"] == true {
		logs = "On"
	}
	status := ""
	if f["disabled"] == true {
		status = "Disabled"
	}
	return console.Resource{Name: vpcStr(f, "name"), Status: status, Fields: map[string]string{
		"Type": typ, "Targets": targets, "Filters": vpcDash(strings.Join(filters, "; ")),
		"Protocols / ports": protocolsText(entries), "Action": action, "Priority": vpcStr(f, "priority"),
		"Network": vpcLast(vpcStr(f, "network")), "Logs": logs}}
}

// protocolsText is a rule's entries as Google's list shows them:
// tcp:22,80 udp icmp, or all.
func protocolsText(v any) string {
	l, _ := v.([]any)
	var parts []string
	for _, e := range l {
		m, _ := e.(map[string]any)
		p := vpcStr(m, "IPProtocol")
		if p == "all" {
			return "all"
		}
		if ports := vpcStrs(m, "ports"); len(ports) > 0 {
			p += ":" + strings.Join(ports, ",")
		}
		parts = append(parts, p)
	}
	return vpcDash(strings.Join(parts, " "))
}

// parseProtocols reads "tcp:22,80 udp:53 icmp" or "all".
func parseProtocols(s string) ([]any, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "all" {
		return []any{map[string]any{"IPProtocol": "all"}}, nil
	}
	var out []any
	for _, f := range strings.Fields(strings.ReplaceAll(s, ";", " ")) {
		proto, ports, has := strings.Cut(f, ":")
		e := map[string]any{"IPProtocol": strings.ToLower(proto)}
		if has {
			var l []any
			for _, p := range strings.Split(ports, ",") {
				if p = strings.TrimSpace(p); p != "" {
					l = append(l, p)
				}
			}
			e["ports"] = l
		}
		out = append(out, e)
	}
	return out, nil
}

func (p computeFirewallsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: firewallColumns(), Noun: "firewall rules", Note: computeNote, TableTitle: "VPC firewall rules"}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Firewall rules are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	fws, err := p.list(project, "global/firewalls")
	if err != nil {
		return listing, err
	}
	for _, f := range fws {
		listing.Items = append(listing.Items, firewallRow(f))
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (computeFirewallsProvider) CreateOnPage() bool  { return true }
func (computeFirewallsProvider) CreateTitle() string { return "Create a firewall rule" }
func (computeFirewallsProvider) CreateLayout() string {
	return console.CreateLayoutPage
}

// Google's Create firewall rule (firewall/docs/using-firewalls, read
// 2026-11-22): Name, Description, Logs, Network, Priority, Direction of
// traffic, Action on match, Targets (All instances in the network,
// Specified target tags, Specified service account), the Source filter
// with its Second source filter (ingress) or the Destination filter
// (egress), Protocols and ports (Allow all / Deny all, or TCP, UDP and
// Other) and Enforcement.
const (
	fwAllTargets = "All instances in the network"
	fwTargetTags = "Specified target tags"
	fwTargetSA   = "Specified service account"
	fwIPv4       = "IPv4 ranges"
	fwIPv6       = "IPv6 ranges"
	fwSourceTags = "Source tags"
	fwSA         = "Service account"
	fwNone       = "None"
	fwAll        = "all"
	fwSpecified  = "specified"
)

// fwPorts splits a rule's entries into TCP ports, UDP ports and the other
// protocols, as Google's Specified protocols and ports holds them; all is
// whether the rule covers every protocol.
func fwPorts(v any) (tcp, udp bool, tcpPorts, udpPorts, other string, all bool) {
	l, _ := v.([]any)
	var others []string
	for _, e := range l {
		m, _ := e.(map[string]any)
		ports := strings.Join(vpcStrs(m, "ports"), ",")
		switch p := vpcStr(m, "IPProtocol"); p {
		case "all":
			all = true
		case "tcp":
			tcp, tcpPorts = true, ports
		case "udp":
			udp, udpPorts = true, ports
		default:
			others = append(others, p)
		}
	}
	return tcp, udp, tcpPorts, udpPorts, strings.Join(others, ", "), all || len(l) == 0
}

func firewallFields(networks []string, cur map[string]any) []console.Field {
	def := func(k, d string) string {
		if cur == nil {
			return d
		}
		return vpcStr(cur, k)
	}
	join := func(k string) string {
		if cur == nil {
			return ""
		}
		return strings.Join(vpcStrs(cur, k), ", ")
	}
	dir, action, logs := "Ingress", "Allow", "Off"
	targets, source, second, dest := fwAllTargets, fwIPv4, fwNone, fwIPv4
	var entries any
	if cur != nil {
		if vpcStr(cur, "direction") == "EGRESS" {
			dir = "Egress"
		}
		entries = cur["allowed"]
		if _, ok := cur["denied"]; ok {
			action, entries = "Deny", cur["denied"]
		}
		if lc, _ := cur["logConfig"].(map[string]any); lc["enable"] == true {
			logs = "On"
		}
		switch {
		case len(vpcStrs(cur, "targetTags")) > 0:
			targets = fwTargetTags
		case len(vpcStrs(cur, "targetServiceAccounts")) > 0:
			targets = fwTargetSA
		}
		ranges, tags, sas := len(vpcStrs(cur, "sourceRanges")) > 0, len(vpcStrs(cur, "sourceTags")) > 0, len(vpcStrs(cur, "sourceServiceAccounts")) > 0
		switch {
		case ranges && tags:
			second = fwSourceTags
		case ranges && sas:
			second = fwSA
		case !ranges && tags:
			source = fwSourceTags
		case !ranges && sas:
			source = fwSA
		case !ranges:
			source = fwNone
		}
		if len(vpcStrs(cur, "destinationRanges")) == 0 {
			dest = fwNone
		}
		if r := vpcStrs(cur, "sourceRanges"); len(r) > 0 && strings.Contains(r[0], ":") {
			source = fwIPv6
		}
		if r := vpcStrs(cur, "destinationRanges"); len(r) > 0 && strings.Contains(r[0], ":") {
			dest = fwIPv6
		}
	}
	tcp, udp, tcpPorts, udpPorts, other, all := fwPorts(entries)
	mode := fwAll
	if cur != nil && !all {
		mode = fwSpecified
	}
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Default: "default", Options: networks, Immutable: cur != nil}
	if len(networks) == 0 {
		netField.Type = "text"
	}
	if cur != nil {
		netField.Default = vpcLast(vpcStr(cur, "network"))
	}
	enforcement := "Enabled"
	if cur != nil && cur["disabled"] == true {
		enforcement = "Disabled"
	}
	ingress := func(m map[string]string) map[string]string { m["direction"] = "Ingress"; return m }
	specified := map[string]string{"protocolsMode": fwSpecified}
	sa := func(k string) string {
		if l := vpcStrs(cur, k); cur != nil && len(l) > 0 {
			return l[0]
		}
		return ""
	}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp, Immutable: cur != nil, Default: def("name", "")},
		{Name: "description", Label: "Description", Type: "text", Default: def("description", "")},
		{Name: "logs", Label: "Logs", Type: "radio", Default: logs, Options: []string{"On", "Off"},
			Help: "Kept as the rule's logConfig; firewall logs are not written locally."},
		netField,
		{Name: "priority", Label: "Priority", Type: "text", Required: true, Default: def("priority", "1000"), Pattern: `^[0-9]{1,5}$`,
			Help: "Priority can be 0 - 65535."},
		{Name: "direction", Label: "Direction of traffic", Type: "radio", Default: dir, Options: []string{"Ingress", "Egress"}, Immutable: cur != nil},
		{Name: "action", Label: "Action on match", Type: "radio", Default: action, Options: []string{"Allow", "Deny"}, Immutable: cur != nil},
		{Name: "targets", Label: "Targets", Type: "select", Default: targets, Options: []string{fwAllTargets, fwTargetTags, fwTargetSA}},
		{Name: "targetTags", Label: "Target tags", Type: "text", Required: true, Default: join("targetTags"),
			VisibleWhen: map[string]string{"targets": fwTargetTags}, Help: "Comma-separated network tags."},
		{Name: "targetServiceAccount", Label: "Target service account", Type: "text", Required: true, Default: sa("targetServiceAccounts"),
			VisibleWhen: map[string]string{"targets": fwTargetSA}, Help: "The email of a service account in this project."},
		{Name: "sourceFilter", Label: "Source filter", Type: "select", Default: source, Options: []string{fwIPv4, fwIPv6, fwSourceTags, fwSA, fwNone},
			VisibleWhen: ingress(map[string]string{})},
		{Name: "sourceRanges", Label: "Source IPv4 ranges", Type: "text", Required: true, Default: join("sourceRanges"),
			VisibleWhen: ingress(map[string]string{"sourceFilter": fwIPv4}), Help: "Comma-separated, for example 0.0.0.0/0, 192.168.2.0/24."},
		{Name: "sourceRanges6", Label: "Source IPv6 ranges", Type: "text", Required: true, Default: join("sourceRanges"),
			VisibleWhen: ingress(map[string]string{"sourceFilter": fwIPv6}), Help: "Comma-separated, for example ::/0, fd20:a:b::/48."},
		{Name: "sourceTags", Label: "Source tags", Type: "text", Required: true, Default: join("sourceTags"),
			VisibleWhen: ingress(map[string]string{"sourceFilter": fwSourceTags})},
		{Name: "sourceServiceAccount", Label: "Source service account", Type: "text", Required: true, Default: sa("sourceServiceAccounts"),
			VisibleWhen: ingress(map[string]string{"sourceFilter": fwSA})},
		{Name: "secondSourceFilter", Label: "Second source filter", Type: "select", Default: second, Options: []string{fwNone, fwSourceTags, fwSA},
			VisibleWhen: ingress(map[string]string{"sourceFilter": fwIPv4})},
		{Name: "secondSourceTags", Label: "Source tags", Type: "text", Required: true, Default: join("sourceTags"),
			VisibleWhen: ingress(map[string]string{"sourceFilter": fwIPv4, "secondSourceFilter": fwSourceTags})},
		{Name: "secondSourceServiceAccount", Label: "Source service account", Type: "text", Required: true, Default: sa("sourceServiceAccounts"),
			VisibleWhen: ingress(map[string]string{"sourceFilter": fwIPv4, "secondSourceFilter": fwSA})},
		{Name: "destinationFilter", Label: "Destination filter", Type: "select", Default: dest, Options: []string{fwIPv4, fwIPv6, fwNone},
			VisibleWhen: map[string]string{"direction": "Egress"}},
		{Name: "destinationRanges", Label: "Destination IPv4 ranges", Type: "text", Required: true, Default: join("destinationRanges"),
			VisibleWhen: map[string]string{"direction": "Egress", "destinationFilter": fwIPv4}},
		{Name: "destinationRanges6", Label: "Destination IPv6 ranges", Type: "text", Required: true, Default: join("destinationRanges"),
			VisibleWhen: map[string]string{"direction": "Egress", "destinationFilter": fwIPv6}},
		{Name: "protocolsMode", Label: "Protocols and ports", Type: "radio", Default: mode, Options: []string{fwAll, fwSpecified},
			OptionLabels: map[string]string{fwAll: "Allow all", fwSpecified: "Specified protocols and ports"},
			LabelsBy:     "action", LabelsFor: map[string]map[string]string{"Deny": {fwAll: "Deny all"}}},
		{Name: "tcp", Label: "TCP", Type: "checkbox", Default: strconv.FormatBool(tcp), VisibleWhen: specified},
		{Name: "tcpPorts", Label: "Ports", Type: "text", Default: tcpPorts, VisibleWhen: map[string]string{"protocolsMode": fwSpecified, "tcp": "true"},
			Help: "For example, 20, 50-60. Empty is every port."},
		{Name: "udp", Label: "UDP", Type: "checkbox", Default: strconv.FormatBool(udp), VisibleWhen: specified},
		{Name: "udpPorts", Label: "Ports", Type: "text", Default: udpPorts, VisibleWhen: map[string]string{"protocolsMode": fwSpecified, "udp": "true"},
			Help: "For example, 20, 50-60. Empty is every port."},
		{Name: "other", Label: "Other", Type: "checkbox", Default: strconv.FormatBool(other != ""), VisibleWhen: specified},
		{Name: "otherProtocols", Label: "Protocols", Type: "text", Default: other, VisibleWhen: map[string]string{"protocolsMode": fwSpecified, "other": "true"},
			Help: "Comma-separated, for example icmp, sctp, esp, ah."},
		{Name: "enforcement", Label: "Enforcement", Type: "radio", Default: enforcement, Options: []string{"Enabled", "Disabled"}},
	}
}

func (computeFirewallsProvider) CreateForm() (string, []console.Field) {
	return "Create firewall rule", firewallFields(nil, nil)
}

func (p computeFirewallsProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return firewallFields(p.networkNames(ctx, project), nil)
}

// firewallEntries are Protocols and ports as the rule's allowed or denied
// entries.
func firewallEntries(values map[string]string) ([]any, error) {
	if values["protocolsMode"] != fwSpecified {
		// A form without the radio (an older client) sends protocols.
		if p := strings.TrimSpace(values["protocols"]); p != "" {
			return parseProtocols(p)
		}
		return []any{map[string]any{"IPProtocol": "all"}}, nil
	}
	var out []any
	add := func(proto, ports string) {
		e := map[string]any{"IPProtocol": proto}
		var l []any
		for _, p := range strings.Split(ports, ",") {
			if p = strings.TrimSpace(p); p != "" {
				l = append(l, p)
			}
		}
		if len(l) > 0 {
			e["ports"] = l
		}
		out = append(out, e)
	}
	if values["tcp"] == "true" {
		add("tcp", values["tcpPorts"])
	}
	if values["udp"] == "true" {
		add("udp", values["udpPorts"])
	}
	if values["other"] == "true" {
		for _, p := range vpcList(strings.ToLower(values["otherProtocols"])) {
			add(p.(string), "")
		}
	}
	if len(out) == 0 {
		return nil, errors.New("Protocols and ports: choose TCP, UDP or Other, or allow all")
	}
	return out, nil
}

func firewallBody(values map[string]string) (map[string]any, error) {
	disabled := values["enforcement"] == "Disabled" || values["disabled"] == "true"
	b := map[string]any{"name": strings.TrimSpace(values["name"]), "description": strings.TrimSpace(values["description"]),
		"network":   "global/networks/" + strings.TrimSpace(vpcOr(values["network"], "default")),
		"direction": strings.ToUpper(vpcOr(values["direction"], "Ingress")), "disabled": disabled,
		"logConfig": map[string]any{"enable": values["logs"] == "On"}}
	if pr := strings.TrimSpace(values["priority"]); pr != "" {
		n, err := strconv.Atoi(pr)
		if err != nil {
			return nil, fmt.Errorf("priority: %q is not a number", pr)
		}
		b["priority"] = float64(n)
	}
	entries, err := firewallEntries(values)
	if err != nil {
		return nil, err
	}
	if values["action"] == "Deny" {
		b["denied"] = entries
		b["allowed"] = nil
	} else {
		b["allowed"] = entries
		b["denied"] = nil
	}
	list := func(v string) any {
		if l := vpcList(v); len(l) > 0 {
			return l
		}
		return nil
	}
	b["targetTags"], b["targetServiceAccounts"] = nil, nil
	switch values["targets"] {
	case fwTargetTags:
		b["targetTags"] = list(values["targetTags"])
	case fwTargetSA:
		b["targetServiceAccounts"] = list(values["targetServiceAccount"])
	case "":
		// A form without Targets (an older client) sends target tags alone.
		b["targetTags"] = list(values["targetTags"])
	}
	b["sourceRanges"], b["sourceTags"], b["sourceServiceAccounts"], b["destinationRanges"] = nil, nil, nil, nil
	if b["direction"] == "EGRESS" {
		switch values["destinationFilter"] {
		case fwNone:
		case fwIPv6:
			b["destinationRanges"] = list(values["destinationRanges6"])
		default:
			b["destinationRanges"] = list(values["destinationRanges"])
		}
		return b, nil
	}
	switch values["sourceFilter"] {
	case fwIPv6:
		b["sourceRanges"] = list(values["sourceRanges6"])
	case fwIPv4, "":
		b["sourceRanges"] = list(values["sourceRanges"])
		switch values["secondSourceFilter"] {
		case fwSourceTags:
			b["sourceTags"] = list(values["secondSourceTags"])
		case fwSA:
			b["sourceServiceAccounts"] = list(values["secondSourceServiceAccount"])
		case "":
			b["sourceTags"] = list(values["sourceTags"])
		}
	case fwSourceTags:
		b["sourceTags"] = list(values["sourceTags"])
	case fwSA:
		b["sourceServiceAccounts"] = list(values["sourceServiceAccount"])
	}
	return b, nil
}

func (p computeFirewallsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	b, err := firewallBody(values)
	if err != nil {
		return "", err
	}
	for k, v := range b {
		if v == nil {
			delete(b, k)
		}
	}
	if _, err := p.call("POST", project, "global/firewalls", nil, b); err != nil {
		return "", err
	}
	return vpcStr(b, "name"), nil
}

func (p computeFirewallsProvider) Delete(ctx context.Context, project, name string) error {
	_, err := p.call("DELETE", project, "global/firewalls/"+name, nil, nil)
	return err
}

func (p computeFirewallsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	f, err := p.call("GET", project, "global/firewalls/"+path[0], nil, nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	row := firewallRow(f)
	var summary []console.Property
	for _, c := range firewallColumns() {
		summary = append(summary, console.Property{Label: c, Value: row.Fields[c]})
	}
	summary = append(summary, console.Property{Label: "Description", Value: vpcDash(vpcStr(f, "description"))},
		console.Property{Label: "Enforcement", Value: map[bool]string{true: "Disabled", false: "Enabled"}[f["disabled"] == true]})
	fields := firewallFields(p.networkNames(ctx, project), f)
	return console.Detail{Summary: summary,
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Note: computeNote,
			Groups: []console.PropertyGroup{{Properties: []console.Property{{Label: "Created", Value: vpcStr(f, "creationTimestamp")}}}}}},
		Edit: &console.EditForm{Label: "Edit", Fields: fields, Note: "A rule's name, network, direction and action cannot be changed."}}, nil
}

func (p computeFirewallsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("no firewall rule")
	}
	values["name"] = path[0]
	// Direction and Action on match are fixed: a form leaves them out, so
	// the rule's own are kept.
	cur, err := p.call("GET", project, "global/firewalls/"+path[0], nil, nil)
	if err != nil {
		return err
	}
	if values["direction"] == "" {
		values["direction"] = map[bool]string{true: "Egress", false: "Ingress"}[vpcStr(cur, "direction") == "EGRESS"]
	}
	if values["action"] == "" {
		values["action"] = "Allow"
		if _, ok := cur["denied"]; ok {
			values["action"] = "Deny"
		}
	}
	b, err := firewallBody(values)
	if err != nil {
		return err
	}
	delete(b, "name")
	delete(b, "network")
	_, err = p.call("PATCH", project, "global/firewalls/"+path[0], nil, b)
	return err
}

// --- Routes -----------------------------------------------------------------

// computeRoutesProvider is Routes: the project's routes (each network's
// default route, its subnet routes and the static routes), Create route
// (to the default internet gateway or an internal IP address) and Delete.
type computeRoutesProvider struct{ computeScreen }

func (computeRoutesProvider) ID() string    { return "vpc-routes" }
func (computeRoutesProvider) Title() string { return "Routes" }

// The Routes page's tabs (#2140, vpc/docs/using-routes): Effective routes
// and Route management, this one.

func routeColumns() []string {
	return []string{"Description", "Destination IP range", "Priority", "Instance tags", "Next hop", "Network", "Route type"}
}

func routeRow(r map[string]any) console.Resource {
	hop := "—"
	switch {
	case vpcStr(r, "nextHopGateway") != "":
		hop = "Default internet gateway"
	case vpcStr(r, "nextHopIp") != "":
		hop = vpcStr(r, "nextHopIp")
	case vpcStr(r, "nextHopInstance") != "":
		hop = "Instance " + vpcLast(vpcStr(r, "nextHopInstance")) + " (zone " + zoneOf(vpcStr(r, "nextHopInstance")) + ")"
	case vpcStr(r, "nextHopNetwork") != "":
		hop = "Virtual network " + vpcLast(vpcStr(r, "nextHopNetwork"))
	}
	typ := "Static"
	if vpcStr(r, "routeType") == "SUBNET" {
		typ = "Subnet"
	}
	return console.Resource{Name: vpcStr(r, "name"), Fields: map[string]string{
		"Description": vpcDash(vpcStr(r, "description")), "Destination IP range": vpcStr(r, "destRange"), "Priority": vpcStr(r, "priority"),
		"Instance tags": vpcDash(strings.Join(vpcStrs(r, "tags"), ", ")), "Next hop": hop, "Network": vpcLast(vpcStr(r, "network")), "Route type": typ}}
}

func (p computeRoutesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: routeColumns(), Noun: "routes", Note: computeNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Routes are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	rs, err := p.list(project, "global/routes")
	if err != nil {
		return listing, err
	}
	for _, r := range rs {
		listing.Items = append(listing.Items, routeRow(r))
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computeRoutesProvider) CreateOnPage() bool  { return true }
func (computeRoutesProvider) CreateTitle() string { return "Create a route" }

// Google's Create route (vpc/docs/using-routes, read 2026-11-22): Name,
// Description, Network, Route type, IP version, Destination IPv4 range,
// Priority, Instance tags and the Next hop: Default internet gateway,
// Specify an instance, Specify IP address of an instance, Specify VPN
// tunnel, Specify a forwarding rule of an internal passthrough Network Load
// Balancer (the last two drawn unavailable: neither is served).
const (
	hopGateway  = "Default internet gateway"
	hopInstance = "Specify an instance"
	hopIP       = "Specify IP address of an instance"
	hopVPN      = "Specify VPN tunnel"
	hopILB      = "Specify a forwarding rule of an internal passthrough Network Load Balancer"
)

func routeFields(networks, instances []string) []console.Field {
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Default: "default", Options: networks}
	if len(networks) == 0 {
		netField.Type = "text"
	}
	instField := console.Field{Name: "nextHopInstance", Label: "Next hop instance", Type: "select", Required: true, Options: instances,
		VisibleWhen: map[string]string{"nextHop": hopInstance}, Help: "An instance of the route's network, as ZONE/NAME."}
	if len(instances) == 0 {
		instField.Type, instField.Help = "text", "An instance of the route's network, as ZONE/NAME: none exists yet."
	}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text"},
		netField,
		{Name: "routeType", Label: "Route type", Type: "select", Default: "Static route", Options: []string{"Static route"}},
		{Name: "ipVersion", Label: "IP version", Type: "radio", Default: "IPv4", Options: []string{"IPv4", "IPv6"},
			Help: "An IPv6 route's next hop is the default internet gateway, an IPv6 address of a dual-stack subnet, or a dual-stack instance."},
		{Name: "destRange", Label: "Destination IPv4 range", Type: "text", Required: true, VisibleWhen: map[string]string{"ipVersion": "IPv4"},
			Help: "For example, 192.168.0.0/16. It may not match or fit within a subnet's range."},
		{Name: "destRange6", Label: "Destination IPv6 range", Type: "text", Required: true, VisibleWhen: map[string]string{"ipVersion": "IPv6"},
			Help: "For example, 2001:db8::/32. It may not match or fit within a subnet's IPv6 range."},
		{Name: "priority", Label: "Priority", Type: "text", Required: true, Default: "1000", Pattern: `^[0-9]{1,5}$`, Help: "Priority can be 0 - 65535; a lower number is preferred."},
		{Name: "tags", Label: "Instance tags", Type: "text", Help: "Optional, comma-separated: the route applies only to instances with one of these tags; blank applies it to all instances."},
		{Name: "nextHop", Label: "Next hop", Type: "select", Default: hopGateway,
			Options: []string{hopGateway, hopInstance, hopIP, hopVPN, hopILB}, DisabledOptions: []string{hopVPN, hopILB},
			Help: "VPN tunnels and internal passthrough Network Load Balancers are not served."},
		instField,
		{Name: "nextHopIp", Label: "Next hop IP address", Type: "text", Required: true, VisibleWhen: map[string]string{"nextHop": hopIP},
			Help: "An internal address in one of the network's subnets, of the route's IP version."},
	}
}

// instanceNames are the project's instances as ZONE/NAME.
func (c computeScreen) instanceNames(project string) []string {
	m, err := c.call("GET", project, "aggregated/instances", nil, nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, o := range aggregatedItems(m, "instances") {
		out = append(out, zoneOf(vpcStr(o, "zone"))+"/"+vpcStr(o, "name"))
	}
	sort.Strings(out)
	return out
}

func (computeRoutesProvider) CreateForm() (string, []console.Field) {
	return "Create route", routeFields(nil, nil)
}

func (p computeRoutesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return routeFields(p.networkNames(ctx, project), p.instanceNames(project))
}

func (p computeRoutesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	b := map[string]any{"name": name, "network": "global/networks/" + strings.TrimSpace(vpcOr(values["network"], "default")),
		"destRange": strings.TrimSpace(values["destRange"])}
	if values["ipVersion"] == "IPv6" {
		b["destRange"] = strings.TrimSpace(values["destRange6"])
	}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	if pr := strings.TrimSpace(values["priority"]); pr != "" {
		n, err := strconv.Atoi(pr)
		if err != nil {
			return "", fmt.Errorf("priority: %q is not a number", pr)
		}
		b["priority"] = float64(n)
	}
	if t := vpcList(values["tags"]); len(t) > 0 {
		b["tags"] = t
	}
	switch values["nextHop"] {
	case hopIP, "Specify an IP address":
		b["nextHopIp"] = strings.TrimSpace(values["nextHopIp"])
	case hopInstance:
		zone, name, ok := strings.Cut(strings.TrimSpace(values["nextHopInstance"]), "/")
		if !ok {
			return "", fmt.Errorf("next hop instance: %q is not ZONE/NAME", values["nextHopInstance"])
		}
		b["nextHopInstance"] = "zones/" + zone + "/instances/" + name
	case hopVPN, hopILB:
		return "", fmt.Errorf("%s: VPN tunnels and internal passthrough Network Load Balancers are not served", values["nextHop"])
	default:
		b["nextHopGateway"] = "global/gateways/default-internet-gateway"
	}
	if _, err := p.call("POST", project, "global/routes", nil, b); err != nil {
		return "", err
	}
	return name, nil
}

func (p computeRoutesProvider) Delete(ctx context.Context, project, name string) error {
	_, err := p.call("DELETE", project, "global/routes/"+name, nil, nil)
	return err
}

// --- IP addresses -----------------------------------------------------------

// computeAddressesProvider is IP addresses: the project's reserved
// addresses, regional and global, internal and external, with Reserve
// external static IP address, Reserve internal static IP address, Allocate
// IP range for private services access, and each row's Release static
// address. A row's actions are at [scope, name], scope "global" or a
// region.
type computeAddressesProvider struct{ computeScreen }

func (computeAddressesProvider) ID() string    { return "vpc-addresses" }
func (computeAddressesProvider) Title() string { return "IP addresses" }

func (p computeAddressesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	return p.listOf(project, "EXTERNAL")
}

// listOf is the IP addresses page's tab of one access type (#2140): the
// project's static addresses of that type, regional and global, and each
// VM's ephemeral one, which Promote to static IP address reserves.
func (p computeAddressesProvider) listOf(project, accessType string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"IP address", "Access type", "Region", "Type", "Version", "Purpose", "Network", "Subnetwork", "Network tier", "In use by"},
		Noun: map[string]string{"EXTERNAL": "external IP addresses", "INTERNAL": "internal IP addresses"}[accessType], Note: computeNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "IP addresses are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	agg, err := p.call("GET", project, "aggregated/addresses", nil, nil)
	if err != nil {
		return listing, err
	}
	items, _ := agg["items"].(map[string]any)
	scopes := make([]string, 0, len(items))
	for k := range items {
		scopes = append(scopes, k)
	}
	sort.Strings(scopes)
	static := map[string]bool{}
	for _, scope := range scopes {
		sv, _ := items[scope].(map[string]any)
		l, _ := sv["addresses"].([]any)
		for _, e := range l {
			a, _ := e.(map[string]any)
			access := "External"
			if vpcStr(a, "addressType") == "INTERNAL" {
				access = "Internal"
			}
			if strings.ToUpper(access) != accessType {
				continue
			}
			region := "global"
			if strings.HasPrefix(scope, "regions/") {
				region = strings.TrimPrefix(scope, "regions/")
			}
			ip := vpcStr(a, "address")
			static[ip] = true
			if pl := vpcStr(a, "prefixLength"); pl != "" && pl != "0" {
				ip += "/" + pl
			}
			var users []string
			for _, u := range vpcStrs(a, "users") {
				users = append(users, vpcLast(u))
			}
			target := []string{region, vpcStr(a, "name")}
			listing.Items = append(listing.Items, console.Resource{Name: vpcStr(a, "name"), Status: vpcTitle(vpcStr(a, "status")), Target: target,
				Actions: p.addressActions(a),
				Fields: map[string]string{"IP address": ip, "Access type": access, "Region": region, "Type": "Static", "Version": map[bool]string{true: "IPv6", false: "IPv4"}[vpcStr(a, "ipVersion") == "IPV6"],
					"Purpose": vpcDash(vpcStr(a, "purpose")), "Network": vpcDash(vpcLast(vpcStr(a, "network"))), "Subnetwork": vpcDash(vpcLast(vpcStr(a, "subnetwork"))),
					"Network tier": vpcDash(vpcTitle(vpcStr(a, "networkTier"))), "In use by": vpcDash(strings.Join(users, ", "))}})
		}
	}
	// Each VM's ephemeral addresses, as Google's tabs list them (Type
	// Ephemeral) with Promote to static IP address.
	if m, err := p.call("GET", project, "aggregated/instances", nil, nil); err == nil {
		for _, o := range aggregatedItems(m, "instances") {
			zone := zoneOf(vpcStr(o, "zone"))
			region := zone[:strings.LastIndex(zone, "-")]
			for _, n := range vmMaps(o, "networkInterfaces") {
				ip, tier := vpcStr(n, "networkIP"), "—"
				if accessType == "EXTERNAL" {
					ip = ""
					for _, ac := range vmMaps(n, "accessConfigs") {
						ip, tier = vpcStr(ac, "natIP"), vpcTitle(vpcStr(ac, "networkTier"))
					}
				}
				if ip == "" || static[ip] {
					continue
				}
				// Promote to static IP address (vpc/docs/reserve-static-external-ip-address
				// and reserve-static-internal-ip-address, read 2026-11-22):
				// Name and Description, then Reserve; the address keeps its IP
				// and stays IN_USE by the VM.
				promote := console.Action{ID: "promote", Label: "Promote to static IP address", Submit: "Reserve", Fields: []console.Field{
					{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
					{Name: "description", Label: "Description", Type: "text"}}}
				target := []string{"instance", zone, vpcStr(o, "name"), ip}
				listing.Items = append(listing.Items, console.Resource{Name: "—", Status: "In use", Target: target, Actions: []console.Action{promote},
					Fields: map[string]string{"IP address": ip, "Access type": vpcTitle(accessType), "Region": region, "Type": "Ephemeral", "Version": "IPv4",
						"Purpose": "—", "Network": vpcLast(vpcStr(n, "network")), "Subnetwork": vpcLast(vpcStr(n, "subnetwork")),
						"Network tier": tier, "In use by": "VM instance " + vpcStr(o, "name") + " (" + zone + ")"}})
			}
		}
	}
	listing.Total = len(listing.Items)
	listing.AlwaysStatus = true
	return listing, nil
}

// computeInternalAddressesProvider is the IP addresses page's Internal IP
// addresses tab (#2140, vpc/docs/reserve-static-internal-ip-address).
type computeInternalAddressesProvider struct{ computeAddressesProvider }

func (computeInternalAddressesProvider) ID() string    { return "vpc-addresses-internal" }
func (computeInternalAddressesProvider) Title() string { return "IP addresses" }

func (p computeInternalAddressesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	return p.listOf(project, "INTERNAL")
}

func vpcTitle(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func (computeAddressesProvider) releaseAction(name string) console.Action {
	return console.Action{ID: "release", Label: "Release static address", Destructive: true, Confirm: "Release static address " + name + "?"}
}

func (p computeAddressesProvider) ListActions(ctx context.Context, project string) []console.Action {
	if p.api() == nil || project == "" {
		return nil
	}
	nets := p.networkNames(ctx, project)
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Default: "default", Options: nets}
	name := console.Field{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp}
	desc := console.Field{Name: "description", Label: "Description", Type: "text"}
	regional := map[string]string{"ipType": "Regional"}
	return []console.Action{
		{ID: "reserve-external", Label: "Reserve external static IP address", Submit: "Reserve", Fields: []console.Field{name, desc,
			{Name: "networkTier", Label: "Network Service Tier", Type: "radio", Default: "Premium", Options: []string{"Premium", "Standard"}},
			{Name: "ipType", Label: "Type", Type: "radio", Default: "Regional", Options: []string{"Regional", "Global"}},
			func() console.Field { f := computeRegionField("region"); f.VisibleWhen = regional; return f }(),
			p.attachToField(project, regional),
		}},
		// Google's Reserve internal static IP address
		// (vpc/docs/reserve-static-internal-ip-address, read 2026-11-22):
		// Name, Description, IP version, Network, Region, Subnetwork, Static
		// IP address (Assign automatically or Let me choose) and Purpose.
		{ID: "reserve-internal", Label: "Reserve internal static IP address", Submit: "Reserve", Fields: []console.Field{name, desc,
			{Name: "ipVersion", Label: "IP version", Type: "radio", Default: "IPv4", Options: []string{"IPv4", "IPv6"},
				Help: "IPv6 reserves a /96 of a dual-stack subnet with internal IPv6 access; give it to a VM as its interface's IPv6 address."},
			netField, computeRegionField("region"),
			{Name: "subnetwork", Label: "Subnetwork", Type: "text", Required: true, Default: "default", Help: "The subnet, in the region, the address is in."},
			{Name: "assign", Label: "Static IP address", Type: "select", Default: "Assign automatically", Options: []string{"Assign automatically", "Let me choose"}},
			{Name: "address", Label: "Custom IP address", Type: "text", Required: true, VisibleWhen: map[string]string{"assign": "Let me choose"},
				Help: "An address in the subnet's primary range, not one of the four it keeps; a VM's ephemeral address is promoted to static."},
			{Name: "purpose", Label: "Purpose", Type: "radio", Default: "Non-shared", Options: []string{"Non-shared", "Shared"}},
		}},
		{ID: "allocate-psa", Label: "Allocate IP range for private services access", Submit: "Allocate", Fields: []console.Field{name, desc, netField,
			{Name: "prefixLength", Label: "Prefix length", Type: "text", Required: true, Default: "16", Pattern: `^[0-9]{1,2}$`,
				Help: "8 to 29: the size of the range; Google recommends /16 or larger for private services access."},
			{Name: "address", Label: "Start of the range", Type: "text",
				Help: "Optional: the first address of the range. Empty chooses one that overlaps none of the network's subnets."},
		}},
	}
}

func (p computeAddressesProvider) addressActions(a map[string]any) []console.Action {
	labels := map[string]string{}
	if l, ok := a["labels"].(map[string]any); ok {
		for k, v := range l {
			labels[k] = fmt.Sprint(v)
		}
	}
	return []console.Action{
		{ID: "labels", Label: "Edit labels", Submit: "Save", Fields: []console.Field{labelsField(labels, ""),
			{Name: "labelFingerprint", Type: "hidden", Default: vpcStr(a, "labelFingerprint")}}},
		p.releaseAction(vpcStr(a, "name")),
	}
}

func addressScope(region string) string {
	if region == "global" {
		return "global"
	}
	return "regions/" + region
}

func (p computeAddressesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) == 4 && path[0] == "instance" && p.api() != nil {
		for _, access := range []string{"EXTERNAL", "INTERNAL"} {
			l, err := p.listOf(project, access)
			if err != nil {
				return nil
			}
			for _, r := range l.Items {
				if strings.Join(r.Target, "/") == strings.Join(path, "/") {
					return r.Actions
				}
			}
		}
		return nil
	}
	if len(path) != 2 || p.api() == nil {
		return nil
	}
	a, err := p.call("GET", project, addressScope(path[0])+"/addresses/"+path[1], nil, nil)
	if err != nil {
		return []console.Action{p.releaseAction(path[1])}
	}
	return p.addressActions(a)
}

func (p computeAddressesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 2 && action == "release" {
		_, err := p.call("DELETE", project, addressScope(path[0])+"/addresses/"+path[1], nil, nil)
		return err
	}
	if len(path) == 2 && action == "labels" {
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		fp := values["labelFingerprint"]
		if fp == "" {
			a, err := p.call("GET", project, addressScope(path[0])+"/addresses/"+path[1], nil, nil)
			if err != nil {
				return err
			}
			fp = vpcStr(a, "labelFingerprint")
		}
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		_, err = p.call("POST", project, addressScope(path[0])+"/addresses/"+path[1]+"/setLabels", nil,
			map[string]any{"labels": l, "labelFingerprint": fp})
		return err
	}
	if len(path) == 4 && path[0] == "instance" && action == "promote" {
		// An instance's ephemeral internal address, reserved where it is.
		o, err := p.call("GET", project, "zones/"+path[1]+"/instances/"+path[2], nil, nil)
		if err != nil {
			return err
		}
		region := path[1][:strings.LastIndex(path[1], "-")]
		for _, n := range vmMaps(o, "networkInterfaces") {
			for _, ac := range vmMaps(n, "accessConfigs") {
				if vpcStr(ac, "natIP") != path[3] {
					continue
				}
				// An instance's ephemeral external address, promoted.
				b := map[string]any{"name": strings.TrimSpace(values["name"]), "addressType": "EXTERNAL", "address": path[3],
					"networkTier": vpcStr(ac, "networkTier")}
				if d := strings.TrimSpace(values["description"]); d != "" {
					b["description"] = d
				}
				_, err := p.call("POST", project, "regions/"+region+"/addresses", nil, b)
				return err
			}
			if vpcStr(n, "networkIP") != path[3] {
				continue
			}
			b := map[string]any{"name": strings.TrimSpace(values["name"]), "addressType": "INTERNAL", "address": path[3], "subnetwork": vpcStr(n, "subnetwork")}
			if d := strings.TrimSpace(values["description"]); d != "" {
				b["description"] = d
			}
			_, err := p.call("POST", project, "regions/"+path[1][:strings.LastIndex(path[1], "-")]+"/addresses", nil, b)
			return err
		}
		return fmt.Errorf("%s no longer has the address %s", path[2], path[3])
	}
	if len(path) != 0 {
		return fmt.Errorf("%s is not available on this resource", action)
	}
	b := map[string]any{"name": strings.TrimSpace(values["name"])}
	if d := strings.TrimSpace(values["description"]); d != "" {
		b["description"] = d
	}
	region := strings.TrimSpace(values["region"])
	switch action {
	case "reserve-external":
		b["networkTier"] = strings.ToUpper(vpcOr(values["networkTier"], "Premium"))
		if values["ipType"] == "Global" {
			_, err := p.call("POST", project, "global/addresses", nil, b)
			return err
		}
		if _, err := p.call("POST", project, "regions/"+region+"/addresses", nil, b); err != nil {
			return err
		}
		return p.attachExternal(project, region, b["name"].(string), values["attachTo"])
	case "reserve-internal":
		b["addressType"] = "INTERNAL"
		b["subnetwork"] = "regions/" + region + "/subnetworks/" + strings.TrimSpace(vpcOr(values["subnetwork"], "default"))
		if a := strings.TrimSpace(values["address"]); a != "" && values["assign"] != "Assign automatically" {
			b["address"] = a
		}
		if values["purpose"] == "Shared" {
			b["purpose"] = "SHARED_LOADBALANCER_VIP"
		}
		if values["ipVersion"] == "IPv6" {
			b["ipVersion"] = "IPV6"
		}
		_, err := p.call("POST", project, "regions/"+region+"/addresses", nil, b)
		return err
	case "allocate-psa":
		pl, err := strconv.Atoi(strings.TrimSpace(values["prefixLength"]))
		if err != nil {
			return fmt.Errorf("prefix length: %q is not a number", values["prefixLength"])
		}
		b["addressType"], b["purpose"], b["prefixLength"] = "INTERNAL", "VPC_PEERING", float64(pl)
		b["network"] = "global/networks/" + strings.TrimSpace(vpcOr(values["network"], "default"))
		if a := strings.TrimSpace(values["address"]); a != "" {
			b["address"] = a
		}
		_, err = p.call("POST", project, "global/addresses", nil, b)
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}
