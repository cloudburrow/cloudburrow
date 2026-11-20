package main

// IAM & Admin > Groups, the rest of Cloud Identity (#1817): the list's
// search box (groups.search), a group's Transitive members tab
// (searchTransitiveMemberships), a member's page with its membership graph
// up to the group (getMembershipGraph) and the groups it is a direct member
// of (searchDirectGroups), and Change roles on a member's row and page
// (modifyMembershipRoles: OWNER and MANAGER added or removed, and the
// MEMBER role's expiry), as the Admin console's Members page offers a
// member's role and its "Set expiration" beside it.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

const iamGroupsSearchHelp = "A word finds the groups whose email starts with it or whose display name contains it. " +
	"Or a groups.search query: group_key or display_name with ==, startsWith('…') or contains('…'), '…' in labels " +
	"(" + resourcemanager.LabelSecurity + " for security groups) and domain_name == '…', joined by &&; " +
	"parent == '" + resourcemanager.CustomerName + "' is added when the query names no parent."

// searchWord is a search-box entry that is a word, not a query.
var searchWord = regexp.MustCompile(`^[\pL\pN@._+\- ]+$`)

// searchGroups runs the Groups page's search through groups.search.
func searchGroups(reg *resourcemanager.Registry, filter string) ([]*resourcemanager.Group, error) {
	filter = strings.TrimSpace(filter)
	parent := "parent == '" + resourcemanager.CustomerName + "'"
	if !searchWord.MatchString(filter) {
		q := filter
		if !strings.Contains(q, "parent") {
			q = parent + " && " + q
		}
		return reg.SearchGroups(q)
	}
	// group_key.startsWith or display_name.contains: groups.search joins
	// terms with && only, so the two are searched apart and merged.
	byEmail := map[string]*resourcemanager.Group{}
	for _, q := range []string{
		parent + " && group_key.startsWith('" + strings.ToLower(filter) + "')",
		parent + " && display_name.contains('" + filter + "')",
	} {
		gs, err := reg.SearchGroups(q)
		if err != nil {
			return nil, err
		}
		for _, g := range gs {
			byEmail[g.Email] = g
		}
	}
	out := make([]*resourcemanager.Group, 0, len(byEmail))
	for _, g := range byEmail {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// relationLabel is a transitive relation as the console words it.
func relationLabel(direct, indirect bool) string {
	switch {
	case direct && indirect:
		return "Direct and indirect"
	case direct:
		return "Direct"
	}
	return "Indirect"
}

func memberExpiry(m resourcemanager.GroupMember) string {
	if m.ExpireTime == nil {
		return "Never"
	}
	return m.ExpireTime.UTC().Format(time.RFC3339)
}

// parseMemberExpiry reads an expiry field: empty is none.
func parseMemberExpiry(v string) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil, fmt.Errorf("member expiry %q is not an RFC 3339 time such as 2027-01-31T00:00:00Z", v)
	}
	return &t, nil
}

func hasRole(m resourcemanager.GroupMember, role string) bool {
	for _, r := range m.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// memberActions are Change roles and Remove member, on a direct member's
// row and page.
func memberActions(m resourcemanager.GroupMember) []console.Action {
	b := func(role string) string {
		if hasRole(m, role) {
			return "true"
		}
		return "false"
	}
	expire := ""
	if m.ExpireTime != nil {
		expire = m.ExpireTime.UTC().Format(time.RFC3339)
	}
	return []console.Action{
		{ID: "change-roles", Label: "Change roles", Fields: []console.Field{
			{Name: "owner", Label: "Owner", Type: "checkbox", Default: b("OWNER"), Help: "OWNER: manages the group's members and settings, and can delete it."},
			{Name: "manager", Label: "Manager", Type: "checkbox", Default: b("MANAGER"), Help: "MANAGER: manages the group's members."},
			{Name: "expire", Label: "Member expiry", Type: "text", Default: expire,
				Help: "When the MEMBER role, and so the membership, expires: RFC 3339 (2027-01-31T00:00:00Z), in the future. Empty for never. Every member keeps MEMBER."},
		}},
		{ID: "remove-member", Label: "Remove member", Destructive: true, Leaves: true,
			Confirm: "the membership of " + m.Email + ": it loses every role the group grants"},
	}
}

func (p iamGroupsProvider) member(path []string) (*resourcemanager.Group, resourcemanager.GroupMember, bool, error) {
	if len(path) != 2 {
		return nil, resourcemanager.GroupMember{}, false, errors.New("a member is addressed by its group and its email")
	}
	g, err := p.group(path[0])
	if err != nil {
		return nil, resourcemanager.GroupMember{}, false, err
	}
	email := strings.ToLower(strings.TrimSpace(path[1]))
	for _, m := range g.Members {
		if m.Email == email {
			return g, m, true, nil
		}
	}
	return g, resourcemanager.GroupMember{}, false, nil
}

func (p iamGroupsProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) == 1 {
		if g, err := p.group(path[0]); err == nil {
			return groupPageActions(g)
		}
		return nil
	}
	if len(path) != 2 {
		return nil
	}
	if _, m, ok, err := p.member(path); err == nil && ok {
		return memberActions(m)
	}
	return nil
}

func (p iamGroupsProvider) ActAt(_ context.Context, _ string, path []string, action string, values map[string]string) error {
	if len(path) == 1 {
		g, err := p.group(path[0])
		if err != nil {
			return err
		}
		return p.actOnGroup(g, action, values)
	}
	g, m, ok, err := p.member(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not a direct member of %s", path[1], g.Email)
	}
	name := resourcemanager.MembershipName(g, m)
	switch action {
	case "remove-member":
		return p.registry.DeleteMembership(name)
	case "change-roles":
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	var add []resourcemanager.RoleChange
	var remove []string
	for field, role := range map[string]string{"owner": "OWNER", "manager": "MANAGER"} {
		want := values[field] == "true"
		switch have := hasRole(m, role); {
		case want && !have:
			add = append(add, resourcemanager.RoleChange{Name: role})
		case !want && have:
			remove = append(remove, role)
		}
	}
	sort.Slice(add, func(i, j int) bool { return add[i].Name < add[j].Name })
	sort.Strings(remove)
	expire, err := parseMemberExpiry(values["expire"])
	if err != nil {
		return err
	}
	expiryChanged := (expire == nil) != (m.ExpireTime == nil) || (expire != nil && !expire.Equal(*m.ExpireTime))
	if len(add) == 0 && len(remove) == 0 && !expiryChanged {
		return errors.New("change a role or the member expiry")
	}
	// modifyMembershipRoles takes added and removed roles, or an expiry
	// update (updateRolesParams), never both in one request: two calls.
	if len(add) > 0 || len(remove) > 0 {
		if _, _, err := p.registry.ModifyMembershipRoles(name, add, remove, nil, false); err != nil {
			return err
		}
	}
	if expiryChanged {
		if _, _, err := p.registry.ModifyMembershipRoles(name, nil, nil,
			[]resourcemanager.RoleChange{{Name: "MEMBER", ExpireTime: expire}}, true); err != nil {
			return err
		}
	}
	return nil
}

// transitiveSection is a group's Transitive members tab: every member,
// direct or through nested groups (searchTransitiveMemberships).
func (p iamGroupsProvider) transitiveSection(g *resourcemanager.Group) console.Section {
	l := console.Listing{NameColumn: "Member", Columns: []string{"Type", "Relation", "Roles"}, Noun: "members"}
	sec := console.Section{ID: "transitive-members", Label: "Transitive members", Listing: l,
		Note: "Every member of the group, directly or through the groups nested in it, which IAM counts as the group's. Roles are a direct member's. Open a member for its membership graph."}
	rels, err := p.registry.TransitiveMembers(g.Name())
	if err != nil {
		sec.Unavailable = consoleErr(err)
		return sec
	}
	for _, x := range rels {
		roles := "-"
		if len(x.Roles) > 0 {
			roles = strings.Join(x.Roles, ", ")
		}
		typ := x.Type
		if typ == "" {
			typ = "-"
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: x.Email, Opens: []string{g.Email, x.Email},
			Fields: map[string]string{"Type": typ, "Relation": relationLabel(x.Direct, x.Indirect), "Roles": roles}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// memberDetail is a member's page under a group: its membership, when it
// is a direct member, the membership graph of its paths up to the group,
// and the groups it is a direct member of.
func (p iamGroupsProvider) memberDetail(g *resourcemanager.Group, email string) console.Detail {
	email = strings.ToLower(strings.TrimSpace(email))
	edges, err := p.registry.MembershipGraph(g.Name(), email, nil)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the membership graph: " + consoleErr(err)}
	}
	if len(edges) == 0 {
		return console.Detail{Unavailable: email + " is not a member of " + g.Email + ", directly or through a nested group"}
	}
	d := console.Detail{Summary: []console.Property{{Label: "Member", Value: email}, {Label: "Group", Value: g.Email}}}
	props := []console.Property{{Label: "Relation", Value: "Indirect: through a nested group"}}
	for _, m := range g.Members {
		if m.Email != email {
			continue
		}
		props = []console.Property{
			{Label: "Relation", Value: "Direct"},
			{Label: "Type", Value: m.Type},
			{Label: "Roles", Value: strings.Join(m.Roles, ", ")},
			{Label: "Member expiry", Value: memberExpiry(m)},
			{Label: "Membership", Value: resourcemanager.MembershipName(g, m)},
			{Label: "Added", Value: m.CreateTime.UTC().Format("2006-01-02 15:04:05 UTC")},
		}
	}
	paths := console.Listing{NameColumn: "Group", Columns: []string{"Member", "Roles"}, Noun: "memberships"}
	for _, e := range edges {
		for _, m := range e.Members {
			paths.Items = append(paths.Items, console.Resource{Name: e.Group.Email,
				Fields: map[string]string{"Member": m.Email, "Roles": strings.Join(m.Roles, ", ")}})
		}
	}
	paths.Total = len(paths.Items)
	direct := console.Listing{NameColumn: "Group", Columns: []string{"Display name", "Roles"}, Noun: "groups"}
	directSec := console.Section{ID: "direct-groups", Label: "Direct groups", Listing: direct,
		Note: "The groups " + email + " is a direct member of (searchDirectGroups)."}
	if dgs, err := p.registry.DirectGroups(email, nil); err != nil {
		directSec.Unavailable = consoleErr(err)
	} else {
		for _, x := range dgs {
			directSec.Listing.Items = append(directSec.Listing.Items, console.Resource{Name: x.Group.Email,
				Fields: map[string]string{"Display name": orDash(x.Group.DisplayName), "Roles": strings.Join(x.Member.Roles, ", ")}})
		}
		directSec.Listing.Total = len(directSec.Listing.Items)
	}
	d.Sections = []console.Section{
		{ID: "membership", Label: "Membership", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Membership", Properties: props}}},
		{ID: "graph", Label: "Membership graph", Kind: console.KindText, Text: membershipTree(g, email, edges),
			Note: "Every path from " + email + " up to " + g.Email + " (getMembershipGraph), drawn from the group down."},
		{ID: "graph-edges", Label: "Graph memberships", Listing: paths},
		directSec,
	}
	return d
}

// membershipTree draws a membership graph from the group down to the
// member, one line per membership on a path, a group reached twice drawn
// again under each parent.
func membershipTree(top *resourcemanager.Group, email string, edges []resourcemanager.GraphEdge) string {
	under := map[string][]resourcemanager.GroupMember{}
	for _, e := range edges {
		ms := append([]resourcemanager.GroupMember(nil), e.Members...)
		sort.Slice(ms, func(i, j int) bool { return ms[i].Email < ms[j].Email })
		under[e.Group.Email] = ms
	}
	var b strings.Builder
	b.WriteString(top.Email + "\n")
	var draw func(group, indent string, seen map[string]bool)
	draw = func(group, indent string, seen map[string]bool) {
		ms := under[group]
		for i, m := range ms {
			branch, next := "├── ", "│   "
			if i == len(ms)-1 {
				branch, next = "└── ", "    "
			}
			line := m.Email + " (" + strings.Join(m.Roles, ", ") + ")"
			if m.Email == email {
				line += "  ← member"
			}
			b.WriteString(indent + branch + line + "\n")
			if m.Email != email && !seen[m.Email] {
				seen[m.Email] = true
				draw(m.Email, indent+next, seen)
				delete(seen, m.Email)
			}
		}
	}
	draw(top.Email, "", map[string]bool{top.Email: true})
	return strings.TrimRight(b.String(), "\n")
}
