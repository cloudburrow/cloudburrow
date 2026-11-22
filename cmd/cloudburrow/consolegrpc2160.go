package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Target gRPC proxies (#2160), on Cloud Service Mesh's Routing rule maps
// page, as Google's console shows them: a routing rule map is a global
// forwarding rule of the INTERNAL_SELF_MANAGED scheme, its target proxy
// (here a target gRPC proxy, or an HTTP one) and the proxy's URL map
// (service-mesh/docs/route-rule-map-concepts and
// .../legacy/load-balancing-apis/target-proxies, read 2026-11-22: "If
// you're using the Google Cloud console to set up a target HTTP proxy, the
// target proxy is set up implicitly as part of your routing rule map
// configuration"). Google's console is read-only for gRPC's routing rule
// maps (service-mesh/docs/service-routing/limitations-proxyless, read
// 2026-11-22: "you cannot configure backend services and routing rule maps
// with the gRPC protocol in the Google Cloud console"); CloudBurrow keeps
// Create, Edit and Delete here so that every feature stays reachable from
// the console (the UI parity rule), and marks them UNVERIFIED, as are the
// page's columns.

type grpcRoutingProvider struct{ computeScreen }

func (grpcRoutingProvider) ID() string    { return "servicemesh-routing" }
func (grpcRoutingProvider) Title() string { return "Routing rule maps" }

const grpcRoutingNote = "A proxyless gRPC client (target xds:///HOST:PORT) learns these routing rule maps from the xDS API on the Compute Engine API's port " +
	"and calls the backend services' endpoints itself: the forwarding rule (0.0.0.0, a port and a network) listens on nothing."

// grpcMap is one routing rule map: a forwarding rule and its proxy and
// URL map.
type grpcMap struct {
	rule, proxy, urlMap map[string]any
	proxyColl           string
}

func (p grpcRoutingProvider) maps(project string) []grpcMap {
	var out []grpcMap
	grpc := map[string]map[string]any{}
	for _, px := range p.lbAll(project, "targetGrpcProxies", true, false) {
		grpc[lbRel(vpcStr(px, "selfLink"))] = px
	}
	for _, r := range p.lbAll(project, "forwardingRules", true, false) {
		if vpcStr(r, "loadBalancingScheme") != "INTERNAL_SELF_MANAGED" {
			continue
		}
		t := lbRel(vpcStr(r, "target"))
		coll := ""
		switch {
		case strings.Contains(t, "/targetGrpcProxies/"):
			coll = "targetGrpcProxies"
		case strings.Contains(t, "/targetHttpProxies/"):
			coll = "targetHttpProxies"
		default:
			continue
		}
		m := grpcMap{rule: r, proxyColl: coll}
		if m.proxy = grpc[t]; m.proxy == nil {
			m.proxy, _ = p.call("GET", project, "global/"+coll+"/"+vpcLast(t), nil, nil)
		}
		if um := vpcStr(m.proxy, "urlMap"); um != "" {
			m.urlMap, _ = p.call("GET", project, "global/urlMaps/"+vpcLast(um), nil, nil)
		}
		out = append(out, m)
	}
	return out
}

func grpcProtocol(m grpcMap) string {
	if m.proxyColl == "targetGrpcProxies" {
		return "gRPC"
	}
	return "HTTP"
}

func (p grpcRoutingProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Protocol", "Network", "Address", "Target proxy", "URL map", "Validate for proxyless"},
		NameColumn: "Forwarding rule", Noun: "routing rule maps", Note: grpcRoutingNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Routing rule maps are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, m := range p.maps(project) {
		name := vpcStr(m.rule, "name")
		path := []string{name}
		listing.Items = append(listing.Items, console.Resource{Name: name, Opens: path, Target: path,
			Fields: map[string]string{"Protocol": grpcProtocol(m), "Network": vpcDash(vpcLast(vpcStr(m.rule, "network"))),
				"Address":      vpcStr(m.rule, "IPAddress") + ":" + strings.SplitN(vpcStr(m.rule, "portRange"), "-", 2)[0],
				"Target proxy": vpcDash(vpcStr(m.proxy, "name")), "URL map": vpcDash(vpcStr(m.urlMap, "name")),
				"Validate for proxyless": grpcValidateText(m)},
			Actions: []console.Action{grpcDelete(name)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func grpcValidateText(m grpcMap) string {
	if m.proxyColl != "targetGrpcProxies" {
		return "—"
	}
	return map[bool]string{true: "Enabled", false: "Disabled"}[m.proxy["validateForProxyless"] == true]
}

func grpcDelete(name string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Delete routing rule map " + name + "? Its forwarding rule, target proxy and URL map are deleted; the backend services are kept."}
}

func (grpcRoutingProvider) CreateOnPage() bool  { return true }
func (grpcRoutingProvider) CreateTitle() string { return "Create routing rule map" }

func (p grpcRoutingProvider) CreateForm() (string, []console.Field) {
	return "Save", grpcCreateFields(nil)
}

func (p grpcRoutingProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	var svcs []string
	for _, b := range p.lbAll(project, "backendServices", true, false) {
		if vpcStr(b, "loadBalancingScheme") == "INTERNAL_SELF_MANAGED" {
			svcs = append(svcs, vpcStr(b, "name"))
		}
	}
	sort.Strings(svcs)
	return grpcCreateFields(svcs)
}

func grpcCreateFields(services []string) []console.Field {
	const s1, s2, s3 = "Routing rule map", "Forwarding rule", "Hosts and path rules"
	svc := console.Field{Name: "defaultService", Label: "Default backend service", Type: "text", Required: true, Section: s3,
		Help: "A global backend service of the INTERNAL_SELF_MANAGED scheme; for gRPC, with the GRPC protocol."}
	if services != nil {
		svc.Type, svc.Options = "select", services
	}
	return []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: "The routing rule map's URL map name.", Section: s1},
		{Name: "protocol", Label: "Protocol", Type: "radio", Default: "GRPC", Options: []string{"GRPC", "HTTP"}, Section: s1,
			OptionLabels: map[string]string{"GRPC": "gRPC", "HTTP": "HTTP"}, Help: "gRPC makes a target gRPC proxy, for proxyless gRPC clients; HTTP a target HTTP proxy, for Envoy."},
		{Name: "validateForProxyless", Label: "Validate for proxyless", Type: "checkbox", Default: "true", Section: s1, ShowWhen: "protocol=GRPC",
			Help: "Refuse URL map features proxyless gRPC does not support (URL rewrites, redirects, CORS, request mirroring); the address must then be 0.0.0.0."},
		{Name: "rule", Label: "Forwarding rule name", Type: "text", Required: true, Pattern: computeNamePattern, Section: s2},
		{Name: "network", Label: "Network", Type: "text", Default: "default", Section: s2},
		{Name: "port", Label: "Port", Type: "text", Default: "50051", Required: true, Pattern: `^[0-9]{1,5}$`, Section: s2,
			Help: "The port of the clients' xds:///HOST:PORT target; the address is 0.0.0.0."},
		{Name: "hosts", Label: "Hosts", Type: "text", Section: s3, Help: "Optional: host names a client names, such as helloworld-gce or helloworld-gce:50051, comma separated; empty matches every host."},
		svc,
	}
}

func (p grpcRoutingProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name, rule := strings.TrimSpace(v["name"]), strings.TrimSpace(v["rule"])
	if name == "" || rule == "" {
		return "", errors.New("Name and Forwarding rule name: give both")
	}
	svc := strings.TrimSpace(v["defaultService"])
	if svc == "" {
		return "", errors.New("Default backend service: choose one")
	}
	if !strings.Contains(svc, "/") {
		svc = "global/backendServices/" + svc
	}
	um := map[string]any{"name": name, "defaultService": svc}
	if hosts := vpcList(v["hosts"]); len(hosts) > 0 {
		um["hostRules"] = []any{map[string]any{"hosts": hosts, "pathMatcher": "matcher1"}}
		um["pathMatchers"] = []any{map[string]any{"name": "matcher1", "defaultService": svc}}
	}
	if _, err := p.call("POST", project, "global/urlMaps", nil, um); err != nil {
		return "", err
	}
	coll, proxy := "targetGrpcProxies", map[string]any{"name": name + "-proxy", "urlMap": "global/urlMaps/" + name}
	if v["protocol"] == "HTTP" {
		coll = "targetHttpProxies"
	} else {
		proxy["validateForProxyless"] = v["validateForProxyless"] == "true"
	}
	if _, err := p.call("POST", project, "global/"+coll, nil, proxy); err != nil {
		_, _ = p.call("DELETE", project, "global/urlMaps/"+name, nil, nil)
		return "", err
	}
	network := vpcOr(strings.TrimSpace(v["network"]), "default")
	if !strings.Contains(network, "/") {
		network = "global/networks/" + network
	}
	if _, err := p.call("POST", project, "global/forwardingRules", nil, map[string]any{"name": rule, "loadBalancingScheme": "INTERNAL_SELF_MANAGED",
		"IPAddress": "0.0.0.0", "portRange": strings.TrimSpace(v["port"]), "network": network, "target": "global/" + coll + "/" + name + "-proxy"}); err != nil {
		_, _ = p.call("DELETE", project, "global/"+coll+"/"+name+"-proxy", nil, nil)
		_, _ = p.call("DELETE", project, "global/urlMaps/"+name, nil, nil)
		return "", err
	}
	return rule, nil
}

func (p grpcRoutingProvider) find(project string, path []string) (grpcMap, error) {
	if len(path) != 1 {
		return grpcMap{}, fmt.Errorf("a routing rule map is addressed by its forwarding rule, not %v", path)
	}
	for _, m := range p.maps(project) {
		if vpcStr(m.rule, "name") == path[0] {
			return m, nil
		}
	}
	return grpcMap{}, fmt.Errorf("no routing rule map %s", path[0])
}

func (p grpcRoutingProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	m, err := p.find(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Forwarding rule", Value: path[0]}, {Label: "Protocol", Value: grpcProtocol(m)},
		{Label: "Network", Value: vpcDash(vpcLast(vpcStr(m.rule, "network")))}, {Label: "IP address", Value: vpcStr(m.rule, "IPAddress")},
		{Label: "Port", Value: vpcStr(m.rule, "portRange")}, {Label: "Target proxy", Value: vpcDash(vpcStr(m.proxy, "name"))},
		{Label: "Validate for proxyless", Value: grpcValidateText(m)}, {Label: "URL map", Value: vpcDash(vpcStr(m.urlMap, "name"))},
		{Label: "Default backend service", Value: vpcDash(vpcLast(vpcStr(m.urlMap, "defaultService")))}}
	rules := console.Listing{Columns: []string{"Hosts", "Paths", "Backend service"}, NameColumn: "Path matcher", Noun: "host and path rules", Items: []console.Resource{}}
	matchers := map[string]map[string]any{}
	for _, pm := range vmMaps(m.urlMap, "pathMatchers") {
		matchers[vpcStr(pm, "name")] = pm
	}
	for _, hr := range vmMaps(m.urlMap, "hostRules") {
		pm := matchers[vpcStr(hr, "pathMatcher")]
		hosts := strings.Join(vpcStrs(hr, "hosts"), ", ")
		rules.Items = append(rules.Items, console.Resource{Name: vpcStr(hr, "pathMatcher"),
			Fields: map[string]string{"Hosts": hosts, "Paths": "(default)", "Backend service": vpcDash(vpcLast(vpcStr(pm, "defaultService")))}})
		for _, pr := range vmMaps(pm, "pathRules") {
			rules.Items = append(rules.Items, console.Resource{Name: vpcStr(hr, "pathMatcher"),
				Fields: map[string]string{"Hosts": hosts, "Paths": strings.Join(vpcStrs(pr, "paths"), ", "), "Backend service": vpcDash(vpcLast(vpcStr(pr, "service")))}})
		}
	}
	rules.Total = len(rules.Items)
	d := console.Detail{Title: path[0], Heading: "Routing rule map details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}, Note: grpcRoutingNote},
			{ID: "rules", Label: "Routing rules", Listing: rules}}}
	if m.proxyColl == "targetGrpcProxies" {
		d.Edit = &console.EditForm{Label: "Edit", Title: "Edit routing rule map", Submit: "Save", Fields: []console.Field{
			{Name: "validateForProxyless", Label: "Validate for proxyless", Type: "checkbox", Default: fmt.Sprint(m.proxy["validateForProxyless"] == true),
				Help: "Refuse URL map features proxyless gRPC does not support."},
			{Name: "description", Label: "Target proxy description", Type: "text", Default: vpcStr(m.proxy, "description")}},
			Note: "The forwarding rule's address, port and network cannot be changed: delete the routing rule map and create it again."}
	}
	return d, nil
}

func (p grpcRoutingProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 1 {
		return nil
	}
	a := grpcDelete(path[0])
	a.Leaves = true
	return []console.Action{a}
}

// Edit patches the target gRPC proxy (validateForProxyless, description)
// under its fingerprint.
func (p grpcRoutingProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	m, err := p.find(project, path)
	if err != nil {
		return err
	}
	if m.proxyColl != "targetGrpcProxies" {
		return errors.New("Edit applies to a gRPC routing rule map")
	}
	_, err = p.call("PATCH", project, "global/targetGrpcProxies/"+vpcStr(m.proxy, "name"), nil, map[string]any{
		"validateForProxyless": v["validateForProxyless"] == "true", "description": strings.TrimSpace(v["description"]), "fingerprint": vpcStr(m.proxy, "fingerprint")})
	return err
}

func (p grpcRoutingProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	m, err := p.find(project, path)
	if err != nil {
		return err
	}
	if _, err := p.call("DELETE", project, "global/forwardingRules/"+path[0], nil, nil); err != nil {
		return err
	}
	if _, err := p.call("DELETE", project, "global/"+m.proxyColl+"/"+vpcStr(m.proxy, "name"), nil, nil); err != nil {
		return err
	}
	if um := vpcStr(m.urlMap, "name"); um != "" {
		_, err = p.call("DELETE", project, "global/urlMaps/"+um, nil, nil)
	}
	return err
}
