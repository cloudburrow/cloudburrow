package iameval

// Cloud Asset Inventory's IAM policy analysis (#1879, #2112): which
// principals have which access on which resources, from the allow policies
// of a scope (an organization, a folder or a project) and its descendants,
// as https://docs.cloud.google.com/asset-inventory/docs/analyzing-iam-policy
// and the IamPolicyAnalysisQuery reference describe it (read 2026-11-21):
//
//   - "Only IAM allow policies attached to this resource and to its
//     descendants will be analyzed": a project scope does not read its
//     folders' or organization's policies.
//   - resourceSelector narrows the analysis to the policies of the
//     resource and of its ancestors within the scope.
//   - identitySelector and accessSelector decide the identities and
//     accesses of each result; expandGroups and expandRoles may not be set
//     with them.
//   - expandGroups lists the members of the groups a binding names, capped
//     at 1,000 members per group; expandRoles a role's permissions;
//     expandResources the descendants of the resource a policy is attached
//     to (or of the selected resource, which must then be a project or
//     lower), capped at 1,000 per resource for analyzeIamPolicy and
//     100,000 for analyzeIamPolicyLongrunning; outputResourceEdges and
//     outputGroupEdges the parent/child and membership relationships.
//   - analyzeServiceAccountImpersonation adds the derived analyses through
//     the six impersonation permissions.
//   - A condition is evaluated over the resource's attributes and
//     conditionContext.accessTime: TRUE, CONDITIONAL when it reads what the
//     analysis does not know (request.time without an access time, or any
//     other request attribute), and a binding whose condition is false is
//     not included.
//
// Deny policies are not considered ("Results do not account for other
// access control mechanisms, like IAM deny policies").

import (
	"sort"
	"strings"
	"time"

	asset "google.golang.org/api/cloudasset/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// Analysis limits, from "Analyze IAM policies".
const (
	// GroupExpansionLimit is the members listed per group.
	GroupExpansionLimit = 1000
	// ResourceExpansionLimit is the descendants listed per resource by
	// analyzeIamPolicy, LongrunningResourceExpansionLimit by
	// analyzeIamPolicyLongrunning.
	ResourceExpansionLimit            = 1000
	LongrunningResourceExpansionLimit = 100000
)

// CRMPrefix is a hierarchy resource's full resource name prefix.
const CRMPrefix = "//cloudresourcemanager.googleapis.com/"

// ImpersonationPermissions are the permissions service account
// impersonation analysis follows.
var ImpersonationPermissions = []string{
	"iam.serviceAccounts.actAs", "iam.serviceAccounts.signBlob", "iam.serviceAccounts.signJwt",
	"iam.serviceAccounts.getAccessToken", "iam.serviceAccounts.getOpenIdToken", "iam.serviceAccounts.implicitDelegation",
}

// AnalysisOptions are IamPolicyAnalysisQuery.options.
type AnalysisOptions struct {
	ExpandGroups, ExpandRoles, ExpandResources bool
	OutputResourceEdges, OutputGroupEdges      bool
	AnalyzeServiceAccountImpersonation         bool
}

// AnalysisQuery is an IamPolicyAnalysisQuery.
type AnalysisQuery struct {
	// Scope is projects/ID, projects/NUMBER, folders/N or organizations/N.
	Scope string
	// Resource is resourceSelector.fullResourceName, "" for none.
	Resource string
	// Identity is identitySelector.identity, "" for none.
	Identity    string
	Permissions []string
	Roles       []string
	Options     AnalysisOptions
	// AccessTime is conditionContext.accessTime, zero for none.
	AccessTime time.Time
}

// Inventory is what the analysis reads of the instance, by full resource
// name.
type Inventory interface {
	// Root is the scope's full resource name, the project by its ID; an
	// error for a scope the instance does not hold.
	Root(scope string) (string, error)
	// Children are a resource's direct children the instance holds.
	Children(full string) []string
	// Policy is a resource's own allow policy, nil for none.
	Policy(full string) *iampolicy.Stored
	// Chain are the resource's own policy and its ancestors', the
	// resource's first and the organization's last, each by its full
	// resource name.
	Chain(full string) ([]Attached, error)
	// Attributes are what a condition reads of the resource.
	Attributes(full string) iamcond.Attributes
	// GroupMembers are a group's direct members, as policy members
	// ("user:a@x", "group:g@x", "serviceAccount:s@x").
	GroupMembers(group string) []string
	// Principals are the policy members that name an account or a group:
	// its own, then every group it is in, directly or not, and its domain.
	Principals(member string) []string
}

// Analyzer answers analyses over an evaluator's roles and an inventory.
type Analyzer struct {
	Eval *Evaluator
	Inv  Inventory
	// ResourceLimit is the descendants listed per resource:
	// ResourceExpansionLimit when 0.
	ResourceLimit int
}

// Validate is the query's errors before it runs.
func (q AnalysisQuery) Validate() error {
	if !(strings.HasPrefix(q.Scope, "projects/") || strings.HasPrefix(q.Scope, "folders/") || strings.HasPrefix(q.Scope, "organizations/")) ||
		strings.Count(q.Scope, "/") != 1 || strings.HasSuffix(q.Scope, "/") {
		return apierror.InvalidArgument("Invalid scope %q: it must be an organization number (such as \"organizations/123\"), a folder number (such as \"folders/123\"), a project ID (such as \"projects/my-project-id\") or a project number (such as \"projects/12345\").", q.Scope)
	}
	if q.Identity == "" && len(q.Permissions) == 0 && len(q.Roles) == 0 && q.Resource == "" {
		return apierror.InvalidArgument("At least one of resource selector, identity selector or access selector must be specified.")
	}
	if strings.ContainsAny(q.Identity, "*?") {
		return apierror.InvalidArgument("Invalid identity %q: wildcard characters (such as * and ?) are not supported. You must give a specific identity.", q.Identity)
	}
	if q.Identity != "" && !strings.Contains(q.Identity, ":") {
		return apierror.InvalidArgument("Invalid identity %q: it must be in the form of a principal in an IAM policy binding, such as \"user:mike@example.com\", \"group:admins@example.com\", \"domain:google.com\" or \"serviceAccount:my-project-id@appspot.gserviceaccount.com\".", q.Identity)
	}
	if q.Resource != "" && !strings.HasPrefix(q.Resource, "//") {
		return apierror.InvalidArgument("Invalid full resource name %q: see https://cloud.google.com/asset-inventory/docs/resource-name-format.", q.Resource)
	}
	if q.Options.ExpandGroups && q.Identity != "" {
		return apierror.InvalidArgument("analysis_query.options.expand_groups is not allowed to be set when analysis_query.identity_selector is specified.")
	}
	if q.Options.ExpandRoles && (len(q.Permissions) > 0 || len(q.Roles) > 0) {
		return apierror.InvalidArgument("analysis_query.options.expand_roles is not allowed to be set when analysis_query.access_selector is specified.")
	}
	if q.Options.ExpandResources && q.Resource != "" &&
		(strings.HasPrefix(q.Resource, CRMPrefix+"folders/") || strings.HasPrefix(q.Resource, CRMPrefix+"organizations/")) {
		return apierror.InvalidArgument("analysis_query.options.expand_resources only supports project or lower resources in analysis_query.resource_selector: folder and organization resources cannot be used together with this option.")
	}
	return nil
}

// tree is the scope's resources, breadth first from the scope.
type tree struct {
	order  []string
	parent map[string]string
	kids   map[string][]string
}

func (a *Analyzer) tree(root string) tree {
	t := tree{order: []string{root}, parent: map[string]string{}, kids: map[string][]string{}}
	seen := map[string]bool{root: true}
	for i := 0; i < len(t.order) && len(t.order) < 4*LongrunningResourceExpansionLimit; i++ {
		for _, c := range a.Inv.Children(t.order[i]) {
			if seen[c] {
				continue
			}
			seen[c] = true
			t.order = append(t.order, c)
			t.parent[c] = t.order[i]
			t.kids[t.order[i]] = append(t.kids[t.order[i]], c)
		}
	}
	return t
}

// descendants are full's descendants in t, breadth first, at most limit,
// and whether there were more.
func (t tree) descendants(full string, limit int) ([]string, bool) {
	var out []string
	queue := append([]string(nil), t.kids[full]...)
	for len(queue) > 0 {
		if len(out) == limit {
			return out, true
		}
		c := queue[0]
		queue = queue[1:]
		out = append(out, c)
		queue = append(queue, t.kids[c]...)
	}
	return out, false
}

// Analyze answers q (Validate first). The analyses after the first are
// the service account impersonation analyses.
func (a *Analyzer) Analyze(q AnalysisQuery) (*asset.IamPolicyAnalysis, []*asset.IamPolicyAnalysis, error) {
	if err := q.Validate(); err != nil {
		return nil, nil, err
	}
	root, err := a.Inv.Root(q.Scope)
	if err != nil {
		return nil, nil, err
	}
	t := a.tree(root)
	main, err := a.analyze(q, root, t)
	if err != nil {
		return nil, nil, err
	}
	if !q.Options.AnalyzeServiceAccountImpersonation {
		return main, nil, nil
	}
	var accounts []string
	seen := map[string]bool{}
	add := func(email string) {
		if email = strings.ToLower(email); email != "" && !seen[email] {
			seen[email] = true
			accounts = append(accounts, email)
		}
	}
	var derived []*asset.IamPolicyAnalysis
	if q.Identity != "" {
		// What the identity reaches through the service accounts it may
		// impersonate: those accounts' own access.
		sub := AnalysisQuery{Scope: q.Scope, Identity: q.Identity, Permissions: ImpersonationPermissions,
			Options: AnalysisOptions{ExpandResources: true}, AccessTime: q.AccessTime}
		sa, err := a.analyze(sub, root, t)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range sa.AnalysisResults {
			for _, acl := range r.AccessControlLists {
				for _, res := range acl.Resources {
					if email, ok := serviceAccountOf(res.FullResourceName); ok {
						add(email)
					}
				}
			}
		}
		for _, email := range accounts {
			d := q
			d.Identity, d.Options.AnalyzeServiceAccountImpersonation = "serviceAccount:"+email, false
			x, err := a.analyze(d, root, t)
			if err != nil {
				return nil, nil, err
			}
			if len(x.AnalysisResults) > 0 {
				derived = append(derived, x)
			}
		}
		return main, derived, nil
	}
	// Who reaches the access through the service accounts that hold it:
	// who may impersonate each.
	for _, r := range main.AnalysisResults {
		for _, id := range r.IdentityList.Identities {
			if k, email, ok := strings.Cut(id.Name, ":"); ok && strings.EqualFold(k, "serviceAccount") {
				add(email)
			}
		}
	}
	for _, email := range accounts {
		d := AnalysisQuery{Scope: q.Scope, Resource: "//iam.googleapis.com/" + ServiceAccountResource("", email),
			Permissions: ImpersonationPermissions, AccessTime: q.AccessTime}
		x, err := a.analyze(d, root, t)
		if err != nil {
			return nil, nil, err
		}
		if len(x.AnalysisResults) > 0 {
			derived = append(derived, x)
		}
	}
	return main, derived, nil
}

// serviceAccountOf is the email of a service account's full resource
// name.
func serviceAccountOf(full string) (string, bool) {
	rest, ok := strings.CutPrefix(full, "//iam.googleapis.com/projects/")
	if !ok {
		return "", false
	}
	_, email, ok := strings.Cut(rest, "/serviceAccounts/")
	return email, ok && email != "" && !strings.Contains(email, "/")
}

// QueryOut is q as an IamPolicyAnalysisQuery.
func (q AnalysisQuery) QueryOut() *asset.IamPolicyAnalysisQuery {
	out := &asset.IamPolicyAnalysisQuery{Scope: q.Scope}
	if q.Identity != "" {
		out.IdentitySelector = &asset.IdentitySelector{Identity: q.Identity}
	}
	if len(q.Permissions) > 0 || len(q.Roles) > 0 {
		out.AccessSelector = &asset.AccessSelector{Permissions: q.Permissions, Roles: q.Roles}
	}
	if q.Resource != "" {
		out.ResourceSelector = &asset.ResourceSelector{FullResourceName: q.Resource}
	}
	if o := q.Options; o != (AnalysisOptions{}) {
		out.Options = &asset.Options{ExpandGroups: o.ExpandGroups, ExpandRoles: o.ExpandRoles, ExpandResources: o.ExpandResources,
			OutputResourceEdges: o.OutputResourceEdges, OutputGroupEdges: o.OutputGroupEdges,
			AnalyzeServiceAccountImpersonation: o.AnalyzeServiceAccountImpersonation}
	}
	if !q.AccessTime.IsZero() {
		out.ConditionContext = &asset.ConditionContext{AccessTime: q.AccessTime.UTC().Format(time.RFC3339Nano)}
	}
	return out
}

// attachedPolicies are the policies q analyzes: the selected resource's
// chain within the scope, or every policy in the scope.
func (a *Analyzer) attachedPolicies(q AnalysisQuery, root string, t tree) ([]Attached, error) {
	if q.Resource == "" {
		var out []Attached
		for _, r := range t.order {
			if p := a.Inv.Policy(r); p != nil && len(p.Bindings) > 0 {
				out = append(out, Attached{Resource: r, Policy: p})
			}
		}
		return out, nil
	}
	chain, err := a.Inv.Chain(q.Resource)
	if err != nil {
		return nil, err
	}
	var out []Attached
	for _, c := range chain {
		out = append(out, c)
		if c.Resource == root {
			return out, nil
		}
	}
	// The resource is not under the scope.
	return nil, nil
}

func (a *Analyzer) analyze(q AnalysisQuery, root string, t tree) (*asset.IamPolicyAnalysis, error) {
	out := &asset.IamPolicyAnalysis{AnalysisQuery: q.QueryOut(), FullyExplored: true}
	attached, err := a.attachedPolicies(q, root, t)
	if err != nil {
		return nil, err
	}
	idMembers := a.identityMembers(q.Identity)
	limit := a.ResourceLimit
	if limit == 0 {
		limit = ResourceExpansionLimit
	}
	for _, p := range attached {
		if p.Policy == nil {
			continue
		}
		for _, b := range p.Policy.Bindings {
			r := a.result(q, p.Resource, b, idMembers, t, limit)
			if r == nil {
				continue
			}
			if !r.FullyExplored {
				out.FullyExplored = false
			}
			out.AnalysisResults = append(out.AnalysisResults, r)
		}
	}
	return out, nil
}

// identityMembers are the policy members that name the selected identity,
// lower case: its own, its groups' and domain's, allAuthenticatedUsers and
// allUsers. nil for no identity selector.
func (a *Analyzer) identityMembers(identity string) map[string]bool {
	if identity == "" {
		return nil
	}
	out := map[string]bool{strings.ToLower(identity): true}
	kind, _, _ := strings.Cut(identity, ":")
	switch strings.ToLower(kind) {
	case "user", "serviceaccount", "group":
		for _, m := range a.Inv.Principals(identity) {
			out[strings.ToLower(m)] = true
		}
		out["allusers"], out["allauthenticatedusers"] = true, true
	}
	return out
}

func (a *Analyzer) result(q AnalysisQuery, attached string, b iampolicy.Binding, idMembers map[string]bool, t tree, limit int) *asset.IamPolicyAnalysisResult {
	explored := true
	// Identities.
	ids := &asset.GoogleCloudAssetV1IdentityList{}
	if idMembers != nil {
		var matched []string
		for _, m := range b.Members {
			if idMembers[strings.ToLower(strings.TrimSpace(m))] {
				matched = append(matched, m)
			}
		}
		if len(matched) == 0 {
			return nil
		}
		ids.Identities = []*asset.GoogleCloudAssetV1Identity{{Name: q.Identity}}
		if q.Options.OutputGroupEdges {
			for _, m := range matched {
				ids.GroupEdges = append(ids.GroupEdges, a.groupPath(m, q.Identity)...)
			}
		}
	} else {
		seen := map[string]bool{}
		addID := func(name string) *asset.GoogleCloudAssetV1Identity {
			if seen[strings.ToLower(name)] {
				return nil
			}
			seen[strings.ToLower(name)] = true
			id := &asset.GoogleCloudAssetV1Identity{Name: name}
			ids.Identities = append(ids.Identities, id)
			return id
		}
		for _, m := range b.Members {
			id := addID(m)
			if !q.Options.ExpandGroups || !isGroup(m) {
				continue
			}
			members, edges, more := a.expandGroup(m)
			for _, x := range members {
				addID(x)
			}
			if q.Options.OutputGroupEdges {
				ids.GroupEdges = append(ids.GroupEdges, edges...)
			}
			if more {
				explored = false
				if id != nil {
					id.AnalysisState = &asset.IamPolicyAnalysisState{Code: "RESOURCE_EXHAUSTED",
						Cause: "The group has more members than the expansion limit of 1000 per group: only the first 1000 are listed."}
				}
			}
		}
	}
	// Accesses.
	var accesses []*asset.GoogleCloudAssetV1Access
	switch {
	case len(q.Permissions) > 0 || len(q.Roles) > 0:
		for _, perm := range q.Permissions {
			if a.Eval.RoleGrants(b.Role, perm, "") {
				accesses = append(accesses, &asset.GoogleCloudAssetV1Access{Permission: perm})
			}
		}
		if contains(q.Roles, b.Role) {
			accesses = append(accesses, &asset.GoogleCloudAssetV1Access{Role: b.Role})
		}
	case q.Options.ExpandRoles:
		for _, perm := range a.rolePermissions(b.Role) {
			accesses = append(accesses, &asset.GoogleCloudAssetV1Access{Permission: perm})
		}
		if len(accesses) == 0 {
			accesses = append(accesses, &asset.GoogleCloudAssetV1Access{Role: b.Role})
		}
	default:
		accesses = append(accesses, &asset.GoogleCloudAssetV1Access{Role: b.Role})
	}
	if len(accesses) == 0 {
		return nil
	}
	// Resources, grouped by the condition's value on each.
	base := attached
	if q.Resource != "" {
		base = q.Resource
	}
	resources := []string{base}
	if q.Options.ExpandResources {
		more, capped := t.descendants(base, limit)
		resources = append(resources, more...)
		if capped {
			explored = false
		}
	}
	groups := map[string][]string{}
	var order []string
	for _, r := range resources {
		v := a.conditionValue(b, r, q.AccessTime)
		if v == "FALSE" {
			continue
		}
		if _, ok := groups[v]; !ok {
			order = append(order, v)
		}
		groups[v] = append(groups[v], r)
	}
	if len(order) == 0 {
		return nil
	}
	ab := &asset.Binding{Role: b.Role, Members: b.Members}
	if c := b.Condition; c != nil {
		ab.Condition = &asset.Expr{Title: c.Title, Description: c.Description, Expression: c.Expression}
	}
	res := &asset.IamPolicyAnalysisResult{AttachedResourceFullName: attached, IamBinding: ab, IdentityList: ids}
	for _, v := range order {
		acl := &asset.GoogleCloudAssetV1AccessControlList{Accesses: accesses}
		in := map[string]bool{}
		for _, r := range groups[v] {
			in[r] = true
			acl.Resources = append(acl.Resources, &asset.GoogleCloudAssetV1Resource{FullResourceName: r})
		}
		if q.Options.OutputResourceEdges && q.Options.ExpandResources {
			for _, r := range groups[v] {
				if p, ok := t.parent[r]; ok && r != base && (in[p] || p == base) {
					acl.ResourceEdges = append(acl.ResourceEdges, &asset.GoogleCloudAssetV1Edge{SourceNode: p, TargetNode: r})
				}
			}
		}
		if v != "" {
			acl.ConditionEvaluation = &asset.ConditionEvaluation{EvaluationValue: v}
		}
		res.AccessControlLists = append(res.AccessControlLists, acl)
	}
	res.FullyExplored = explored
	return res
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func isGroup(member string) bool {
	k, _, ok := strings.Cut(member, ":")
	return ok && strings.EqualFold(k, "group")
}

// expandGroup is a group's members, directly or through nested groups,
// breadth first, at most GroupExpansionLimit, with the membership edges,
// and whether there were more.
func (a *Analyzer) expandGroup(group string) ([]string, []*asset.GoogleCloudAssetV1Edge, bool) {
	var out []string
	var edges []*asset.GoogleCloudAssetV1Edge
	seen := map[string]bool{strings.ToLower(group): true}
	queue := []string{group}
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]
		_, email, _ := strings.Cut(g, ":")
		for _, m := range a.Inv.GroupMembers(email) {
			if seen[strings.ToLower(m)] {
				continue
			}
			if len(out) == GroupExpansionLimit {
				return out, edges, true
			}
			seen[strings.ToLower(m)] = true
			out = append(out, m)
			edges = append(edges, &asset.GoogleCloudAssetV1Edge{SourceNode: g, TargetNode: m})
			if isGroup(m) {
				queue = append(queue, m)
			}
		}
	}
	return out, edges, false
}

// groupPath are the membership edges from a binding's member down to the
// identity: none when the member is the identity itself.
func (a *Analyzer) groupPath(member, identity string) []*asset.GoogleCloudAssetV1Edge {
	if !isGroup(member) || strings.EqualFold(member, identity) {
		return nil
	}
	prev := map[string]string{strings.ToLower(member): ""}
	names := map[string]string{strings.ToLower(member): member}
	queue := []string{member}
	target := strings.ToLower(identity)
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]
		_, email, _ := strings.Cut(g, ":")
		for _, m := range a.Inv.GroupMembers(email) {
			l := strings.ToLower(m)
			if _, ok := prev[l]; ok {
				continue
			}
			prev[l], names[l] = strings.ToLower(g), m
			if l == target {
				var edges []*asset.GoogleCloudAssetV1Edge
				for cur := l; prev[cur] != ""; cur = prev[cur] {
					edges = append([]*asset.GoogleCloudAssetV1Edge{{SourceNode: names[prev[cur]], TargetNode: names[cur]}}, edges...)
				}
				return edges
			}
			if isGroup(m) {
				queue = append(queue, m)
			}
		}
	}
	return nil
}

// rolePermissions are a role's permissions, sorted: a custom or
// predefined role's own, a basic role's derived over the catalog.
func (a *Analyzer) rolePermissions(role string) []string {
	if perms := a.Eval.RolePermissions(role); perms != nil {
		out := append([]string(nil), perms...)
		sort.Strings(out)
		return out
	}
	var out []string
	for _, p := range iamroles.Names() {
		if g, ok := basicGrants(role, p); ok && g {
			out = append(out, p)
		}
	}
	return out
}

// conditionValue is a binding's condition on a resource: "" for none,
// "TRUE", "FALSE" or "CONDITIONAL".
func (a *Analyzer) conditionValue(b iampolicy.Binding, full string, at time.Time) string {
	if b.Condition == nil {
		return ""
	}
	expr := b.Condition.Expression
	if unknownAttributes(expr, !at.IsZero()) {
		return "CONDITIONAL"
	}
	attrs := a.Inv.Attributes(full)
	attrs.RequestTime = at
	if at.IsZero() {
		attrs.RequestTime = time.Now()
	}
	c, err := iamcond.Compile(expr)
	if err != nil {
		return "CONDITIONAL"
	}
	if c.Eval(attrs) {
		return "TRUE"
	}
	return "FALSE"
}

// unknownAttributes is whether expr reads a request attribute the analysis
// is not given: request.time without an access time, or any other.
func unknownAttributes(expr string, haveTime bool) bool {
	rest := expr
	for {
		i := strings.Index(rest, "request.")
		if i < 0 {
			break
		}
		rest = rest[i+len("request."):]
		if !haveTime || !strings.HasPrefix(rest, "time") {
			return true
		}
	}
	return strings.Contains(expr, "api.getAttribute") || strings.Contains(expr, "matchTag") ||
		strings.Contains(expr, "destination.") || strings.Contains(expr, "origin.")
}
