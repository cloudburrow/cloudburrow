package main

// IAM & Admin > Groups (#1812): the Cloud Identity groups of the
// instance's customer, which a policy names as group:EMAIL, with Create
// group, Delete, and on a group's page its members (users, service
// accounts and nested groups), Add member and Remove member, over the same
// registry Cloud Identity's groups and memberships API serves, so a group
// made here is one the official client and IAM see.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type iamGroupsProvider struct {
	registry *resourcemanager.Registry
}

func (iamGroupsProvider) ID() string    { return "iam-groups" }
func (iamGroupsProvider) Title() string { return "Groups" }

const iamGroupsNote = "Cloud Identity groups of the organization's customer (" + resourcemanager.CustomerName + "), in its domain, " +
	resourcemanager.OrganizationDomain + ". Grant a group a role on the IAM page as group:EMAIL, or a dataset's access as groupByEmail: " +
	"its members, and the members of the groups nested in it, hold it where CloudBurrow evaluates policies " +
	"(BigQuery for minted tokens, Memorystore IAM auth, Firestore with MongoDB compatibility, IAM Credentials). " +
	"domain:" + resourcemanager.OrganizationDomain + " names every account of the domain."

func (p iamGroupsProvider) group(row string) (*resourcemanager.Group, error) {
	name, err := p.registry.LookupGroup(row, "")
	if err != nil {
		return nil, err
	}
	return p.registry.GetGroup(name)
}

func groupType(g *resourcemanager.Group) string {
	if _, ok := g.Labels[resourcemanager.LabelSecurity]; ok {
		return "Security"
	}
	return "Google Group"
}

func (p iamGroupsProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Email", Columns: []string{"Display name", "Type", "Members"}, Noun: "groups", Note: iamGroupsNote}
	gs, err := p.registry.ListGroups(resourcemanager.CustomerName)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, g := range gs {
		l.Items = append(l.Items, console.Resource{Name: g.Email, Status: "ACTIVE", Fields: map[string]string{
			"Display name": orDash(g.DisplayName), "Type": groupType(g), "Members": strconv.Itoa(len(g.Members))}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func (iamGroupsProvider) CreateForm() (string, []console.Field) {
	return "Create group", []console.Field{
		{Name: "email", Label: "Group email", Type: "text", Required: true,
			Help: "NAME@" + resourcemanager.OrganizationDomain + ": the customer's groups are in its domain."},
		{Name: "displayName", Label: "Display name", Type: "text"},
		{Name: "description", Label: "Description", Type: "text", Help: "At most 4,096 characters."},
		{Name: "security", Label: "Security group", Type: "select", Options: []string{"No", "Yes"}, Default: "No",
			Help: "Adds the groups.security label, which cannot be removed afterwards."},
	}
}

func (iamGroupsProvider) CreateOnPage() bool { return true }

func (p iamGroupsProvider) Create(_ context.Context, _ string, values map[string]string) (string, error) {
	labels := map[string]string{resourcemanager.LabelDiscussionForum: ""}
	if values["security"] == "Yes" {
		labels[resourcemanager.LabelSecurity] = ""
	}
	g, err := p.registry.CreateGroup(resourcemanager.GroupInput{Email: values["email"], Parent: resourcemanager.CustomerName,
		DisplayName: values["displayName"], Description: values["description"], Labels: labels})
	if err != nil {
		return "", err
	}
	return g.Email, nil
}

func (p iamGroupsProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	g, err := p.group(path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the group: " + consoleErr(err)}, nil
	}
	members := console.Listing{NameColumn: "Member", Columns: []string{"Type", "Roles"}, Noun: "members"}
	ms := append([]resourcemanager.GroupMember(nil), g.Members...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Email < ms[j].Email })
	for _, m := range ms {
		members.Items = append(members.Items, console.Resource{Name: m.Email,
			Fields: map[string]string{"Type": m.Type, "Roles": strings.Join(m.Roles, ", ")}})
	}
	members.Total = len(members.Items)
	var labels []string
	for k := range g.Labels {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	props := []console.Property{
		{Label: "Email", Value: g.Email},
		{Label: "Resource name", Value: g.Name()},
		{Label: "Display name", Value: orDash(g.DisplayName)},
		{Label: "Description", Value: orDash(g.Description)},
		{Label: "Labels", Value: strings.Join(labels, ", ")},
		{Label: "Created", Value: g.CreateTime.UTC().Format("2006-01-02 15:04:05 UTC")},
	}
	// The groups this group belongs to, directly or through nested
	// groups (searchTransitiveGroups).
	parents := console.Listing{NameColumn: "Group", Columns: []string{"Relation"}, Noun: "groups"}
	if rels, err := p.registry.TransitiveGroups(g.Email); err == nil {
		for _, x := range rels {
			rel := "Indirect"
			switch {
			case x.Direct && x.Indirect:
				rel = "Direct and indirect"
			case x.Direct:
				rel = "Direct"
			}
			parents.Items = append(parents.Items, console.Resource{Name: x.Group.Email, Fields: map[string]string{"Relation": rel}})
		}
	}
	parents.Total = len(parents.Items)
	return console.Detail{Summary: props[:2], Sections: []console.Section{
		{ID: "members", Label: "Members", Listing: members},
		{ID: "member-of", Label: "Member of", Listing: parents},
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Group", Properties: props}}},
	}, Edit: &console.EditForm{Label: "Edit group", Fields: []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Default: g.DisplayName},
		{Name: "description", Label: "Description", Type: "text", Default: g.Description, Help: "At most 4,096 characters."},
		{Name: "add", Label: "Add member", Type: "text",
			Help: "A user's, a service account's or another group's email. A nested group's members are this group's too."},
		{Name: "role", Label: "Role", Type: "select", Options: []string{"MEMBER", "MANAGER", "OWNER"}, Default: "MEMBER"},
		{Name: "remove", Label: "Remove member", Type: "text", Help: "The email of a member to remove."},
	}, Note: "Add member and Remove member are optional."}}, nil
}

func (p iamGroupsProvider) Edit(_ context.Context, _ string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("only a group can be edited")
	}
	g, err := p.group(path[0])
	if err != nil {
		return err
	}
	add, remove := strings.TrimSpace(values["add"]), strings.TrimSpace(values["remove"])
	if dn, ok := values["displayName"]; ok && (dn != g.DisplayName || values["description"] != g.Description) {
		if _, err := p.registry.UpdateGroup(g.Name(), resourcemanager.GroupInput{DisplayName: dn, Description: values["description"]},
			[]string{"display_name", "description"}); err != nil {
			return err
		}
	} else if add == "" && remove == "" {
		return errors.New("change the display name or description, or give a member to add or to remove")
	}
	if remove != "" {
		name, err := p.registry.LookupMembership(g.Name(), remove, "")
		if err != nil {
			return err
		}
		if err := p.registry.DeleteMembership(name); err != nil {
			return err
		}
	}
	if add != "" {
		roles := []string{"MEMBER"}
		if r := values["role"]; r != "" && r != "MEMBER" {
			roles = []string{r, "MEMBER"}
		}
		if _, _, err := p.registry.CreateMembership(g.Name(), add, "", roles); err != nil {
			return err
		}
	}
	return nil
}

func (p iamGroupsProvider) Delete(_ context.Context, _ string, row string) error {
	g, err := p.group(row)
	if err != nil {
		return err
	}
	return p.registry.DeleteGroup(g.Name())
}

var _ console.Creator = iamGroupsProvider{}
var _ console.PageCreator = iamGroupsProvider{}
var _ console.Deleter = iamGroupsProvider{}
var _ console.Driller = iamGroupsProvider{}
var _ console.Editor = iamGroupsProvider{}
