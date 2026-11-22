package main

// Private Service Connect's Connected endpoints (#2141), Google's console
// steps (https://docs.cloud.google.com/vpc/docs/configure-private-service-connect-services,
// read 2026-11-22): the Private Service Connect page's Connected
// endpoints tab, and Connect endpoint: Target (Published service), Target
// service (the service attachment URI), Endpoint name, Network,
// Subnetwork, IP address (a reserved one, or Create IP address: Name,
// Assign automatically or Let me choose a Custom IP address), Enable
// global access, then Add endpoint. A Cloud SQL instance with Private
// Service Connect publishes the service attachment its Connections page
// shows. The list's columns are not in the docs (UNVERIFIED,
// re-read 2026-11-22: the same page). Service Directory namespaces are not
// served, so the form has no Namespace.

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

type computePSCProvider struct{ computeScreen }

func (computePSCProvider) ID() string    { return "vpc-psc" }
func (computePSCProvider) Title() string { return "Private Service Connect" }

func (p computePSCProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Endpoint", "Status", "Region", "Network", "IP address", "Target", "Global access"},
		NameColumn: "Endpoint", Noun: "connected endpoints",
		Note: "Endpoints that connect to a published service, such as a Cloud SQL instance's service attachment. A VM of the endpoint's network reaches the service at the endpoint's address while its connection is accepted."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Endpoints are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	agg, err := p.call("GET", project, "aggregated/forwardingRules", nil, nil)
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
		l, _ := sv["forwardingRules"].([]any)
		for _, e := range l {
			r, _ := e.(map[string]any)
			region := strings.TrimPrefix(scope, "regions/")
			global := "Disabled"
			if r["allowPscGlobalAccess"] == true {
				global = "Enabled"
			}
			target := vpcStr(r, "target")
			if i := strings.Index(target, "/projects/"); i >= 0 {
				target = target[i+1:]
			}
			listing.Items = append(listing.Items, console.Resource{Name: region + "/" + vpcStr(r, "name"), Status: vpcTitle(vpcStr(r, "pscConnectionStatus")),
				Fields: map[string]string{"Endpoint": vpcStr(r, "name"), "Status": vpcTitle(vpcStr(r, "pscConnectionStatus")), "Region": region,
					"Network": vpcDash(vpcLast(vpcStr(r, "network"))), "IP address": vpcStr(r, "IPAddress"), "Target": target, "Global access": global}})
		}
	}
	listing.Total = len(listing.Items)
	listing.AlwaysStatus = true
	return listing, nil
}

func pscFields(nets []string) []console.Field {
	if len(nets) == 0 {
		nets = []string{"default"}
	}
	create := map[string]string{"ipChoice": "create"}
	return []console.Field{
		{Name: "target", Label: "Target", Type: "radio", Default: "published", Options: []string{"published"},
			OptionLabels: map[string]string{"published": "Published service"}},
		{Name: "targetService", Label: "Target service", Type: "text", Required: true,
			Help: "The service attachment URI: projects/SERVICE_PROJECT/regions/REGION/serviceAttachments/SERVICE_NAME, such as a Cloud SQL instance's (its Connections page shows it)."},
		{Name: "name", Label: "Endpoint name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "network", Label: "Network", Type: "select", Required: true, Default: "default", Options: nets},
		computeRegionField("region"),
		{Name: "subnetwork", Label: "Subnetwork", Type: "text", Required: true, Default: "default", Help: "A subnet of the network in the region; the endpoint's address is in it."},
		{Name: "ipChoice", Label: "IP address", Type: "radio", Default: "create", Options: []string{"create", "existing"},
			OptionLabels: map[string]string{"create": "Create IP address", "existing": "A reserved internal address"}},
		{Name: "ipName", Label: "Name", Type: "text", Pattern: computeNamePattern, VisibleWhen: create, Help: "Empty names it after the endpoint."},
		{Name: "ipAssign", Label: "Static IP address", Type: "radio", Default: "auto", Options: []string{"auto", "choose"}, VisibleWhen: create,
			OptionLabels: map[string]string{"auto": "Assign automatically", "choose": "Let me choose"}},
		{Name: "customIp", Label: "Custom IP address", Type: "text", VisibleWhen: map[string]string{"ipChoice": "create", "ipAssign": "choose"}},
		{Name: "address", Label: "Reserved address", Type: "text", VisibleWhen: map[string]string{"ipChoice": "existing"},
			Help: "The name of a reserved internal address of the subnetwork (IP addresses, Reserve internal static IP address)."},
		{Name: "globalAccess", Label: "Enable global access", Type: "checkbox",
			Help: "Lets VMs of the network in any region reach the endpoint; otherwise only its region's."},
	}
}

func (computePSCProvider) CreateForm() (string, []console.Field) {
	return "Connect endpoint", pscFields(nil)
}

// CreateSubmit implements console.CreateSubmitter: Google's Add endpoint.
func (computePSCProvider) CreateSubmit() string { return "Add endpoint" }

func (p computePSCProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	return pscFields(p.networkNames(ctx, project))
}

func (computePSCProvider) CreateTitle() string { return "Connect endpoint" }

func (p computePSCProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	region := strings.TrimSpace(v["region"])
	sub := "regions/" + region + "/subnetworks/" + strings.TrimSpace(vpcOr(v["subnetwork"], "default"))
	addr := strings.TrimSpace(v["address"])
	if v["ipChoice"] != "existing" {
		addr = strings.TrimSpace(vpcOr(v["ipName"], name))
		b := map[string]any{"name": addr, "addressType": "INTERNAL", "subnetwork": sub}
		if v["ipAssign"] == "choose" {
			b["address"] = strings.TrimSpace(v["customIp"])
		}
		if _, err := p.call("POST", project, "regions/"+region+"/addresses", nil, b); err != nil {
			return "", err
		}
	}
	b := map[string]any{"name": name, "network": "global/networks/" + strings.TrimSpace(v["network"]), "IPAddress": addr,
		"target": strings.TrimSpace(v["targetService"])}
	if v["globalAccess"] == "true" {
		b["allowPscGlobalAccess"] = true
	}
	if _, err := p.call("POST", project, "regions/"+region+"/forwardingRules", nil, b); err != nil {
		if v["ipChoice"] != "existing" {
			_, _ = p.call("DELETE", project, "regions/"+region+"/addresses/"+addr, nil, nil)
		}
		return "", err
	}
	return region + "/" + name, nil
}

func (p computePSCProvider) Delete(ctx context.Context, project, name string) error {
	region, rule, ok := strings.Cut(name, "/")
	if !ok {
		return errors.New("an endpoint is named REGION/NAME")
	}
	_, err := p.call("DELETE", project, "regions/"+region+"/forwardingRules/"+rule, nil, nil)
	return err
}
