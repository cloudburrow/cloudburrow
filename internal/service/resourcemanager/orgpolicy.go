package resourcemanager

// Resource Manager v1 org policy on projects (#1296), the methods Terraform's
// google_project_organization_policy and `gcloud resource-manager
// org-policies` call: getOrgPolicy, setOrgPolicy, clearOrgPolicy,
// listOrgPolicies, getEffectiveOrgPolicy and
// listAvailableOrgPolicyConstraints.
//
// A policy is stored per project and constraint, in the registry's store,
// and never enforced, as IAM policies are not (ADR-0006). There is no
// organization or folder above a project, so a project's effective policy is
// its own. Any constraints/... name is accepted; one of the constraints
// listed below must be set the way its type allows (a boolean constraint
// takes booleanPolicy, a list constraint listPolicy). A policy that is not
// set reads back as {constraint, etag}. Which fields Google sets on such a
// policy, and the code for a stale etag (ABORTED here), are UNVERIFIED.

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

const orgPolicyPrefix = "resourcemanager/orgpolicies/"

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

func orgPolicyKey(project, constraint string) string {
	return orgPolicyPrefix + project + "/" + strings.TrimPrefix(constraint, "constraints/")
}

func policyEtag(raw []byte) string {
	sum := sha256.Sum256(raw)
	return base64.StdEncoding.EncodeToString(sum[:8])
}

// OrgPolicy is the policy stored for constraint on project, or an unset one.
func (r *Registry) OrgPolicy(project, constraint string) (*crmv1.OrgPolicy, error) {
	if err := checkConstraint(constraint); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.load(project); err != nil {
		return nil, err
	}
	return r.orgPolicy(project, constraint)
}

func (r *Registry) orgPolicy(project, constraint string) (*crmv1.OrgPolicy, error) {
	raw, err := r.st.Get(orgPolicyKey(project, constraint))
	if errors.Is(err, store.ErrNotFound) {
		return &crmv1.OrgPolicy{Constraint: constraint, Etag: policyEtag([]byte(constraint))}, nil
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

// SetOrgPolicy stores p for project. An etag that is not the current
// policy's is ABORTED.
func (r *Registry) SetOrgPolicy(project string, p *crmv1.OrgPolicy) (*crmv1.OrgPolicy, error) {
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.load(project); err != nil {
		return nil, err
	}
	cur, err := r.orgPolicy(project, p.Constraint)
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
	if err := r.st.Put(orgPolicyKey(project, p.Constraint), raw); err != nil {
		return nil, apierror.Internal(err, "writing the org policy")
	}
	return &out, nil
}

// ClearOrgPolicy removes project's policy for constraint, if any. An etag
// that is not the current policy's is ABORTED.
func (r *Registry) ClearOrgPolicy(project, constraint, etag string) error {
	if err := checkConstraint(constraint); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.load(project); err != nil {
		return err
	}
	cur, err := r.orgPolicy(project, constraint)
	if err != nil {
		return err
	}
	if etag != "" && etag != cur.Etag {
		return apierror.Aborted("policy %s: etag %q is not the current one; read the policy and try again", constraint, etag)
	}
	if err := r.st.Delete(orgPolicyKey(project, constraint)); err != nil && !errors.Is(err, store.ErrNotFound) {
		return apierror.Internal(err, "deleting the org policy")
	}
	return nil
}

// OrgPolicies are the policies set on project, by constraint.
func (r *Registry) OrgPolicies(project string) ([]*crmv1.OrgPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.load(project); err != nil {
		return nil, err
	}
	keys, err := r.st.List(orgPolicyPrefix + project + "/")
	if err != nil {
		return nil, apierror.Internal(err, "listing org policies")
	}
	sort.Strings(keys)
	var out []*crmv1.OrgPolicy
	for _, k := range keys {
		p, err := r.orgPolicy(project, "constraints/"+strings.TrimPrefix(k, orgPolicyPrefix+project+"/"))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// v1OrgPolicy serves the org policy verbs of POST /v1/projects/{p}:{verb}.
func (s *ProjectsServer) v1OrgPolicy(w http.ResponseWriter, r *http.Request, id, verb string) error {
	switch verb {
	case "getOrgPolicy", "getEffectiveOrgPolicy":
		var in crmv1.GetOrgPolicyRequest
		if err := rest.DecodeJSON(r, &in); err != nil {
			return err
		}
		p, err := s.reg.OrgPolicy(id, in.Constraint)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, p)
	case "setOrgPolicy":
		var in crmv1.SetOrgPolicyRequest
		if err := rest.DecodeJSON(r, &in); err != nil {
			return err
		}
		p, err := s.reg.SetOrgPolicy(id, in.Policy)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, p)
	case "clearOrgPolicy":
		var in crmv1.ClearOrgPolicyRequest
		if err := rest.DecodeJSON(r, &in); err != nil {
			return err
		}
		if err := s.reg.ClearOrgPolicy(id, in.Constraint, in.Etag); err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, struct{}{})
	case "listOrgPolicies":
		var in crmv1.ListOrgPoliciesRequest
		if err := rest.DecodeJSON(r, &in); err != nil {
			return err
		}
		all, err := s.reg.OrgPolicies(id)
		if err != nil {
			return err
		}
		byName := map[string]*crmv1.OrgPolicy{}
		names := make([]string, 0, len(all))
		for _, p := range all {
			byName[p.Constraint] = p
			names = append(names, p.Constraint)
		}
		page, next, err := paging.Page("rm-orgpolicies:"+id, names, in.PageToken, int(in.PageSize))
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
		if _, err := s.reg.Get(id); err != nil {
			return err
		}
		names := make([]string, 0, len(Constraints))
		for _, c := range Constraints {
			names = append(names, c.Name)
		}
		page, next, err := paging.Page("rm-constraints:"+id, names, in.PageToken, int(in.PageSize))
		if err != nil {
			return apierror.InvalidArgument("pageToken: %v", err)
		}
		resp := &crmv1.ListAvailableOrgPolicyConstraintsResponse{NextPageToken: next}
		for _, n := range page {
			resp.Constraints = append(resp.Constraints, KnownConstraint(n))
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	}
	return ErrNotImplemented("v1 projects." + verb)
}
