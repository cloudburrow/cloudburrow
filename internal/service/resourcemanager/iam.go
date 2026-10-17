package resourcemanager

// IAM policies on projects, folders and the organization (#1368), stored and
// never enforced, as ADR-0006 decides for every service that serves them.
// The maintainer decided on 2026-10-15 to store them, which reversed the
// refusal ADR-0006 recorded for project IAM.
//
// There is no inheritance. On Google a project's bindings apply to every
// resource in it; here they apply to nothing, and a resource's own
// GetIamPolicy (a bucket's, a topic's, a secret's) still shows only its own
// bindings. The policy is a record of what was set, read back as it was set.
//
// GetIamPolicy, SetIamPolicy and TestIamPermissions are served over gRPC,
// the v3 REST paths, v1 REST (projects and organizations, which gcloud
// projects and Terraform's google_project_iam_* call) and v2 REST folders
// (gcloud resource-manager folders), through the shared internal/iampolicy:
// a set returns a new etag and a stale one is ABORTED, conditions are
// UNIMPLEMENTED naming the field, and a version 3 policy without conditions
// is accepted. Audit configs are stored on a project, a folder and the
// organization as update_mask says (#1486), and configure no audit log.
//
// Tag keys and tag values keep a policy too (#1486), served by TagKeysServer
// and TagValuesServer, with no audit configs (Google keeps those on projects,
// folders and organizations only). A tag's policy goes with the tag.
//
// A policy is kept in the registry's store beside its resource, so it has
// the registry's persistence and snapshots. It goes with the resource: a
// project or folder purged after RetentionPeriod loses it, and so does a
// project ID registered again. /admin/reset clears them (ResetIAM), as it
// clears every other service's stored policies; the projects themselves are
// a registry reset does not touch.

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

const iamPrefix = "resourcemanager/iam/"

// iamKey is where a resource's policy is kept: resource is "projects/{id}",
// "folders/{number}" or OrganizationName.
func iamKey(resource string) string { return iamPrefix + resource }

// iamTargetLocked checks that a policy's resource exists, and whether it is
// pending deletion. The caller holds r.mu.
func (r *Registry) iamTargetLocked(resource string) (deleting bool, err error) {
	switch {
	case strings.HasPrefix(resource, "projects/"):
		p, err := r.load(strings.TrimPrefix(resource, "projects/"))
		if err != nil {
			return false, err
		}
		return p.State == StateDeleteRequested, nil
	case strings.HasPrefix(resource, "folders/"):
		f, err := r.loadFolder(resource)
		if err != nil {
			return false, err
		}
		return f.State == StateDeleteRequested, nil
	case strings.HasPrefix(resource, "tagKeys/"):
		_, _, err := r.tagKeyLocked(resource)
		return false, err
	case strings.HasPrefix(resource, "tagValues/"):
		_, _, err := r.tagValueLocked(resource)
		return false, err
	default:
		return false, checkOrg(resource)
	}
}

// isTag reports whether a policy's resource is a tag key or a tag value.
func isTag(resource string) bool {
	return strings.HasPrefix(resource, "tagKeys/") || strings.HasPrefix(resource, "tagValues/")
}

// loadIAMLocked reads a stored policy, nil for none. The caller holds r.mu.
func (r *Registry) loadIAMLocked(resource string) (*iampolicy.Stored, error) {
	b, err := r.st.Get(iamKey(resource))
	if err != nil {
		return nil, nil
	}
	var s iampolicy.Stored
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, apierror.Internal(err, "decode the IAM policy of %s", resource)
	}
	return &s, nil
}

// IAMPolicy returns a resource's stored policy, nil for none. The resource
// must exist; one pending deletion has its policy still.
func (r *Registry) IAMPolicy(resource string) (*iampolicy.Stored, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.iamTargetLocked(resource); err != nil {
		return nil, err
	}
	return r.loadIAMLocked(resource)
}

// SetIAMPolicy applies a SetIamPolicy request to a resource's policy and
// stores the result. A resource pending deletion is FAILED_PRECONDITION, as
// every other change to one is.
func (r *Registry) SetIAMPolicy(resource string, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	deleting, err := r.iamTargetLocked(resource)
	if err != nil {
		return nil, err
	}
	if deleting {
		return nil, apierror.FailedPrecondition("%s is pending deletion; undelete it before setting its IAM policy", resource)
	}
	current, err := r.loadIAMLocked(resource)
	if err != nil {
		return nil, err
	}
	set := iampolicy.SetWithAuditConfigs
	if isTag(resource) {
		set = iampolicy.Set
	}
	next, err := set(current, req)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(next)
	if err != nil {
		return nil, apierror.Internal(err, "encode the IAM policy")
	}
	if err := r.st.Put(iamKey(resource), b); err != nil {
		return nil, apierror.Internal(err, "store the IAM policy")
	}
	return next.Proto(), nil
}

// ResetIAM deletes the stored policies: every one when project is empty,
// otherwise that project's. It is /admin/reset's (resetters.go in
// cmd/cloudburrow); the projects, folders and organization stay.
func (r *Registry) ResetIAM(project string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if project != "" {
		_ = r.st.Delete(iamKey("projects/" + project))
		return nil
	}
	keys, err := r.st.List(iamPrefix)
	if err != nil {
		return apierror.Internal(err, "list IAM policies")
	}
	for _, k := range keys {
		if err := r.st.Delete(k); err != nil {
			return apierror.Internal(err, "delete %s", k)
		}
	}
	return nil
}

// dropIAM deletes a resource's policy, when the resource is purged or its
// name is registered again. The caller holds r.mu, or reads only.
func (r *Registry) dropIAM(resource string) { _ = r.st.Delete(iamKey(resource)) }

// permissionPattern is a permission's form: service.resource.verb.
var permissionPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*\.[a-zA-Z][a-zA-Z0-9]*\.[a-zA-Z][a-zA-Z0-9]*$`)

// heldOn answers TestIamPermissions on a project, a folder or the
// organization: of the permissions asked, those that can be held there.
// Nothing is enforced, so nothing is denied; but a permission that is not
// well formed, or a folder's or organization's permission asked of a
// project (an organization's of a folder), is not one a caller could hold
// on that resource, as on Google, where those cannot be granted there.
// Every other service's permissions can be granted on a project, so they
// are answered.
func heldOn(resource string, perms []string) *iampb.TestIamPermissionsResponse {
	out := &iampb.TestIamPermissionsResponse{}
	for _, p := range perms {
		if !permissionPattern.MatchString(p) {
			continue
		}
		// On a tag key or value, the tag permissions alone can be held.
		if isTag(resource) && !isTagPermission(p) {
			continue
		}
		if strings.HasPrefix(p, "resourcemanager.organizations.") && !strings.HasPrefix(resource, "organizations/") {
			continue
		}
		if strings.HasPrefix(p, "resourcemanager.folders.") && strings.HasPrefix(resource, "projects/") {
			continue
		}
		out.Permissions = append(out.Permissions, p)
	}
	return out
}

// tagPermissionPrefixes are the permissions that can be granted on a tag key
// or value: those of tag keys, values, value bindings and holds.
var tagPermissionPrefixes = []string{"resourcemanager.tagKeys.", "resourcemanager.tagValues.",
	"resourcemanager.tagValueBindings.", "resourcemanager.tagHolds."}

func isTagPermission(p string) bool {
	for _, prefix := range tagPermissionPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// --- the three methods, on each kind ----------------------------------

func (s *ProjectsServer) iamResource(name string) (string, error) {
	id, err := s.idFromName(name)
	if err != nil {
		return "", err
	}
	return "projects/" + id, nil
}

// GetIamPolicy is a project's stored policy (#1368). It is not inherited by
// anything, and inherits nothing.
func (s *ProjectsServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := s.iamResource(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return getIAM(s.reg, res, req)
}

// SetIamPolicy stores a project's policy, enforcing none of it.
func (s *ProjectsServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := s.iamResource(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := s.reg.SetIAMPolicy(res, req)
	return p, apierror.Wrap(err)
}

// TestIamPermissions answers the permissions that can be held on a project.
func (s *ProjectsServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	res, err := s.iamResource(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return testIAM(s.reg, res, req)
}

func (s *FoldersServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := FolderName(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return getIAM(s.reg, res, req)
}

func (s *FoldersServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := FolderName(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := s.reg.SetIAMPolicy(res, req)
	return p, apierror.Wrap(err)
}

func (s *FoldersServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	res, err := FolderName(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return testIAM(s.reg, res, req)
}

func (o OrganizationsServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	if err := checkOrg(req.GetResource()); err != nil {
		return nil, apierror.Wrap(err)
	}
	return getIAM(o.reg, req.GetResource(), req)
}

func (o OrganizationsServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	if err := checkOrg(req.GetResource()); err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := o.reg.SetIAMPolicy(req.GetResource(), req)
	return p, apierror.Wrap(err)
}

func (o OrganizationsServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	if err := checkOrg(req.GetResource()); err != nil {
		return nil, apierror.Wrap(err)
	}
	return testIAM(o.reg, req.GetResource(), req)
}

func getIAM(reg *Registry, resource string, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	if reg == nil {
		return nil, apierror.Wrap(apierror.FailedPrecondition("no project registry"))
	}
	cur, err := reg.IAMPolicy(resource)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := iampolicy.Get(cur, req.GetOptions())
	return p, apierror.Wrap(err)
}

func testIAM(reg *Registry, resource string, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	if reg == nil {
		return nil, apierror.Wrap(apierror.FailedPrecondition("no project registry"))
	}
	if _, err := reg.IAMPolicy(resource); err != nil {
		return nil, apierror.Wrap(err)
	}
	return heldOn(resource, req.GetPermissions()), nil
}

// iamServer is the IAM half of the Projects, Folders and Organizations
// servers.
type iamServer interface {
	GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error)
	SetIamPolicy(context.Context, *iampb.SetIamPolicyRequest) (*iampb.Policy, error)
	TestIamPermissions(context.Context, *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error)
}

// isIAMVerb reports whether a custom method is one of the three.
func isIAMVerb(verb string) bool {
	return verb == "getIamPolicy" || verb == "setIamPolicy" || verb == "testIamPermissions"
}

// serveIAM is POST {resource}:{verb} for the three methods, over every REST
// version: v1's, v2's and v3's bodies and answers are the protos' JSON, so
// one transcoding serves them all.
func serveIAM(w http.ResponseWriter, r *http.Request, srv iamServer, resource, verb string) error {
	switch verb {
	case "getIamPolicy":
		var in iampb.GetIamPolicyRequest
		if err := readProto(r, &in); err != nil {
			return err
		}
		in.Resource = resource
		p, err := srv.GetIamPolicy(r.Context(), &in)
		if err != nil {
			return err
		}
		return writeProto(w, p)
	case "setIamPolicy":
		var in iampb.SetIamPolicyRequest
		if err := readProto(r, &in); err != nil {
			return err
		}
		in.Resource = resource
		p, err := srv.SetIamPolicy(r.Context(), &in)
		if err != nil {
			return err
		}
		return writeProto(w, p)
	default:
		var in iampb.TestIamPermissionsRequest
		if err := readProto(r, &in); err != nil {
			return err
		}
		in.Resource = resource
		resp, err := srv.TestIamPermissions(r.Context(), &in)
		if err != nil {
			return err
		}
		return writeProto(w, resp)
	}
}

// tagIAMResource checks a tag key's or value's name for its three methods.
func tagIAMResource(prefix, name string) (string, error) {
	if _, err := numericID(prefix, name); err != nil {
		return "", err
	}
	return name, nil
}

// GetIamPolicy is a tag key's stored policy (#1486), enforced by nothing.
func (s *TagKeysServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := tagIAMResource("tagKeys/", req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return getIAM(s.reg, res, req)
}

// SetIamPolicy stores a tag key's policy, enforcing none of it.
func (s *TagKeysServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := tagIAMResource("tagKeys/", req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := s.reg.SetIAMPolicy(res, req)
	return p, apierror.Wrap(err)
}

// TestIamPermissions answers the tag permissions asked of a tag key.
func (s *TagKeysServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	res, err := tagIAMResource("tagKeys/", req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return testIAM(s.reg, res, req)
}

// GetIamPolicy is a tag value's stored policy (#1486), enforced by nothing.
func (s *TagValuesServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := tagIAMResource("tagValues/", req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return getIAM(s.reg, res, req)
}

// SetIamPolicy stores a tag value's policy, enforcing none of it.
func (s *TagValuesServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	res, err := tagIAMResource("tagValues/", req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := s.reg.SetIAMPolicy(res, req)
	return p, apierror.Wrap(err)
}

// TestIamPermissions answers the tag permissions asked of a tag value.
func (s *TagValuesServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	res, err := tagIAMResource("tagValues/", req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return testIAM(s.reg, res, req)
}
