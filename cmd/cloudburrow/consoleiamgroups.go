package main

// IAM & Admin > Groups (#1812): the Cloud Identity groups of the
// instance's customer, which a policy names as group:EMAIL, with Create
// group, Delete, and on a group's page its members (users, service
// accounts and nested groups), Add member and Remove member, over the same
// registry Cloud Identity's groups and memberships API serves, so a group
// made here is one the official client and IAM see.
//
// #1817 adds the rest of the API: the list's search box (groups.search),
// Change roles and a MEMBER expiry on a member's row
// (modifyMembershipRoles), Add member with an expiry, the group's
// Transitive members tab (searchTransitiveMemberships), and a member's page
// with its membership graph up to the group (getMembershipGraph) and the
// groups it belongs to directly (searchDirectGroups); see
// consoleiamgroupsmembers.go.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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

func (p iamGroupsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	return p.ListFiltered(ctx, project, "")
}

// ListFiltered is the customer's groups, or with a search the ones
// groups.search finds (#1817): a word is matched against the group's
// email (group_key.startsWith) and display name (display_name.contains),
// as the Groups page's search box matches either; a groups.search query
// is evaluated as it is, its parent supplied when it names none.
func (p iamGroupsProvider) ListFiltered(_ context.Context, _ string, filter string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Email", Columns: []string{"Display name", "Type", "Members"}, Noun: "groups", Note: iamGroupsNote,
		ServerFilter: &console.ServerFilter{Value: filter, Placeholder: "eng, or group_key.startsWith('eng') && '" + resourcemanager.LabelSecurity + "' in labels",
			Help: iamGroupsSearchHelp}}
	var gs []*resourcemanager.Group
	var err error
	if strings.TrimSpace(filter) == "" {
		gs, err = p.registry.ListGroups(resourcemanager.CustomerName)
		if err != nil {
			l.Unavailable = consoleErr(err)
			return l, nil
		}
	} else if gs, err = searchGroups(p.registry, filter); err != nil {
		l.ServerFilter.Error = consoleErr(err)
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
		{Name: "email", Label: "Group email", Type: "text", Required: true, Pattern: groupEmailPattern,
			Help: "NAME@" + resourcemanager.OrganizationDomain + ": the customer's groups are in its domain. NAME follows a username's rules: " +
				"at most 64 characters, no period at the start or end or two in a row, and no = & < > + , or !."},
		groupDisplayNameField(""),
		{Name: "description", Label: "Description", Type: "text", Help: "At most 4,096 characters."},
		{Name: "security", Label: "Security group", Type: "select", Options: []string{"No", "Yes"}, Default: "No",
			Help: "Adds the groups.security label, which cannot be removed afterwards."},
	}
}

// groupEmailPattern is a group email's shape in the instance's domain:
// periods only between other characters, none of = & < > + , !
// (checkGroupEmail has the length and the sources).
var groupEmailPattern = `[^\s.=&<>+,!@]+(\.[^\s.=&<>+,!@]+)*@` + regexp.QuoteMeta(resourcemanager.OrganizationDomain)

// groupDisplayNameField is the display name, "up to 73 characters".
func groupDisplayNameField(def string) console.Field {
	return console.Field{Name: "displayName", Label: "Display name", Type: "text", Default: def, Pattern: `.{0,73}`,
		Help: "At most 73 characters."}
}

// addMemberHelp explains who the group can take: a security group holds
// users, service accounts and security groups only.
func addMemberHelp(g *resourcemanager.Group) string {
	h := "A user's, a service account's or another group's email. A nested group's members are this group's too."
	if _, ok := g.Labels[resourcemanager.LabelSecurity]; ok {
		h += " This is a security group: it can only contain users, service accounts and security groups, so a group that is not a security group is refused."
	}
	return h
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
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	g, err := p.group(path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the group: " + consoleErr(err)}, nil
	}
	if len(path) == 2 {
		return p.memberDetail(g, path[1]), nil
	}
	members := console.Listing{NameColumn: "Member", Columns: []string{"Type", "Roles", "Expires"}, Noun: "members", RowsOpenable: true}
	ms := append([]resourcemanager.GroupMember(nil), g.Members...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Email < ms[j].Email })
	for _, m := range ms {
		members.Items = append(members.Items, console.Resource{Name: m.Email,
			Fields:  map[string]string{"Type": m.Type, "Roles": strings.Join(m.Roles, ", "), "Expires": memberExpiry(m)},
			Actions: memberActions(m)})
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
			parents.Items = append(parents.Items, console.Resource{Name: x.Group.Email, Fields: map[string]string{"Relation": relationLabel(x.Direct, x.Indirect)}})
		}
	}
	parents.Total = len(parents.Items)
	return console.Detail{Summary: props[:2], Sections: []console.Section{
		{ID: "members", Label: "Members", Listing: members},
		p.transitiveSection(g),
		{ID: "member-of", Label: "Member of", Listing: parents},
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Group", Properties: props}}},
	}, Edit: &console.EditForm{Label: "Edit group", Fields: []console.Field{
		groupDisplayNameField(g.DisplayName),
		{Name: "description", Label: "Description", Type: "text", Default: g.Description, Help: "At most 4,096 characters."},
		{Name: "add", Label: "Add member", Type: "text",
			Help: addMemberHelp(g)},
		{Name: "role", Label: "Role", Type: "select", Options: []string{"MEMBER", "MANAGER", "OWNER"}, Default: "MEMBER"},
		{Name: "expire", Label: "Member expiry", Type: "text",
			Help: "Optional: when the added member's MEMBER role expires, RFC 3339 (2027-01-31T00:00:00Z), in the future. Only the MEMBER role takes an expiry."},
		{Name: "remove", Label: "Remove member", Type: "text", Help: "The email of a member to remove."},
	}, Note: "Add member and Remove member are optional. Change a member's roles or expiry from its row in Members."}}, nil
}

func (p iamGroupsProvider) Edit(_ context.Context, _ string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("only a group can be edited; change a member's roles from its row")
	}
	g, err := p.group(path[0])
	if err != nil {
		return err
	}
	add, remove := strings.TrimSpace(values["add"]), strings.TrimSpace(values["remove"])
	if add == "" && strings.TrimSpace(values["expire"]) != "" {
		return errors.New("a member expiry needs a member to add; change an existing member's expiry from its row in Members")
	}
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
		expire, err := parseMemberExpiry(values["expire"])
		if err != nil {
			return err
		}
		if _, _, err := p.registry.CreateMembershipExpiring(g.Name(), add, "", roles, expire); err != nil {
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
var _ console.FilterLister = iamGroupsProvider{}
var _ console.PathActor = iamGroupsProvider{}
