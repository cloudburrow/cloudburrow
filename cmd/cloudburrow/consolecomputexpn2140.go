package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Shared VPC (#2140), over the Compute Engine API's projects
// enableXpnHost, disableXpnHost, enableXpnResource, disableXpnResource,
// getXpnHost and getXpnResources.
//
// Google's page (vpc/docs/provisioning-shared-vpc, read 2026-11-22): a
// project that is neither host nor service project offers Set up Shared
// VPC (Enable host project, then Select subnets: Share all subnets
// (project-level permissions) or Individual subnets (subnet-level
// permissions), then the service projects' Project names); a host
// project's page has the Attached projects tab with Attach projects, and
// its subnets beside it (the tab's name, "Subnets in host project", is
// UNVERIFIED, re-read 2026-11-22: the docs name only Attached projects); a service project
// sees the host's subnets it can use, from subnetworks.listUsable, as
// `gcloud compute networks subnets list-usable` lists them (project,
// region, network, subnet, range, secondary ranges). Individual subnets
// grants roles/compute.networkUser on each chosen subnet through its
// IAM policy (subnetworks.setIamPolicy).
type computeSharedVPCProvider struct{ computeScreen }

func (computeSharedVPCProvider) ID() string    { return "vpc-sharedvpc" }
func (computeSharedVPCProvider) Title() string { return "Shared VPC" }

const sharedVPCNote = "Shared VPC is for projects of an organization: a host project's networks and subnets are used by the VMs and internal addresses of its attached service projects, on the host network's segment under its firewall rules. " +
	"A service project attaches to one host; a host cannot be a service project."

// xpnRole is "host", "service" or "".
func (p computeSharedVPCProvider) xpnRole(project string) (string, string, error) {
	pr, err := p.call("GET", project, "", nil, nil)
	if err != nil {
		return "", "", err
	}
	if vpcStr(pr, "xpnProjectStatus") == "HOST" {
		return "host", project, nil
	}
	h, err := p.call("GET", project, "getXpnHost", nil, nil)
	if err != nil {
		return "", "", err
	}
	if name := vpcStr(h, "name"); name != "" {
		return "service", name, nil
	}
	return "", "", nil
}

// hostSubnets are the host project's subnets, as list-usable shows them.
func (p computeSharedVPCProvider) hostSubnets(host string) []map[string]any {
	m, err := p.call("GET", host, "aggregated/subnetworks", nil, nil)
	if err != nil {
		return nil
	}
	subs := aggregatedItems(m, "subnetworks")
	sort.Slice(subs, func(i, j int) bool {
		a := vpcLast(vpcStr(subs[i], "network")) + "/" + vpcLast(vpcStr(subs[i], "region")) + "/" + vpcStr(subs[i], "name")
		b := vpcLast(vpcStr(subs[j], "network")) + "/" + vpcLast(vpcStr(subs[j], "region")) + "/" + vpcStr(subs[j], "name")
		return a < b
	})
	return subs
}

func sharedSubnetColumns() []string {
	return []string{"Project", "Region", "Network", "Subnet", "IP address range", "Secondary IP ranges"}
}

func sharedSubnetRow(host string, s map[string]any) console.Resource {
	return console.Resource{Name: vpcStr(s, "name"), Fields: map[string]string{"Project": host, "Region": vpcLast(vpcStr(s, "region")),
		"Network": vpcLast(vpcStr(s, "network")), "Subnet": vpcStr(s, "name"), "IP address range": vpcStr(s, "ipCidrRange"),
		"Secondary IP ranges": vpcDash(secondaryText(s))}}
}

func (p computeSharedVPCProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Project ID", "Resources on shared subnets"}, Noun: "attached projects", Note: sharedVPCNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Shared VPC is set up per project. Choose one in the toolbar."
		return listing, nil
	}
	role, host, err := p.xpnRole(project)
	if err != nil {
		return listing, err
	}
	switch role {
	case "service":
		// A service project's view: the host's subnets it can use.
		l := console.Listing{Columns: sharedSubnetColumns(), NameColumn: "Name", Noun: "shared subnets",
			TableTitle: "Subnets shared with this project",
			Note:       "This project is a service project of the Shared VPC host project " + host + ". Its VMs choose one of these subnets under Networks shared with me."}
		for _, s := range p.usableSubnets(project, host) {
			l.Items = append(l.Items, s)
		}
		l.Total = len(l.Items)
		return l, nil
	case "host":
		res, err := p.call("GET", project, "getXpnResources", nil, nil)
		if err != nil {
			return listing, err
		}
		for _, r := range vmMaps(res, "resources") {
			id := vpcStr(r, "id")
			users := p.sharedUsers(id, project)
			listing.Items = append(listing.Items, console.Resource{Name: id, Target: []string{id},
				Fields:  map[string]string{"Project ID": id, "Resources on shared subnets": fmt.Sprint(users)},
				Actions: []console.Action{{ID: "detach", Label: "Detach", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Detach service project " + id + " from this host project?"}}})
		}
		listing.Total = len(listing.Items)
		var groups []console.PropertyGroup
		for _, s := range p.hostSubnets(project) {
			groups = append(groups, console.PropertyGroup{Heading: vpcLast(vpcStr(s, "network")) + " / " + vpcStr(s, "name"),
				Properties: []console.Property{{Label: "Region", Value: vpcLast(vpcStr(s, "region"))},
					{Label: "IP address range", Value: vpcStr(s, "ipCidrRange")},
					{Label: "Secondary IP ranges", Value: vpcDash(secondaryText(s))},
					{Label: "Compute Network Users on this subnet", Value: vpcDash(strings.Join(p.subnetUsers(project, s), ", "))}}})
		}
		listing.Tabs = []console.Section{
			{ID: "subnets", Label: "Subnets in host project", Groups: groups,
				Note: "Share all subnets: principals with Compute Network User (roles/compute.networkUser) on this host project use every subnet. " +
					"Individual subnets: principals granted it on a subnet use only that subnet (its IAM policy, subnetworks.setIamPolicy). Share subnets grants it on the subnets you choose."},
			{ID: "attached", Label: "Attached projects"},
		}
		listing.ListTab = "attached"
		return listing, nil
	}
	listing.Note = "Shared VPC is not set up for this project. Set up Shared VPC makes it a host project and attaches service projects to it. " + sharedVPCNote
	return listing, nil
}

// sharedUsers counts the service project's instances on the host's subnets.
func (p computeSharedVPCProvider) sharedUsers(service, host string) int {
	m, err := p.call("GET", service, "aggregated/instances", nil, nil)
	if err != nil {
		return 0
	}
	n := 0
	for _, inst := range aggregatedItems(m, "instances") {
		for _, nic := range vmMaps(inst, "networkInterfaces") {
			if strings.Contains(vpcStr(nic, "subnetwork"), "projects/"+host+"/") {
				n++
				break
			}
		}
	}
	return n
}

func sharingModeField() console.Field {
	return console.Field{Name: "sharing", Label: "Select subnets", Type: "radio", Default: "all", Options: []string{"all", "individual"},
		OptionLabels: map[string]string{"all": "Share all subnets (project-level permissions)", "individual": "Individual subnets (subnet-level permissions)"}}
}

// subnetFieldPrefix names a "Subnets to share" checkbox: subnet:REGION/NAME.
const subnetFieldPrefix = "subnet:"

// subnetFields are the host's subnets to share, one checkbox each, and
// the principals granted Compute Network User on those chosen, shown for
// Individual subnets (when is the ShowWhen condition, "" for always).
func (p computeSharedVPCProvider) subnetFields(host, when string) []console.Field {
	var out []console.Field
	for _, s := range p.hostSubnets(host) {
		region := vpcLast(vpcStr(s, "region"))
		out = append(out, console.Field{Name: subnetFieldPrefix + region + "/" + vpcStr(s, "name"), Type: "checkbox", Section: "Subnets to share", ShowWhen: when,
			Label: vpcStr(s, "name") + " (" + region + ", " + vpcLast(vpcStr(s, "network")) + ", " + vpcStr(s, "ipCidrRange") + ")"})
	}
	return append(out, console.Field{Name: "members", Label: "Compute Network Users", Type: "text", ShowWhen: when, Section: "Subnets to share",
		Help: "The service projects' principals granted Compute Network User (roles/compute.networkUser) on each subnet chosen, separated by commas: user:EMAIL, group:EMAIL, serviceAccount:EMAIL or domain:DOMAIN."})
}

// shareSubnets grants roles/compute.networkUser to values' members on
// the host subnets values chooses, through each subnet's IAM policy.
func (p computeSharedVPCProvider) shareSubnets(host string, values map[string]string) error {
	chosen, members, err := shareChoice(values)
	if err != nil {
		return err
	}
	for _, name := range chosen {
		if err := p.grantSubnet(host, name, members); err != nil {
			return err
		}
	}
	return nil
}

// shareChoice is the subnets (REGION/NAME, sorted) and principals the
// form chose, or why it cannot be saved.
func shareChoice(values map[string]string) ([]string, []any, error) {
	var members []any
	for _, m := range strings.Split(values["members"], ",") {
		if m = strings.TrimSpace(m); m != "" {
			members = append(members, m)
		}
	}
	var chosen []string
	for k, v := range values {
		if name, ok := strings.CutPrefix(k, subnetFieldPrefix); ok && (v == "true" || v == "on") {
			chosen = append(chosen, name)
		}
	}
	sort.Strings(chosen)
	if len(chosen) == 0 {
		return nil, nil, errors.New("Subnets to share: choose at least one subnet")
	}
	if len(members) == 0 {
		return nil, nil, errors.New("Compute Network Users: name at least one principal to grant the chosen subnets")
	}
	return chosen, members, nil
}

// grantSubnet adds members to roles/compute.networkUser on host's subnet
// name (REGION/NAME).
func (p computeSharedVPCProvider) grantSubnet(host, name string, members []any) error {
	region, sub, _ := strings.Cut(name, "/")
	base := "regions/" + region + "/subnetworks/" + sub
	pol, err := p.call("GET", host, base+"/getIamPolicy", nil, nil)
	if err != nil {
		return err
	}
	bindings, _ := pol["bindings"].([]any)
	found := false
	for _, b := range bindings {
		bm, _ := b.(map[string]any)
		if vpcStr(bm, "role") != "roles/compute.networkUser" || bm["condition"] != nil {
			continue
		}
		found = true
		have, _ := bm["members"].([]any)
		for _, m := range members {
			dup := false
			for _, h := range have {
				dup = dup || h == m
			}
			if !dup {
				have = append(have, m)
			}
		}
		bm["members"] = have
	}
	if !found {
		bindings = append(bindings, map[string]any{"role": "roles/compute.networkUser", "members": members})
	}
	pol["bindings"] = bindings
	_, err = p.call("POST", host, base+"/setIamPolicy", nil, map[string]any{"policy": pol})
	return err
}

// subnetUsers are the principals granted Compute Network User on the
// subnet s of host.
func (p computeSharedVPCProvider) subnetUsers(host string, s map[string]any) []string {
	pol, err := p.call("GET", host, "regions/"+vpcLast(vpcStr(s, "region"))+"/subnetworks/"+vpcStr(s, "name")+"/getIamPolicy", nil, nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, b := range vmMaps(pol, "bindings") {
		if vpcStr(b, "role") == "roles/compute.networkUser" {
			ms, _ := b["members"].([]any)
			for _, m := range ms {
				if ms, ok := m.(string); ok {
					out = append(out, ms)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// usableSubnets are the host's subnets a service project can use, as
// subnetworks.listUsable lists them (the project's own left out).
func (p computeSharedVPCProvider) usableSubnets(project, host string) []console.Resource {
	m, err := p.call("GET", project, "aggregated/subnetworks/listUsable", nil, nil)
	if err != nil {
		return nil
	}
	var out []console.Resource
	for _, u := range computeItems(m) {
		link := vpcStr(u, "subnetwork")
		i := strings.Index(link, "projects/")
		if i < 0 {
			continue
		}
		parts := strings.Split(link[i:], "/")
		if len(parts) != 6 || parts[1] != host {
			continue
		}
		row := sharedSubnetRow(host, map[string]any{"name": parts[5], "region": parts[3], "network": u["network"],
			"ipCidrRange": u["ipCidrRange"], "secondaryIpRanges": u["secondaryIpRanges"]})
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Fields, out[j].Fields
		return a["Network"]+"/"+a["Region"]+"/"+a["Subnet"] < b["Network"]+"/"+b["Region"]+"/"+b["Subnet"]
	})
	return out
}

func projectNamesField() console.Field {
	return console.Field{Name: "projects", Label: "Project names", Type: "text", Required: true,
		Help: "The service projects to attach, by project ID, separated by commas. Each must be in the host project's organization and attached to no other host."}
}

func (p computeSharedVPCProvider) ListActions(ctx context.Context, project string) []console.Action {
	if p.api() == nil || project == "" {
		return nil
	}
	role, _, err := p.xpnRole(project)
	if err != nil {
		return nil
	}
	switch role {
	case "host":
		return []console.Action{
			{ID: "attach", Label: "Attach projects", Submit: "Save", Fields: append([]console.Field{projectNamesField(), sharingModeField()}, p.subnetFields(project, "sharing=individual")...)},
			{ID: "share", Label: "Share subnets", Submit: "Save", Title: "Share subnets", Fields: p.subnetFields(project, "")},
			{ID: "disable", Label: "Disable Shared VPC", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Disable Shared VPC for host project " + project + "? Detach every service project first."},
		}
	case "service":
		return nil
	}
	return []console.Action{{ID: "setup", Label: "Set up Shared VPC", Submit: "Save", Title: "Set up Shared VPC",
		Fields: append([]console.Field{
			{Name: "enable", Label: "Enable host project", Type: "checkbox", Default: "true", Help: "Makes " + project + " a Shared VPC host project."},
			sharingModeField(), {Name: "projects", Label: "Project names", Type: "text",
				Help: "The service projects to attach, by project ID, separated by commas (optional: attach them later)."}}, p.subnetFields(project, "sharing=individual")...)}}
}

func (p computeSharedVPCProvider) attach(project, list string) error {
	for _, id := range strings.Split(list, ",") {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if _, err := p.call("POST", project, "enableXpnResource", nil, map[string]any{"xpnResource": map[string]any{"id": id, "type": "PROJECT"}}); err != nil {
			return err
		}
	}
	return nil
}

// DetailActions: the page has no resource of its own to act on; its
// actions are on the list (ListActions). Without it the provider is no
// console.PathActor, and the console drops its list actions.
func (p computeSharedVPCProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	return nil
}

var _ console.ListActor = computeSharedVPCProvider{}

func (p computeSharedVPCProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	individual := values["sharing"] == "individual"
	if individual {
		if _, _, err := shareChoice(values); err != nil {
			return err
		}
	}
	switch {
	case len(path) == 0 && action == "setup":
		if _, err := p.call("POST", project, "enableXpnHost", nil, nil); err != nil {
			return err
		}
		if err := p.attach(project, values["projects"]); err != nil {
			return err
		}
		if individual {
			return p.shareSubnets(project, values)
		}
		return nil
	case len(path) == 0 && action == "attach":
		if strings.TrimSpace(values["projects"]) == "" {
			return errors.New("Project names: name at least one project to attach")
		}
		if err := p.attach(project, values["projects"]); err != nil {
			return err
		}
		if individual {
			return p.shareSubnets(project, values)
		}
		return nil
	case len(path) == 0 && action == "share":
		return p.shareSubnets(project, values)
	case len(path) == 0 && action == "disable":
		_, err := p.call("POST", project, "disableXpnHost", nil, nil)
		return err
	case len(path) == 1 && action == "detach":
		_, err := p.call("POST", project, "disableXpnResource", nil, map[string]any{"xpnResource": map[string]any{"id": path[0], "type": "PROJECT"}})
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}
