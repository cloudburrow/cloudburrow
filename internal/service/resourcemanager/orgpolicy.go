package resourcemanager

// Resource Manager v1 org policy on projects (#1296), folders and the
// organization (#1534): the methods Terraform's
// google_{project,folder,organization}_organization_policy and `gcloud
// resource-manager org-policies` call: getOrgPolicy, setOrgPolicy,
// clearOrgPolicy, listOrgPolicies, getEffectiveOrgPolicy and
// listAvailableOrgPolicyConstraints.
//
// A policy is stored per resource and constraint, in the registry's store,
// and never enforced, as IAM policies are not (ADR-0006). A resource is a
// project, a folder (#1294) or the one local organization. Any
// constraints/... name is accepted; one of the constraints listed below
// must be set the way its type allows (a boolean constraint takes
// booleanPolicy, a list constraint listPolicy). A policy that is not set
// reads back as {constraint, etag}. Which fields Google sets on such a
// policy, and the code for a stale etag (ABORTED here), are UNVERIFIED.
//
// getEffectiveOrgPolicy evaluates the hierarchy as v1 documents it: from
// the organization down through the folders to the resource, a boolean
// policy, a restoreDefault or a list policy replaces what is above it,
// except that a list policy with inheritFromParent adds its values to the
// effective list policy above it. The result carries no etag, as v1
// documents for a policy computed across resources. How an inheriting list
// is merged with an allValues above it is UNVERIFIED: here allValues ALLOW
// above keeps the child's denials (all but those), and DENY above keeps the
// child's allowed values (only those).

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Where policies are kept: a project's under orgPolicyPrefix (the key #1296
// stored them under, so existing state reads back), a folder's and the
// organization's under prefixes of their own, which no project ID can
// collide with.
const (
	orgPolicyPrefix       = "resourcemanager/orgpolicies/"
	folderOrgPolicyPrefix = "resourcemanager/orgpolicies-folders/"
	orgOrgPolicyPrefix    = "resourcemanager/orgpolicies-organizations/"
)

// Constraints are what listAvailableOrgPolicyConstraints answers: a set of
// Google's commonly used constraints, each with its type.
var Constraints = []*crmv1.Constraint{
	boolConstraint("constraints/compute.disableSerialPortAccess", "Disable VM serial port access"),
	boolConstraint("constraints/compute.requireOsLogin", "Require OS Login"),
	boolConstraint("constraints/compute.skipDefaultNetworkCreation", "Skip default network creation"),
	listConstraint("constraints/compute.vmExternalIpAccess", "Define allowed external IPs for VM instances"),
	listConstraint("constraints/gcp.resourceLocations", "Resource Location Restriction"),
	listConstraint("constraints/iam.allowedPolicyMemberDomains", "Domain restricted sharing"),
	boolConstraint("constraints/iam.disableServiceAccountKeyCreation", "Disable service account key creation"),
	listConstraint("constraints/run.allowedIngress", "Allowed ingress settings (Cloud Run)"),
	boolConstraint("constraints/sql.restrictPublicIp", "Restrict Public IP access on Cloud SQL instances"),
	boolConstraint("constraints/storage.publicAccessPrevention", "Enforce Public Access Prevention"),
	boolConstraint("constraints/storage.uniformBucketLevelAccess", "Enforce uniform bucket-level access"),
}

func boolConstraint(name, title string) *crmv1.Constraint {
	return &crmv1.Constraint{Name: name, DisplayName: title, Version: 1, ConstraintDefault: "ALLOW", BooleanConstraint: &crmv1.BooleanConstraint{}}
}

func listConstraint(name, title string) *crmv1.Constraint {
	return &crmv1.Constraint{Name: name, DisplayName: title, Version: 1, ConstraintDefault: "ALLOW", ListConstraint: &crmv1.ListConstraint{}}
}

// KnownConstraint is the listed constraint of that name, or nil.
func KnownConstraint(name string) *crmv1.Constraint {
	for _, c := range Constraints {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func checkConstraint(name string) error {
	if !strings.HasPrefix(name, "constraints/") || len(name) == len("constraints/") || strings.ContainsAny(name[len("constraints/"):], "/ ") {
		return apierror.InvalidArgument("constraint %q must be constraints/{constraint}", name)
	}
	return nil
}

// policyResource normalizes what the org policy methods take: a project ID,
// projects/{id}, folders/{number} or organizations/{number}.
func policyResource(resource string) string {
	if !strings.Contains(resource, "/") {
		return "projects/" + resource
	}
	return resource
}

// orgPolicyScope is the store prefix of resource's policies.
func orgPolicyScope(resource string) (string, error) {
	switch {
	case strings.HasPrefix(resource, "projects/"):
		return orgPolicyPrefix + strings.TrimPrefix(resource, "projects/") + "/", nil
	case strings.HasPrefix(resource, "folders/"):
		return folderOrgPolicyPrefix + strings.TrimPrefix(resource, "folders/") + "/", nil
	case strings.HasPrefix(resource, "organizations/"):
		return orgOrgPolicyPrefix + strings.TrimPrefix(resource, "organizations/") + "/", nil
	}
	return "", apierror.InvalidArgument("resource %q must be projects/{project}, folders/{folder} or organizations/{organization}", resource)
}

// checkPolicyResource reports whether resource exists. The caller holds
// r.mu.
func (r *Registry) checkPolicyResource(resource string) error {
	switch {
	case strings.HasPrefix(resource, "projects/"):
		_, err := r.load(strings.TrimPrefix(resource, "projects/"))
		return err
	case strings.HasPrefix(resource, "folders/"):
		_, err := r.loadFolder(resource)
		return err
	case strings.HasPrefix(resource, "organizations/"):
		return checkOrg(resource)
	}
	_, err := orgPolicyScope(resource)
	return err
}

func policyEtag(raw []byte) string {
	sum := sha256.Sum256(raw)
	return base64.StdEncoding.EncodeToString(sum[:8])
}

// OrgPolicy is the policy stored for constraint on resource (a project ID,
// projects/..., folders/... or organizations/...), or an unset one.
func (r *Registry) OrgPolicy(resource, constraint string) (*crmv1.OrgPolicy, error) {
	resource = policyResource(resource)
	if err := checkConstraint(constraint); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkPolicyResource(resource); err != nil {
		return nil, err
	}
	return r.orgPolicy(resource, constraint)
}

// stored is the policy stored for constraint on resource, or nil. The
// caller holds r.mu.
func (r *Registry) stored(resource, constraint string) (*crmv1.OrgPolicy, error) {
	scope, err := orgPolicyScope(resource)
	if err != nil {
		return nil, err
	}
	raw, err := r.st.Get(scope + strings.TrimPrefix(constraint, "constraints/"))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the org policy")
	}
	p := &crmv1.OrgPolicy{}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, apierror.Internal(err, "decoding the org policy")
	}
	return p, nil
}

func (r *Registry) orgPolicy(resource, constraint string) (*crmv1.OrgPolicy, error) {
	p, err := r.stored(resource, constraint)
	if err != nil || p != nil {
		return p, err
	}
	return &crmv1.OrgPolicy{Constraint: constraint, Etag: policyEtag([]byte(constraint))}, nil
}

// SetOrgPolicy stores p for resource. An etag that is not the current
// policy's is ABORTED.
func (r *Registry) SetOrgPolicy(resource string, p *crmv1.OrgPolicy) (*crmv1.OrgPolicy, error) {
	resource = policyResource(resource)
	if p == nil {
		return nil, apierror.InvalidArgument("policy is required")
	}
	if err := checkConstraint(p.Constraint); err != nil {
		return nil, err
	}
	if p.BooleanPolicy != nil && p.ListPolicy != nil {
		return nil, apierror.InvalidArgument("policy %s: set booleanPolicy or listPolicy, not both", p.Constraint)
	}
	if c := KnownConstraint(p.Constraint); c != nil {
		if c.BooleanConstraint != nil && p.ListPolicy != nil {
			return nil, apierror.InvalidArgument("%s is a boolean constraint: set booleanPolicy", p.Constraint)
		}
		if c.ListConstraint != nil && p.BooleanPolicy != nil {
			return nil, apierror.InvalidArgument("%s is a list constraint: set listPolicy", p.Constraint)
		}
	}
	if lp := p.ListPolicy; lp != nil && lp.AllValues != "" && (len(lp.AllowedValues) > 0 || len(lp.DeniedValues) > 0) {
		return nil, apierror.InvalidArgument("policy %s: allValues cannot be set with allowedValues or deniedValues", p.Constraint)
	}
	scope, err := orgPolicyScope(resource)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkPolicyResource(resource); err != nil {
		return nil, err
	}
	cur, err := r.orgPolicy(resource, p.Constraint)
	if err != nil {
		return nil, err
	}
	if p.Etag != "" && p.Etag != cur.Etag {
		return nil, apierror.Aborted("policy %s: etag %q is not the current one; read the policy and try again", p.Constraint, p.Etag)
	}
	out := *p
	out.Etag, out.UpdateTime = "", r.now().UTC().Format("2006-01-02T15:04:05.000000Z")
	if out.Version == 0 {
		out.Version = 1
	}
	raw, err := json.Marshal(&out)
	if err != nil {
		return nil, apierror.Internal(err, "encoding the org policy")
	}
	out.Etag = policyEtag(raw)
	if raw, err = json.Marshal(&out); err != nil {
		return nil, apierror.Internal(err, "encoding the org policy")
	}
	if err := r.st.Put(scope+strings.TrimPrefix(p.Constraint, "constraints/"), raw); err != nil {
		return nil, apierror.Internal(err, "writing the org policy")
	}
	return &out, nil
}

// ClearOrgPolicy removes resource's policy for constraint, if any. An etag
// that is not the current policy's is ABORTED.
func (r *Registry) ClearOrgPolicy(resource, constraint, etag string) error {
	resource = policyResource(resource)
	if err := checkConstraint(constraint); err != nil {
		return err
	}
	scope, err := orgPolicyScope(resource)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkPolicyResource(resource); err != nil {
		return err
	}
	cur, err := r.orgPolicy(resource, constraint)
	if err != nil {
		return err
	}
	if etag != "" && etag != cur.Etag {
		return apierror.Aborted("policy %s: etag %q is not the current one; read the policy and try again", constraint, etag)
	}
	if err := r.st.Delete(scope + strings.TrimPrefix(constraint, "constraints/")); err != nil && !errors.Is(err, store.ErrNotFound) {
		return apierror.Internal(err, "deleting the org policy")
	}
	return nil
}

// OrgPolicies are the policies set on resource, by constraint.
func (r *Registry) OrgPolicies(resource string) ([]*crmv1.OrgPolicy, error) {
	resource = policyResource(resource)
	scope, err := orgPolicyScope(resource)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkPolicyResource(resource); err != nil {
		return nil, err
	}
	keys, err := r.st.List(scope)
	if err != nil {
		return nil, apierror.Internal(err, "listing org policies")
	}
	sort.Strings(keys)
	var out []*crmv1.OrgPolicy
	for _, k := range keys {
		p, err := r.orgPolicy(resource, "constraints/"+strings.TrimPrefix(k, scope))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// policyChain is resource and what is above it, the organization first and
// resource last. The caller holds r.mu.
func (r *Registry) policyChain(resource string) ([]string, error) {
	parent := ""
	switch {
	case strings.HasPrefix(resource, "projects/"):
		p, err := r.load(strings.TrimPrefix(resource, "projects/"))
		if err != nil {
			return nil, err
		}
		parent = p.Parent
	case strings.HasPrefix(resource, "folders/"):
		f, err := r.loadFolder(resource)
		if err != nil {
			return nil, err
		}
		parent = f.Parent
	default:
		if err := r.checkPolicyResource(resource); err != nil {
			return nil, err
		}
	}
	chain := []string{resource}
	for i := 0; parent != "" && i <= MaxFolderDepth+1; i++ {
		chain = append([]string{parent}, chain...)
		if !strings.HasPrefix(parent, "folders/") {
			break
		}
		f, err := r.loadFolder(parent)
		if err != nil {
			return nil, err
		}
		parent = f.Parent
	}
	return chain, nil
}

// EffectiveOrgPolicy is getEffectiveOrgPolicy: constraint's policy on
// resource evaluated down the hierarchy, as the comment at the top of this
// file describes.
func (r *Registry) EffectiveOrgPolicy(resource, constraint string) (*crmv1.OrgPolicy, error) {
	resource = policyResource(resource)
	if err := checkConstraint(constraint); err != nil {
		return nil, err
	}
	if _, err := orgPolicyScope(resource); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	chain, err := r.policyChain(resource)
	if err != nil {
		return nil, err
	}
	var eff *crmv1.OrgPolicy
	for _, res := range chain {
		p, err := r.stored(res, constraint)
		if err != nil {
			return nil, err
		}
		if p == nil || (p.BooleanPolicy == nil && p.ListPolicy == nil && p.RestoreDefault == nil) {
			continue
		}
		eff = mergePolicy(eff, p)
	}
	out := &crmv1.OrgPolicy{Constraint: constraint, Version: 1}
	if eff != nil {
		out.BooleanPolicy, out.ListPolicy, out.RestoreDefault, out.UpdateTime = eff.BooleanPolicy, eff.ListPolicy, eff.RestoreDefault, eff.UpdateTime
	}
	return out, nil
}

// mergePolicy is the effective policy of a resource whose own policy is p,
// below a resource whose effective policy is above (nil when none is set).
func mergePolicy(above, p *crmv1.OrgPolicy) *crmv1.OrgPolicy {
	lp := p.ListPolicy
	if lp == nil || !lp.InheritFromParent || above == nil || above.ListPolicy == nil {
		return p
	}
	al := above.ListPolicy
	out := &crmv1.ListPolicy{SuggestedValue: lp.SuggestedValue}
	if out.SuggestedValue == "" {
		out.SuggestedValue = al.SuggestedValue
	}
	switch {
	case lp.AllValues != "":
		out.AllValues = lp.AllValues
	case al.AllValues == "ALLOW":
		if len(lp.DeniedValues) == 0 {
			out.AllValues = "ALLOW"
		}
		out.DeniedValues = lp.DeniedValues
	case al.AllValues == "DENY":
		if len(lp.AllowedValues) == 0 {
			out.AllValues = "DENY"
		}
		out.AllowedValues = lp.AllowedValues
	default:
		out.AllowedValues = union(al.AllowedValues, lp.AllowedValues)
		out.DeniedValues = union(al.DeniedValues, lp.DeniedValues)
	}
	return &crmv1.OrgPolicy{Constraint: p.Constraint, ListPolicy: out, UpdateTime: p.UpdateTime}
}

func union(a, b []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range append(append([]string{}, a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// v1OrgPolicy serves the org policy verbs of POST /v1/{resource}:{verb},
// where resource is projects/{id}, folders/{number} or
// organizations/{number}.
func (s *ProjectsServer) v1OrgPolicy(w http.ResponseWriter, r *http.Request, resource, verb string) error {
	switch verb {
	case "getOrgPolicy", "getEffectiveOrgPolicy":
		var in crmv1.GetOrgPolicyRequest
		if err := rest.DecodeJSON(r, &in); err != nil {
			return err
		}
		get := s.reg.OrgPolicy
		if verb == "getEffectiveOrgPolicy" {
			get = s.reg.EffectiveOrgPolicy
		}
		p, err := get(resource, in.Constraint)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, p)
	case "setOrgPolicy":
		var in crmv1.SetOrgPolicyRequest
		if err := rest.DecodeJSON(r, &in); err != nil {
			return err
		}
		p, err := s.reg.SetOrgPolicy(resource, in.Policy)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, p)
	case "clearOrgPolicy":
		var in crmv1.ClearOrgPolicyRequest
		if err := rest.DecodeJSON(r, &in); err != nil {
			return err
		}
		if err := s.reg.ClearOrgPolicy(resource, in.Constraint, in.Etag); err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, struct{}{})
	case "listOrgPolicies":
		var in crmv1.ListOrgPoliciesRequest
		if err := rest.DecodeJSON(r, &in); err != nil {
			return err
		}
		all, err := s.reg.OrgPolicies(resource)
		if err != nil {
			return err
		}
		byName := map[string]*crmv1.OrgPolicy{}
		names := make([]string, 0, len(all))
		for _, p := range all {
			byName[p.Constraint] = p
			names = append(names, p.Constraint)
		}
		page, next, err := paging.Page("rm-orgpolicies:"+resource, names, in.PageToken, int(in.PageSize))
		if err != nil {
			return apierror.InvalidArgument("pageToken: %v", err)
		}
		resp := &crmv1.ListOrgPoliciesResponse{NextPageToken: next}
		for _, n := range page {
			resp.Policies = append(resp.Policies, byName[n])
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	case "listAvailableOrgPolicyConstraints":
		var in crmv1.ListAvailableOrgPolicyConstraintsRequest
		if err := rest.DecodeJSON(r, &in); err != nil {
			return err
		}
		if err := s.reg.CheckPolicyResource(resource); err != nil {
			return err
		}
		names := make([]string, 0, len(Constraints))
		for _, c := range Constraints {
			names = append(names, c.Name)
		}
		page, next, err := paging.Page("rm-constraints:"+resource, names, in.PageToken, int(in.PageSize))
		if err != nil {
			return apierror.InvalidArgument("pageToken: %v", err)
		}
		resp := &crmv1.ListAvailableOrgPolicyConstraintsResponse{NextPageToken: next}
		for _, n := range page {
			resp.Constraints = append(resp.Constraints, KnownConstraint(n))
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	}
	kind, _, _ := strings.Cut(resource, "/")
	return ErrNotImplemented("v1 " + kind + "." + verb)
}

// CheckPolicyResource reports whether resource, which org policy can be set
// on, exists.
func (r *Registry) CheckPolicyResource(resource string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkPolicyResource(policyResource(resource))
}

// OrgPolicyChain is resource and the folders and organization above it whose
// policies its effective policy is evaluated from, the organization first.
func (r *Registry) OrgPolicyChain(resource string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policyChain(policyResource(resource))
}
