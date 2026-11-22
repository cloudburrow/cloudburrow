package resourcemanager

import (
	"fmt"

	"google.golang.org/grpc/codes"

	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Identity-mapped groups (#2103): groups of an external identity source,
// whose key carries the source as its namespace. Group.parent "Must be of
// the form identitysources/{identity_source} for external
// identity-mapped groups"; EntityKey.namespace "If specified, the
// EntityKey represents an external-identity-mapped group. The namespace
// must correspond to an identity source created in Admin Console and must
// be in the form of identitysources/{identity_source}"; "Identity-mapped
// groups for Cloud Search have a label with a key of
// system/groups/external and an empty value"
// (https://docs.cloud.google.com/identity/docs/reference/rest/v1/groups and
// .../v1/EntityKey, read 2026-11-21). The instance's identity sources are
// its external identity providers: each workforce pool (#2056), whose users
// are federated by its providers or provisioned by its SCIM tenant (#2065),
// is identitysources/{pool ID}. An identity-mapped group's key and its
// members' keys are that source's identifiers (a group name and its users'
// subjects), not emails; its members are added and removed through the
// memberships methods, as a directory sync tool does. They are directory
// records: IAM does not resolve group: or principalSet:// members through
// them.

const labelExternal = "system/groups/external"

// IdentitySourceOf checks an identity source name, identitysources/{id},
// and that its workforce pool exists.
func (r *Registry) IdentitySourceOf(name string) error {
	id, ok := strings.CutPrefix(name, "identitysources/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return apierror.InvalidArgument("Invalid identity source %q: it must be identitysources/{identity_source}.", name)
	}
	p, err := r.WorkforcePool(id)
	if err != nil || p.State == "DELETED" {
		return permissionDenied(name)
	}
	return nil
}

func (r *Registry) createMappedGroup(in GroupInput) (*Group, error) {
	if in.Parent == "" {
		in.Parent = in.Namespace
	}
	if !strings.HasPrefix(in.Parent, "identitysources/") {
		return nil, apierror.InvalidArgument("Invalid parent %q: an identity-mapped group's parent is its identity source, identitysources/{identity_source}.", in.Parent)
	}
	if in.Namespace != in.Parent {
		return nil, apierror.InvalidArgument("Invalid groupKey.namespace %q: it must be the group's parent, %s.", in.Namespace, in.Parent)
	}
	if err := r.IdentitySourceOf(in.Parent); err != nil {
		return nil, err
	}
	if in.Dynamic {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: an identity-mapped group cannot be dynamic.")
	}
	key := strings.TrimSpace(in.Email)
	if key == "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: groupKey.id is required.")
	}
	if _, ok := in.Labels[labelExternal]; !ok || len(in.Labels) != 1 || in.Labels[labelExternal] != "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: an identity-mapped group's labels are %s alone, with an empty value.", labelExternal)
	}
	if err := checkGroupText(in.DisplayName, in.Description); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, err := r.groupByKeyLocked(key, in.Namespace); err != nil {
		return nil, err
	} else if g != nil {
		return nil, apierror.AlreadyExists("Error(4003): Cannot create group which already exists.")
	}
	now := r.now().UTC()
	g := &Group{ID: newIdentityID(), Email: key, Namespace: in.Namespace, DisplayName: in.DisplayName, Description: in.Description,
		Labels: map[string]string{labelExternal: ""}, CreateTime: now, UpdateTime: now}
	return g, r.putGroupLocked(g)
}

func (r *Registry) listMappedGroups(parent string) ([]*Group, error) {
	if err := r.IdentitySourceOf(parent); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	gs, err := r.groupsLocked()
	if err != nil {
		return nil, err
	}
	var out []*Group
	for _, g := range gs {
		if g.Namespace == parent {
			out = append(out, g)
		}
	}
	return out, nil
}

func (r *Registry) createMappedMembership(parent, key, namespace string, roles []string, expire *time.Time) (*Group, GroupMember, error) {
	if err := r.IdentitySourceOf(namespace); err != nil {
		return nil, GroupMember{}, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, GroupMember{}, apierror.InvalidArgument("Request contains an invalid argument: preferredMemberKey.id is required.")
	}
	roles, err := membershipRoles(roles)
	if err != nil {
		return nil, GroupMember{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.groupFromNameLocked(parent)
	if err != nil {
		return nil, GroupMember{}, err
	}
	if g.Namespace != namespace {
		return nil, GroupMember{}, apierror.InvalidArgument("Invalid preferredMemberKey.namespace %q: the group's members are of %s.", namespace, orDefault(g.Namespace, "Google accounts (no namespace)"))
	}
	for _, m := range g.Members {
		if m.Email == key && m.Namespace == namespace {
			return nil, GroupMember{}, apierror.AlreadyExists("Error(4003): Member already exists.")
		}
	}
	typ := "USER"
	if inner, err := r.groupByKeyLocked(key, namespace); err != nil {
		return nil, GroupMember{}, err
	} else if inner != nil {
		if inner.ID == g.ID {
			return nil, GroupMember{}, apierror.InvalidArgument("A group cannot be a member of itself.")
		}
		typ = "GROUP"
	}
	now := r.now().UTC()
	m := GroupMember{ID: newIdentityID(), Email: key, Namespace: namespace, Roles: roles, Type: typ, CreateTime: now, UpdateTime: now, ExpireTime: expire}
	g.Members = append(g.Members, m)
	return g, m, r.putGroupLocked(g)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// dynamicQueryOf is a dynamic group's one USER query.
func dynamicQueryOf(in GroupInput) (string, error) {
	if len(in.DynamicQueries) == 0 {
		return "", apierror.InvalidArgument("Request contains an invalid argument: a dynamic group needs dynamicGroupMetadata.queries.")
	}
	if len(in.DynamicQueries) > 1 {
		return "", apierror.InvalidArgument("Request contains an invalid argument: only one entry with USER resource is currently supported in dynamicGroupMetadata.queries.")
	}
	q := in.DynamicQueries[0]
	if q[0] != resourceTypeUser {
		return "", apierror.InvalidArgument("Invalid dynamicGroupMetadata.queries[0].resourceType %q: it must be USER.", q[0])
	}
	if _, err := CompileDynamicQuery(q[1]); err != nil {
		return "", apierror.InvalidArgument("Invalid dynamic group query %q: %v", q[1], err)
	}
	if in.Labels != nil {
		if _, ok := in.Labels[labelDynamic]; !ok {
			return "", apierror.InvalidArgument("Request contains an invalid argument: a dynamic group's labels must contain %s.", labelDynamic)
		}
	}
	return strings.TrimSpace(q[1]), nil
}

func (r *Registry) dynamicLimitLocked() error {
	gs, err := r.groupsLocked()
	if err != nil {
		return err
	}
	n := 0
	for _, g := range gs {
		if g.Dynamic() {
			n++
		}
	}
	if n >= maxDynamicGroups {
		return &apierror.Error{Code: codes.ResourceExhausted, Message: fmt.Sprintf("The customer has %d dynamic groups, the most it may have.", maxDynamicGroups)}
	}
	return nil
}

func dynamicMembershipsRefused(g *Group) error {
	return apierror.FailedPrecondition("The memberships of the dynamic group %s are managed by its query; they cannot be created, changed or deleted.", g.Email)
}
