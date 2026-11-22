package resourcemanager

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/wif"
)

// Cloud Identity groups (cloudidentity.googleapis.com v1, #1812, the last item of #1780),
// as far as IAM membership needs them: a
// group is "a named collection of Google Accounts" which a policy names as
// group:EMAIL, and a domain (domain:D) is "a virtual group of all the
// Google Accounts" of a Google Workspace or Cloud Identity account
// (https://docs.cloud.google.com/iam/docs/principals-overview, read
// 2026-11-18). The groups live under the instance's one organization
// customer, customers/OrganizationCustomerID, and their emails in its
// domain, OrganizationDomain: Cloud Identity creates a customer's groups in
// a domain the customer owns. A group may be a member of another; IAM
// grants a nested group's members what the outer group holds
// (Principals). Groups are Google Groups (the
// cloudidentity.googleapis.com/groups.discussion_forum label, with the
// optional groups.security label), dynamic groups (dynamicgroups.go,
// #2103) and identity-mapped groups (identitymapped.go, #2103).

const (
	groupPrefix = "resourcemanager/cloudidentity/groups/"
	// LabelDiscussionForum marks a Google Group, the default type of
	// group, and LabelSecurity a security group (Group.labels).
	LabelDiscussionForum = "cloudidentity.googleapis.com/groups.discussion_forum"
	LabelSecurity        = "cloudidentity.googleapis.com/groups.security"
	labelDynamic         = LabelDynamic
	// LabelDynamic marks a dynamic group (#2103).
	LabelDynamic = "cloudidentity.googleapis.com/groups.dynamic"
	labelLocked  = "cloudidentity.googleapis.com/groups.locked"
	// CustomerName is the parent of the instance's groups.
	CustomerName = "customers/" + OrganizationCustomerID
	// maxGroupDescription is Group.description's limit ("Must not be
	// longer than 4,096 characters").
	maxGroupDescription = 4096
	// maxGroupDisplayName is a group name's limit: "Can be up to 73
	// characters" (Name guidelines for users & groups,
	// https://support.google.com/a/answer/9193374, read 2026-11-18).
	maxGroupDisplayName = 73
	// maxGroupEmailLocal is the part of a group's email before the @: "Use
	// no more than 64 characters before the at sign (@) in a group email
	// address" (the same guidelines).
	maxGroupEmailLocal = 64
)

// Group is a stored Cloud Identity group with its direct memberships.
type Group struct {
	ID          string            `json:"id"`
	Email       string            `json:"email"`
	DisplayName string            `json:"displayName,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels"`
	CreateTime  time.Time         `json:"createTime"`
	UpdateTime  time.Time         `json:"updateTime"`
	Members     []GroupMember     `json:"members,omitempty"`
	// MemberRestriction is the security settings' member restriction
	// query (groupsecurity.go, #1959); empty for none.
	MemberRestriction string `json:"memberRestriction,omitempty"`
	// DynamicQuery is a dynamic group's USER query (#2103); its
	// memberships are computed from it.
	DynamicQuery string `json:"dynamicQuery,omitempty"`
	// Namespace is an identity-mapped group's identity source,
	// identitysources/{id} (#2103); empty for a Google Group.
	Namespace string `json:"namespace,omitempty"`
}

// Dynamic is whether g is a dynamic group.
func (g *Group) Dynamic() bool { return g.DynamicQuery != "" }

// Name is the group's resource name, groups/{id}.
func (g *Group) Name() string { return "groups/" + g.ID }

// GroupMember is one membership: its member's email, roles and type.
type GroupMember struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	Roles      []string  `json:"roles"`
	Type       string    `json:"type"`
	CreateTime time.Time `json:"createTime"`
	UpdateTime time.Time `json:"updateTime"`
	// ExpireTime is when the MEMBER role, and so the membership, expires
	// (MembershipRole.expiryDetail, "only supported for MEMBER"); nil for
	// never.
	ExpireTime *time.Time `json:"expireTime,omitempty"`
	// Namespace is an identity-mapped member's identity source (#2103).
	Namespace string `json:"namespace,omitempty"`
}

// expired is whether m's MEMBER role has expired at now.
func (m GroupMember) expired(now time.Time) bool {
	return m.ExpireTime != nil && !now.Before(*m.ExpireTime)
}

func newIdentityID() string {
	b := make([]byte, 7)
	_, _ = rand.Read(b)
	return "0" + hex.EncodeToString(b)
}

// permissionDenied is Cloud Identity's answer for a group or membership
// that does not exist: Google does not tell the two apart.
func permissionDenied(resource string) error {
	return &apierror.Error{Code: codes.PermissionDenied, Reason: "forbidden",
		Message: fmt.Sprintf("Error(2028): Permission denied for resource %s (or it may not exist).", resource)}
}

// customerParent reads a customer parent, customers/{id}, or its
// customerId/{id} spelling, which gcloud identity groups sends
// (googlecloudsdk command_lib/identity/groups/hooks.py GetCustomerId).
func customerParent(p string) string {
	if id, ok := strings.CutPrefix(p, "customerId/"); ok {
		return "customers/" + id
	}
	return p
}

func normEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// validEmail is a minimal check of a group or member key: local@domain.
// An apostrophe is allowed: a username "can contain letters (a-z), numbers
// (0-9), dashes (-), underscores (_), apostrophes ('), and periods (.)"
// (https://support.google.com/a/answer/9193374, read 2026-11-18).
func validEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && domain != "" && strings.Contains(domain, ".") && !strings.ContainsAny(s, " /\\,;:\"<>()[]")
}

// checkGroupEmail applies a username's rules to a group's email, as
// Google's Directory API states them: "Group email addresses are subject to
// the same character usage rules as usernames"
// (https://developers.google.com/workspace/admin/directory/reference/rest/v1/groups,
// read 2026-11-18). From the name guidelines
// (https://support.google.com/a/answer/9193374): no more than 64
// characters before the @; no period at the start or the end; never two
// periods in a row; no equal sign, ampersand, angle bracket, plus sign,
// comma or exclamation point.
func checkGroupEmail(raw, email string) error {
	if !validEmail(email) {
		return apierror.InvalidArgument("Invalid group email %q.", raw)
	}
	local, _, _ := strings.Cut(email, "@")
	switch {
	case utf8.RuneCountInString(local) > maxGroupEmailLocal:
		return apierror.InvalidArgument("Invalid group email %q: use no more than %d characters before the at sign (@).", raw, maxGroupEmailLocal)
	case strings.HasPrefix(local, ".") || strings.HasSuffix(local, "."):
		return apierror.InvalidArgument("Invalid group email %q: the part before the at sign (@) cannot begin or end with a period (.).", raw)
	case strings.Contains(local, ".."):
		return apierror.InvalidArgument("Invalid group email %q: it cannot contain more than one period (.) in a row.", raw)
	case strings.ContainsAny(local, "=&<>+,!"):
		return apierror.InvalidArgument("Invalid group email %q: it cannot contain an equal sign (=), ampersand (&), angle brackets (<, >), plus sign (+), comma (,) or exclamation point (!).", raw)
	}
	return nil
}

// checkGroupText checks a group's display name ("Can be up to 73
// characters") and description ("Must not be longer than 4,096
// characters"), counted in characters.
func checkGroupText(displayName, description string) error {
	if utf8.RuneCountInString(displayName) > maxGroupDisplayName {
		return apierror.InvalidArgument("The display name must not be longer than %d characters.", maxGroupDisplayName)
	}
	if utf8.RuneCountInString(description) > maxGroupDescription {
		return apierror.InvalidArgument("The description must not be longer than %d characters.", maxGroupDescription)
	}
	return nil
}

// securityMemberLocked refuses a group member that a security group
// cannot hold. "Security groups can only contain the following: Users
// inside or outside of your domain ...; Service accounts inside or outside
// of your domain; Security groups inside of your domain"
// (https://docs.cloud.google.com/identity/docs/how-to/update-group-to-security-group,
// read 2026-11-18). Google states no error for it; this is
// FAILED_PRECONDITION.
func (r *Registry) securityMemberLocked(g *Group, email string) error {
	if _, ok := g.Labels[LabelSecurity]; !ok {
		return nil
	}
	inner, err := r.groupByEmailLocked(email)
	if err != nil || inner == nil {
		return err
	}
	if _, ok := inner.Labels[LabelSecurity]; !ok {
		return apierror.FailedPrecondition("Security groups can only contain users, service accounts and security groups inside of your domain: %s is a group that is not a security group.", email)
	}
	return nil
}

func (r *Registry) loadGroupLocked(id string) (*Group, error) {
	b, err := r.st.Get(groupPrefix + id)
	if err != nil {
		return nil, nil
	}
	var g Group
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, apierror.Internal(err, "decode group %s", id)
	}
	// A membership whose MEMBER role has expired is gone.
	now := r.now()
	g.Members = slices.DeleteFunc(g.Members, func(m GroupMember) bool { return m.expired(now) })
	if g.Dynamic() {
		g.Members = r.dynamicMembersLocked(&g)
	}
	return &g, nil
}

func (r *Registry) putGroupLocked(g *Group) error {
	b, err := json.Marshal(g)
	if err != nil {
		return apierror.Internal(err, "encode group")
	}
	if err := r.st.Put(groupPrefix+g.ID, b); err != nil {
		return apierror.Internal(err, "store group")
	}
	return nil
}

// groupsLocked are every group, by email.
func (r *Registry) groupsLocked() ([]*Group, error) {
	keys, err := r.st.List(groupPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list groups")
	}
	var out []*Group
	for _, k := range keys {
		g, err := r.loadGroupLocked(strings.TrimPrefix(k, groupPrefix))
		if err != nil {
			return nil, err
		}
		if g != nil {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

func (r *Registry) groupByEmailLocked(email string) (*Group, error) {
	gs, err := r.groupsLocked()
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		if g.Email == email && g.Namespace == "" {
			return g, nil
		}
	}
	return nil, nil
}

// groupByKeyLocked is the group whose key is (id, namespace).
func (r *Registry) groupByKeyLocked(id, namespace string) (*Group, error) {
	if namespace == "" {
		return r.groupByEmailLocked(normEmail(id))
	}
	gs, err := r.groupsLocked()
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		if g.Namespace == namespace && g.Email == id {
			return g, nil
		}
	}
	return nil, nil
}

// groupFromNameLocked reads "groups/{id}".
func (r *Registry) groupFromNameLocked(name string) (*Group, error) {
	id, ok := strings.CutPrefix(name, "groups/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return nil, apierror.InvalidArgument("Invalid group name %q: it must be groups/{group}.", name)
	}
	g, err := r.loadGroupLocked(id)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, permissionDenied(name)
	}
	return g, nil
}

// GroupInput is what CreateGroup takes.
type GroupInput struct {
	Email, Namespace, Parent, DisplayName, Description string
	Labels                                             map[string]string
	Dynamic                                            bool
	// DynamicQueries are dynamicGroupMetadata.queries: resource type and
	// query.
	DynamicQueries [][2]string
}

// CreateGroup creates a Google Group under the instance's customer.
func (r *Registry) CreateGroup(in GroupInput) (*Group, error) {
	if in.Namespace != "" || strings.HasPrefix(in.Parent, "identitysources/") {
		return r.createMappedGroup(in)
	}
	var query string
	if in.Dynamic {
		q, err := dynamicQueryOf(in)
		if err != nil {
			return nil, err
		}
		query = q
	}
	in.Parent = customerParent(in.Parent)
	if in.Parent == "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: parent is required.")
	}
	if !strings.HasPrefix(in.Parent, "customers/C") {
		return nil, apierror.InvalidArgument("Invalid parent %q: it must be customers/{customer_id}, the customer ID beginning with C.", in.Parent)
	}
	if in.Parent != CustomerName {
		return nil, permissionDenied(in.Parent)
	}
	email := normEmail(in.Email)
	if email == "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: groupKey.id is required.")
	}
	if err := checkGroupEmail(in.Email, email); err != nil {
		return nil, err
	}
	if _, d, _ := strings.Cut(email, "@"); d != OrganizationDomain {
		return nil, apierror.InvalidArgument("Invalid group email %q: the customer's groups are created in its domain, %s.", in.Email, OrganizationDomain)
	}
	if _, ok := in.Labels[LabelDiscussionForum]; !ok && query == "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: labels must contain %s (a Google Group).", LabelDiscussionForum)
	}
	for k, v := range in.Labels {
		switch k {
		case LabelDiscussionForum, LabelSecurity, labelLocked, labelDynamic:
		default:
			return nil, apierror.InvalidArgument("Invalid label %q: a Google Group's labels are %s, %s and %s.", k, LabelDiscussionForum, LabelSecurity, labelLocked)
		}
		if v != "" {
			return nil, apierror.InvalidArgument("Invalid label %q: a group label's value is empty.", k)
		}
	}
	if err := checkGroupText(in.DisplayName, in.Description); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, err := r.groupByEmailLocked(email); err != nil {
		return nil, err
	} else if g != nil {
		return nil, apierror.AlreadyExists("Error(4003): Cannot create group which already exists.")
	}
	if query != "" {
		if err := r.dynamicLimitLocked(); err != nil {
			return nil, err
		}
	}
	now := r.now().UTC()
	g := &Group{ID: newIdentityID(), Email: email, DisplayName: in.DisplayName, Description: in.Description,
		Labels: map[string]string{}, CreateTime: now, UpdateTime: now, DynamicQuery: query}
	for k := range in.Labels {
		g.Labels[k] = ""
	}
	if err := r.putGroupLocked(g); err != nil {
		return nil, err
	}
	return g, nil
}

// GetGroup reads "groups/{id}".
func (r *Registry) GetGroup(name string) (*Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.groupFromNameLocked(name)
}

// ListGroups are the customer's groups, by email.
func (r *Registry) ListGroups(parent string) ([]*Group, error) {
	parent = customerParent(parent)
	if parent == "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: parent is required.")
	}
	if strings.HasPrefix(parent, "identitysources/") {
		return r.listMappedGroups(parent)
	}
	if !strings.HasPrefix(parent, "customers/C") {
		return nil, apierror.InvalidArgument("Invalid parent %q: it must be customers/{customer_id}, the customer ID beginning with C.", parent)
	}
	if parent != CustomerName {
		return nil, permissionDenied(parent)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.customerGroupsLocked()
}

// customerGroupsLocked are the customer's groups, without the
// identity-mapped ones.
func (r *Registry) customerGroupsLocked() ([]*Group, error) {
	gs, err := r.groupsLocked()
	if err != nil {
		return nil, err
	}
	out := gs[:0]
	for _, g := range gs {
		if g.Namespace == "" {
			out = append(out, g)
		}
	}
	return out, nil
}

// LookupGroup is the name of the group whose email is email.
func (r *Registry) LookupGroup(email, namespace string) (string, error) {
	if email == "" {
		return "", apierror.InvalidArgument("Request contains an invalid argument: groupKey.id is required.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.groupByKeyLocked(email, namespace)
	if err != nil {
		return "", err
	}
	if g == nil {
		return "", permissionDenied(email)
	}
	return g.Name(), nil
}

// UpdateGroup sets a group's displayName, description and labels, those
// mask names ("display_name", "description", "labels").
func (r *Registry) UpdateGroup(name string, in GroupInput, mask []string) (*Group, error) {
	if len(mask) == 0 {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: updateMask is required.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.groupFromNameLocked(name)
	if err != nil {
		return nil, err
	}
	for _, f := range mask {
		switch f {
		case "display_name", "displayName":
			if err := checkGroupText(in.DisplayName, ""); err != nil {
				return nil, err
			}
			g.DisplayName = in.DisplayName
		case "description":
			if err := checkGroupText("", in.Description); err != nil {
				return nil, err
			}
			g.Description = in.Description
		case "dynamic_group_metadata", "dynamicGroupMetadata":
			if !g.Dynamic() {
				return nil, apierror.InvalidArgument("Request contains an invalid argument: dynamicGroupMetadata is a dynamic group's; %s is not one.", g.Email)
			}
			q, err := dynamicQueryOf(in)
			if err != nil {
				return nil, err
			}
			g.DynamicQuery = q
		case "labels":
			if g.Dynamic() || g.Namespace != "" {
				return nil, apierror.InvalidArgument("Request contains an invalid argument: a dynamic or identity-mapped group's labels cannot be changed.")
			}
			if _, ok := in.Labels[LabelDiscussionForum]; !ok {
				return nil, apierror.InvalidArgument("Request contains an invalid argument: labels must contain %s (a Google Group).", LabelDiscussionForum)
			}
			if _, had := g.Labels[LabelSecurity]; had {
				if _, ok := in.Labels[LabelSecurity]; !ok {
					return nil, apierror.InvalidArgument("The %s label cannot be removed once added.", LabelSecurity)
				}
			}
			if _, had := g.Labels[LabelSecurity]; !had {
				if _, adds := in.Labels[LabelSecurity]; adds {
					// "You can't apply the security group label to a
					// Google Group that doesn't meet these conditions."
					probe := &Group{Labels: map[string]string{LabelSecurity: ""}}
					for _, m := range g.Members {
						if err := r.securityMemberLocked(probe, m.Email); err != nil {
							return nil, err
						}
					}
				}
			}
			labels := map[string]string{}
			for k := range in.Labels {
				switch k {
				case LabelDiscussionForum, LabelSecurity, labelLocked:
					labels[k] = ""
				default:
					return nil, apierror.InvalidArgument("Invalid label %q: a Google Group's labels are %s, %s and %s.", k, LabelDiscussionForum, LabelSecurity, labelLocked)
				}
			}
			g.Labels = labels
		default:
			return nil, apierror.InvalidArgument("Invalid update mask field %q: display_name, description and labels can be updated.", f)
		}
	}
	g.UpdateTime = r.now().UTC()
	if err := r.putGroupLocked(g); err != nil {
		return nil, err
	}
	if g.Dynamic() {
		g.Members = r.dynamicMembersLocked(g)
	}
	return g, nil
}

// DeleteGroup deletes a group, and its memberships of other groups.
func (r *Registry) DeleteGroup(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.groupFromNameLocked(name)
	if err != nil {
		return err
	}
	all, err := r.groupsLocked()
	if err != nil {
		return err
	}
	for _, o := range all {
		n := len(o.Members)
		o.Members = slices.DeleteFunc(o.Members, func(m GroupMember) bool { return m.Email == g.Email && m.Namespace == g.Namespace })
		if len(o.Members) != n {
			if err := r.putGroupLocked(o); err != nil {
				return err
			}
		}
	}
	if err := r.st.Delete(groupPrefix + g.ID); err != nil {
		return apierror.Internal(err, "delete group")
	}
	// Its SSO assignment (#1959) targets nothing any more.
	return r.dropGroupAssignmentsLocked(g.Name())
}

// membershipRoles checks roles (default MEMBER): each OWNER, MANAGER or
// MEMBER, none twice.
func membershipRoles(roles []string) ([]string, error) {
	if len(roles) == 0 {
		return []string{"MEMBER"}, nil
	}
	seen := map[string]bool{}
	for _, r := range roles {
		switch r {
		case "OWNER", "MANAGER", "MEMBER":
		default:
			return nil, apierror.InvalidArgument("Invalid membership role %q: it must be one of OWNER, MANAGER, MEMBER.", r)
		}
		if seen[r] {
			return nil, apierror.InvalidArgument("Membership roles must not contain duplicate roles: %s.", r)
		}
		seen[r] = true
	}
	out := slices.Clone(roles)
	if !seen["MEMBER"] {
		// Every membership holds MEMBER, which cannot be removed
		// (ModifyMembershipRolesRequest.removeRoles).
		out = append(out, "MEMBER")
	}
	sort.Slice(out, func(i, j int) bool { return roleRank(out[i]) < roleRank(out[j]) })
	return out, nil
}

func roleRank(r string) int { return map[string]int{"OWNER": 0, "MANAGER": 1, "MEMBER": 2}[r] }

// memberType is what kind of entity email is.
func (r *Registry) memberTypeLocked(email string) (string, error) {
	if strings.HasSuffix(email, ".gserviceaccount.com") {
		return "SERVICE_ACCOUNT", nil
	}
	g, err := r.groupByEmailLocked(email)
	if err != nil {
		return "", err
	}
	if g != nil {
		return "GROUP", nil
	}
	return "USER", nil
}

// MembershipName is a membership's resource name.
func MembershipName(g *Group, m GroupMember) string { return g.Name() + "/memberships/" + m.ID }

// CreateMembership adds email to the group parent names, with roles.
func (r *Registry) CreateMembership(parent, email, namespace string, roles []string) (*Group, GroupMember, error) {
	return r.CreateMembershipExpiring(parent, email, namespace, roles, nil)
}

// checkExpiry checks a MEMBER role's expiry time: in the future.
func (r *Registry) checkExpiry(t *time.Time) error {
	if t != nil && !t.After(r.now()) {
		return apierror.InvalidArgument("Invalid expiryDetail.expireTime %s: it must be in the future.", t.UTC().Format(time.RFC3339))
	}
	return nil
}

// CreateMembershipExpiring is CreateMembership with the MEMBER role's
// expiry (nil for none).
func (r *Registry) CreateMembershipExpiring(parent, email, namespace string, roles []string, expire *time.Time) (*Group, GroupMember, error) {
	if err := r.checkExpiry(expire); err != nil {
		return nil, GroupMember{}, err
	}
	if namespace != "" {
		return r.createMappedMembership(parent, email, namespace, roles, expire)
	}
	email = normEmail(email)
	if email == "" {
		return nil, GroupMember{}, apierror.InvalidArgument("Request contains an invalid argument: preferredMemberKey.id is required.")
	}
	if !validEmail(email) {
		return nil, GroupMember{}, apierror.InvalidArgument("Invalid member email %q.", email)
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
	if g.Dynamic() {
		return nil, GroupMember{}, dynamicMembershipsRefused(g)
	}
	if g.Namespace != "" {
		return nil, GroupMember{}, apierror.InvalidArgument("Invalid preferredMemberKey: an identity-mapped group's members are of its identity source, %s; set preferredMemberKey.namespace.", g.Namespace)
	}
	if email == g.Email {
		return nil, GroupMember{}, apierror.InvalidArgument("A group cannot be a member of itself.")
	}
	if inner, err := r.groupByEmailLocked(email); err != nil {
		return nil, GroupMember{}, err
	} else if inner != nil && inner.Dynamic() {
		// "You cannot add a dynamic group as a member of a regular Google
		// Group."
		return nil, GroupMember{}, apierror.FailedPrecondition("A dynamic group cannot be a member of a Google Group: %s is a dynamic group.", email)
	}
	for _, m := range g.Members {
		if m.Email == email {
			return nil, GroupMember{}, apierror.AlreadyExists("Error(4003): Member already exists.")
		}
	}
	if err := r.securityMemberLocked(g, email); err != nil {
		return nil, GroupMember{}, err
	}
	if err := r.checkRestrictionLocked(g, email); err != nil {
		return nil, GroupMember{}, err
	}
	typ, err := r.memberTypeLocked(email)
	if err != nil {
		return nil, GroupMember{}, err
	}
	now := r.now().UTC()
	m := GroupMember{ID: newIdentityID(), Email: email, Roles: roles, Type: typ, CreateTime: now, UpdateTime: now, ExpireTime: expire}
	g.Members = append(g.Members, m)
	if err := r.putGroupLocked(g); err != nil {
		return nil, GroupMember{}, err
	}
	return g, m, nil
}

// membershipFromNameLocked reads "groups/{g}/memberships/{m}".
func (r *Registry) membershipFromNameLocked(name string) (*Group, int, error) {
	gname, mid, ok := strings.Cut(name, "/memberships/")
	if !ok || mid == "" || strings.Contains(mid, "/") {
		return nil, 0, apierror.InvalidArgument("Invalid membership name %q: it must be groups/{group}/memberships/{membership}.", name)
	}
	g, err := r.groupFromNameLocked(gname)
	if err != nil {
		return nil, 0, err
	}
	for i, m := range g.Members {
		if m.ID == mid {
			return g, i, nil
		}
	}
	return nil, 0, permissionDenied(name)
}

// GetMembership reads a membership.
func (r *Registry) GetMembership(name string) (*Group, GroupMember, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, i, err := r.membershipFromNameLocked(name)
	if err != nil {
		return nil, GroupMember{}, err
	}
	return g, g.Members[i], nil
}

// DeleteMembership removes a membership.
func (r *Registry) DeleteMembership(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, i, err := r.membershipFromNameLocked(name)
	if err != nil {
		return err
	}
	if g.Dynamic() {
		return dynamicMembershipsRefused(g)
	}
	g.Members = slices.Delete(g.Members, i, i+1)
	return r.putGroupLocked(g)
}

// ListMemberships are a group's direct memberships.
func (r *Registry) ListMemberships(parent string) (*Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.groupFromNameLocked(parent)
}

// LookupMembership is the name of email's membership of parent.
func (r *Registry) LookupMembership(parent, email, namespace string) (string, error) {
	if email == "" {
		return "", apierror.InvalidArgument("Request contains an invalid argument: memberKey.id is required.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.groupFromNameLocked(parent)
	if err != nil {
		return "", err
	}
	for _, m := range g.Members {
		if m.Namespace == namespace && (m.Email == normEmail(email) || (namespace != "" && m.Email == email)) {
			return MembershipName(g, m), nil
		}
	}
	return "", permissionDenied(parent + "/memberships/" + email)
}

// GroupRelation is one group a member belongs to, directly or through
// nested groups.
type GroupRelation struct {
	Group    *Group
	Direct   bool
	Indirect bool
	// Roles are the member's roles in the group when it is a direct member.
	Roles []string
}

// transitiveLocked are the groups email belongs to: directly, or as a
// member of a member group, with each relation.
func (r *Registry) transitiveLocked(email string) ([]GroupRelation, error) {
	all, err := r.groupsLocked()
	if err != nil {
		return nil, err
	}
	rel := map[string]*GroupRelation{}
	var order []string
	add := func(g *Group, direct bool, roles []string) bool {
		x, ok := rel[g.Email]
		if !ok {
			x = &GroupRelation{Group: g}
			rel[g.Email] = x
			order = append(order, g.Email)
		}
		if direct {
			x.Direct, x.Roles = true, roles
		} else {
			x.Indirect = true
		}
		return !ok
	}
	queue := []string{}
	for _, g := range all {
		for _, m := range g.Members {
			if m.Email == email && m.Namespace == "" {
				add(g, true, m.Roles)
				queue = append(queue, g.Email)
			}
		}
	}
	visited := map[string]bool{email: true}
	for len(queue) > 0 {
		inner := queue[0]
		queue = queue[1:]
		if visited[inner] {
			continue
		}
		visited[inner] = true
		for _, g := range all {
			for _, m := range g.Members {
				if m.Email == inner && m.Namespace == "" {
					add(g, false, nil)
					queue = append(queue, g.Email)
				}
			}
		}
	}
	out := make([]GroupRelation, 0, len(order))
	for _, e := range order {
		out = append(out, *rel[e])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group.Email < out[j].Group.Email })
	return out, nil
}

// TransitiveGroups are the groups email belongs to, directly or through
// nested groups.
func (r *Registry) TransitiveGroups(email string) ([]GroupRelation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.transitiveLocked(normEmail(email))
}

// HasTransitiveMembership is whether email belongs to the group parent
// names, directly or through nested groups.
func (r *Registry) HasTransitiveMembership(parent, email string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.groupFromNameLocked(parent)
	if err != nil {
		return false, err
	}
	rels, err := r.transitiveLocked(normEmail(email))
	if err != nil {
		return false, err
	}
	for _, x := range rels {
		if x.Group.ID == g.ID {
			return true, nil
		}
	}
	return false, nil
}

// InDomain is whether email is a Google Account of the instance's Cloud
// Identity domain, which domain:OrganizationDomain names. A service account
// is the project's, not the domain's, and its email is never under it.
func InDomain(email string) bool {
	_, d, ok := strings.Cut(normEmail(email), "@")
	return ok && d == OrganizationDomain
}

// Principals are the policy members that name the account email: its own
// (user: or serviceAccount:), group:G for every group it belongs to,
// directly or through nested groups, and domain:D when it is an account of
// the instance's domain. All lower case.
func (r *Registry) Principals(email string) []string {
	if wif.IsFederated(strings.TrimSpace(email)) {
		// A federated subject (#1941): its principal:// identifier and
		// the principalSet:// identifiers its last exchange mapped.
		if r == nil {
			return []string{strings.TrimSpace(email)}
		}
		return r.federatedPrincipals(strings.TrimSpace(email))
	}
	if l := strings.ToLower(strings.TrimSpace(email)); strings.HasPrefix(l, "principal://") {
		// A resource's own identity, such as a Parameter Manager
		// parameter's (#2054), is its own policy member.
		return []string{strings.TrimSpace(email)}
	}
	email = normEmail(email)
	if email == "" {
		return nil
	}
	out := []string{"user:" + email}
	if strings.HasSuffix(email, ".gserviceaccount.com") {
		out = []string{"serviceAccount:" + email}
	}
	if r == nil {
		return out
	}
	if rels, err := r.TransitiveGroups(email); err == nil {
		for _, x := range rels {
			out = append(out, "group:"+x.Group.Email)
		}
	}
	if InDomain(email) {
		out = append(out, "domain:"+OrganizationDomain)
	}
	return out
}

// MemberNames is whether the policy member names the account email:
// directly, through a group (nested or not) or through its domain.
// allUsers and allAuthenticatedUsers are not matched here.
func (r *Registry) MemberNames(member, email string) bool {
	kind, m, ok := strings.Cut(strings.TrimSpace(member), ":")
	if !ok {
		return false
	}
	kind = strings.ToLower(kind)
	m = normEmail(m)
	email = normEmail(email)
	switch kind {
	case "user", "serviceaccount":
		return m == email
	case "domain":
		return m == OrganizationDomain && InDomain(email)
	case "group":
		if r == nil {
			return false
		}
		rels, err := r.TransitiveGroups(email)
		if err != nil {
			return false
		}
		for _, x := range rels {
			if x.Group.Email == m {
				return true
			}
		}
	}
	return false
}
