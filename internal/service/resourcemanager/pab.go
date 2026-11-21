package resourcemanager

// IAM v3 principal access boundary (PAB) policies and policy bindings
// (#2057): google.iam.v3.PrincipalAccessBoundaryPolicies and
// google.iam.v3.PolicyBindings on the IAM API's port (this one), over gRPC
// and JSON, as "Create and apply principal access boundary policies" and the
// v3 REST reference describe them
// (https://docs.cloud.google.com/iam/docs/principal-access-boundary-policies-create,
// https://docs.cloud.google.com/iam/docs/reference/rest/v3/organizations.locations.principalAccessBoundaryPolicies,
// https://docs.cloud.google.com/iam/docs/reference/rest/v3/organizations.locations.policyBindings,
// and the google.iam.v3 protos' field documentation, read 2026-11-20):
//
//   - a policy is organizations/{org}/locations/global/principalAccessBoundaryPolicies/{id},
//     a binding {projects/P|folders/F|organizations/O}/locations/global/policyBindings/{id}
//     (a project by its number), each ID matching [a-z][a-z0-9-.]{2,62};
//   - a policy holds up to 500 rules and 500 resources across them, each
//     rule's effect ALLOW and its resources Resource Manager organizations,
//     folders or projects; its enforcementVersion is 1 to 4 or latest, and
//     an empty one is set to the newest version at creation; the display
//     name is at most 63 characters, a rule's description 256;
//   - an organization holds up to 1000 policies, and a principal set up to
//     10 bound PAB policies;
//   - a binding's target.principalSet is the binding parent itself (the
//     organization, folder or project) or a set the parent directly holds:
//     a workforce pool or the Workspace (organization), a workload identity
//     pool (project); its policyKind is PRINCIPAL_ACCESS_BOUNDARY (or empty,
//     then set), its policy one of the parent's organization's PAB
//     policies, and its condition reads principal.type and
//     principal.subject only (internal/iamcond's CompilePrincipal); target,
//     policy and policyKind are immutable;
//   - create, patch and delete answer a long-running operation, done here
//     (metadata google.iam.v3.OperationMetadata), which operations.get
//     reads back; validateOnly validates and stores nothing;
//   - deleting a policy that is bound is FAILED_PRECONDITION unless force
//     is set, which deletes its bindings too: "To delete the policy and all
//     associated bindings, select the Forcefully delete policy checkbox"
//     (https://docs.cloud.google.com/iam/docs/principal-access-boundary-policies-remove,
//     re-read 2026-11-20);
//   - an etag that is not the stored one is ABORTED, as it is for IAM v2.
//
// Access policies (policyKind ACCESS, target.resource) are not served: 501.
// The wording of the INVALID_ARGUMENT, NOT_FOUND and FAILED_PRECONDITION
// messages is UNVERIFIED: Google's pages do not quote them (re-read
// 2026-11-20: the v3 reference, and the removal page, which says only that
// the request "fails").

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const (
	pabPrefix     = "iam/pab/"
	bindingPrefix = "iam/policybindings/"

	// Google's limits (https://docs.cloud.google.com/iam/docs/principal-access-boundary-policies).
	MaxPABRules            = 500
	MaxPABResources        = 500
	MaxPABPoliciesPerOrg   = 1000
	MaxPABBindingsPerSet   = 10
	pabDefaultPageSize     = 50
	pabCollection          = "principalAccessBoundaryPolicies"
	bindingCollection      = "policyBindings"
	pabWorkforceSet        = "//iam.googleapis.com/locations/global/workforcePools/"
	pabWorkspaceSet        = "//iam.googleapis.com/locations/global/workspace/"
	pabCRM                 = "//cloudresourcemanager.googleapis.com/"
	pabLocationGlobal      = "locations/global"
	pabEnforcementVersions = "1, 2, 3, 4 or latest"
)

var (
	pabResourceRe = regexp.MustCompile(`^//cloudresourcemanager\.googleapis\.com/(organizations/[0-9]+|folders/[0-9]+|projects/[a-z0-9][a-z0-9-]*)$`)
	workloadSetRe = regexp.MustCompile(`^//iam\.googleapis\.com/projects/([0-9]+)/locations/global/workloadIdentityPools/([a-z0-9-]+)$`)
	principalType = map[string]bool{iameval.TypeWorkspace: true, iameval.TypeWorkforce: true, iameval.TypeWorkload: true,
		iameval.TypeSA: true, "iam.googleapis.com/AgentPoolIdentity": true}
)

// v3Parent is a parsed {projects|folders|organizations}/X/locations/L[/COLLECTION/ID].
type v3Parent struct {
	hierarchy string // "projects/p", "folders/n", "organizations/n" as given
	id        string
}

func parseV3(name, collection string, withID bool) (v3Parent, error) {
	parts := strings.Split(name, "/")
	want := 4
	if withID {
		want = 6
	}
	if len(parts) != want || parts[1] == "" || parts[2] != "locations" ||
		(parts[0] != "projects" && parts[0] != "folders" && parts[0] != "organizations") ||
		withID && (parts[4] != collection || parts[5] == "") {
		form := "{projects|folders|organizations}/{id}/locations/{location}"
		if collection == pabCollection {
			form = "organizations/{organization_id}/locations/{location}"
		}
		if withID {
			form += "/" + collection + "/{id}"
		}
		return v3Parent{}, apierror.InvalidArgument("%q is not a valid name: it must be %s", name, form)
	}
	if parts[3] != "global" {
		return v3Parent{}, apierror.InvalidArgument("location %q is not supported; the only location is global", parts[3])
	}
	p := v3Parent{hierarchy: parts[0] + "/" + parts[1]}
	if withID {
		p.id = parts[5]
	}
	return p, nil
}

func (r *Registry) readProto(key string, m proto.Message) (bool, error) {
	raw, err := r.st.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, apierror.Internal(err, "reading %s", key)
	}
	if err := protojson.Unmarshal(raw, m); err != nil {
		return false, apierror.Internal(err, "decoding %s", key)
	}
	return true, nil
}

func (r *Registry) writeProto(key string, m proto.Message) error {
	raw, err := protojson.Marshal(m)
	if err != nil {
		return apierror.Internal(err, "encoding %s", key)
	}
	if err := r.st.Put(key, raw); err != nil {
		return apierror.Internal(err, "writing %s", key)
	}
	return nil
}

func v3Etag(m proto.Message, clear func(proto.Message)) string {
	c := proto.Clone(m)
	clear(c)
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	return policyEtag(b)
}

func pabEtag(p *iamv3.PrincipalAccessBoundaryPolicy) string {
	return v3Etag(p, func(m proto.Message) { m.(*iamv3.PrincipalAccessBoundaryPolicy).Etag = "" })
}

func bindingEtag(b *iamv3.PolicyBinding) string {
	return v3Etag(b, func(m proto.Message) { m.(*iamv3.PolicyBinding).Etag = "" })
}

// ---- Principal access boundary policies ----

func (r *Registry) pabPoliciesLocked(org string) ([]*iamv3.PrincipalAccessBoundaryPolicy, error) {
	keys, err := r.st.List(pabPrefix + org + "/")
	if err != nil {
		return nil, apierror.Internal(err, "listing principal access boundary policies")
	}
	sort.Strings(keys)
	var out []*iamv3.PrincipalAccessBoundaryPolicy
	for _, k := range keys {
		p := &iamv3.PrincipalAccessBoundaryPolicy{}
		if ok, err := r.readProto(k, p); err != nil {
			return nil, err
		} else if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func checkPABContent(p *iamv3.PrincipalAccessBoundaryPolicy) error {
	if n := len([]rune(p.GetDisplayName())); n > 63 {
		return apierror.InvalidArgument("principal_access_boundary_policy.display_name must be at most 63 characters, not %d", n)
	}
	d := p.GetDetails()
	if d == nil || len(d.GetRules()) == 0 {
		return apierror.InvalidArgument("principal_access_boundary_policy.details.rules is required")
	}
	if n := len(d.GetRules()); n > MaxPABRules {
		return apierror.InvalidArgument("principal_access_boundary_policy.details.rules: a policy can contain at most %d rules, not %d", MaxPABRules, n)
	}
	if _, ok := iameval.EnforcementVersion(d.GetEnforcementVersion()); !ok {
		return apierror.InvalidArgument("principal_access_boundary_policy.details.enforcement_version %q is not valid: it must be %s", d.GetEnforcementVersion(), pabEnforcementVersions)
	}
	total := 0
	for i, rule := range d.GetRules() {
		if n := len([]rune(rule.GetDescription())); n > 256 {
			return apierror.InvalidArgument("principal_access_boundary_policy.details.rules[%d].description must be at most 256 characters, not %d", i, n)
		}
		if rule.GetEffect() != iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW {
			return apierror.InvalidArgument("principal_access_boundary_policy.details.rules[%d].effect must be ALLOW", i)
		}
		if len(rule.GetResources()) == 0 {
			return apierror.InvalidArgument("principal_access_boundary_policy.details.rules[%d].resources is required", i)
		}
		for j, res := range rule.GetResources() {
			if !pabResourceRe.MatchString(res) {
				return apierror.InvalidArgument("principal_access_boundary_policy.details.rules[%d].resources[%d]: %q is not a Resource Manager organization, folder or project, such as //cloudresourcemanager.googleapis.com/projects/PROJECT_ID", i, j, res)
			}
		}
		total += len(rule.GetResources())
	}
	if total > MaxPABResources {
		return apierror.InvalidArgument("principal_access_boundary_policy.details.rules: a policy can reference at most %d resources across its rules, not %d", MaxPABResources, total)
	}
	return nil
}

// CreatePABPolicy creates p in parent as id.
func (r *Registry) CreatePABPolicy(parent, id string, p *iamv3.PrincipalAccessBoundaryPolicy, validateOnly bool) (*iamv3.PrincipalAccessBoundaryPolicy, error) {
	n, err := parseV3(parent, pabCollection, false)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(n.hierarchy, "organizations/") {
		return nil, apierror.InvalidArgument("parent %q: principal access boundary policies are created in an organization", parent)
	}
	if !policyIDRe.MatchString(id) {
		return nil, apierror.InvalidArgument("principal_access_boundary_policy_id %q is not valid: it must start with a lowercase letter followed by 2 to 62 lowercase letters, numbers, hyphens or dots", id)
	}
	if p == nil {
		p = &iamv3.PrincipalAccessBoundaryPolicy{}
	}
	if err := checkPABContent(p); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	if checkOrg(n.hierarchy) != nil {
		return nil, apierror.NotFound("organization %s was not found", n.hierarchy)
	}
	key := pabPrefix + n.hierarchy + "/" + id
	name := parent + "/" + pabCollection + "/" + id
	if ok, err := r.readProto(key, &iamv3.PrincipalAccessBoundaryPolicy{}); err != nil {
		return nil, err
	} else if ok {
		return nil, apierror.AlreadyExists("principal access boundary policy %s already exists", name)
	}
	list, err := r.pabPoliciesLocked(n.hierarchy)
	if err != nil {
		return nil, err
	}
	if len(list) >= MaxPABPoliciesPerOrg {
		return nil, apierror.FailedPrecondition("an organization can have at most %d principal access boundary policies", MaxPABPoliciesPerOrg)
	}
	now := r.nowTS()
	details := proto.Clone(p.GetDetails()).(*iamv3.PrincipalAccessBoundaryPolicyDetails)
	if details.EnforcementVersion == "" {
		// "If empty, the PAB policy version will be set to the most recent
		// version number at the time of the policy's creation."
		details.EnforcementVersion = strconv.Itoa(iameval.MaxEnforcementVersion)
	}
	out := &iamv3.PrincipalAccessBoundaryPolicy{Name: name, Uid: newDenyUID(), DisplayName: p.GetDisplayName(),
		Annotations: p.GetAnnotations(), CreateTime: now, UpdateTime: now, Details: details}
	out.Etag = pabEtag(out)
	if validateOnly {
		return out, nil
	}
	return out, r.writeProto(key, out)
}

func (r *Registry) lookupPAB(name string) (string, *iamv3.PrincipalAccessBoundaryPolicy, error) {
	n, err := parseV3(name, pabCollection, true)
	if err != nil {
		return "", nil, err
	}
	key := pabPrefix + n.hierarchy + "/" + n.id
	p := &iamv3.PrincipalAccessBoundaryPolicy{}
	ok, err := r.readProto(key, p)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, apierror.NotFound("principal access boundary policy %s not found", name)
	}
	return key, p, nil
}

// GetPABPolicy is the policy name names.
func (r *Registry) GetPABPolicy(name string) (*iamv3.PrincipalAccessBoundaryPolicy, error) {
	r.mu.Lock()
	defer r.unlock()
	_, p, err := r.lookupPAB(name)
	return p, err
}

// ListPABPolicies is one page of parent's policies.
func (r *Registry) ListPABPolicies(parent string, size int32, token string) ([]*iamv3.PrincipalAccessBoundaryPolicy, string, error) {
	n, err := parseV3(parent, pabCollection, false)
	if err != nil {
		return nil, "", err
	}
	r.mu.Lock()
	defer r.unlock()
	list, err := r.pabPoliciesLocked(n.hierarchy)
	if err != nil {
		return nil, "", err
	}
	byName := map[string]*iamv3.PrincipalAccessBoundaryPolicy{}
	var names []string
	for _, p := range list {
		byName[p.GetName()] = p
		names = append(names, p.GetName())
	}
	page, next, err := paging.Page("pab:"+parent, names, token, pabPageSize(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("page_token %q is not valid", token)
	}
	out := make([]*iamv3.PrincipalAccessBoundaryPolicy, 0, len(page))
	for _, k := range page {
		out = append(out, byName[k])
	}
	return out, next, nil
}

func pabPageSize(size int32) int {
	if size <= 0 {
		return pabDefaultPageSize
	}
	return int(size)
}

// maskHas is whether mask (empty for all) names field or a field below it,
// in snake_case or camelCase.
func maskHas(mask *fieldmaskpb.FieldMask, fields ...string) bool {
	if len(mask.GetPaths()) == 0 {
		return true
	}
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }
	for _, p := range mask.GetPaths() {
		for _, f := range fields {
			if norm(p) == norm(f) || strings.HasPrefix(norm(f), norm(p)+".") {
				return true
			}
		}
	}
	return false
}

// UpdatePABPolicy replaces the fields mask names of the policy p names.
func (r *Registry) UpdatePABPolicy(p *iamv3.PrincipalAccessBoundaryPolicy, mask *fieldmaskpb.FieldMask, validateOnly bool) (*iamv3.PrincipalAccessBoundaryPolicy, error) {
	if p == nil || p.GetName() == "" {
		return nil, apierror.InvalidArgument("principal_access_boundary_policy.name is required")
	}
	for _, path := range mask.GetPaths() {
		if !maskHas(&fieldmaskpb.FieldMask{Paths: []string{path}}, "display_name", "annotations", "details.rules", "details.enforcement_version") {
			return nil, apierror.InvalidArgument("update_mask: %q is not a field that can be updated; the fields are display_name, annotations and details", path)
		}
	}
	r.mu.Lock()
	defer r.unlock()
	key, cur, err := r.lookupPAB(p.GetName())
	if err != nil {
		return nil, err
	}
	if p.GetEtag() != "" && p.GetEtag() != cur.GetEtag() {
		return nil, apierror.Aborted("the etag %q of principal access boundary policy %s is not the current one; read the policy and try again", p.GetEtag(), cur.GetName())
	}
	out := proto.Clone(cur).(*iamv3.PrincipalAccessBoundaryPolicy)
	if maskHas(mask, "display_name") {
		out.DisplayName = p.GetDisplayName()
	}
	if maskHas(mask, "annotations") {
		out.Annotations = p.GetAnnotations()
	}
	if maskHas(mask, "details.rules") {
		out.Details.Rules = p.GetDetails().GetRules()
	}
	if maskHas(mask, "details.enforcement_version") {
		out.Details.EnforcementVersion = p.GetDetails().GetEnforcementVersion()
		if out.Details.EnforcementVersion == "" {
			out.Details.EnforcementVersion = cur.GetDetails().GetEnforcementVersion()
		}
	}
	if err := checkPABContent(out); err != nil {
		return nil, err
	}
	out.UpdateTime = r.nowTS()
	out.Etag = pabEtag(out)
	if validateOnly {
		return out, nil
	}
	return out, r.writeProto(key, out)
}

// DeletePABPolicy removes the policy name names; a bound policy is
// FAILED_PRECONDITION unless force, which removes its bindings too.
func (r *Registry) DeletePABPolicy(name, etag string, validateOnly, force bool) (*iamv3.PrincipalAccessBoundaryPolicy, error) {
	r.mu.Lock()
	defer r.unlock()
	key, cur, err := r.lookupPAB(name)
	if err != nil {
		return nil, err
	}
	if etag != "" && etag != cur.GetEtag() {
		return nil, apierror.Aborted("the etag %q of principal access boundary policy %s is not the current one; read the policy and try again", etag, cur.GetName())
	}
	bound, err := r.bindingsLocked(func(b *iamv3.PolicyBinding) bool { return b.GetPolicy() == cur.GetName() })
	if err != nil {
		return nil, err
	}
	if len(bound) > 0 && !force {
		return nil, apierror.FailedPrecondition("principal access boundary policy %s is referenced in %d policy bindings; delete them first, or set force", cur.GetName(), len(bound))
	}
	if validateOnly {
		return cur, nil
	}
	for _, kb := range bound {
		if err := r.st.Delete(kb.key); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, apierror.Internal(err, "deleting policy binding %s", kb.b.GetName())
		}
	}
	if err := r.st.Delete(key); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, apierror.Internal(err, "deleting the principal access boundary policy")
	}
	return cur, nil
}

// ---- Policy bindings ----

type keyedBinding struct {
	key string
	b   *iamv3.PolicyBinding
}

// bindingsLocked are the stored bindings keep selects, by name.
func (r *Registry) bindingsLocked(keep func(*iamv3.PolicyBinding) bool) ([]keyedBinding, error) {
	keys, err := r.st.List(bindingPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "listing policy bindings")
	}
	sort.Strings(keys)
	var out []keyedBinding
	for _, k := range keys {
		b := &iamv3.PolicyBinding{}
		if ok, err := r.readProto(k, b); err != nil {
			return nil, err
		} else if ok && (keep == nil || keep(b)) {
			out = append(out, keyedBinding{key: k, b: b})
		}
	}
	return out, nil
}

// canonicalHierarchy resolves projects/{id or number}, folders/N or
// organizations/N to the resource, a project by its number. The caller
// holds r.mu.
func (r *Registry) canonicalHierarchy(h string) (string, error) {
	switch {
	case strings.HasPrefix(h, "projects/"):
		p, err := r.resolveProjectLocked(h)
		if err != nil {
			return "", apierror.NotFound("%s was not found", h)
		}
		return "projects/" + strconv.FormatInt(r.projectNumber(p.ProjectID), 10), nil
	case strings.HasPrefix(h, "folders/"):
		if _, err := r.loadFolder(h); err != nil {
			return "", apierror.NotFound("%s was not found", h)
		}
		return h, nil
	case strings.HasPrefix(h, "organizations/"):
		if checkOrg(h) != nil {
			return "", apierror.NotFound("%s was not found", h)
		}
		return h, nil
	}
	return "", apierror.InvalidArgument("%q is not a project, folder or organization", h)
}

// canonicalSet is a principal set as bindings are matched on it: a
// project's by its number; "" when it names nothing that exists. The
// caller holds r.mu.
func (r *Registry) canonicalSet(set string) string {
	if h, ok := strings.CutPrefix(set, pabCRM); ok {
		c, err := r.canonicalHierarchy(h)
		if err != nil {
			return ""
		}
		return pabCRM + c
	}
	return set
}

// checkTarget validates a binding's principal set against its parent
// (canonical).
func (r *Registry) checkTarget(parent, set string) error {
	bad := func() error {
		return apierror.InvalidArgument("policy_binding.target.principal_set %q is not a principal set a binding in %s can target: it must be the parent itself, or a workforce pool or the Workspace for an organization, or a workload identity pool for a project", set, parent)
	}
	switch {
	case strings.HasPrefix(set, pabCRM):
		if r.canonicalSet(set) != pabCRM+parent {
			return bad()
		}
	case strings.HasPrefix(set, pabWorkforceSet), strings.HasPrefix(set, pabWorkspaceSet):
		rest := strings.TrimPrefix(strings.TrimPrefix(set, pabWorkforceSet), pabWorkspaceSet)
		if !strings.HasPrefix(parent, "organizations/") || rest == "" || strings.Contains(rest, "/") {
			return bad()
		}
	default:
		m := workloadSetRe.FindStringSubmatch(set)
		if m == nil || parent != "projects/"+m[1] {
			return bad()
		}
	}
	return nil
}

// organizationOf is the organization a canonical hierarchy resource is in.
func (r *Registry) organizationOfLocked(h string) string {
	if strings.HasPrefix(h, "organizations/") {
		return h
	}
	// The instance has one organization, which holds every folder and
	// every project it holds.
	return OrganizationName
}

func checkBindingCondition(b *iamv3.PolicyBinding) error {
	c := b.GetCondition()
	if c == nil || c.GetExpression() == "" {
		return nil
	}
	if _, err := iamcond.CompilePrincipal(c.GetExpression()); err != nil {
		return apierror.InvalidArgument("policy_binding.condition: %v", err)
	}
	for _, s := range regexp.MustCompile(`principal\.type\s*(?:==|!=)\s*['"]([^'"]*)['"]`).FindAllStringSubmatch(c.GetExpression(), -1) {
		if !principalType[s[1]] {
			return apierror.InvalidArgument("policy_binding.condition: %q is not a principal type; it must be one of iam.googleapis.com/WorkspaceIdentity, WorkforcePoolIdentity, WorkloadPoolIdentity, ServiceAccount or AgentPoolIdentity", s[1])
		}
	}
	return nil
}

// CreatePolicyBinding binds b in parent as id.
func (r *Registry) CreatePolicyBinding(parent, id string, b *iamv3.PolicyBinding, validateOnly bool) (*iamv3.PolicyBinding, error) {
	n, err := parseV3(parent, bindingCollection, false)
	if err != nil {
		return nil, err
	}
	if !policyIDRe.MatchString(id) {
		return nil, apierror.InvalidArgument("policy_binding_id %q is not valid: it must start with a lowercase letter followed by 2 to 62 lowercase letters, numbers, hyphens or dots", id)
	}
	if b == nil {
		b = &iamv3.PolicyBinding{}
	}
	if b.GetPolicyKind() == iamv3.PolicyBinding_ACCESS || b.GetTarget().GetResource() != "" {
		return nil, apierror.Unimplemented("access policy bindings are not supported; only principal access boundary policies can be bound")
	}
	if n := len([]rune(b.GetDisplayName())); n > 63 {
		return nil, apierror.InvalidArgument("policy_binding.display_name must be at most 63 characters, not %d", n)
	}
	set := b.GetTarget().GetPrincipalSet()
	if set == "" {
		return nil, apierror.InvalidArgument("policy_binding.target.principal_set is required")
	}
	if b.GetPolicy() == "" {
		return nil, apierror.InvalidArgument("policy_binding.policy is required")
	}
	if err := checkBindingCondition(b); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	c, err := r.canonicalHierarchy(n.hierarchy)
	if err != nil {
		return nil, err
	}
	if err := r.checkTarget(c, set); err != nil {
		return nil, err
	}
	pn, err := parseV3(b.GetPolicy(), pabCollection, true)
	if err != nil {
		return nil, apierror.InvalidArgument("policy_binding.policy: %v", err)
	}
	if pn.hierarchy != r.organizationOfLocked(c) {
		return nil, apierror.InvalidArgument("policy_binding.policy %s is not in the binding parent's organization %s", b.GetPolicy(), r.organizationOfLocked(c))
	}
	_, pol, err := r.lookupPAB(b.GetPolicy())
	if err != nil {
		return nil, err
	}
	key := bindingPrefix + c + "/" + id
	name := c + "/" + pabLocationGlobal + "/" + bindingCollection + "/" + id
	if ok, err := r.readProto(key, &iamv3.PolicyBinding{}); err != nil {
		return nil, err
	} else if ok {
		return nil, apierror.AlreadyExists("policy binding %s already exists", name)
	}
	canonical := r.canonicalSet(set)
	same, err := r.bindingsLocked(func(x *iamv3.PolicyBinding) bool {
		return r.canonicalSet(x.GetTarget().GetPrincipalSet()) == canonical
	})
	if err != nil {
		return nil, err
	}
	if len(same) >= MaxPABBindingsPerSet {
		return nil, apierror.FailedPrecondition("principal set %s already has %d principal access boundary policies bound; the limit is %d", set, len(same), MaxPABBindingsPerSet)
	}
	now := r.nowTS()
	out := &iamv3.PolicyBinding{Name: name, Uid: newDenyUID(), DisplayName: b.GetDisplayName(), Annotations: b.GetAnnotations(),
		Target:     &iamv3.PolicyBinding_Target{Target: &iamv3.PolicyBinding_Target_PrincipalSet{PrincipalSet: set}},
		PolicyKind: iamv3.PolicyBinding_PRINCIPAL_ACCESS_BOUNDARY, Policy: pol.GetName(), PolicyUid: pol.GetUid(),
		Condition: b.GetCondition(), CreateTime: now, UpdateTime: now}
	out.Etag = bindingEtag(out)
	if validateOnly {
		return out, nil
	}
	return out, r.writeProto(key, out)
}

func (r *Registry) lookupBinding(name string) (string, *iamv3.PolicyBinding, error) {
	n, err := parseV3(name, bindingCollection, true)
	if err != nil {
		return "", nil, err
	}
	c, err := r.canonicalHierarchy(n.hierarchy)
	if err != nil {
		return "", nil, apierror.NotFound("policy binding %s not found", name)
	}
	key := bindingPrefix + c + "/" + n.id
	b := &iamv3.PolicyBinding{}
	ok, err := r.readProto(key, b)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, apierror.NotFound("policy binding %s not found", name)
	}
	return key, b, nil
}

// GetPolicyBinding is the binding name names.
func (r *Registry) GetPolicyBinding(name string) (*iamv3.PolicyBinding, error) {
	r.mu.Lock()
	defer r.unlock()
	_, b, err := r.lookupBinding(name)
	return b, err
}

// bindingFilter is a list filter: target:X or policy:X, X a name or a
// prefix ending in *, case insensitive; "" for none.
func bindingFilter(filter string) (func(*iamv3.PolicyBinding) bool, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return func(*iamv3.PolicyBinding) bool { return true }, nil
	}
	field, val, ok := strings.Cut(filter, ":")
	if !ok {
		field, val, ok = strings.Cut(filter, "=")
	}
	field = strings.ToLower(strings.TrimSpace(field))
	val = strings.ToLower(strings.Trim(strings.TrimSpace(val), `"`))
	if !ok || field != "target" && field != "policy" {
		return nil, apierror.InvalidArgument("filter %q is not valid: filter on target or policy, such as target:example or policy:example*", filter)
	}
	return func(b *iamv3.PolicyBinding) bool {
		v := b.GetPolicy()
		if field == "target" {
			v = b.GetTarget().GetPrincipalSet()
		}
		v = strings.ToLower(v)
		short := v[strings.LastIndex(v, "/")+1:]
		if p, ok := strings.CutSuffix(val, "*"); ok {
			return strings.HasPrefix(v, p) || strings.HasPrefix(short, p)
		}
		return v == val || short == val
	}, nil
}

func pageBindings(scope string, list []keyedBinding, size int32, token string) ([]*iamv3.PolicyBinding, string, error) {
	byName := map[string]*iamv3.PolicyBinding{}
	var names []string
	for _, kb := range list {
		byName[kb.b.GetName()] = kb.b
		names = append(names, kb.b.GetName())
	}
	page, next, err := paging.Page(scope, names, token, pabPageSize(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("page_token %q is not valid", token)
	}
	out := make([]*iamv3.PolicyBinding, 0, len(page))
	for _, k := range page {
		out = append(out, byName[k])
	}
	return out, next, nil
}

// ListPolicyBindings is one page of parent's bindings that filter selects.
func (r *Registry) ListPolicyBindings(parent string, size int32, token, filter string) ([]*iamv3.PolicyBinding, string, error) {
	n, err := parseV3(parent, bindingCollection, false)
	if err != nil {
		return nil, "", err
	}
	keep, err := bindingFilter(filter)
	if err != nil {
		return nil, "", err
	}
	r.mu.Lock()
	defer r.unlock()
	c, err := r.canonicalHierarchy(n.hierarchy)
	if err != nil {
		return nil, "", err
	}
	list, err := r.bindingsLocked(func(b *iamv3.PolicyBinding) bool {
		return strings.HasPrefix(b.GetName(), c+"/") && keep(b)
	})
	if err != nil {
		return nil, "", err
	}
	return pageBindings("bindings:"+parent+"|"+filter, list, size, token)
}

// SearchTargetPolicyBindings are parent's bindings whose target is target.
func (r *Registry) SearchTargetPolicyBindings(parent, target string, size int32, token, filter string) ([]*iamv3.PolicyBinding, string, error) {
	n, err := parseV3(parent, bindingCollection, false)
	if err != nil {
		return nil, "", err
	}
	if target == "" {
		return nil, "", apierror.InvalidArgument("target is required")
	}
	accessOnly := false
	switch strings.ReplaceAll(filter, " ", "") {
	case "", "policy_kind=PRINCIPAL_ACCESS_BOUNDARY":
	case "policy_kind=ACCESS":
		// Access policy bindings are not served, so none matches.
		accessOnly = true
	default:
		return nil, "", apierror.InvalidArgument("filter %q is not valid: it must be policy_kind=PRINCIPAL_ACCESS_BOUNDARY or policy_kind=ACCESS", filter)
	}
	r.mu.Lock()
	defer r.unlock()
	c, err := r.canonicalHierarchy(n.hierarchy)
	if err != nil {
		return nil, "", err
	}
	want := r.canonicalSet(target)
	list, err := r.bindingsLocked(func(b *iamv3.PolicyBinding) bool {
		return !accessOnly && strings.HasPrefix(b.GetName(), c+"/") && r.canonicalSet(b.GetTarget().GetPrincipalSet()) == want
	})
	if err != nil {
		return nil, "", err
	}
	return pageBindings("search:"+parent+"|"+target+"|"+filter, list, size, token)
}

// SearchPABPolicyBindings are the bindings of the policy name names.
func (r *Registry) SearchPABPolicyBindings(name string, size int32, token string) ([]*iamv3.PolicyBinding, string, error) {
	r.mu.Lock()
	defer r.unlock()
	_, p, err := r.lookupPAB(name)
	if err != nil {
		return nil, "", err
	}
	list, err := r.bindingsLocked(func(b *iamv3.PolicyBinding) bool { return b.GetPolicy() == p.GetName() })
	if err != nil {
		return nil, "", err
	}
	return pageBindings("pabsearch:"+name, list, size, token)
}

// UpdatePolicyBinding replaces the display name, annotations and
// condition mask names of the binding b names.
func (r *Registry) UpdatePolicyBinding(b *iamv3.PolicyBinding, mask *fieldmaskpb.FieldMask, validateOnly bool) (*iamv3.PolicyBinding, error) {
	if b == nil || b.GetName() == "" {
		return nil, apierror.InvalidArgument("policy_binding.name is required")
	}
	for _, path := range mask.GetPaths() {
		if !maskHas(&fieldmaskpb.FieldMask{Paths: []string{path}}, "display_name", "annotations", "condition") {
			return nil, apierror.InvalidArgument("update_mask: %q cannot be updated; the fields that can are display_name, annotations and condition", path)
		}
	}
	if n := len([]rune(b.GetDisplayName())); n > 63 {
		return nil, apierror.InvalidArgument("policy_binding.display_name must be at most 63 characters, not %d", n)
	}
	if err := checkBindingCondition(b); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.unlock()
	key, cur, err := r.lookupBinding(b.GetName())
	if err != nil {
		return nil, err
	}
	if b.GetEtag() != "" && b.GetEtag() != cur.GetEtag() {
		return nil, apierror.Aborted("the etag %q of policy binding %s is not the current one; read the binding and try again", b.GetEtag(), cur.GetName())
	}
	if s := b.GetTarget().GetPrincipalSet(); s != "" && s != cur.GetTarget().GetPrincipalSet() {
		return nil, apierror.InvalidArgument("policy_binding.target is immutable")
	}
	if b.GetPolicy() != "" && b.GetPolicy() != cur.GetPolicy() {
		return nil, apierror.InvalidArgument("policy_binding.policy is immutable")
	}
	if k := b.GetPolicyKind(); k != iamv3.PolicyBinding_POLICY_KIND_UNSPECIFIED && k != cur.GetPolicyKind() {
		return nil, apierror.InvalidArgument("policy_binding.policy_kind is immutable")
	}
	out := proto.Clone(cur).(*iamv3.PolicyBinding)
	if maskHas(mask, "display_name") {
		out.DisplayName = b.GetDisplayName()
	}
	if maskHas(mask, "annotations") {
		out.Annotations = b.GetAnnotations()
	}
	if maskHas(mask, "condition") {
		out.Condition = b.GetCondition()
	}
	out.UpdateTime = r.nowTS()
	out.Etag = bindingEtag(out)
	if validateOnly {
		return out, nil
	}
	return out, r.writeProto(key, out)
}

// DeletePolicyBinding removes the binding name names.
func (r *Registry) DeletePolicyBinding(name, etag string, validateOnly bool) (*iamv3.PolicyBinding, error) {
	r.mu.Lock()
	defer r.unlock()
	key, cur, err := r.lookupBinding(name)
	if err != nil {
		return nil, err
	}
	if etag != "" && etag != cur.GetEtag() {
		return nil, apierror.Aborted("the etag %q of policy binding %s is not the current one; read the binding and try again", etag, cur.GetName())
	}
	if validateOnly {
		return cur, nil
	}
	if err := r.st.Delete(key); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, apierror.Internal(err, "deleting the policy binding")
	}
	return cur, nil
}

// BoundaryBindings are the PAB bindings whose target is set, each with
// its policy, for the evaluator.
func (r *Registry) BoundaryBindings(set string) []iameval.Bound {
	r.mu.Lock()
	defer r.unlock()
	want := r.canonicalSet(set)
	if want == "" {
		return nil
	}
	list, err := r.bindingsLocked(func(b *iamv3.PolicyBinding) bool {
		return b.GetPolicyKind() == iamv3.PolicyBinding_PRINCIPAL_ACCESS_BOUNDARY && r.canonicalSet(b.GetTarget().GetPrincipalSet()) == want
	})
	if err != nil {
		return nil
	}
	out := make([]iameval.Bound, 0, len(list))
	for _, kb := range list {
		bd := iameval.Bound{Binding: kb.b}
		if _, p, err := r.lookupPAB(kb.b.GetPolicy()); err == nil {
			bd.Policy = p
		}
		out = append(out, bd)
	}
	return out
}

// ---- gRPC ----

// PABServer is google.iam.v3.PrincipalAccessBoundaryPolicies and
// google.iam.v3.PolicyBindings over reg.
type PABServer struct {
	iamv3.UnimplementedPrincipalAccessBoundaryPoliciesServer
	reg *Registry
}

// BindingsServer is google.iam.v3.PolicyBindings over reg.
type BindingsServer struct {
	iamv3.UnimplementedPolicyBindingsServer
	reg *Registry
}

// NewPABServer is the IAM v3 PrincipalAccessBoundaryPolicies service.
func NewPABServer(reg *Registry) *PABServer { return &PABServer{reg: reg} }

// NewBindingsServer is the IAM v3 PolicyBindings service.
func NewBindingsServer(reg *Registry) *BindingsServer { return &BindingsServer{reg: reg} }

// v3Operation is a done operation in parent (the resource's
// {hierarchy}/locations/global) for target, answering resp.
func v3Operation(reg *Registry, target, verb string, resp proto.Message) (*longrunningpb.Operation, error) {
	r, err := anypb.New(resp)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "packing the response"))
	}
	now := reg.nowTS()
	md, err := anypb.New(&iamv3.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v3"})
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "packing the metadata"))
	}
	parent := target
	if i := strings.Index(target, "/"+pabCollection+"/"); i >= 0 {
		parent = target[:i]
	} else if i := strings.Index(target, "/"+bindingCollection+"/"); i >= 0 {
		parent = target[:i]
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	op := &longrunningpb.Operation{Name: parent + "/operations/operation-" + hex.EncodeToString(b[:]), Metadata: md, Done: true,
		Result: &longrunningpb.Operation_Response{Response: r}}
	v3Operations.put(op)
	return op, nil
}

func (s *PABServer) CreatePrincipalAccessBoundaryPolicy(_ context.Context, req *iamv3.CreatePrincipalAccessBoundaryPolicyRequest) (*longrunningpb.Operation, error) {
	p, err := s.reg.CreatePABPolicy(req.GetParent(), req.GetPrincipalAccessBoundaryPolicyId(), req.GetPrincipalAccessBoundaryPolicy(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return v3Operation(s.reg, p.GetName(), "create", p)
}

func (s *PABServer) GetPrincipalAccessBoundaryPolicy(_ context.Context, req *iamv3.GetPrincipalAccessBoundaryPolicyRequest) (*iamv3.PrincipalAccessBoundaryPolicy, error) {
	p, err := s.reg.GetPABPolicy(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return p, nil
}

func (s *PABServer) UpdatePrincipalAccessBoundaryPolicy(_ context.Context, req *iamv3.UpdatePrincipalAccessBoundaryPolicyRequest) (*longrunningpb.Operation, error) {
	p, err := s.reg.UpdatePABPolicy(req.GetPrincipalAccessBoundaryPolicy(), req.GetUpdateMask(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return v3Operation(s.reg, p.GetName(), "update", p)
}

func (s *PABServer) DeletePrincipalAccessBoundaryPolicy(_ context.Context, req *iamv3.DeletePrincipalAccessBoundaryPolicyRequest) (*longrunningpb.Operation, error) {
	p, err := s.reg.DeletePABPolicy(req.GetName(), req.GetEtag(), req.GetValidateOnly(), req.GetForce())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return v3Operation(s.reg, p.GetName(), "delete", &emptypb.Empty{})
}

func (s *PABServer) ListPrincipalAccessBoundaryPolicies(_ context.Context, req *iamv3.ListPrincipalAccessBoundaryPoliciesRequest) (*iamv3.ListPrincipalAccessBoundaryPoliciesResponse, error) {
	list, next, err := s.reg.ListPABPolicies(req.GetParent(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &iamv3.ListPrincipalAccessBoundaryPoliciesResponse{PrincipalAccessBoundaryPolicies: list, NextPageToken: next}, nil
}

func (s *PABServer) SearchPrincipalAccessBoundaryPolicyBindings(_ context.Context, req *iamv3.SearchPrincipalAccessBoundaryPolicyBindingsRequest) (*iamv3.SearchPrincipalAccessBoundaryPolicyBindingsResponse, error) {
	list, next, err := s.reg.SearchPABPolicyBindings(req.GetName(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &iamv3.SearchPrincipalAccessBoundaryPolicyBindingsResponse{PolicyBindings: list, NextPageToken: next}, nil
}

func (s *BindingsServer) CreatePolicyBinding(_ context.Context, req *iamv3.CreatePolicyBindingRequest) (*longrunningpb.Operation, error) {
	b, err := s.reg.CreatePolicyBinding(req.GetParent(), req.GetPolicyBindingId(), req.GetPolicyBinding(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return v3Operation(s.reg, b.GetName(), "create", b)
}

func (s *BindingsServer) GetPolicyBinding(_ context.Context, req *iamv3.GetPolicyBindingRequest) (*iamv3.PolicyBinding, error) {
	b, err := s.reg.GetPolicyBinding(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return b, nil
}

func (s *BindingsServer) UpdatePolicyBinding(_ context.Context, req *iamv3.UpdatePolicyBindingRequest) (*longrunningpb.Operation, error) {
	b, err := s.reg.UpdatePolicyBinding(req.GetPolicyBinding(), req.GetUpdateMask(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return v3Operation(s.reg, b.GetName(), "update", b)
}

func (s *BindingsServer) DeletePolicyBinding(_ context.Context, req *iamv3.DeletePolicyBindingRequest) (*longrunningpb.Operation, error) {
	b, err := s.reg.DeletePolicyBinding(req.GetName(), req.GetEtag(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return v3Operation(s.reg, b.GetName(), "delete", &emptypb.Empty{})
}

func (s *BindingsServer) ListPolicyBindings(_ context.Context, req *iamv3.ListPolicyBindingsRequest) (*iamv3.ListPolicyBindingsResponse, error) {
	list, next, err := s.reg.ListPolicyBindings(req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &iamv3.ListPolicyBindingsResponse{PolicyBindings: list, NextPageToken: next}, nil
}

func (s *BindingsServer) SearchTargetPolicyBindings(_ context.Context, req *iamv3.SearchTargetPolicyBindingsRequest) (*iamv3.SearchTargetPolicyBindingsResponse, error) {
	list, next, err := s.reg.SearchTargetPolicyBindings(req.GetParent(), req.GetTarget(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &iamv3.SearchTargetPolicyBindingsResponse{PolicyBindings: list, NextPageToken: next}, nil
}

// isIAMv3Path is whether a JSON request is IAM v3's: below a hierarchy
// resource's locations, which Resource Manager v3 does not serve.
func isIAMv3Path(path string) bool { return iamV3PathRe.MatchString(path) }

var iamV3PathRe = regexp.MustCompile(`^/v3/(projects|folders|organizations)/[^/]+/locations/[^/]+/(principalAccessBoundaryPolicies|policyBindings|operations)([/:]|$)`)
