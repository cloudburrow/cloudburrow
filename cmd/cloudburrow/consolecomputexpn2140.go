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
// sees the host's subnets it can use, as `gcloud compute networks subnets
// list-usable` lists them (project, region, network, subnet, range,
// secondary ranges).
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
		for _, s := range p.hostSubnets(host) {
			l.Items = append(l.Items, sharedSubnetRow(host, s))
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
				Actions: []console.Action{{ID: "detach", Label: "Detach", Destructive: true, Confirm: "Detach service project " + id + " from this host project?"}}})
		}
		listing.Total = len(listing.Items)
		var groups []console.PropertyGroup
		for _, s := range p.hostSubnets(project) {
			groups = append(groups, console.PropertyGroup{Heading: vpcLast(vpcStr(s, "network")) + " / " + vpcStr(s, "name"),
				Properties: []console.Property{{Label: "Region", Value: vpcLast(vpcStr(s, "region"))},
					{Label: "IP address range", Value: vpcStr(s, "ipCidrRange")},
					{Label: "Secondary IP ranges", Value: vpcDash(secondaryText(s))}}})
		}
		listing.Tabs = []console.Section{
			{ID: "subnets", Label: "Subnets in host project", Groups: groups,
				Note: "Every subnet of this host project is shared with its attached projects (Share all subnets)."},
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
		OptionLabels:       map[string]string{"all": "Share all subnets (project-level permissions)", "individual": "Individual subnets (subnet-level permissions)"},
		UnavailableOptions: map[string]string{"individual": "Subnet-level IAM policies (subnetworks.setIamPolicy) are not served: share all subnets, with compute.networkUser on the host project."}}
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
			{ID: "attach", Label: "Attach projects", Submit: "Save", Fields: []console.Field{projectNamesField(), sharingModeField()}},
			{ID: "disable", Label: "Disable Shared VPC", Destructive: true, Confirm: "Disable Shared VPC for host project " + project + "? Detach every service project first."},
		}
	case "service":
		return nil
	}
	return []console.Action{{ID: "setup", Label: "Set up Shared VPC", Submit: "Save", Title: "Set up Shared VPC",
		Fields: []console.Field{
			{Name: "enable", Label: "Enable host project", Type: "checkbox", Default: "true", Help: "Makes " + project + " a Shared VPC host project."},
			sharingModeField(), {Name: "projects", Label: "Project names", Type: "text",
				Help: "The service projects to attach, by project ID, separated by commas (optional: attach them later)."}}}}
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

func (p computeSharedVPCProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if values["sharing"] == "individual" {
		return errors.New("Individual subnets (subnet-level permissions) are not served: share all subnets")
	}
	switch {
	case len(path) == 0 && action == "setup":
		if _, err := p.call("POST", project, "enableXpnHost", nil, nil); err != nil {
			return err
		}
		return p.attach(project, values["projects"])
	case len(path) == 0 && action == "attach":
		if strings.TrimSpace(values["projects"]) == "" {
			return errors.New("Project names: name at least one project to attach")
		}
		return p.attach(project, values["projects"])
	case len(path) == 0 && action == "disable":
		_, err := p.call("POST", project, "disableXpnHost", nil, nil)
		return err
	case len(path) == 1 && action == "detach":
		_, err := p.call("POST", project, "disableXpnResource", nil, map[string]any{"xpnResource": map[string]any{"id": path[0], "type": "PROJECT"}})
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}
