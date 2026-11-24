package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Identity-Aware Proxy's SSH and TCP resources tab (#2160), as
// iap/docs/using-tcp-forwarding and iap/docs/tcp-by-host (read
// 2026-11-22) draw it: the project's VM instances, whose checkbox opens
// the info panel's principals (Add principal, IAP-Secured Tunnel User,
// roles/iap.tunnelResourceAccessor, with an optional condition such as
// destination.port == 22), and the tunnel destination groups: Create
// destination group (Name, "only lowercase letters (a-z) and dashes (-)";
// Region; IP address, "comma-separated ranges using CIDR notation";
// FQDNs, wildcards allowed), a group's page by its name, Edit destination
// group and delete. The columns beyond the name are UNVERIFIED (the docs
// name none, re-read 2026-11-22).

type iapTCPProvider struct{ computeScreen }

func (iapTCPProvider) ID() string    { return "security-iap-tcp" }
func (iapTCPProvider) Title() string { return "SSH and TCP resources" }

const iapTCPNote = "IAP TCP forwarding tunnels through this instance: gcloud compute start-iap-tunnel (and ssh --tunnel-through-iap) " +
	"with --iap-tunnel-url-override=ws://<the Compute Engine port> reaches an instance's port, or a host a destination group lists, " +
	"for a principal with IAP-Secured Tunnel User; the network's firewall rules must allow 35.235.240.0/20 on the port."

// iapTCPResource is a row's tunnel resource: an instance (zones, Z, I) or
// a destination group (locations, R, G).
func iapTCPResource(project string, path []string) (string, error) {
	if len(path) != 3 {
		return "", fmt.Errorf("a tunnel resource is addressed by its zone or region and name, not %v", path)
	}
	switch path[0] {
	case "zones":
		return "projects/" + project + "/iap_tunnel/zones/" + path[1] + "/instances/" + path[2], nil
	case "locations":
		return "projects/" + project + "/iap_tunnel/locations/" + path[1] + "/destGroups/" + path[2], nil
	}
	return "", fmt.Errorf("no tunnel resource %v", path)
}

func (p iapTCPProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Type", "Location", "Status", "Destinations"}, NameColumn: "Resource", Noun: "SSH and TCP resources",
		Note: iapTCPNote, RowsOpenable: false}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "SSH and TCP resources are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	perms := []console.InfoTab{{ID: "permissions", Label: "Principals", Permissions: true}}
	if m, err := p.call("GET", project, "aggregated/instances", nil, nil); err == nil {
		vms := aggregatedItems(m, "instances")
		sort.Slice(vms, func(i, j int) bool { return vpcStr(vms[i], "name") < vpcStr(vms[j], "name") })
		for _, o := range vms {
			zone := vpcLast(vpcStr(o, "zone"))
			st := vpcStr(o, "status")
			listing.Items = append(listing.Items, console.Resource{Name: vpcStr(o, "name"), Target: []string{"zones", zone, vpcStr(o, "name")}, Selectable: true,
				Fields: map[string]string{"Type": "VM instance", "Location": zone, "Status": st, "Destinations": "—"}, Info: perms})
		}
	}
	groups, err := p.groups(ctx, project)
	if err != nil {
		return listing, err
	}
	for _, g := range groups {
		r := iapGroupRow(g)
		r.Info = perms
		r.Actions = []console.Action{iapGroupEdit(g), iapGroupDelete(r.Target)}
		listing.Items = append(listing.Items, r)
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p iapTCPProvider) groups(ctx context.Context, project string) ([]map[string]any, error) {
	m, err := p.v1(ctx, "GET", "/v1/projects/"+project+"/iap_tunnel/locations/-/destGroups?pageSize=1000", nil)
	if err != nil {
		return nil, err
	}
	return vmMaps(m, "tunnelDestGroups"), nil
}

func iapGroupRow(g map[string]any) console.Resource {
	parts := strings.Split(vpcStr(g, "name"), "/")
	target := []string{"locations", parts[4], parts[6]}
	dest := append(vpcStrs(g, "cidrs"), vpcStrs(g, "fqdns")...)
	return console.Resource{Name: parts[6], Target: target, Opens: target, Selectable: true,
		Fields: map[string]string{"Type": "Destination group", "Location": parts[4], "Status": "—", "Destinations": strings.Join(dest, ", ")}}
}

func iapGroupFields(g map[string]any, create bool) []console.Field {
	fs := []console.Field{}
	if create {
		fs = append(fs,
			console.Field{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[a-z]([a-z\-]{0,61}[a-z])?$`,
				Help: "The group name can contain only lowercase letters (a-z) and dashes (-)."},
			console.Field{Name: "region", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: compute.Regions(),
				Help: "The region of the destination resources, such as a VPN gateway's."})
	}
	return append(fs,
		console.Field{Name: "cidrs", Label: "IP address", Type: "text", Default: strings.Join(vpcStrs(g, "cidrs"), ","), Section: "Destinations",
			Help: "Comma-separated ranges using CIDR notation, such as 10.1.2.0/24,172.0.0.0/8."},
		console.Field{Name: "fqdns", Label: "FQDNs", Type: "text", Default: strings.Join(vpcStrs(g, "fqdns"), ","), Section: "Destinations",
			Help: "Comma-separated host names; wildcards are allowed, such as *.internal.company.com."})
}

func iapGroupEdit(g map[string]any) console.Action {
	return console.Action{ID: "edit", Label: "Edit", Title: "Edit destination group", Submit: "Save", Fields: iapGroupFields(g, false)}
}

func iapGroupDelete(path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "Delete destination group " + path[2] + "? Tunnels to its destinations are then refused unless another group lists them."}
}

func (iapTCPProvider) CreateOnPage() bool  { return true }
func (iapTCPProvider) CreateTitle() string { return "Create destination group" }

func (iapTCPProvider) CreateForm() (string, []console.Field) {
	return "Create destination group", iapGroupFields(nil, true)
}

func iapSplitList(v string) []any {
	out := []any{}
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (p iapTCPProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if p.api() == nil {
		return "", errors.New(computeNotStarted)
	}
	name, region := strings.TrimSpace(v["name"]), strings.TrimSpace(v["region"])
	_, err := p.v1(ctx, "POST", "/v1/projects/"+project+"/iap_tunnel/locations/"+region+"/destGroups?tunnelDestGroupId="+url.QueryEscape(name),
		map[string]any{"cidrs": iapSplitList(v["cidrs"]), "fqdns": iapSplitList(v["fqdns"])})
	if err != nil {
		return "", err
	}
	return name, nil
}

func (p iapTCPProvider) group(ctx context.Context, project string, path []string) (map[string]any, error) {
	res, err := iapTCPResource(project, path)
	if err != nil || path[0] != "locations" {
		return nil, fmt.Errorf("no destination group %v", path)
	}
	return p.v1(ctx, "GET", "/v1/"+res, nil)
}

func (p iapTCPProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	g, err := p.group(ctx, project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[2]}, {Label: "Region", Value: path[1]},
		{Label: "IP address", Value: vpcDash(strings.Join(vpcStrs(g, "cidrs"), ", "))},
		{Label: "FQDNs", Value: vpcDash(strings.Join(vpcStrs(g, "fqdns"), ", "))}}
	return console.Detail{Title: path[2], Heading: "Destination group details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Note: iapTCPNote,
			Groups: []console.PropertyGroup{{Properties: props}}}}}, nil
}

func (p iapTCPProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil {
		return nil
	}
	g, err := p.group(ctx, project, path)
	if err != nil {
		return nil
	}
	del := iapGroupDelete(path)
	del.Leaves = true
	return []console.Action{iapGroupEdit(g), del}
}

func (p iapTCPProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if _, err := p.group(ctx, project, path); err != nil {
		return err
	}
	res, _ := iapTCPResource(project, path)
	switch action {
	case "edit":
		_, err := p.v1(ctx, "PATCH", "/v1/"+res+"?updateMask=cidrs,fqdns", map[string]any{"name": res, "cidrs": iapSplitList(v["cidrs"]), "fqdns": iapSplitList(v["fqdns"])})
		return err
	case "delete":
		_, err := p.v1(ctx, "DELETE", "/v1/"+res, nil)
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}

// PolicyOn implements console.PolicyEditor: an instance's or a destination
// group's tunnel policy, which the tunnel relay enforces.
func (p iapTCPProvider) PolicyOn(ctx context.Context, path []string) *console.PolicyTarget {
	if len(path) != 3 {
		return nil
	}
	return &console.PolicyTarget{RoleHelp: "A role, such as roles/iap.tunnelResourceAccessor (IAP-Secured Tunnel User).", Conditions: true,
		Enforced: "IAP checks iap.tunnelInstances.accessViaIAP (an instance) or iap.tunnelDestGroups.accessViaIAP (a destination group), which IAP-Secured Tunnel User " +
			"(roles/iap.tunnelResourceAccessor) holds, on every tunnel, with each binding's condition (destination.ip, destination.port)."}
}

func (p iapTCPProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	res, err := iapTCPResource(project, path)
	if err != nil {
		return console.Policy{}, err
	}
	return iapGetPolicy(ctx, p.computeScreen, res)
}

func (p iapTCPProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	res, err := iapTCPResource(project, path)
	if err != nil {
		return err
	}
	return iapSetPolicy(ctx, p.computeScreen, res, pol)
}

func (p iapTCPProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	res, err := iapTCPResource(project, path)
	if err != nil {
		return nil, err
	}
	return iapTestPermissions(ctx, p.computeScreen, res, perms)
}

// PolicyResource implements console.PolicyResourcer.
func (p iapTCPProvider) PolicyResource(ctx context.Context, project string, path []string) string {
	res, err := iapTCPResource(project, path)
	if err != nil {
		return ""
	}
	return "//iap.googleapis.com/" + res
}
