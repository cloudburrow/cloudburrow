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

const computeNote = "Networks here are records: Memorystore, Kubernetes Engine and Cloud SQL check their network fields against them, " +
	"as Google does, but no traffic crosses them, and firewall rules and routes are not enforced. Operations complete at once."

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

func (computeNetworksProvider) CreateForm() (string, []console.Field) {
	custom := map[string]string{"subnetMode": "Custom"}
	return "Create VPC network", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "mtu", Label: "Maximum transmission unit (MTU)", Type: "select", Default: "1460", Options: []string{"1460", "1500", "8896"},
			Help: "1460 is the default; any value from 1300 to 8896 can be set through the API."},
		{Name: "subnetMode", Label: "Subnet creation mode", Type: "radio", Section: "Subnets", Default: "Custom", Options: []string{"Custom", "Automatic"},
			Help: "Automatic makes one subnet in each region, in 10.128.0.0/9; Custom makes only the subnets you add."},
		{Name: "subnetName", Label: "New subnet: name", Type: "text", Section: "Subnets", Pattern: `^$|` + computeNamePattern, VisibleWhen: custom,
			Help: "Optional: a first subnet, made with the network."},
		{Name: "subnetRegion", Label: "Region", Type: "select", Section: "Subnets", Default: "us-central1", Options: compute.Regions(), VisibleWhen: custom},
		{Name: "subnetRange", Label: "IPv4 range", Type: "text", Section: "Subnets", VisibleWhen: custom, Help: "For example, 10.0.0.0/24"},
		{Name: "privateGoogleAccess", Label: "Private Google Access", Type: "checkbox", Section: "Subnets", VisibleWhen: custom},
		{Name: "routingMode", Label: "Dynamic routing mode", Type: "radio", Section: "Dynamic routing mode", Default: "Regional", Options: []string{"Regional", "Global"}},
	}
}

func (p computeNetworksProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	body := map[string]any{"name": name, "autoCreateSubnetworks": values["subnetMode"] == "Automatic",
		"routingConfig": map[string]any{"routingMode": strings.ToUpper(vpcOr(values["routingMode"], "Regional"))}}
	if d := strings.TrimSpace(values["description"]); d != "" {
		body["description"] = d
	}
	if m := strings.TrimSpace(values["mtu"]); m != "" {
		n, err := strconv.Atoi(m)
		if err != nil {
			return "", fmt.Errorf("MTU: %q is not a number", m)
		}
		body["mtu"] = float64(n)
	}
	sub := strings.TrimSpace(values["subnetName"])
	if values["subnetMode"] != "Automatic" && sub != "" && strings.TrimSpace(values["subnetRange"]) == "" {
		return "", errors.New("the new subnet needs an IPv4 range")
	}
	if _, err := p.call("POST", project, "global/networks", nil, body); err != nil {
		return "", err
	}
	if values["subnetMode"] != "Automatic" && sub != "" {
		if err := p.addSubnet(project, name, map[string]string{"name": sub, "region": values["subnetRegion"],
			"ipCidrRange": values["subnetRange"], "privateGoogleAccess": values["privateGoogleAccess"]}); err != nil {
			return name, fmt.Errorf("the network %s was created, but not its subnet: %w", name, err)
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
	body := map[string]any{"name": strings.TrimSpace(values["name"]), "network": "global/networks/" + network,
		"ipCidrRange": strings.TrimSpace(values["ipCidrRange"]), "privateIpGoogleAccess": values["privateGoogleAccess"] == "true"}
	if d := strings.TrimSpace(values["description"]); d != "" {
		body["description"] = d
	}
	sec, err := parseSecondaries(values["secondaryRanges"])
	if err != nil {
		return err
	}
	if len(sec) > 0 {
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
		},
		Sections: []console.Section{
			{ID: "subnets", Label: "Subnets", Listing: subs, Note: computeNote},
			{ID: "firewalls", Label: "Firewalls", Listing: fws},
			{ID: "routes", Label: "Routes", Listing: routes},
		},
		Edit: &console.EditForm{Label: "Edit", Note: "A network's name and subnet creation mode are fixed; Switch to custom mode changes an auto mode network once.",
			Fields: []console.Field{
				{Name: "mtu", Label: "Maximum transmission unit (MTU)", Type: "text", Default: vpcStr(n, "mtu"), Pattern: `^[0-9]{4}$`, Help: "1300 to 8896."},
				{Name: "routingMode", Label: "Dynamic routing mode", Type: "radio", Default: routingModeText(n), Options: []string{"Regional", "Global"}},
			}},
	}, nil
}

// Edit is the network page's Edit: its MTU and dynamic routing mode
// (networks.patch).
func (p computeNetworksProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a network's own page has Edit")
	}
	b := map[string]any{"routingConfig": map[string]any{"routingMode": strings.ToUpper(vpcOr(values["routingMode"], "Regional"))}}
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
			{Label: "Purpose", Value: vpcStr(s, "purpose")},
			{Label: "Stack type", Value: vpcStr(s, "stackType")},
			{Label: "Description", Value: vpcDash(vpcStr(s, "description"))},
		},
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Note: computeNote,
			Groups: []console.PropertyGroup{{Properties: []console.Property{{Label: "Fingerprint", Value: vpcStr(s, "fingerprint")}}}}}},
	}, nil
}

func (p computeNetworksProvider) subnetActions(project string, path []string, s map[string]any) []console.Action {
	acts := []console.Action{
		{ID: "expand", Label: "Expand IP range", Submit: "Save", Fields: []console.Field{
			{Name: "ipCidrRange", Label: "Primary IPv4 range", Type: "text", Required: true, Default: vpcStr(s, "ipCidrRange"),
				Help: "A larger range containing the current one: a range can be expanded, never shrunk or replaced."}}},
		{ID: "secondary", Label: "Edit secondary IPv4 ranges", Submit: "Save", Fields: []console.Field{
			func() console.Field { f := secondaryRangesField(""); f.Default = secondaryRows(s); return f }(),
			{Name: "fingerprint", Type: "hidden", Default: vpcStr(s, "fingerprint")}}},
		{ID: "privateaccess", Label: "Private Google Access", Submit: "Save", Fields: []console.Field{
			{Name: "privateIpGoogleAccess", Label: "Private Google Access", Type: "checkbox", Default: strconv.FormatBool(s["privateIpGoogleAccess"] == true)}}},
		{ID: "delete", Label: "Delete subnet", Destructive: true, Confirm: "Delete subnet " + path[3] + "?"},
	}
	return acts
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
		acts := []console.Action{{ID: "addsubnet", Label: "Add subnet", Submit: "Add", Fields: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
			{Name: "description", Label: "Description", Type: "text"},
			computeRegionField("region"),
			{Name: "ipCidrRange", Label: "IPv4 range", Type: "text", Required: true, Help: "For example, 10.0.0.0/24"},
			secondaryRangesField(""),
			{Name: "privateGoogleAccess", Label: "Private Google Access", Type: "checkbox"},
		}}}
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

func (computeFirewallsProvider) ID() string    { return "vpc-firewalls" }
func (computeFirewallsProvider) Title() string { return "Firewall" }

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
	listing := console.Listing{Columns: firewallColumns(), Noun: "firewall rules", Note: computeNote}
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
	dir, action, protos := "Ingress", "Allow", "all"
	if cur != nil {
		if vpcStr(cur, "direction") == "EGRESS" {
			dir = "Egress"
		}
		if _, ok := cur["denied"]; ok {
			action, protos = "Deny", protocolsText(cur["denied"])
		} else {
			protos = protocolsText(cur["allowed"])
		}
	}
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Default: "default", Options: networks}
	if len(networks) == 0 {
		netField.Type = "text"
	}
	disabled := "false"
	if cur != nil && cur["disabled"] == true {
		disabled = "true"
	}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp, Immutable: cur != nil, Default: def("name", "")},
		{Name: "description", Label: "Description", Type: "text", Default: def("description", "")},
		netField,
		{Name: "priority", Label: "Priority", Type: "text", Default: def("priority", "1000"), Pattern: `^[0-9]{1,5}$`,
			Help: "0 to 65535; a lower number is applied first."},
		{Name: "direction", Label: "Direction of traffic", Type: "radio", Default: dir, Options: []string{"Ingress", "Egress"}},
		{Name: "action", Label: "Action on match", Type: "radio", Default: action, Options: []string{"Allow", "Deny"}},
		{Name: "targetTags", Label: "Target tags", Type: "text", Default: join("targetTags"),
			Help: "Optional, comma-separated: the rule applies to instances with one of these tags; empty applies it to all instances in the network."},
		{Name: "sourceRanges", Label: "Source IPv4 ranges", Type: "text", Default: join("sourceRanges"), VisibleWhen: map[string]string{"direction": "Ingress"},
			Help: "Comma-separated CIDR ranges; an ingress rule with no source applies to 0.0.0.0/0."},
		{Name: "sourceTags", Label: "Source tags", Type: "text", Default: join("sourceTags"), VisibleWhen: map[string]string{"direction": "Ingress"}},
		{Name: "destinationRanges", Label: "Destination IPv4 ranges", Type: "text", Default: join("destinationRanges"), VisibleWhen: map[string]string{"direction": "Egress"},
			Help: "Comma-separated CIDR ranges; an egress rule with no destination applies to 0.0.0.0/0."},
		{Name: "protocols", Label: "Protocols and ports", Type: "text", Default: protos,
			Help: "all, or protocols with optional ports, such as tcp:22,80 udp:53 icmp."},
		{Name: "disabled", Label: "Disable rule", Type: "checkbox", Default: disabled},
	}
}

func (computeFirewallsProvider) CreateForm() (string, []console.Field) {
	return "Create firewall rule", firewallFields(nil, nil)
}

func (p computeFirewallsProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return firewallFields(p.networkNames(ctx, project), nil)
}

func firewallBody(values map[string]string) (map[string]any, error) {
	b := map[string]any{"name": strings.TrimSpace(values["name"]), "description": strings.TrimSpace(values["description"]),
		"network":   "global/networks/" + strings.TrimSpace(vpcOr(values["network"], "default")),
		"direction": strings.ToUpper(vpcOr(values["direction"], "Ingress")), "disabled": values["disabled"] == "true"}
	if pr := strings.TrimSpace(values["priority"]); pr != "" {
		n, err := strconv.Atoi(pr)
		if err != nil {
			return nil, fmt.Errorf("priority: %q is not a number", pr)
		}
		b["priority"] = float64(n)
	}
	entries, err := parseProtocols(values["protocols"])
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
	set := func(k string) {
		if l := vpcList(values[k]); len(l) > 0 {
			b[k] = l
		} else {
			b[k] = nil
		}
	}
	set("targetTags")
	if b["direction"] == "EGRESS" {
		set("destinationRanges")
		b["sourceRanges"], b["sourceTags"] = nil, nil
	} else {
		set("sourceRanges")
		set("sourceTags")
		b["destinationRanges"] = nil
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
	for i := range fields {
		if fields[i].Name == "network" {
			fields[i].Immutable = true
			fields[i].Default = vpcLast(vpcStr(f, "network"))
		}
	}
	return console.Detail{Summary: summary,
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Note: computeNote,
			Groups: []console.PropertyGroup{{Properties: []console.Property{{Label: "Created", Value: vpcStr(f, "creationTimestamp")}}}}}},
		Edit: &console.EditForm{Label: "Edit", Fields: fields, Note: "A rule's name and network cannot be changed."}}, nil
}

func (p computeFirewallsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("no firewall rule")
	}
	values["name"] = path[0]
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

func routeFields(networks []string) []console.Field {
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Default: "default", Options: networks}
	if len(networks) == 0 {
		netField.Type = "text"
	}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text"},
		netField,
		{Name: "destRange", Label: "Destination IPv4 range", Type: "text", Required: true, Help: "For example, 192.168.0.0/16. It may not match or fit within a subnet's range."},
		{Name: "priority", Label: "Priority", Type: "text", Default: "1000", Pattern: `^[0-9]{1,5}$`, Help: "0 to 65535; a lower number is preferred."},
		{Name: "tags", Label: "Instance tags", Type: "text", Help: "Optional, comma-separated: the route applies only to instances with one of these tags."},
		{Name: "nextHop", Label: "Next hop", Type: "select", Default: "Default internet gateway",
			Options: []string{"Default internet gateway", "Specify an IP address"}},
		{Name: "nextHopIp", Label: "Next hop IP address", Type: "text", VisibleWhen: map[string]string{"nextHop": "Specify an IP address"},
			Help: "An internal IPv4 address in one of the network's subnets."},
	}
}

func (computeRoutesProvider) CreateForm() (string, []console.Field) {
	return "Create route", routeFields(nil)
}

func (p computeRoutesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return routeFields(p.networkNames(ctx, project))
}

func (p computeRoutesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	b := map[string]any{"name": name, "network": "global/networks/" + strings.TrimSpace(vpcOr(values["network"], "default")),
		"destRange": strings.TrimSpace(values["destRange"])}
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
	if values["nextHop"] == "Specify an IP address" {
		b["nextHopIp"] = strings.TrimSpace(values["nextHopIp"])
	} else {
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
	listing := console.Listing{Columns: []string{"IP address", "Access type", "Region", "Type", "Version", "Purpose", "Network", "Subnetwork", "Network tier", "In use by"},
		Noun: "IP addresses", Note: computeNote}
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
	for _, scope := range scopes {
		sv, _ := items[scope].(map[string]any)
		l, _ := sv["addresses"].([]any)
		for _, e := range l {
			a, _ := e.(map[string]any)
			region := "global"
			if strings.HasPrefix(scope, "regions/") {
				region = strings.TrimPrefix(scope, "regions/")
			}
			ip := vpcStr(a, "address")
			if pl := vpcStr(a, "prefixLength"); pl != "" && pl != "0" {
				ip += "/" + pl
			}
			access := "External"
			if vpcStr(a, "addressType") == "INTERNAL" {
				access = "Internal"
			}
			target := []string{region, vpcStr(a, "name")}
			listing.Items = append(listing.Items, console.Resource{Name: vpcStr(a, "name"), Status: vpcTitle(vpcStr(a, "status")), Target: target,
				Actions: p.addressActions(a),
				Fields: map[string]string{"IP address": ip, "Access type": access, "Region": region, "Type": "Static", "Version": "IPv4",
					"Purpose": vpcDash(vpcStr(a, "purpose")), "Network": vpcDash(vpcLast(vpcStr(a, "network"))), "Subnetwork": vpcDash(vpcLast(vpcStr(a, "subnetwork"))),
					"Network tier": vpcDash(vpcTitle(vpcStr(a, "networkTier"))), "In use by": vpcDash(strings.Join(vpcStrs(a, "users"), ", "))}})
		}
	}
	listing.Total = len(listing.Items)
	listing.AlwaysStatus = true
	return listing, nil
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
		}},
		{ID: "reserve-internal", Label: "Reserve internal static IP address", Submit: "Reserve", Fields: []console.Field{name, desc,
			netField, computeRegionField("region"),
			{Name: "subnetwork", Label: "Subnetwork", Type: "text", Required: true, Default: "default", Help: "The subnet, in the region, the address is in."},
			{Name: "address", Label: "Static IP address", Type: "text",
				Help: "Optional: an address in the subnet's primary range, not one of the four it keeps. Empty assigns one automatically."},
			{Name: "purpose", Label: "Purpose", Type: "select", Default: "Non-shared", Options: []string{"Non-shared", "Shared"}},
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
		_, err := p.call("POST", project, "regions/"+region+"/addresses", nil, b)
		return err
	case "reserve-internal":
		b["addressType"] = "INTERNAL"
		b["subnetwork"] = "regions/" + region + "/subnetworks/" + strings.TrimSpace(vpcOr(values["subnetwork"], "default"))
		if a := strings.TrimSpace(values["address"]); a != "" {
			b["address"] = a
		}
		if values["purpose"] == "Shared" {
			b["purpose"] = "SHARED_LOADBALANCER_VIP"
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
