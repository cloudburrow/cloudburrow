// Package iameval is CloudBurrow's one evaluator of IAM allow policies
// (#1879): whether a principal holds a permission on a resource, and why,
// which the served APIs' enforcement (cloudburrow up --iam-enforcement=on),
// Policy Troubleshooter's iam:troubleshoot and Cloud Asset's
// analyzeIamPolicy all read.
//
// Google's model, as "Understanding allow policies" and "Policy
// inheritance" describe it (https://docs.cloud.google.com/iam/docs/allow-policies,
// https://docs.cloud.google.com/iam/docs/resource-hierarchy-access-control,
// read 2026-11-19): a principal has a permission on a resource when any
// allow policy in the resource's ancestry, the resource's own, its
// service-level parents' (a crypto key's key ring), its project's, its
// folders' and its organization's, has a binding whose role includes the
// permission, whose members name the principal, and whose condition, if it
// has one, evaluates to true. The effective policy is the union: a grant
// lower in the hierarchy cannot be taken away by a policy higher up.
//
// Roles map to permissions through internal/iamroles (Google's IAM roles
// and permissions index) for predefined roles, through the Source for
// custom roles, and, for the basic roles Google's pages list without their
// permissions, through basicGrants. Conditions are internal/iamcond's.
//
// Principal access boundary policies (IAM v3, #2057) are evaluated first
// (pab.go), then deny policies (IAM v2, #1886, deny.go): a resource outside
// every boundary bound to the principal, or a permission a deny policy in
// the ancestry denies, is refused whatever the allow policies grant.
package iameval

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// Source is what the evaluator reads of the instance: Resource Manager's
// hierarchy, policies, custom roles and Cloud Identity groups.
type Source interface {
	// Ancestry is a project's ancestry as policy resources, the project
	// first: "projects/ID", "folders/N", ..., "organizations/N". An
	// unknown project is just ["projects/ID"].
	Ancestry(project string) []string
	// Policy is a hierarchy resource's stored policy, nil for none.
	Policy(resource string) *iampolicy.Stored
	// CustomRolePermissions are a custom role's permissions; ok is false
	// when role is not a custom role's name.
	CustomRolePermissions(role string) (perms []string, ok bool)
	// Principals are an account's members: its own user: or
	// serviceAccount:, then group:G for every group it is in and
	// domain:D.
	Principals(email string) []string
}

// Attached is one policy in the checked resource's chain.
type Attached struct {
	// Resource is the policy's resource: a full resource name
	// ("//secretmanager.googleapis.com/projects/p/secrets/s") for a
	// service's resource, or "projects/p", "folders/n",
	// "organizations/n" for the hierarchy.
	Resource string
	Policy   *iampolicy.Stored
}

// Request is one access check.
type Request struct {
	// Email is the caller's account, "" for an unauthenticated caller,
	// who is only allUsers.
	Email      string
	Permission string
	// Resource is the checked resource's name as its service names it
	// ("projects/p/secrets/s/versions/1"), which conditions read as
	// resource.name.
	Resource string
	// Type and Service are resource.type and resource.service
	// ("secretmanager.googleapis.com/SecretVersion",
	// "secretmanager.googleapis.com").
	Type    string
	Service string
	// Project is the project ID the resource is in, "" for none.
	Project string
	// Own are the service's own policies on the resource and its
	// service-level parents, the resource's first.
	Own  []Attached
	Time time.Time
	// RequestHost and RequestPath are an Identity-Aware Proxy request's
	// host and URL path, which its bindings' conditions read as
	// request.host and request.path (#2160).
	RequestHost, RequestPath string
}

// Attributes are what req's conditions read: resource.name, resource.type,
// resource.service and request.time.
func (req Request) Attributes() iamcond.Attributes {
	at := req.Time
	if at.IsZero() {
		at = time.Now()
	}
	return iamcond.Attributes{ResourceName: req.Resource, ResourceType: req.Type, ResourceService: req.Service, RequestTime: at,
		RequestHost: req.RequestHost, RequestPath: req.RequestPath}
}

// Access is an access state, in Policy Troubleshooter v3's words.
type Access string

const (
	Granted    Access = "ALLOW_ACCESS_STATE_GRANTED"
	NotGranted Access = "ALLOW_ACCESS_STATE_NOT_GRANTED"
	Unknown    Access = "ALLOW_ACCESS_STATE_UNKNOWN_CONDITIONAL"
)

// BindingResult explains one binding.
type BindingResult struct {
	Role    string
	Members []string
	// RoleHasPermission is whether the role includes the permission.
	RoleHasPermission bool
	// Member is the binding's member that names the principal, "" for
	// none.
	Member    string
	Condition *iampolicy.Condition
	// ConditionTrue is the condition's value; true without one.
	ConditionTrue bool
	// Grants is all three.
	Grants bool
}

// PolicyResult explains one policy of the chain.
type PolicyResult struct {
	Resource string
	Policy   *iampolicy.Stored
	Bindings []BindingResult
	Grants   bool
}

// Decision is a check's answer and its explanation.
type Decision struct {
	// Granted is the answer: allowed and not denied.
	Granted bool
	// Allowed is whether an allow policy grants the permission.
	Allowed  bool
	Policies []PolicyResult
	// Denied is whether a deny policy denies it (#1886), Deny each deny
	// policy that applies.
	Denied bool
	Deny   []DenyPolicyResult
	// Blocked is whether a principal access boundary policy blocks it
	// (#2057, pab.go), Boundary each PAB policy bound to the principal.
	Blocked  bool
	Boundary []BoundaryResult
}

// Evaluator evaluates allow policies over a Source.
type Evaluator struct {
	Source Source
}

// Chain is every policy that applies to req's resource, the resource's own
// first and the organization's last.
func (e *Evaluator) Chain(req Request) []Attached {
	out := append([]Attached(nil), req.Own...)
	if req.Project != "" && e.Source != nil {
		for _, r := range e.Source.Ancestry(req.Project) {
			out = append(out, Attached{Resource: r, Policy: e.Source.Policy(r)})
		}
	}
	return out
}

// Check evaluates req.
func (e *Evaluator) Check(req Request) Decision {
	if req.Time.IsZero() {
		req.Time = time.Now()
	}
	members := e.members(req.Email)
	attrs := req.Attributes()
	attrs.Tags, attrs.TagsKnown = e.tags(req)
	var d Decision
	// Google's order: principal access boundary eligibility, then deny
	// policies, then allow policies (pab.go).
	d.Blocked, d.Boundary = e.boundary(req)
	d.Denied, d.Deny = e.deny(req, members, attrs)
	for _, a := range e.Chain(req) {
		pr := PolicyResult{Resource: a.Resource, Policy: a.Policy}
		if a.Policy != nil {
			for _, b := range a.Policy.Bindings {
				br := BindingResult{Role: b.Role, Members: b.Members, Condition: b.Condition,
					RoleHasPermission: e.RoleGrants(b.Role, req.Permission, req.Project)}
				br.Member = matchMember(b.Members, members)
				br.ConditionTrue = b.Grants(attrs)
				br.Grants = br.RoleHasPermission && br.Member != "" && br.ConditionTrue
				pr.Grants = pr.Grants || br.Grants
				pr.Bindings = append(pr.Bindings, br)
			}
		}
		d.Allowed = d.Allowed || pr.Grants
		d.Policies = append(d.Policies, pr)
	}
	d.Granted = d.Allowed && !d.Denied && !d.Blocked
	return d
}

// tags are req's resource's effective tags: its own and inherited, or,
// when the Source cannot name the resource (a bucket's name holds no
// project), its project's, which it inherits.
func (e *Evaluator) tags(req Request) ([]iamcond.Tag, bool) {
	ts, ok := e.Source.(TagSource)
	if !ok {
		return nil, false
	}
	if req.Service != "" && req.Resource != "" {
		if t, ok := ts.Tags("//" + req.Service + "/" + req.Resource); ok {
			return t, true
		}
	}
	if req.Project != "" {
		return ts.Tags("//cloudresourcemanager.googleapis.com/projects/" + req.Project)
	}
	return nil, false
}

// Denies is whether a deny policy denies req's permission to req's
// principal, or a principal access boundary policy blocks it (#2057),
// whatever the allow policies grant: what the instance's own account, an
// owner of everything, is still refused (#1886).
func (e *Evaluator) Denies(req Request) bool {
	if req.Time.IsZero() {
		req.Time = time.Now()
	}
	if blocked, _ := e.boundary(req); blocked {
		return true
	}
	attrs := req.Attributes()
	attrs.Tags, attrs.TagsKnown = e.tags(req)
	d, _ := e.deny(req, e.members(req.Email), attrs)
	return d
}

// ProjectTeams are the project teams email is on (#1887): "owners",
// "editors" and "viewers" for the basic roles Owner, Editor and Viewer the
// project's own policy grants it (unconditionally, or under a condition
// that holds at at), which Cloud Storage's project- ACL entities name.
func (e *Evaluator) ProjectTeams(project, email string, at time.Time) []string {
	if e.Source == nil || project == "" {
		return nil
	}
	pol := e.Source.Policy("projects/" + project)
	if pol == nil && len(e.Source.Ancestry(project)) > 0 {
		pol = e.Source.Policy(e.Source.Ancestry(project)[0])
	}
	if pol == nil {
		return nil
	}
	members := e.members(email)
	attrs := iamcond.Attributes{ResourceName: "projects/" + project, ResourceType: "cloudresourcemanager.googleapis.com/Project",
		ResourceService: "cloudresourcemanager.googleapis.com", RequestTime: at}
	team := map[string]string{"roles/owner": "owners", "roles/editor": "editors", "roles/viewer": "viewers"}
	seen := map[string]bool{}
	var out []string
	for _, b := range pol.Bindings {
		t, ok := team[b.Role]
		if !ok || seen[t] || matchMember(b.Members, members) == "" || !b.Grants(attrs) {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Members are the policy members that name email: its own, its groups'
// and domain's, allAuthenticatedUsers and allUsers; only allUsers for "".
func (e *Evaluator) members(email string) map[string]bool {
	out := map[string]bool{"allusers": true}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return out
	}
	out["allauthenticatedusers"] = true
	var ps []string
	if e.Source != nil {
		ps = e.Source.Principals(email)
	}
	if len(ps) == 0 {
		ps = []string{Member(email)}
	}
	for _, p := range ps {
		out[strings.ToLower(p)] = true
	}
	return out
}

// Members is Members for callers outside the package.
func (e *Evaluator) Members(email string) []string {
	m := e.members(email)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Member is an account's own policy member: serviceAccount: for a service
// account, user: for anyone else.
func Member(email string) string {
	if l := strings.ToLower(email); strings.HasPrefix(l, "principal://") || strings.HasPrefix(l, "principalset://") {
		// A federated identity (#1941) is its own member.
		return email
	}
	if strings.HasSuffix(strings.ToLower(email), ".gserviceaccount.com") {
		return "serviceAccount:" + email
	}
	return "user:" + email
}

func matchMember(bound []string, members map[string]bool) string {
	for _, m := range bound {
		if members[strings.ToLower(strings.TrimSpace(m))] {
			return m
		}
	}
	return ""
}

// RoleGrants reports whether role includes permission.
func (e *Evaluator) RoleGrants(role, permission, project string) bool {
	if perms, ok := e.customPerms(role); ok {
		return iamroles.Grants(perms, permission)
	}
	if g, ok := basicGrants(role, permission); ok {
		return g
	}
	return predefinedGrants(role, permission)
}

func (e *Evaluator) customPerms(role string) ([]string, bool) {
	if e.Source == nil || !(strings.HasPrefix(role, "projects/") || strings.HasPrefix(role, "organizations/")) {
		return nil, false
	}
	return e.Source.CustomRolePermissions(role)
}

// RolePermissions are the permissions role includes, for Policy Analyzer's
// permission queries: nil for a basic role, whose permissions are derived
// per permission (basicGrants).
func (e *Evaluator) RolePermissions(role string) []string {
	if perms, ok := e.customPerms(role); ok {
		return perms
	}
	if r := iamroles.Predefined(role); r != nil {
		return r.Permissions
	}
	return nil
}

var (
	predefinedMu   sync.Mutex
	predefinedSets = map[string]map[string]bool{}
)

func predefinedGrants(role, permission string) bool {
	predefinedMu.Lock()
	set, ok := predefinedSets[role]
	if !ok {
		if r := iamroles.Predefined(role); r != nil {
			set = make(map[string]bool, len(r.Permissions))
			for _, p := range r.Permissions {
				set[p] = true
			}
		}
		predefinedSets[role] = set
	}
	predefinedMu.Unlock()
	// Google lists some roles' permissions by their v2 names alone
	// (Workload Identity Pool Viewer's iam.googleapis.com/workloadIdentityPools.get,
	// #1879 round 12), which grant the v1 permission they name.
	return set[permission] || set[V2Permission(permission)]
}

// basicGrants is whether a basic role includes permission. Google's role
// pages list the basic roles without their permissions ("Roles overview"
// points to the console), so they are derived from what that page says of
// each, over the permissions of Google's catalog: Viewer has the read-only
// ones (get, list and getIamPolicy), Editor every permission but those that manage access
// (setIamPolicy, IAM roles, and the organization's and folders' own
// management), and Owner every permission. ok is false for a role that is
// not basic. Which permissions Google's basic roles hold exactly is
// UNVERIFIED (docs re-read 2026-11-19:
// https://docs.cloud.google.com/iam/docs/roles-overview#basic). Viewer's
// verbs exclude data access such as
// secretmanager.versions.access and cloudkms.cryptoKeyVersions.useToDecrypt.
func basicGrants(role, permission string) (grants, ok bool) {
	parts := strings.Split(permission, ".")
	verb := parts[len(parts)-1]
	// IAP's access permissions (iap.webServiceVersions.accessViaIAP, the
	// tunnel's and egress's) are not in the basic roles: a project owner
	// is granted roles/iap.httpsResourceAccessor before reaching an
	// IAP-secured app, as on Google (#2160; UNVERIFIED against
	// `gcloud iam roles describe roles/owner`, which the docs point to and
	// the role catalog here does not carry, re-read 2026-11-22:
	// https://docs.cloud.google.com/iam/docs/roles-overview).
	if strings.HasPrefix(permission, "iap.") && (verb == "accessViaIAP" || verb == "egressViaIAP") && strings.HasPrefix(role, "roles/") &&
		(role == "roles/owner" || role == "roles/editor" || role == "roles/viewer") {
		return false, true
	}
	switch role {
	case "roles/owner":
		return true, true
	case "roles/editor":
		switch {
		case verb == "setIamPolicy", strings.HasPrefix(permission, "iam.roles."),
			strings.HasPrefix(permission, "resourcemanager.organizations."),
			strings.HasPrefix(permission, "resourcemanager.folders."):
			return false, true
		}
		return true, true
	case "roles/viewer":
		switch verb {
		case "get", "list", "getIamPolicy":
			return true, true
		}
		return false, true
	}
	return false, false
}
