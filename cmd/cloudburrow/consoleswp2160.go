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
)

// Secure Web Proxy (#2160), Network Security's pages, as
// https://docs.cloud.google.com/secure-web-proxy/docs/quickstart gives
// their console steps (read 2026-11-22):
//
//   - Web Proxies (/net-security/swp/web-proxies/list): Create a secure web
//     proxy with Name, Description, Routing mode (Explicit), Region,
//     Network, Subnetwork, Web proxy IP address, Ports, Certificate and
//     Associated policy;
//   - SWP Policies (/net-security/swp/policies/list): Create a policy with
//     Name, Description and Region; a policy's page lists its rules and
//     has Add rule with Priority, Name, Description, Action (Allow or
//     Deny), Status (Enabled or Disabled), Session Match and Application
//     Match;
//   - URL lists and TLS inspection policies, which Google's docs give as
//     gcloud steps only: their paths, columns and forms are UNVERIFIED
//     (re-read 2026-11-22: secure-web-proxy/docs/url-list-syntax-reference,
//     tls-inspection-overview).

const swpNote = "A secure web proxy here is a forward proxy on this machine: point a client at the address in Listening at (curl -x ADDRESS https://...). " +
	"It applies its policy's rules to every request, lowest priority first, and denies what no rule allows. It never connects to Google."

type swpScreen struct{ ccmScreen }

// swpList is a regional collection's items in every location.
func (s swpScreen) swpList(ctx context.Context, project, coll string) ([]map[string]any, error) {
	return s.ccmList(ctx, project, coll)
}

func swpRegionField() console.Field {
	return console.Field{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: computeRegionField("").Options}
}

func swpNameField() console.Field {
	return console.Field{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[a-z]([\-a-z0-9]{0,61}[a-z0-9])?$`,
		Help: "1 to 63 lowercase letters, digits and hyphens, starting with a letter."}
}

func swpDelete(noun string, path []string) console.Action {
	a := ccmDelete(noun, path)
	return a
}

// --- Web proxies -----------------------------------------------------------

type swpProxiesProvider struct{ swpScreen }

func (swpProxiesProvider) ID() string    { return "netsecurity-swp-proxies" }
func (swpProxiesProvider) Title() string { return "Web Proxies" }

func (p swpProxiesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "IP address", "Ports", "Associated policy", "Network", "Listening at"}, NameColumn: "Name",
		Noun: "web proxies", Note: swpNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Web proxies are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.swpList(ctx, project, "gateways")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		if vpcStr(o, "type") != "SECURE_WEB_GATEWAY" {
			continue
		}
		path := ccmPath(vpcStr(o, "name"))
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path, Fields: swpProxyFields(o, path),
			Actions: []console.Action{swpDelete("web proxy", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func swpPorts(o map[string]any) string {
	if o["allPorts"] == true {
		return "All ports"
	}
	var ps []string
	for _, v := range listAny(o["ports"]) {
		ps = append(ps, fmt.Sprint(v))
	}
	return vpcDash(strings.Join(ps, ", "))
}

func swpProxyFields(o map[string]any, path []string) map[string]string {
	var at []string
	for _, a := range listAny(o["cloudburrowAddresses"]) {
		at = append(at, strAny(a))
	}
	return map[string]string{"Region": path[0], "IP address": vpcDash(strings.Join(vpcStrs(o, "addresses"), ", ")), "Ports": swpPorts(o),
		"Associated policy": vpcDash(vpcLast(vpcStr(o, "gatewaySecurityPolicy"))), "Network": vpcDash(vpcLast(vpcStr(o, "network"))),
		"Listening at": vpcDash(strings.Join(at, ", "))}
}

func (swpProxiesProvider) CreateOnPage() bool   { return true }
func (swpProxiesProvider) CreateTitle() string  { return "Create a secure web proxy" }
func (swpProxiesProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p swpProxiesProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{
		swpNameField(),
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
		{Name: "routingMode", Label: "Routing mode", Type: "radio", Default: "EXPLICIT_ROUTING_MODE", Options: []string{"EXPLICIT_ROUTING_MODE", "NEXT_HOP_ROUTING_MODE"},
			OptionLabels: map[string]string{"EXPLICIT_ROUTING_MODE": "Explicit", "NEXT_HOP_ROUTING_MODE": "Next hop"},
			Help:         "Explicit: clients send their requests to the proxy's address. Next hop routes nothing to the proxy on this machine (docs/gaps.md)."},
		swpRegionField(),
		{Name: "network", Label: "Network", Type: "text", Required: true, Default: "default", Help: "The VPC network; it needs a proxy-only subnet in the region."},
		{Name: "subnetwork", Label: "Subnetwork", Type: "text", Required: true, Default: "default", Help: "The subnetwork in the region the proxy's address comes from."},
		{Name: "address", Label: "Web proxy IP address", Type: "text", Help: "Optional: an address of the subnetwork; one is allocated when empty."},
		{Name: "ports", Label: "Ports", Type: "text", Required: true, Default: "443", Help: "Up to five ports, comma separated; they cannot be changed later."},
		{Name: "certificate", Label: "Certificate", Type: "text", Help: "Optional: a Certificate Manager certificate in the region, for clients that connect to the proxy over HTTPS."},
		{Name: "policy", Label: "Associated policy", Type: "text", Help: "The SWP policy (gateway security policy) in the region, by name."},
	}
}

func (p swpProxiesProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	region := vpcOr(v["region"], "us-central1")
	body := map[string]any{"type": "SECURE_WEB_GATEWAY", "routingMode": vpcOr(v["routingMode"], "EXPLICIT_ROUTING_MODE"),
		"network":    "projects/" + project + "/global/networks/" + vpcOr(strings.TrimSpace(v["network"]), "default"),
		"subnetwork": "projects/" + project + "/regions/" + region + "/subnetworks/" + vpcOr(strings.TrimSpace(v["subnetwork"]), "default")}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if a := strings.TrimSpace(v["address"]); a != "" {
		body["addresses"] = []string{a}
	}
	var ports []int
	for _, f := range strings.Split(v["ports"], ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return "", fmt.Errorf("Ports: %q is not a port number", f)
		}
		ports = append(ports, n)
	}
	body["ports"] = ports
	if c := strings.TrimSpace(v["certificate"]); c != "" {
		body["certificateUrls"] = []string{"projects/" + project + "/locations/" + region + "/certificates/" + vpcLast(c)}
	}
	if pol := strings.TrimSpace(v["policy"]); pol != "" {
		body["gatewaySecurityPolicy"] = "projects/" + project + "/locations/" + region + "/gatewaySecurityPolicies/" + vpcLast(pol)
	}
	if err := p.ccmCreate(ctx, project, region, "gateways", "gatewayId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p swpProxiesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a web proxy is addressed by its region and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "gateways"), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	f := swpProxyFields(o, path)
	mode := map[string]string{"EXPLICIT_ROUTING_MODE": "Explicit", "NEXT_HOP_ROUTING_MODE": "Next hop"}[vpcOr(vpcStr(o, "routingMode"), "EXPLICIT_ROUTING_MODE")]
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Routing mode", Value: mode}, {Label: "Region", Value: path[0]}, {Label: "Network", Value: f["Network"]},
		{Label: "Subnetwork", Value: vpcDash(vpcLast(vpcStr(o, "subnetwork")))}, {Label: "Web proxy IP address", Value: f["IP address"]},
		{Label: "Ports", Value: f["Ports"]}, {Label: "Certificate", Value: vpcDash(strings.Join(lastAllAny(listAny(o["certificateUrls"])), ", "))},
		{Label: "Associated policy", Value: f["Associated policy"]}, {Label: "Listening at", Value: f["Listening at"]},
		{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	return console.Detail{Title: path[1], Heading: "Web proxy details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit web proxy", Submit: "Save", Page: true, Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description"), Help: "Optional."},
			{Name: "policy", Label: "Associated policy", Type: "text", Default: vpcLast(vpcStr(o, "gatewaySecurityPolicy"))}}}}, nil
}

func (p swpProxiesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := swpDelete("web proxy", path)
	a.Leaves = true
	return []console.Action{a}
}

func (p swpProxiesProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to a web proxy")
	}
	body := map[string]any{"description": strings.TrimSpace(v["description"])}
	if pol := strings.TrimSpace(v["policy"]); pol != "" {
		body["gatewaySecurityPolicy"] = "projects/" + project + "/locations/" + path[0] + "/gatewaySecurityPolicies/" + vpcLast(pol)
	}
	_, err := p.v1(ctx, "PATCH", "/v1/"+ccmName(project, path, "gateways")+"?updateMask=description,gatewaySecurityPolicy", body)
	return err
}

func (p swpProxiesProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, "gateways"), nil)
	return err
}

// --- SWP policies and their rules ------------------------------------------

type swpPoliciesProvider struct{ swpScreen }

func (swpPoliciesProvider) ID() string    { return "netsecurity-swp-policies" }
func (swpPoliciesProvider) Title() string { return "SWP Policies" }

func (p swpPoliciesProvider) rules(ctx context.Context, project string, path []string) ([]map[string]any, error) {
	m, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "gatewaySecurityPolicies")+"/rules?pageSize=1000", nil)
	if err != nil {
		return nil, err
	}
	rs := vmMaps(m, "gatewaySecurityPolicyRules")
	sort.SliceStable(rs, func(i, j int) bool {
		a, _ := rs[i]["priority"].(float64)
		b, _ := rs[j]["priority"].(float64)
		return a < b
	})
	return rs, nil
}

func (p swpPoliciesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "Description", "Rules", "TLS inspection policy"}, NameColumn: "Name", Noun: "policies", Note: swpNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Policies are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.swpList(ctx, project, "gatewaySecurityPolicies")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		rs, _ := p.rules(ctx, project, path)
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path,
			Fields: map[string]string{"Region": path[0], "Description": vpcDash(vpcStr(o, "description")), "Rules": strconv.Itoa(len(rs)),
				"TLS inspection policy": vpcDash(vpcLast(vpcStr(o, "tlsInspectionPolicy")))},
			Actions: []console.Action{swpDelete("policy", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (swpPoliciesProvider) CreateOnPage() bool   { return true }
func (swpPoliciesProvider) CreateTitle() string  { return "Create a policy" }
func (swpPoliciesProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p swpPoliciesProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{swpNameField(), {Name: "description", Label: "Description", Type: "text", Help: "Optional."}, swpRegionField(),
		{Name: "tlsInspectionPolicy", Label: "TLS inspection policy", Type: "text", Help: "Optional: a TLS inspection policy in the region, by name."}}
}

func (p swpPoliciesProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name, region := strings.TrimSpace(v["name"]), vpcOr(v["region"], "us-central1")
	body := map[string]any{}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if t := strings.TrimSpace(v["tlsInspectionPolicy"]); t != "" {
		body["tlsInspectionPolicy"] = "projects/" + project + "/locations/" + region + "/tlsInspectionPolicies/" + vpcLast(t)
	}
	if err := p.ccmCreate(ctx, project, region, "gatewaySecurityPolicies", "gatewaySecurityPolicyId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

// swpRuleFields are Add rule's inputs, in the quickstart's order.
func swpRuleFields(r map[string]any, create bool) []console.Field {
	enabled := "true"
	if r != nil && r["enabled"] != true {
		enabled = "false"
	}
	fields := []console.Field{
		{Name: "priority", Label: "Priority", Type: "number", Required: true, Default: num2s(r, "priority"),
			Help: "Rules are evaluated from highest to lowest priority, where 0 is the highest priority."},
	}
	if create {
		fields = append(fields, swpNameField())
	}
	tls := "false"
	if r != nil && r["tlsInspectionEnabled"] == true {
		tls = "true"
	}
	return append(fields,
		console.Field{Name: "description", Label: "Description", Type: "text", Default: vpcStr(r, "description"), Help: "Optional."},
		console.Field{Name: "action", Label: "Action", Type: "radio", Default: vpcOr(vpcStr(r, "basicProfile"), "ALLOW"), Options: []string{"ALLOW", "DENY"},
			OptionLabels: map[string]string{"ALLOW": "Allow", "DENY": "Deny"}},
		console.Field{Name: "enabled", Label: "Status", Type: "radio", Default: enabled, Options: []string{"true", "false"},
			OptionLabels: map[string]string{"true": "Enabled", "false": "Disabled"}},
		console.Field{Name: "sessionMatcher", Label: "Session Match", Type: "textarea", Required: true, Default: vpcStr(r, "sessionMatcher"),
			Help: "A CEL expression on the connection, such as host() == 'www.example.com'."},
		console.Field{Name: "applicationMatcher", Label: "Application Match", Type: "textarea", Default: vpcStr(r, "applicationMatcher"),
			Help: "Optional: a CEL expression on the request, such as request.path.contains('index.html')."},
		console.Field{Name: "tlsInspectionEnabled", Label: "TLS inspection", Type: "radio", Default: tls, Options: []string{"false", "true"},
			OptionLabels: map[string]string{"false": "Disabled", "true": "Enabled"},
			Help:         "Only under a policy with a TLS inspection policy. Tunnels are not inspected on this machine (docs/gaps.md)."})
}

func swpRuleBody(v map[string]string) (map[string]any, error) {
	pri, err := strconv.Atoi(strings.TrimSpace(v["priority"]))
	if err != nil {
		return nil, errors.New("Priority: give a number")
	}
	return map[string]any{"priority": pri, "description": strings.TrimSpace(v["description"]), "basicProfile": vpcOr(v["action"], "ALLOW"),
		"enabled": v["enabled"] != "false", "sessionMatcher": strings.TrimSpace(v["sessionMatcher"]),
		"applicationMatcher": strings.TrimSpace(v["applicationMatcher"]), "tlsInspectionEnabled": v["tlsInspectionEnabled"] == "true"}, nil
}

func (p swpPoliciesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a policy is addressed by its region and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "gatewaySecurityPolicies"), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	rs, err := p.rules(ctx, project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	rules := console.Listing{Columns: []string{"Priority", "Action", "Status", "Session Match", "Application Match", "Description"}, NameColumn: "Name",
		Noun: "rules", Items: []console.Resource{}}
	for _, r := range rs {
		id := vpcLast(vpcStr(r, "name"))
		status := "Disabled"
		if r["enabled"] == true {
			status = "Enabled"
		}
		rules.Items = append(rules.Items, console.Resource{Name: id, Target: []string{path[0], path[1], "rule", id},
			Fields: map[string]string{"Priority": num2s(r, "priority"), "Action": vpcTitle(strings.ToLower(vpcStr(r, "basicProfile"))), "Status": status,
				"Session Match": vpcDash(vpcStr(r, "sessionMatcher")), "Application Match": vpcDash(vpcStr(r, "applicationMatcher")),
				"Description": vpcDash(vpcStr(r, "description"))},
			Actions: []console.Action{{ID: "editrule", Label: "Edit", Title: "Edit rule", Submit: "Save", Fields: swpRuleFields(r, false)},
				{ID: "deleterule", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, ConfirmTitle: "Delete rule", Confirm: "Delete the rule " + id + "?"}}})
	}
	rules.Total = len(rules.Items)
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Region", Value: path[0]}, {Label: "TLS inspection policy", Value: vpcDash(vpcLast(vpcStr(o, "tlsInspectionPolicy")))},
		{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	return console.Detail{Title: path[1], Heading: "Policy details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "rules", Label: "Rules", Listing: rules, Actions: []console.Action{swpAddRule()},
				Note: "Rules are evaluated from highest to lowest priority, where 0 is the highest priority; a request no rule allows is denied."}},
		OpenTab: "rules",
		Edit: &console.EditForm{Label: "Edit", Title: "Edit policy", Submit: "Save", Page: true, Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description"), Help: "Optional."}}}}, nil
}

func swpAddRule() console.Action {
	return console.Action{ID: "addrule", Label: "Add rule", Title: "Add rule", Submit: "Create", Fields: swpRuleFields(nil, true)}
}

func (p swpPoliciesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := swpDelete("policy", path)
	a.Leaves = true
	return []console.Action{swpAddRule(), a}
}

func (p swpPoliciesProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to a policy")
	}
	_, err := p.v1(ctx, "PATCH", "/v1/"+ccmName(project, path, "gatewaySecurityPolicies")+"?updateMask=description",
		map[string]any{"description": strings.TrimSpace(v["description"])})
	return err
}

func (p swpPoliciesProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) < 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := "/v1/" + ccmName(project, path[:2], "gatewaySecurityPolicies")
	switch {
	case action == "delete" && len(path) == 2:
		_, err := p.v1(ctx, "DELETE", base, nil)
		return err
	case action == "addrule" && len(path) == 2:
		body, err := swpRuleBody(v)
		if err != nil {
			return err
		}
		name := strings.TrimSpace(v["name"])
		if name == "" {
			return errors.New("Name: give one")
		}
		_, err = p.v1(ctx, "POST", base+"/rules?gatewaySecurityPolicyRuleId="+url.QueryEscape(name), body)
		return err
	case action == "editrule" && len(path) == 4:
		body, err := swpRuleBody(v)
		if err != nil {
			return err
		}
		_, err = p.v1(ctx, "PATCH", base+"/rules/"+url.PathEscape(path[3])+
			"?updateMask=priority,description,basicProfile,enabled,sessionMatcher,applicationMatcher,tlsInspectionEnabled", body)
		return err
	case action == "deleterule" && len(path) == 4:
		_, err := p.v1(ctx, "DELETE", base+"/rules/"+url.PathEscape(path[3]), nil)
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}

// --- URL lists -------------------------------------------------------------

type swpURLListsProvider struct{ swpScreen }

func (swpURLListsProvider) ID() string    { return "netsecurity-swp-urllists" }
func (swpURLListsProvider) Title() string { return "URL lists" }

func (p swpURLListsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "Description", "Entries"}, NameColumn: "Name", Noun: "URL lists",
		Note: "A rule matches a URL list with inUrlList(host(), 'projects/P/locations/REGION/urlLists/NAME') or inUrlList(request.url(), ...)."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "URL lists are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.swpList(ctx, project, "urlLists")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path,
			Fields:  map[string]string{"Region": path[0], "Description": vpcDash(vpcStr(o, "description")), "Entries": strconv.Itoa(len(vpcStrs(o, "values")))},
			Actions: []console.Action{swpDelete("URL list", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (swpURLListsProvider) CreateOnPage() bool   { return true }
func (swpURLListsProvider) CreateTitle() string  { return "Create a URL list" }
func (swpURLListsProvider) CreateLayout() string { return console.CreateLayoutPage }

func swpValuesField(def string) console.Field {
	return console.Field{Name: "values", Label: "URL list", Type: "textarea", Required: true, Default: def,
		Help: "One entry a line, at most 2,500: a domain (example.com, *.example.com) or a URL without its scheme (example.com/path/*)."}
}

func (p swpURLListsProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{swpNameField(), {Name: "description", Label: "Description", Type: "text", Help: "Optional."}, swpRegionField(), swpValuesField("")}
}

func swpValues(s string) []string {
	var out []string
	for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (p swpURLListsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name, region := strings.TrimSpace(v["name"]), vpcOr(v["region"], "us-central1")
	body := map[string]any{"values": swpValues(v["values"])}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if err := p.ccmCreate(ctx, project, region, "urlLists", "urlListId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p swpURLListsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a URL list is addressed by its region and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "urlLists"), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	vals := strings.Join(vpcStrs(o, "values"), "\n")
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Region", Value: path[0]}, {Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	return console.Detail{Title: path[1], Heading: "URL list details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "entries", Label: "Entries", Kind: console.KindText, Text: vals}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit URL list", Submit: "Save", Page: true, Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: vpcStr(o, "description"), Help: "Optional."}, swpValuesField(vals)}}}, nil
}

func (p swpURLListsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := swpDelete("URL list", path)
	a.Leaves = true
	return []console.Action{a}
}

func (p swpURLListsProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 2 {
		return errors.New("Edit applies to a URL list")
	}
	_, err := p.v1(ctx, "PATCH", "/v1/"+ccmName(project, path, "urlLists")+"?updateMask=description,values",
		map[string]any{"description": strings.TrimSpace(v["description"]), "values": swpValues(v["values"])})
	return err
}

func (p swpURLListsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, "urlLists"), nil)
	return err
}

// --- TLS inspection policies -------------------------------------------------

type swpTLSProvider struct{ swpScreen }

func (swpTLSProvider) ID() string    { return "netsecurity-swp-tls" }
func (swpTLSProvider) Title() string { return "TLS inspection policies" }

func (p swpTLSProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Region", "CA pool", "Trust config", "Description"}, NameColumn: "Name", Noun: "TLS inspection policies",
		Note: "A TLS inspection policy names the CA pool that issues interception certificates. This instance keeps it, but inspects no tunnel (docs/gaps.md)."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "TLS inspection policies are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.swpList(ctx, project, "tlsInspectionPolicies")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path,
			Fields: map[string]string{"Region": path[0], "CA pool": vpcDash(vpcLast(vpcStr(o, "caPool"))), "Trust config": vpcDash(vpcLast(vpcStr(o, "trustConfig"))),
				"Description": vpcDash(vpcStr(o, "description"))},
			Actions: []console.Action{swpDelete("TLS inspection policy", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (swpTLSProvider) CreateOnPage() bool   { return true }
func (swpTLSProvider) CreateTitle() string  { return "Create a TLS inspection policy" }
func (swpTLSProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p swpTLSProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{swpNameField(), {Name: "description", Label: "Description", Type: "text", Help: "Optional."}, swpRegionField(),
		{Name: "caPool", Label: "CA pool", Type: "text", Required: true, Help: "A Certificate Authority Service CA pool in the region, by name."},
		{Name: "trustConfig", Label: "Trust config", Type: "text", Help: "Optional: a Certificate Manager trust config in the region, by name."}}
}

func (p swpTLSProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name, region := strings.TrimSpace(v["name"]), vpcOr(v["region"], "us-central1")
	body := map[string]any{"caPool": "projects/" + project + "/locations/" + region + "/caPools/" + vpcLast(strings.TrimSpace(v["caPool"]))}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if t := strings.TrimSpace(v["trustConfig"]); t != "" {
		body["trustConfig"] = "projects/" + project + "/locations/" + region + "/trustConfigs/" + vpcLast(t)
	}
	if err := p.ccmCreate(ctx, project, region, "tlsInspectionPolicies", "tlsInspectionPolicyId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p swpTLSProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a TLS inspection policy is addressed by its region and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "tlsInspectionPolicies"), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Region", Value: path[0]}, {Label: "CA pool", Value: vpcDash(vpcStr(o, "caPool"))}, {Label: "Trust config", Value: vpcDash(vpcStr(o, "trustConfig"))},
		{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	return console.Detail{Title: path[1], Heading: "TLS inspection policy details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}}}, nil
}

func (p swpTLSProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := swpDelete("TLS inspection policy", path)
	a.Leaves = true
	return []console.Action{a}
}

func (p swpTLSProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, "tlsInspectionPolicies"), nil)
	return err
}
