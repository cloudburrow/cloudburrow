package main

// The organization's page on the Resource hierarchy screen (#1554): what
// the v3 Organizations API's GetOrganization answers, the folders and
// projects directly under it, its organization policies with Set and Clear
// organization policy (#1534), and its Permissions tab
// (consolehierarchypermissions.go), as Google's console offers them once the
// organization is picked in the resource selector, and its Log router tab
// (#1572).

import (
	"context"

	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// organizationSegment is the organization's page under /resource-hierarchy.
// A folder's segment is its number, so a word cannot be mistaken for one.
const organizationSegment = "organization"

const organizationLink = "/resource-hierarchy/" + organizationSegment

func isOrganizationPath(path []string) bool {
	return len(path) == 1 && path[0] == organizationSegment
}

func (p hierarchyProvider) organizationDetail(ctx context.Context) console.Detail {
	org, err := resourcemanager.NewOrganizationsServer(p.registry).GetOrganization(ctx,
		&resourcemanagerpb.GetOrganizationRequest{Name: resourcemanager.OrganizationName})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the organization: " + consoleErr(err)}
	}
	props := []console.Property{
		{Label: "Organization name", Value: org.GetDisplayName()},
		{Label: "Resource name", Value: org.GetName()},
		{Label: "Directory customer ID", Value: org.GetDirectoryCustomerId()},
		{Label: "State", Value: org.GetState().String()},
		{Label: "Created", Value: org.GetCreateTime().AsTime().Local().Format("2006-01-02 15:04")},
	}
	folders := console.Listing{NameColumn: "Folder ID", Columns: []string{"Name"}, Noun: "folders"}
	if _, all, err := p.folders(); err != nil {
		folders.Unavailable = consoleErr(err)
	} else {
		for _, f := range all {
			if f.Parent == resourcemanager.OrganizationName && f.State == resourcemanager.StateActive {
				folders.Items = append(folders.Items, console.Resource{Name: f.ID(), Status: string(f.State),
					Fields: map[string]string{"Name": f.DisplayName}, Link: "/resource-hierarchy/" + f.ID()})
			}
		}
	}
	folders.Total = len(folders.Items)
	projects := console.Listing{NameColumn: "ID", Columns: []string{"Name"}, Noun: "projects"}
	if ps, err := p.registry.Active(); err == nil {
		for _, pr := range ps {
			if pr.Parent == resourcemanager.OrganizationName {
				projects.Items = append(projects.Items, console.Resource{Name: pr.ProjectID, Status: string(pr.State), Fields: map[string]string{"Name": pr.DisplayName}})
			}
		}
	}
	projects.Total = len(projects.Items)
	sections := []console.Section{
		{ID: "folders", Label: "Folders", Listing: folders},
		{ID: "projects", Label: "Projects", Listing: projects},
		{ID: "org-policies", Label: "Organization policies", Listing: orgPolicyListing(p.registry, resourcemanager.OrganizationName)},
	}
	// Its log sinks (#1572).
	props, sections = p.logRouter(ctx, resourcemanager.OrganizationName, props, sections)
	return console.Detail{
		Summary: props[:2],
		Sections: append([]console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Organization", Properties: props}}},
		}, sections...),
	}
}
