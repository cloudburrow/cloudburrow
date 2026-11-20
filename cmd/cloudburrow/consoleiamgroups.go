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

// CreateForm is Google's Create group (#1864): Group name, Group email
// and Group description, then the members to add with their Google Groups
// role, on one page.
// CreateSubmit implements console.CreateSubmitter: the create flow ends
// in Submit, as docs.cloud.google.com/iam/docs/groups-in-cloud-console words it (#1933).
func (iamGroupsProvider) CreateSubmit() string { return "Submit" }

func (iamGroupsProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{
		{Name: "displayName", Label: "Group name", Type: "text", Pattern: `.{0,73}`, Section: "Group details", Help: "At most 73 characters."},
		{Name: "email", Label: "Group email", Type: "text", Required: true, Pattern: groupEmailPattern, Section: "Group details",
			Help: "NAME@" + resourcemanager.OrganizationDomain + ": the customer's groups are in its domain. NAME follows a username's rules: " +
				"at most 64 characters, no period at the start or end or two in a row, and no = & < > + , or !."},
		{Name: "description", Label: "Group description", Type: "text", Section: "Group details", Help: "Optional. At most 4,096 characters."},
		{Name: "security", Label: "Security group", Type: "select", Options: []string{"No", "Yes"}, Default: "No", Section: "Group details",
			Help: "Adds the groups.security label, which cannot be removed afterwards."},
		{Name: "members", Label: "Member email", Type: "textarea", Section: "Add members",
			Help: "Optional: users, service accounts or groups to add, one per line or separated by commas."},
		{Name: "memberRole", Label: "Google Groups role", Type: "select", Options: []string{"MEMBER", "MANAGER", "OWNER"},
			OptionLabels: map[string]string{"MEMBER": "Member", "MANAGER": "Manager", "OWNER": "Owner"}, Default: "MEMBER", Section: "Add members"},
	}
}

func (iamGroupsProvider) CreateOnPage() bool   { return true }
func (iamGroupsProvider) CreateLayout() string { return console.CreateLayoutPage }
func (iamGroupsProvider) CreateTitle() string  { return "Create group" }

// groupEmailPattern is a group email's shape in the instance's domain:
// periods only between other characters, none of = & < > + , !
// (checkGroupEmail has the length and the sources).
var groupEmailPattern = `[^\s.=&<>+,!@]+(\.[^\s.=&<>+,!@]+)*@` + regexp.QuoteMeta(resourcemanager.OrganizationDomain)

// addMemberHelp explains who the group can take: a security group holds
// users, service accounts and security groups only.
func addMemberHelp(g *resourcemanager.Group) string {
	h := "A user's, a service account's or another group's email. A nested group's members are this group's too."
	if _, ok := g.Labels[resourcemanager.LabelSecurity]; ok {
		h += " This is a security group: it can only contain users, service accounts and security groups, so a group that is not a security group is refused."
	}
	return h
}

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
	roles := []string{"MEMBER"}
	if r := values["memberRole"]; r == "MANAGER" || r == "OWNER" {
		roles = []string{r, "MEMBER"}
	}
	for _, m := range strings.FieldsFunc(values["members"], func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' }) {
		if _, _, err := p.registry.CreateMembership(g.Name(), m, "", roles); err != nil {
			return g.Email, fmt.Errorf("created %s, but could not add %s: %w", g.Email, m, err)
		}
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
	cols := []string{"Type", "Roles", "Expires"}
	if g.MemberRestriction != "" {
		cols = append(cols, "Restriction")
	}
	members := console.Listing{NameColumn: "Member", Columns: cols, Noun: "members", RowsOpenable: true}
	ms := append([]resourcemanager.GroupMember(nil), g.Members...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Email < ms[j].Email })
	for _, m := range ms {
		members.Items = append(members.Items, console.Resource{Name: m.Email,
			Fields: map[string]string{"Type": m.Type, "Roles": strings.Join(m.Roles, ", "), "Expires": memberExpiry(m),
				"Restriction": orDash(p.registry.MemberEvaluation(g, m.Email))},
			Actions: memberActions(m), Selectable: true})
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
	// Security settings (#1959): the member restriction and its state.
	security := []console.Property{{Label: "Member restriction", Value: orDash(g.MemberRestriction)}}
	if ss, err := p.registry.GetSecuritySettings(g.Name() + "/securitySettings"); err == nil && ss.Evaluation != "" {
		security = append(security, console.Property{Label: "Restriction state", Value: ss.Evaluation})
	}
	// Google's group page (#1864): Add members over the Members table,
	// Remove members on its checked rows, and Edit group's Group name and
	// Group description, then Save
	// (docs.cloud.google.com/iam/docs/groups-in-cloud-console).
	members.SelectActions = groupSelectActions(g)
	return console.Detail{Summary: props[:2], Sections: []console.Section{
		{ID: "members", Label: "Members", Listing: members, Actions: []console.Action{addMembersAction(g)}},
		p.transitiveSection(g),
		{ID: "member-of", Label: "Member of", Listing: parents},
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Group", Properties: props},
			{Heading: "Security settings", Properties: security}}},
	}, Edit: &console.EditForm{Label: "Edit group", Submit: "Save", Fields: []console.Field{
		{Name: "displayName", Label: "Group name", Type: "text", Default: g.DisplayName, Pattern: `.{0,73}`, Help: "At most 73 characters."},
		{Name: "description", Label: "Group description", Type: "text", Default: g.Description, Help: "At most 4,096 characters."},
		{Name: "memberRestriction", Label: "Member restriction", Type: "text", Default: g.MemberRestriction,
			Help: "Who can be added directly, in CEL: member.type == 1 (users), 2 (service accounts) or 3 (groups), and " +
				"member.customer_id == groupCustomerId() (only the organization's), joined by && and ||; for example " +
				"member.type == 1 && member.customer_id == groupCustomerId(). Empty for no restriction. Existing members stay; the state says whether they adhere."},
	}, Note: "The group email cannot change. Add members and Remove members are on the Members tab."}}, nil
}

const groupMembersField = "members"

// addMembersAction is Google's Add members: member emails and their
// Google Groups role.
func addMembersAction(g *resourcemanager.Group) console.Action {
	return console.Action{ID: "add-members", Label: "Add members", Submit: "Add", Fields: []console.Field{
		{Name: "members", Label: "Member email", Type: "textarea", Required: true,
			Help: addMemberHelp(g) + " Separate several with commas or lines."},
		{Name: "role", Label: "Google Groups role", Type: "select", Options: []string{"MEMBER", "MANAGER", "OWNER"}, Default: "MEMBER"},
		{Name: "expire", Label: "Member expiry", Type: "text",
			Help: "Optional: when the added members' MEMBER role expires, RFC 3339 (2027-01-31T00:00:00Z), in the future. Only the MEMBER role takes an expiry."},
	}}
}

// groupSelectActions is Remove members, over the checked member rows.
func groupSelectActions(g *resourcemanager.Group) []console.Action {
	return []console.Action{{ID: "remove-members", Label: "Remove members", Destructive: true, SelectionField: groupMembersField,
		Fields:  []console.Field{{Name: groupMembersField, Type: "hidden"}},
		Confirm: "the checked members' memberships of " + g.Email + ": they lose every role the group grants"}}
}

// groupPageActions are the group page's Add members, Remove members,
// View in Google Groups (not served) and Delete group.
func groupPageActions(g *resourcemanager.Group) []console.Action {
	out := []console.Action{addMembersAction(g)}
	out = append(out, groupSelectActions(g)...)
	return append(out,
		console.Action{ID: "view-in-groups", Label: "View in Google Groups",
			Unavailable: "Not available locally: Google Groups (groups.google.com) is not part of CloudBurrow; the group's members and settings are on this page."},
		console.Action{ID: "delete-group", Label: "Delete group", Destructive: true, Leaves: true,
			Confirm: "the group " + g.Email + " and every membership in it"})
}

// actOnGroup performs a group page's action.
func (p iamGroupsProvider) actOnGroup(g *resourcemanager.Group, action string, values map[string]string) error {
	split := func(v string) []string {
		return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' })
	}
	switch action {
	case "delete-group":
		return p.registry.DeleteGroup(g.Name())
	case "remove-members":
		ms := split(values[groupMembersField])
		if len(ms) == 0 {
			return errors.New("check the members to remove")
		}
		for _, m := range ms {
			name, err := p.registry.LookupMembership(g.Name(), m, "")
			if err != nil {
				return err
			}
			if err := p.registry.DeleteMembership(name); err != nil {
				return err
			}
		}
		return nil
	case "add-members":
		ms := split(values["members"])
		if len(ms) == 0 {
			return errors.New("give a member email")
		}
		roles := []string{"MEMBER"}
		if r := values["role"]; r == "MANAGER" || r == "OWNER" {
			roles = []string{r, "MEMBER"}
		}
		expire, err := parseMemberExpiry(values["expire"])
		if err != nil {
			return err
		}
		for _, m := range ms {
			if _, _, err := p.registry.CreateMembershipExpiring(g.Name(), m, "", roles, expire); err != nil {
				return fmt.Errorf("could not add %s: %w", m, err)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown action %q", action)
}

func (p iamGroupsProvider) Edit(_ context.Context, _ string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("only a group can be edited; change a member's roles from its row")
	}
	g, err := p.group(path[0])
	if err != nil {
		return err
	}
	restriction, restrictionChanged := values["memberRestriction"]
	restrictionChanged = restrictionChanged && strings.TrimSpace(restriction) != g.MemberRestriction
	if restrictionChanged {
		if _, err := p.registry.UpdateSecuritySettings(g.Name()+"/securitySettings", restriction, []string{"member_restriction.query"}); err != nil {
			return err
		}
	}
	if dn, ok := values["displayName"]; ok && (dn != g.DisplayName || values["description"] != g.Description) {
		_, err := p.registry.UpdateGroup(g.Name(), resourcemanager.GroupInput{DisplayName: dn, Description: values["description"]},
			[]string{"display_name", "description"})
		return err
	} else if !restrictionChanged {
		return errors.New("change the group name, the description or the member restriction")
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
