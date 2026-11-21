package resourcemanager

// A workforce pool's own IAM policy (#2065): the IAM API's
// locations.workforcePools getIamPolicy, setIamPolicy and
// testIamPermissions (docs.cloud.google.com/iam/docs/reference/rest/v1/locations.workforcePools,
// read 2026-11-20), kept as every other resource's policy (iam.go) under
// the pool's name, locations/global/workforcePools/ID. IAM enforcement
// reads it beneath the organization's (cmd/cloudburrow's
// iamenforce_workforce.go). A deleted pool keeps its policy, which cannot
// be set until it is undeleted; testIamPermissions of a pool that does not
// exist answers no permissions rather than NOT_FOUND, as Google documents.

import (
	"context"
	"net/http"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// wfResourcePrefix is every workforce pool resource's name prefix.
const wfResourcePrefix = "locations/global/workforcePools/"

// wfIAMTarget reports whether a workforce pool (or a SCIM tenant of one)
// exists, and whether it is deleted. The caller holds r.mu.
func (r *Registry) wfIAMTarget(resource string) (deleting bool, err error) {
	rest := strings.TrimPrefix(resource, wfResourcePrefix)
	parts := strings.Split(rest, "/")
	rec := r.wfLoad(wfPoolKey(parts[0]))
	if rec == nil || rec.PoolV == nil {
		return false, apierror.NotFound("Requested entity was not found.")
	}
	switch len(parts) {
	case 1:
		return rec.Deleted, nil
	case 5:
		if parts[1] == "providers" && parts[3] == "scimTenants" {
			if t, ok := r.wfScimTenantForIAM(parts[0], parts[2], parts[4]); ok {
				return rec.Deleted || t, nil
			}
		}
	}
	return false, apierror.NotFound("Requested entity was not found.")
}

// wfIAMServer serves the three methods on a workforce pool's resources.
type wfIAMServer struct{ reg *Registry }

func (s wfIAMServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return getIAM(s.reg, req.GetResource(), req)
}

func (s wfIAMServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	p, err := s.reg.SetIAMPolicy(req.GetResource(), req)
	return p, apierror.Wrap(err)
}

func (s wfIAMServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	if _, err := s.reg.IAMPolicy(req.GetResource()); err != nil {
		// "If the pool doesn't exist, this call returns an empty set of
		// permissions. It doesn't return a NOT_FOUND error."
		return &iampb.TestIamPermissionsResponse{}, nil
	}
	return heldOn(req.GetResource(), req.GetPermissions()), nil
}

// serveWFIAM is POST {resource}:{getIamPolicy|setIamPolicy|testIamPermissions}
// on a workforce pool or one of its SCIM tenants.
func (r *Registry) serveWFIAM(w http.ResponseWriter, req *http.Request, resource, verb string) error {
	if !isIAMVerb(verb) || req.Method != http.MethodPost {
		return apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path)
	}
	return serveIAM(w, req, wfIAMServer{reg: r}, resource, verb)
}

// wfScimTenantForIAM reports whether a SCIM tenant exists, and whether it
// is deleted. The caller holds r.mu.
func (r *Registry) wfScimTenantForIAM(pool, prov, tenant string) (deleted, ok bool) {
	rec := r.wfScimLoad(wfScimTenantKey(pool, prov, tenant))
	if rec == nil || rec.TenantV == nil {
		return false, false
	}
	return rec.Deleted, true
}

// WorkforceIAMServer is the IAM methods of workforce pools and their SCIM
// tenants, by full resource name (locations/global/workforcePools/...).
type WorkforceIAMServer interface {
	GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error)
	SetIamPolicy(context.Context, *iampb.SetIamPolicyRequest) (*iampb.Policy, error)
	TestIamPermissions(context.Context, *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error)
}

// WorkforceIAM serves the console's Permissions tabs of pools and SCIM
// tenants (#2067) as the IAM API serves them.
func (r *Registry) WorkforceIAM() WorkforceIAMServer { return wfIAMServer{reg: r} }
