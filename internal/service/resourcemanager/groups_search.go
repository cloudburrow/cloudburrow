package resourcemanager

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cloud Identity's searches and role changes (#1812): groups.search,
// memberships.searchDirectGroups, searchTransitiveMemberships,
// getMembershipGraph and modifyMembershipRoles, as the v1 discovery
// document describes them (google.golang.org/api/cloudidentity/v1,
// revision 20261019, read 2026-11-18), and the caller WITH_INITIAL_OWNER
// makes a group's owner.

// SetCallerResolver sets how a bearer token names its account, for
// groups.create's WITH_INITIAL_OWNER; nil (the default) knows no caller.
func (r *Registry) SetCallerResolver(f func(bearer string) (string, bool)) {
	r.mu.Lock()
	r.callerOf = f
	r.mu.Unlock()
}

// Caller is the account bearer was minted for, if the instance knows it.
func (r *Registry) Caller(bearer string) (string, bool) {
	r.mu.Lock()
	f := r.callerOf
	r.mu.Unlock()
	if f == nil || bearer == "" {
		return "", false
	}
	email, ok := f(bearer)
	return normEmail(email), ok && email != ""
}

// groupSearch is groups.search's query: "Must contain equality operators
// on the parent", with optional `'label' in labels`, `domain_name == 'd'`,
// and startsWith/contains/equality on group_key and display_name, joined
// by &&.
type groupSearch struct {
	parent string
	labels []string
	domain string
	// keys and names are the group_key and display_name tests.
	keys, names []stringTest
}

type stringTest struct{ op, v string }

func (t stringTest) match(s string, fold bool) bool {
	v := t.v
	if fold {
		s, v = strings.ToLower(s), strings.ToLower(v)
	}
	switch t.op {
	case "startsWith":
		return strings.HasPrefix(s, v)
	case "contains":
		return strings.Contains(s, v)
	}
	return s == v
}

const celString = `(?:'([^']*)'|"([^"]*)")`

var (
	reSearchEq   = regexp.MustCompile(`^(parent|domain_name|group_key|display_name)\s*==\s*` + celString + `$`)
	reSearchCall = regexp.MustCompile(`^(group_key|display_name)\.(startsWith|contains)\(\s*` + celString + `\s*\)$`)
	reSearchIn   = regexp.MustCompile(`^` + celString + `\s+in\s+labels$`)
)

func celValue(m []string, i int) string {
	if m[i] != "" {
		return m[i]
	}
	return m[i+1]
}

func parseGroupSearch(q string) (groupSearch, error) {
	var out groupSearch
	if strings.TrimSpace(q) == "" {
		return out, apierror.InvalidArgument("Request contains an invalid argument: query is required.")
	}
	for _, term := range strings.Split(q, "&&") {
		term = strings.TrimSpace(term)
		for strings.HasPrefix(term, "(") && strings.HasSuffix(term, ")") {
			term = strings.TrimSpace(term[1 : len(term)-1])
		}
		if m := reSearchEq.FindStringSubmatch(term); m != nil {
			v := celValue(m, 2)
			switch m[1] {
			case "parent":
				out.parent = customerParent(v)
			case "domain_name":
				out.domain = strings.ToLower(v)
			case "group_key":
				out.keys = append(out.keys, stringTest{"==", v})
			case "display_name":
				out.names = append(out.names, stringTest{"==", v})
			}
			continue
		}
		if m := reSearchCall.FindStringSubmatch(term); m != nil {
			t := stringTest{m[2], celValue(m, 3)}
			if m[1] == "group_key" {
				out.keys = append(out.keys, t)
			} else {
				out.names = append(out.names, t)
			}
			continue
		}
		if m := reSearchIn.FindStringSubmatch(term); m != nil {
			out.labels = append(out.labels, celValue(m, 1))
			continue
		}
		return out, apierror.InvalidArgument("Invalid query %q: the term %q is not one groups.search takes (parent == '...', '...' in labels, domain_name == '...', and ==, startsWith or contains on group_key and display_name, joined by &&).", q, term)
	}
	if out.parent == "" {
		return out, apierror.InvalidArgument("Invalid query %q: it must contain parent == 'customers/{customer_id}'.", q)
	}
	if !strings.HasPrefix(out.parent, "customers/C") {
		return out, apierror.InvalidArgument("Invalid query %q: the parent must be customers/{customer_id}, the customer ID beginning with C.", q)
	}
	return out, nil
}

// SearchGroups are the groups query matches, by email.
func (r *Registry) SearchGroups(query string) ([]*Group, error) {
	sq, err := parseGroupSearch(query)
	if err != nil {
		return nil, err
	}
	if sq.parent != CustomerName {
		return nil, permissionDenied(sq.parent)
	}
	r.mu.Lock()
	all, err := r.groupsLocked()
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []*Group
next:
	for _, g := range all {
		for _, l := range sq.labels {
			if _, ok := g.Labels[l]; !ok {
				continue next
			}
		}
		if sq.domain != "" {
			if _, d, _ := strings.Cut(g.Email, "@"); d != sq.domain {
				continue
			}
		}
		for _, t := range sq.keys {
			if !t.match(g.Email, true) {
				continue next
			}
		}
		for _, t := range sq.names {
			if !t.match(g.DisplayName, false) {
				continue next
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// DirectGroup is a group email is a direct member of, with its membership.
type DirectGroup struct {
	Group  *Group
	Member GroupMember
}

// DirectGroups are the groups email is a direct member of that carry
// every label, by group email.
func (r *Registry) DirectGroups(email string, labels []string) ([]DirectGroup, error) {
	email = normEmail(email)
	r.mu.Lock()
	all, err := r.groupsLocked()
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []DirectGroup
next:
	for _, g := range all {
		for _, l := range labels {
			if _, ok := g.Labels[l]; !ok {
				continue next
			}
		}
		for _, m := range g.Members {
			if m.Email == email {
				out = append(out, DirectGroup{g, m})
			}
		}
	}
	return out, nil
}

// MemberRelation is one transitive member of a group.
type MemberRelation struct {
	Email, Type      string
	Direct, Indirect bool
	// Roles are the member's roles when it is a direct member.
	Roles []string
	// Group is the member when it is a group.
	Group *Group
}

// MemberResource is a member's resource name: groups/{id} for a group;
// for an account, users/{id} with a stable numeric ID derived from its
// email, as the instance keeps no user directory.
func MemberResource(x MemberRelation) string {
	if x.Group != nil {
		return x.Group.Name()
	}
	sum := sha256.Sum256([]byte(x.Email))
	return fmt.Sprintf("users/1%020d", binary.BigEndian.Uint64(sum[:8]))
}

// TransitiveMembers are every direct and indirect member of the group
// parent names, by email.
func (r *Registry) TransitiveMembers(parent string) ([]MemberRelation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.groupFromNameLocked(parent)
	if err != nil {
		return nil, err
	}
	all, err := r.groupsLocked()
	if err != nil {
		return nil, err
	}
	byEmail := map[string]*Group{}
	for _, o := range all {
		byEmail[o.Email] = o
	}
	rel := map[string]*MemberRelation{}
	visited := map[string]bool{g.Email: true}
	var walk func(*Group, bool)
	walk = func(cur *Group, direct bool) {
		for _, m := range cur.Members {
			x, ok := rel[m.Email]
			if !ok {
				x = &MemberRelation{Email: m.Email, Type: m.Type, Group: byEmail[m.Email]}
				rel[m.Email] = x
			}
			if direct {
				x.Direct, x.Roles = true, m.Roles
			} else {
				x.Indirect = true
			}
			if inner := byEmail[m.Email]; inner != nil && !visited[inner.Email] {
				visited[inner.Email] = true
				walk(inner, false)
			}
		}
	}
	walk(g, true)
	out := make([]MemberRelation, 0, len(rel))
	for _, x := range rel {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// GraphEdge is one group of a membership graph and its memberships on the
// graph's paths.
type GraphEdge struct {
	Group   *Group
	Members []GroupMember
}

// MembershipGraph is every membership path from email up to the group
// parent names (groups/- for every group), through groups that carry every
// label: each group on a path with its memberships on the paths.
func (r *Registry) MembershipGraph(parent, email string, labels []string) ([]GraphEdge, error) {
	email = normEmail(email)
	r.mu.Lock()
	defer r.mu.Unlock()
	var top *Group
	if parent != "groups/-" {
		g, err := r.groupFromNameLocked(parent)
		if err != nil {
			return nil, err
		}
		top = g
	}
	all, err := r.groupsLocked()
	if err != nil {
		return nil, err
	}
	labelled := func(g *Group) bool {
		for _, l := range labels {
			if _, ok := g.Labels[l]; !ok {
				return false
			}
		}
		return true
	}
	byEmail := map[string]*Group{}
	for _, g := range all {
		if labelled(g) {
			byEmail[g.Email] = g
		}
	}
	// up: the groups reachable from email through memberships.
	up := map[string]bool{}
	queue := []string{email}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, g := range byEmail {
			if up[g.Email] {
				continue
			}
			if slices.ContainsFunc(g.Members, func(m GroupMember) bool { return m.Email == cur }) {
				up[g.Email] = true
				queue = append(queue, g.Email)
			}
		}
	}
	// down: with a group named, the groups (and so the paths) under it.
	onPath := up
	if top != nil {
		onPath = map[string]bool{}
		if up[top.Email] {
			down := map[string]bool{top.Email: true}
			queue = []string{top.Email}
			for len(queue) > 0 {
				cur := byEmail[queue[0]]
				queue = queue[1:]
				for _, m := range cur.Members {
					if g := byEmail[m.Email]; g != nil && !down[g.Email] {
						down[g.Email] = true
						queue = append(queue, g.Email)
					}
				}
			}
			for e := range up {
				if down[e] {
					onPath[e] = true
				}
			}
		}
	}
	var out []GraphEdge
	for _, g := range all {
		if !onPath[g.Email] {
			continue
		}
		e := GraphEdge{Group: g}
		for _, m := range g.Members {
			if m.Email == email || onPath[m.Email] {
				e.Members = append(e.Members, m)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// RoleChange is one MembershipRole of modifyMembershipRoles: its name and
// the MEMBER role's expiry (nil for none).
type RoleChange struct {
	Name       string
	ExpireTime *time.Time
}

// ModifyMembershipRoles adds and removes a membership's roles, or updates
// its MEMBER role's expiry (update, with fieldMask expiry_detail.expire_time);
// the two cannot be mixed.
func (r *Registry) ModifyMembershipRoles(name string, add []RoleChange, remove []string, update []RoleChange, updating bool) (*Group, GroupMember, error) {
	if updating && (len(add) > 0 || len(remove) > 0) {
		return nil, GroupMember{}, apierror.InvalidArgument("Adding or removing roles in the same request as updating roles is not supported.")
	}
	if !updating && len(add) == 0 && len(remove) == 0 {
		return nil, GroupMember{}, apierror.InvalidArgument("Request contains an invalid argument: one of addRoles, removeRoles or updateRolesParams is required.")
	}
	for _, x := range append(slices.Clone(add), update...) {
		if x.ExpireTime != nil && x.Name != "MEMBER" {
			return nil, GroupMember{}, apierror.InvalidArgument("Expiry details are only supported for MEMBER MembershipRoles, not %s.", x.Name)
		}
		if err := r.checkExpiry(x.ExpireTime); err != nil {
			return nil, GroupMember{}, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, i, err := r.membershipFromNameLocked(name)
	if err != nil {
		return nil, GroupMember{}, err
	}
	m := g.Members[i]
	roles := slices.Clone(m.Roles)
	for _, x := range update {
		if x.Name != "MEMBER" {
			return nil, GroupMember{}, apierror.InvalidArgument("Only the MEMBER MembershipRole can currently be updated, not %q.", x.Name)
		}
		m.ExpireTime = x.ExpireTime
	}
	seen := map[string]bool{}
	for _, x := range add {
		switch x.Name {
		case "OWNER", "MANAGER", "MEMBER":
		default:
			return nil, GroupMember{}, apierror.InvalidArgument("Invalid membership role %q: it must be one of OWNER, MANAGER, MEMBER.", x.Name)
		}
		if seen[x.Name] || slices.Contains(roles, x.Name) {
			return nil, GroupMember{}, apierror.InvalidArgument("The membership already has the role %s.", x.Name)
		}
		seen[x.Name] = true
		roles = append(roles, x.Name)
	}
	for _, n := range remove {
		if n == "MEMBER" {
			return nil, GroupMember{}, apierror.InvalidArgument("It is not possible to remove the MEMBER MembershipRole; delete the membership instead.")
		}
		j := slices.Index(roles, n)
		if j < 0 {
			return nil, GroupMember{}, apierror.InvalidArgument("The membership does not have the role %q.", n)
		}
		roles = slices.Delete(roles, j, j+1)
	}
	sort.Slice(roles, func(a, b int) bool { return roleRank(roles[a]) < roleRank(roles[b]) })
	m.Roles = roles
	m.UpdateTime = r.now().UTC()
	g.Members[i] = m
	if err := r.putGroupLocked(g); err != nil {
		return nil, GroupMember{}, err
	}
	return g, m, nil
}
