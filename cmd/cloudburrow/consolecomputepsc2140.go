package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Private Service Connect (#2140), over the Compute Engine API's
// globalForwardingRules, forwardingRules and serviceAttachments: the
// Private Service Connect page's Connected endpoints and Published
// services tabs (vpc/docs/configure-private-service-connect-apis,
// configure-private-service-connect-services and
// configure-private-service-connect-producer, read 2026-11-22). The two
// lists' columns are not named in the docs (UNVERIFIED).

const pscNote = "An endpoint for Google APIs (All Google APIs or VPC-SC) holds a PRIVATE_SERVICE_CONNECT address of its network; an endpoint for a published service connects to a service attachment, accepted as the attachment's connection preference decides. " +
	"Endpoints and attachments are records: no internal load balancer is served, so no traffic reaches a published service through them, and VMs reach the local Google APIs whatever endpoint exists."

func pscStatusText(s string) string {
	switch s {
	case "":
		return "-"
	case "ACCEPTED":
		return "Accepted"
	case "PENDING":
		return "Pending"
	case "REJECTED":
		return "Rejected"
	case "CLOSED":
		return "Closed"
	}
	return vpcTitle(strings.ToLower(s))
}

func pscTargetText(t string) string {
	switch t {
	case "all-apis":
		return "All Google APIs (all-apis)"
	case "vpc-sc":
		return "VPC-SC (vpc-sc)"
	}
	return t
}

// --- Connected endpoints ------------------------------------------------------

// computePSCEndpointsProvider is the Connected endpoints tab: every
// endpoint of the project, global (Google APIs) and regional (published
// services), Connect endpoint, an endpoint's page and Delete (an endpoint
// cannot be updated, as Google's cannot).
type computePSCEndpointsProvider struct{ computeScreen }

func (computePSCEndpointsProvider) ID() string    { return "netservices-psc-endpoints" }
func (computePSCEndpointsProvider) Title() string { return "Private Service Connect" }

func pscEndpointColumns() []string {
	return []string{"Target", "Scope", "Network", "IP address", "Status", "Connection ID"}
}

func (p computePSCEndpointsProvider) endpoints(project string) []map[string]any {
	var out []map[string]any
	if l, err := p.list(project, "global/forwardingRules"); err == nil {
		out = append(out, l...)
	}
	if m, err := p.call("GET", project, "aggregated/forwardingRules", nil, nil); err == nil {
		out = append(out, aggregatedItems(m, "forwardingRules")...)
	}
	sort.Slice(out, func(i, j int) bool { return vpcStr(out[i], "name") < vpcStr(out[j], "name") })
	return out
}

func pscEndpointRow(e map[string]any) console.Resource {
	scope := "global"
	if r := vpcLast(vpcStr(e, "region")); r != "" {
		scope = r
	}
	target := []string{scope, vpcStr(e, "name")}
	st := pscStatusText(vpcStr(e, "pscConnectionStatus"))
	return console.Resource{Name: vpcStr(e, "name"), Status: st, Opens: target, Target: target,
		Fields: map[string]string{"Target": pscTargetText(vpcLast(vpcStr(e, "target"))), "Scope": map[bool]string{true: "Global", false: scope}[scope == "global"],
			"Network": vpcLast(vpcStr(e, "network")), "IP address": vpcStr(e, "IPAddress"), "Status": st,
			"Connection ID": vpcDash(vpcStr(e, "pscConnectionId"))},
		Actions: []console.Action{pscEndpointDelete(target)}}
}

func pscEndpointDelete(path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, Confirm: "Delete endpoint " + path[1] + "?"}
}

func (p computePSCEndpointsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: pscEndpointColumns(), NameColumn: "Endpoint", Noun: "endpoints", AlwaysStatus: true, Note: pscNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Endpoints are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, e := range p.endpoints(project) {
		listing.Items = append(listing.Items, pscEndpointRow(e))
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computePSCEndpointsProvider) CreateOnPage() bool  { return true }
func (computePSCEndpointsProvider) CreateTitle() string { return "Connect endpoint" }

func (computePSCEndpointsProvider) CreateForm() (string, []console.Field) {
	return "Connect endpoint", pscEndpointFields(nil)
}

func (p computePSCEndpointsProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return pscEndpointFields(p.networkNames(ctx, project))
}

// pscEndpointFields are Google's Connect endpoint's: Target (All Google
// APIs, VPC-SC or Published service and its Target service), Endpoint
// name, Network, the published service's region and subnetwork, and IP
// Address (an address, reserved here as Create IP address reserves it when
// it is not yet). Service Directory region and namespace are unavailable:
// Service Directory is not served.
func pscEndpointFields(networks []string) []console.Field {
	netField := console.Field{Name: "network", Label: "Network", Type: "select", Required: true, Default: "default", Options: networks}
	if networks == nil {
		netField.Type = "text"
	}
	published := map[string]string{"target": "published"}
	return []console.Field{
		{Name: "target", Label: "Target", Type: "radio", Default: "all-apis", Options: []string{"all-apis", "vpc-sc", "published"},
			OptionLabels: map[string]string{"all-apis": "All Google APIs", "vpc-sc": "VPC-SC", "published": "Published service"}},
		{Name: "targetService", Label: "Target service", Type: "text", Required: true, VisibleWhen: published,
			Help: "The service attachment's URI: projects/PROJECT/regions/REGION/serviceAttachments/NAME."},
		{Name: "name", Label: "Endpoint name", Type: "text", Required: true,
			Help: "For Google APIs: 1-20 lowercase letters and numbers, starting with a letter. For a published service: lowercase letters, numbers and hyphens."},
		netField,
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionField("").Options, VisibleWhen: published},
		{Name: "subnetwork", Label: "Subnetwork", Type: "text", Default: "default", VisibleWhen: published,
			Help: "The subnet of the network the endpoint's address is chosen in, when IP address is empty."},
		{Name: "ipAddress", Label: "IP address", Type: "text",
			Help: "For Google APIs: an address outside the network's subnets, such as 10.255.255.254, or the name of a reserved PRIVATE_SERVICE_CONNECT address; a new address is reserved for it (Create IP address). For a published service: a reserved internal address's name, or empty to choose one."},
		{Name: "serviceDirectoryRegion", Label: "Service Directory region", Type: "text",
			Help: "Service Directory is not served: endpoints get no Service Directory namespace."},
	}
}

func (p computePSCEndpointsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name := strings.TrimSpace(values["name"])
	network := "projects/" + project + "/global/networks/" + vpcOr(strings.TrimSpace(values["network"]), "default")
	ip := strings.TrimSpace(values["ipAddress"])
	rule := map[string]any{"name": name, "network": network}
	if values["target"] == "published" {
		region := vpcOr(values["region"], "us-central1")
		rule["target"] = strings.TrimSpace(values["targetService"])
		if ip != "" {
			rule["IPAddress"] = "regions/" + region + "/addresses/" + ip
		} else {
			rule["subnetwork"] = "regions/" + region + "/subnetworks/" + vpcOr(strings.TrimSpace(values["subnetwork"]), "default")
		}
		if _, err := p.call("POST", project, "regions/"+region+"/forwardingRules", nil, rule); err != nil {
			return "", err
		}
		return name, nil
	}
	rule["target"] = vpcOr(values["target"], "all-apis")
	if ip == "" {
		return "", errors.New("IP address: give the endpoint's address")
	}
	if net.ParseIP(ip) != nil {
		// Create IP address: reserve it when no address holds it yet.
		if l, err := p.list(project, "global/addresses"); err == nil {
			found := ""
			for _, a := range l {
				if vpcStr(a, "address") == ip {
					found = vpcStr(a, "name")
				}
			}
			if found == "" {
				found = name + "-ip"
				if _, err := p.call("POST", project, "global/addresses", nil, map[string]any{"name": found, "addressType": "INTERNAL",
					"purpose": "PRIVATE_SERVICE_CONNECT", "address": ip, "network": network}); err != nil {
					return "", err
				}
			}
			ip = found
		}
	}
	rule["IPAddress"] = "global/addresses/" + ip
	if _, err := p.call("POST", project, "global/forwardingRules", nil, rule); err != nil {
		return "", err
	}
	return name, nil
}

func (p computePSCEndpointsProvider) endpoint(project string, path []string) (map[string]any, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("an endpoint is addressed by its scope and name, not %v", path)
	}
	if path[0] == "global" {
		return p.call("GET", project, "global/forwardingRules/"+path[1], nil, nil)
	}
	return p.call("GET", project, "regions/"+path[0]+"/forwardingRules/"+path[1], nil, nil)
}

func (p computePSCEndpointsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	e, err := p.endpoint(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	row := pscEndpointRow(e)
	props := []console.Property{{Label: "Endpoint", Value: vpcStr(e, "name")}}
	for _, c := range pscEndpointColumns() {
		props = append(props, console.Property{Label: c, Value: row.Fields[c]})
	}
	props = append(props, console.Property{Label: "Subnetwork", Value: vpcDash(vpcLast(vpcStr(e, "subnetwork")))},
		console.Property{Label: "Description", Value: vpcDash(vpcStr(e, "description"))})
	return console.Detail{Title: path[1], Heading: "Endpoint details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Properties: props}}}}}, nil
}

func (p computePSCEndpointsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	if _, err := p.endpoint(project, path); err != nil {
		return nil
	}
	a := pscEndpointDelete(path)
	a.Leaves = true
	return []console.Action{a}
}

func (p computePSCEndpointsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	coll := "regions/" + path[0] + "/forwardingRules/"
	if path[0] == "global" {
		coll = "global/forwardingRules/"
	}
	_, err := p.call("DELETE", project, coll+path[1], nil, nil)
	return err
}

// --- Published services --------------------------------------------------------

// computePSCPublishedProvider is the Published services tab: the
// project's service attachments, Publish service, a service's page with
// its connected endpoints and Edit of the accepted and rejected projects,
// and Delete.
type computePSCPublishedProvider struct{ computeScreen }

func (computePSCPublishedProvider) ID() string    { return "netservices-psc-published" }
func (computePSCPublishedProvider) Title() string { return "Private Service Connect" }

func pscPublishedColumns() []string {
	return []string{"Region", "Target service", "Connection preference", "Connected endpoints", "Service attachment"}
}

func pscPreferenceText(s string) string {
	if s == "ACCEPT_MANUAL" {
		return "Accept connections for selected projects"
	}
	return "Automatically accept all connections"
}

func pscPublishedRow(a map[string]any) console.Resource {
	region := vpcLast(vpcStr(a, "region"))
	target := []string{region, vpcStr(a, "name")}
	return console.Resource{Name: vpcStr(a, "name"), Opens: target, Target: target,
		Fields: map[string]string{"Region": region, "Target service": vpcLast(vpcStr(a, "targetService")),
			"Connection preference": pscPreferenceText(vpcStr(a, "connectionPreference")),
			"Connected endpoints":   strconv.Itoa(len(vmMaps(a, "connectedEndpoints"))),
			"Service attachment":    pscRel(vpcStr(a, "selfLink"))},
		Actions: []console.Action{{ID: "delete", Label: "Delete", Destructive: true, Confirm: "Delete published service " + target[1] + "?"}}}
}

func (p computePSCPublishedProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: pscPublishedColumns(), NameColumn: "Service", Noun: "published services", Note: pscNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Published services are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	m, err := p.call("GET", project, "aggregated/serviceAttachments", nil, nil)
	if err != nil {
		return listing, err
	}
	atts := aggregatedItems(m, "serviceAttachments")
	sort.Slice(atts, func(i, j int) bool { return vpcStr(atts[i], "name") < vpcStr(atts[j], "name") })
	for _, a := range atts {
		// The list's items lack connectedEndpoints: read each.
		if full, err := p.call("GET", project, "regions/"+vpcLast(vpcStr(a, "region"))+"/serviceAttachments/"+vpcStr(a, "name"), nil, nil); err == nil {
			a = full
		}
		listing.Items = append(listing.Items, pscPublishedRow(a))
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (computePSCPublishedProvider) CreateOnPage() bool  { return true }
func (computePSCPublishedProvider) CreateTitle() string { return "Publish service" }

func (computePSCPublishedProvider) CreateForm() (string, []console.Field) {
	return "Add service", pscPublishFields(nil)
}

func (p computePSCPublishedProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return pscPublishFields(p.pscSubnets(project))
}

// pscSubnets are the project's subnets of purpose PRIVATE_SERVICE_CONNECT,
// as region/name.
func (p computePSCPublishedProvider) pscSubnets(project string) []string {
	m, err := p.call("GET", project, "aggregated/subnetworks", url.Values{"filter": {`purpose = "PRIVATE_SERVICE_CONNECT"`}}, nil)
	if err != nil {
		return []string{}
	}
	out := []string{}
	for _, s := range aggregatedItems(m, "subnetworks") {
		out = append(out, vpcLast(vpcStr(s, "region"))+"/"+vpcStr(s, "name"))
	}
	sort.Strings(out)
	return out
}

// pscAcceptFields are the consumer settings: Accept connections for
// selected projects' accepted and rejected projects.
func pscAcceptFields(accepted, rejected string) []console.Field {
	manual := map[string]string{"connectionPreference": "ACCEPT_MANUAL"}
	return []console.Field{
		{Name: "acceptedProjects", Label: "Accepted projects", Type: "text", Default: accepted, VisibleWhen: manual,
			Help: "Add accepted project: project ID or number and connection limit, as PROJECT:LIMIT, separated by commas."},
		{Name: "rejectedProjects", Label: "Rejected projects", Type: "text", Default: rejected, VisibleWhen: manual,
			Help: "Add rejected project: project IDs or numbers, separated by commas."},
	}
}

// pscPublishFields are Google's Publish service's: Target details (the
// load balancer's forwarding rule, recorded: no internal load balancer is
// served), Service name, the Private Service Connect subnets, the
// connection preference with accepted and rejected projects, Use Proxy
// Protocol and Advanced configuration's domain name.
func pscPublishFields(subnets []string) []console.Field {
	subField := console.Field{Name: "natSubnets", Label: "Subnets", Type: "select", Required: true, Options: subnets,
		Help: "A subnet of purpose PRIVATE_SERVICE_CONNECT in the service's region (Reserve new subnet: add one with that purpose to the network)."}
	if subnets == nil {
		subField.Type = "text"
	}
	return append([]console.Field{
		{Name: "targetService", Label: "Load balancer", Type: "text", Required: true, Section: "Target details",
			Help: "The internal load balancer's forwarding rule: projects/PROJECT/regions/REGION/forwardingRules/NAME. Internal load balancers are not served, so it is recorded as given."},
		{Name: "name", Label: "Service name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		subField,
		{Name: "connectionPreference", Label: "Connection preference", Type: "radio", Default: "ACCEPT_AUTOMATIC",
			Options:      []string{"ACCEPT_AUTOMATIC", "ACCEPT_MANUAL"},
			OptionLabels: map[string]string{"ACCEPT_AUTOMATIC": "Automatically accept all connections", "ACCEPT_MANUAL": "Accept connections for selected projects"}},
	}, append(pscAcceptFields("", ""),
		console.Field{Name: "enableProxyProtocol", Label: "Use Proxy Protocol", Type: "checkbox", Default: "false"},
		console.Field{Name: "domainName", Label: "Domain name", Type: "text", Section: "Advanced configuration", Help: "With a trailing dot, such as example.com."})...)
}

func pscAcceptLists(values map[string]string) ([]any, []any, error) {
	var acc, rej []any
	for _, e := range strings.Split(values["acceptedProjects"], ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		id, lim, _ := strings.Cut(e, ":")
		n := 10
		if lim != "" {
			v, err := strconv.Atoi(lim)
			if err != nil || v < 0 {
				return nil, nil, fmt.Errorf("Accepted projects: %q's connection limit is not a number", e)
			}
			n = v
		}
		acc = append(acc, map[string]any{"projectIdOrNum": id, "connectionLimit": n})
	}
	for _, e := range strings.Split(values["rejectedProjects"], ",") {
		if e = strings.TrimSpace(e); e != "" {
			rej = append(rej, e)
		}
	}
	return acc, rej, nil
}

func (p computePSCPublishedProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	sub := strings.TrimSpace(values["natSubnets"])
	region, name, ok := strings.Cut(sub, "/")
	if !ok {
		return "", errors.New("Subnets: choose a Private Service Connect subnet (region/name)")
	}
	acc, rej, err := pscAcceptLists(values)
	if err != nil {
		return "", err
	}
	body := map[string]any{"name": strings.TrimSpace(values["name"]), "targetService": strings.TrimSpace(values["targetService"]),
		"natSubnets": []any{"regions/" + region + "/subnetworks/" + name}, "connectionPreference": vpcOr(values["connectionPreference"], "ACCEPT_AUTOMATIC"),
		"enableProxyProtocol": values["enableProxyProtocol"] == "true", "consumerAcceptLists": acc, "consumerRejectLists": rej}
	if d := strings.TrimSpace(values["domainName"]); d != "" {
		body["domainNames"] = []any{d}
	}
	if _, err := p.call("POST", project, "regions/"+region+"/serviceAttachments", nil, body); err != nil {
		return "", err
	}
	return vpcStr(body, "name"), nil
}

func (p computePSCPublishedProvider) attachment(project string, path []string) (map[string]any, error) {
	if len(path) != 2 {
		return nil, fmt.Errorf("a published service is addressed by its region and name, not %v", path)
	}
	return p.call("GET", project, "regions/"+path[0]+"/serviceAttachments/"+path[1], nil, nil)
}

func (p computePSCPublishedProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	a, err := p.attachment(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	row := pscPublishedRow(a)
	var props []console.Property
	for _, c := range pscPublishedColumns() {
		props = append(props, console.Property{Label: c, Value: row.Fields[c]})
	}
	var subs []string
	for _, s := range vpcStrs(a, "natSubnets") {
		subs = append(subs, vpcLast(s))
	}
	props = append(props, console.Property{Label: "Subnets", Value: strings.Join(subs, ", ")},
		console.Property{Label: "Use Proxy Protocol", Value: vpcOnOff(a["enableProxyProtocol"] == true)},
		console.Property{Label: "Domain names", Value: vpcDash(strings.Join(vpcStrs(a, "domainNames"), ", "))})
	eps := console.Listing{Columns: []string{"Endpoint ID", "Status", "Consumer network"}, NameColumn: "Endpoint", Noun: "connected endpoints"}
	for _, e := range vmMaps(a, "connectedEndpoints") {
		link := vpcStr(e, "endpoint")
		eps.Items = append(eps.Items, console.Resource{Name: vpcLast(link), Fields: map[string]string{"Endpoint ID": vpcStr(e, "pscConnectionId"),
			"Status": pscStatusText(vpcStr(e, "status")), "Consumer network": strings.TrimPrefix(pscRel(vpcStr(e, "consumerNetwork")), "projects/")}})
	}
	eps.Total = len(eps.Items)
	var accepted []string
	for _, m := range vmMaps(a, "consumerAcceptLists") {
		accepted = append(accepted, vpcStr(m, "projectIdOrNum")+":"+vpcStr(m, "connectionLimit"))
	}
	return console.Detail{Title: path[1], Heading: "Service details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "endpoints", Label: "Connected endpoints", Listing: eps}},
		Edit: &console.EditForm{Label: "Edit", Fields: append([]console.Field{
			{Name: "connectionPreference", Label: "Connection preference", Type: "radio", Default: vpcOr(vpcStr(a, "connectionPreference"), "ACCEPT_AUTOMATIC"),
				Options:      []string{"ACCEPT_AUTOMATIC", "ACCEPT_MANUAL"},
				OptionLabels: map[string]string{"ACCEPT_AUTOMATIC": "Automatically accept all connections", "ACCEPT_MANUAL": "Accept connections for selected projects"}},
		}, pscAcceptFields(strings.Join(accepted, ","), strings.Join(vpcStrs(a, "consumerRejectLists"), ","))...)}}, nil
}

// pscRel is a link's projects/... part.
func pscRel(link string) string {
	if i := strings.Index(link, "projects/"); i >= 0 {
		return link[i:]
	}
	return link
}

func (p computePSCPublishedProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	if _, err := p.attachment(project, path); err != nil {
		return nil
	}
	return []console.Action{{ID: "delete", Label: "Delete", Destructive: true, Confirm: "Delete published service " + path[1] + "?", Leaves: true}}
}

func (p computePSCPublishedProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.call("DELETE", project, "regions/"+path[0]+"/serviceAttachments/"+path[1], nil, nil)
	return err
}

func (p computePSCPublishedProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	a, err := p.attachment(project, path)
	if err != nil {
		return err
	}
	acc, rej, err := pscAcceptLists(values)
	if err != nil {
		return err
	}
	if acc == nil {
		acc = []any{}
	}
	if rej == nil {
		rej = []any{}
	}
	_, err = p.call("PATCH", project, "regions/"+path[0]+"/serviceAttachments/"+path[1], nil, map[string]any{"fingerprint": vpcStr(a, "fingerprint"),
		"connectionPreference": vpcOr(values["connectionPreference"], vpcStr(a, "connectionPreference")), "consumerAcceptLists": acc, "consumerRejectLists": rej})
	return err
}
