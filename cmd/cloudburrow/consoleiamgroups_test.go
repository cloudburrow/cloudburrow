package main

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// IAM & Admin > Groups (#1812): Create group, the list, the group's page
// with its members, Add member (a nested group too), Remove member and
// Delete, over the registry Cloud Identity's API serves, with its refusals.
func TestConsoleIAMGroups(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	p := iamGroupsProvider{registry: reg}
	ctx := context.Background()
	d := "@" + resourcemanager.OrganizationDomain
	for _, e := range []string{"outer" + d, "inner" + d} {
		if row, err := p.Create(ctx, "", map[string]string{"email": e, "displayName": "G", "security": "Yes"}); err != nil || row != e {
			t.Fatalf("Create %s = %q, %v", e, row, err)
		}
	}
	if _, err := p.Create(ctx, "", map[string]string{"email": "x@example.com"}); err == nil {
		t.Error("created a group outside the customer's domain")
	}
	l, _ := p.List(ctx, "")
	if len(l.Items) != 2 || l.Items[0].Name != "inner"+d || l.Items[0].Fields["Type"] != "Security" {
		t.Fatalf("List = %+v", l.Items)
	}
	sa := "a@p.iam.gserviceaccount.com"
	if err := p.ActAt(ctx, "", []string{"inner" + d}, "add-members", map[string]string{"members": sa, "role": "MEMBER"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", []string{"outer" + d}, "add-members", map[string]string{"members": "inner" + d, "role": "MANAGER"}); err != nil {
		t.Fatal(err)
	}
	if !reg.MemberNames("group:outer"+d, sa) {
		t.Errorf("%s is not in outer through inner", sa)
	}
	det, _ := p.Detail(ctx, "", []string{"outer" + d})
	if det.Unavailable != "" || det.Edit == nil || len(det.Sections[0].Listing.Items) != 1 ||
		det.Sections[0].Listing.Items[0].Fields["Type"] != "GROUP" || det.Sections[0].Listing.Items[0].Fields["Roles"] != "MANAGER, MEMBER" {
		t.Fatalf("Detail = %+v", det.Sections[0].Listing.Items)
	}
	if in, _ := p.Detail(ctx, "", []string{"inner" + d}); len(in.Sections[2].Listing.Items) != 1 || in.Sections[2].Listing.Items[0].Name != "outer"+d {
		t.Errorf("inner's Member of = %+v", in.Sections[2].Listing.Items)
	}
	if err := p.Edit(ctx, "", []string{"outer" + d}, map[string]string{"displayName": "Renamed", "description": "d"}); err != nil {
		t.Fatal(err)
	}
	if g, _ := reg.GetGroup(mustLookup(t, reg, "outer"+d)); g.DisplayName != "Renamed" {
		t.Errorf("Edit group: display name %q", g.DisplayName)
	}
	if err := p.ActAt(ctx, "", []string{"outer" + d}, "remove-members", map[string]string{"members": "inner" + d}); err != nil {
		t.Fatal(err)
	}
	if reg.MemberNames("group:outer"+d, sa) {
		t.Errorf("%s still in outer", sa)
	}
	if err := p.Delete(ctx, "", "inner"+d); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, ""); len(l.Items) != 1 {
		t.Errorf("after Delete, List = %+v", l.Items)
	}
}

func mustLookup(t *testing.T, reg *resourcemanager.Registry, email string) string {
	t.Helper()
	n, err := reg.LookupGroup(email, "")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// IAM & Admin > Groups, the rest of Cloud Identity (#1817): the search box
// (groups.search, a word and a query), Add member with an expiry, Change
// roles and the MEMBER expiry on a member's row (modifyMembershipRoles),
// the Transitive members tab (searchTransitiveMemberships), and a member's
// page with its membership graph (getMembershipGraph) and direct groups
// (searchDirectGroups).
func TestConsoleIAMGroupsSearchRolesGraph(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	p := iamGroupsProvider{registry: reg}
	ctx := context.Background()
	d := "@" + resourcemanager.OrganizationDomain
	for _, g := range []struct{ email, name, security string }{
		{"eng" + d, "Engineering", "Yes"}, {"eng-infra" + d, "Infra team", "No"}, {"sales" + d, "Sales", "No"}} {
		if _, err := p.Create(ctx, "", map[string]string{"email": g.email, "displayName": g.name, "security": g.security}); err != nil {
			t.Fatal(err)
		}
	}
	names := func(filter string) []string {
		t.Helper()
		l, err := p.ListFiltered(ctx, "", filter)
		if err != nil {
			t.Fatal(err)
		}
		if l.ServerFilter == nil || l.ServerFilter.Value != filter {
			t.Fatalf("ServerFilter = %+v", l.ServerFilter)
		}
		if l.ServerFilter.Error != "" {
			return []string{"error: " + l.ServerFilter.Error}
		}
		var out []string
		for _, it := range l.Items {
			out = append(out, it.Name)
		}
		return out
	}
	for filter, want := range map[string]string{
		"":     "eng-infra" + d + ",eng" + d + ",sales" + d,
		"eng":  "eng-infra" + d + ",eng" + d,
		"team": "eng-infra" + d,
		"'" + resourcemanager.LabelSecurity + "' in labels":           "eng" + d,
		"group_key.startsWith('sa') && display_name == 'Sales'":       "sales" + d,
		"parent == 'customers/C000nope' && group_key.startsWith('e')": "",
	} {
		got := strings.Join(names(filter), ",")
		if filter == "parent == 'customers/C000nope' && group_key.startsWith('e')" {
			if !strings.HasPrefix(got, "error: ") {
				t.Errorf("search %q (another customer) = %q, want a refusal", filter, got)
			}
			continue
		}
		if got != want {
			t.Errorf("search %q = %q, want %q", filter, got, want)
		}
	}
	if got := names("group_key.endsWith('x')"); len(got) != 1 || !strings.HasPrefix(got[0], "error: ") {
		t.Errorf("an unsupported term = %q, want the API's refusal", got)
	}

	// Add member with an expiry; a past one and an expiry without a member are refused.
	future := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	ada := "ada@example.com"
	if err := p.ActAt(ctx, "", []string{"eng-infra" + d}, "add-members", map[string]string{"members": ada, "expire": future.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", []string{"eng-infra" + d}, "add-members", map[string]string{"members": "bob@example.com", "expire": "2001-01-01T00:00:00Z"}); err == nil {
		t.Error("added a member with a past expiry")
	}
	if err := p.ActAt(ctx, "", []string{"eng-infra" + d}, "add-members", map[string]string{"expire": future.Format(time.RFC3339)}); err == nil {
		t.Error("an expiry without a member to add was accepted")
	}
	// eng is a security group: it takes security groups only
	// (update-group-to-security-group), so eng-infra joins once it is one.
	if err := p.ActAt(ctx, "", []string{"eng" + d}, "add-members", map[string]string{"members": "eng-infra" + d}); err == nil {
		t.Error("a Google Group was added to a security group")
	}
	if !strings.Contains(det0Help(t, p, "eng"+d), "security group") {
		t.Error("a security group's Add member does not explain what it can contain")
	}
	if _, err := p.registry.UpdateGroup(mustGroup(t, p, "eng-infra"+d).Name(), resourcemanager.GroupInput{
		Labels: map[string]string{resourcemanager.LabelDiscussionForum: "", resourcemanager.LabelSecurity: ""}}, []string{"labels"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", []string{"eng" + d}, "add-members", map[string]string{"members": "eng-infra" + d}); err != nil {
		t.Fatal(err)
	}
	det, _ := p.Detail(ctx, "", []string{"eng-infra" + d})
	row := det.Sections[0].Listing.Items[0]
	if row.Name != ada || row.Fields["Expires"] != future.Format(time.RFC3339) || !det.Sections[0].Listing.RowsOpenable {
		t.Fatalf("Members row = %+v", row)
	}
	if len(row.Actions) != 2 || row.Actions[0].ID != "change-roles" || row.Actions[0].Fields[2].Default != future.Format(time.RFC3339) {
		t.Fatalf("row actions = %+v", row.Actions)
	}

	// Change roles: add MANAGER and clear the expiry, then OWNER; then remove both and set an expiry.
	path := []string{"eng-infra" + d, ada}
	if acts := p.DetailActions(ctx, "", path); len(acts) != 2 {
		t.Fatalf("DetailActions = %+v", acts)
	}
	if err := p.ActAt(ctx, "", path, "change-roles", map[string]string{"owner": "false", "manager": "false", "expire": future.Format(time.RFC3339)}); err == nil {
		t.Error("Change roles with nothing changed was accepted")
	}
	if err := p.ActAt(ctx, "", path, "change-roles", map[string]string{"owner": "true", "manager": "true", "expire": ""}); err != nil {
		t.Fatal(err)
	}
	_, m, _, _ := p.member(path)
	if strings.Join(m.Roles, ",") != "OWNER,MANAGER,MEMBER" || m.ExpireTime != nil {
		t.Fatalf("after Change roles: %+v", m)
	}
	later := future.Add(24 * time.Hour)
	if err := p.ActAt(ctx, "", path, "change-roles", map[string]string{"owner": "false", "manager": "false", "expire": later.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if _, m, _, _ = p.member(path); strings.Join(m.Roles, ",") != "MEMBER" || m.ExpireTime == nil || !m.ExpireTime.Equal(later) {
		t.Fatalf("after Change roles back: %+v", m)
	}
	if err := p.ActAt(ctx, "", path, "change-roles", map[string]string{"expire": "tomorrow"}); err == nil {
		t.Error("a malformed expiry was accepted")
	}

	// Transitive members of eng: eng-infra directly, ada through it.
	det, _ = p.Detail(ctx, "", []string{"eng" + d})
	var tr *console.Section
	for i := range det.Sections {
		if det.Sections[i].ID == "transitive-members" {
			tr = &det.Sections[i]
		}
	}
	if tr == nil || len(tr.Listing.Items) != 2 {
		t.Fatalf("Transitive members = %+v", tr)
	}
	got := map[string]string{}
	for _, it := range tr.Listing.Items {
		got[it.Name] = it.Fields["Relation"] + "/" + it.Fields["Type"]
	}
	if got[ada] != "Indirect/USER" || got["eng-infra"+d] != "Direct/GROUP" {
		t.Errorf("Transitive members = %v", got)
	}
	if acts := p.DetailActions(ctx, "", []string{"eng" + d, ada}); acts != nil {
		t.Errorf("an indirect member has actions %+v", acts)
	}
	if err := p.ActAt(ctx, "", []string{"eng" + d, ada}, "change-roles", map[string]string{"owner": "true"}); err == nil {
		t.Error("changed the roles of an indirect member")
	}

	// ada's page under eng: the graph through eng-infra, and its direct groups.
	md, _ := p.Detail(ctx, "", []string{"eng" + d, ada})
	if md.Unavailable != "" || len(md.Sections) != 4 {
		t.Fatalf("member page = %+v", md)
	}
	want := "eng" + d + "\n└── eng-infra" + d + " (MEMBER)\n    └── " + ada + " (MEMBER)  ← member"
	if md.Sections[1].Text != want {
		t.Errorf("graph =\n%s\nwant\n%s", md.Sections[1].Text, want)
	}
	if n := len(md.Sections[2].Listing.Items); n != 2 {
		t.Errorf("graph memberships = %+v", md.Sections[2].Listing.Items)
	}
	if dg := md.Sections[3].Listing.Items; len(dg) != 1 || dg[0].Name != "eng-infra"+d {
		t.Errorf("direct groups = %+v", dg)
	}
	if out, _ := p.Detail(ctx, "", []string{"sales" + d, ada}); out.Unavailable == "" {
		t.Error("a non-member's page under sales is not refused")
	}

	// Remove member from its row.
	if err := p.ActAt(ctx, "", path, "remove-member", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := p.member(path); ok {
		t.Error("Remove member left the membership")
	}
}

func mustGroup(t *testing.T, p iamGroupsProvider, row string) *resourcemanager.Group {
	t.Helper()
	g, err := p.group(row)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// det0Help is the help of a group's Add members > Member email field.
func det0Help(t *testing.T, p iamGroupsProvider, row string) string {
	t.Helper()
	for _, a := range p.DetailActions(context.Background(), "", []string{row}) {
		if a.ID != "add-members" {
			continue
		}
		for _, f := range a.Fields {
			if f.Name == "members" {
				return f.Help
			}
		}
	}
	t.Fatalf("%s's page has no Add members", row)
	return ""
}

// TestConsoleIAMGroupsNameRules: the create and edit forms carry the
// group email's and display name's rules, and the API's refusal reaches
// the operator.
func TestConsoleIAMGroupsNameRules(t *testing.T) {
	p := iamGroupsProvider{registry: resourcemanager.New(store.NewMemory())}
	_, fields := p.CreateForm()
	re := map[string]*regexp.Regexp{}
	for _, f := range fields {
		if f.Pattern != "" {
			re[f.Name] = regexp.MustCompile("^(?:" + f.Pattern + ")$")
		}
	}
	d := "@" + resourcemanager.OrganizationDomain
	for email, ok := range map[string]bool{"eng" + d: true, "o'brien.team" + d: true, ".eng" + d: false, "a..b" + d: false,
		"a+b" + d: false, "eng@example.com": false} {
		if re["email"].MatchString(email) != ok {
			t.Errorf("email pattern on %q = %v, want %v", email, !ok, ok)
		}
	}
	if !re["displayName"].MatchString(strings.Repeat("é", 73)) || re["displayName"].MatchString(strings.Repeat("a", 74)) {
		t.Error("the display name pattern is not 73 characters")
	}
	if _, err := p.Create(context.Background(), "", map[string]string{"email": strings.Repeat("a", 65) + d}); err == nil ||
		!strings.Contains(err.Error(), "64 characters") {
		t.Errorf("a 65-character name = %v, want the 64-character refusal", err)
	}
}

// TestConsoleIAMGroupPageButtons (#1864): a group's page has Google's Add
// members, Remove members (the Members table's checked rows), View in
// Google Groups (not available locally) and Delete group; Edit group's
// fields are Group name and Group description, ending in Save.
func TestConsoleIAMGroupPageButtons(t *testing.T) {
	p := iamGroupsProvider{registry: resourcemanager.New(store.NewMemory())}
	ctx := context.Background()
	email := "team@" + resourcemanager.OrganizationDomain
	if _, err := p.Create(ctx, "", map[string]string{"email": email, "displayName": "Team"}); err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, a := range p.DetailActions(ctx, "", []string{email}) {
		labels = append(labels, a.Label)
		if a.ID == "view-in-groups" && a.Unavailable == "" {
			t.Error("View in Google Groups does not say why it is not available")
		}
	}
	if got := strings.Join(labels, ","); got != "Add members,Remove members,View in Google Groups,Delete group" {
		t.Errorf("the group page's actions are %s", got)
	}
	d, _ := p.Detail(ctx, "", []string{email})
	if d.Edit == nil || d.Edit.Submit != "Save" || d.Edit.Fields[0].Label != "Group name" || d.Edit.Fields[1].Label != "Group description" {
		t.Fatalf("Edit group is %+v", d.Edit)
	}
	if sa := d.Sections[0].Listing.SelectActions; len(sa) != 1 || sa[0].ID != "remove-members" || len(d.Sections[0].Actions) != 1 {
		t.Errorf("the Members tab's actions are %+v and %+v", d.Sections[0].Actions, sa)
	}
	if err := p.ActAt(ctx, "", []string{email}, "add-members", map[string]string{"members": "ada@example.com, bob@example.com", "role": "MANAGER"}); err != nil {
		t.Fatal(err)
	}
	if g := mustGroup(t, p, email); len(g.Members) != 2 || !hasRole(g.Members[0], "MANAGER") {
		t.Fatalf("after Add members the group holds %+v", g.Members)
	}
	if err := p.ActAt(ctx, "", []string{email}, "remove-members", map[string]string{"members": "ada@example.com\nbob@example.com"}); err != nil {
		t.Fatal(err)
	}
	if g := mustGroup(t, p, email); len(g.Members) != 0 {
		t.Fatalf("after Remove members the group holds %+v", g.Members)
	}
	if err := p.ActAt(ctx, "", []string{email}, "delete-group", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.group(email); err == nil {
		t.Error("Delete group left the group")
	}
}
