package spannerfront

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// IAM policies on instances and databases (#1486), stored and never
// enforced, as ADR-0006 decides for every service that serves them. The
// emulator answers the three methods UNIMPLEMENTED; the front answers them
// through the shared internal/iampolicy: a set returns a new etag and a
// stale one is ABORTED, conditions and audit configs are UNIMPLEMENTED naming
// the field, and a version 3 policy without conditions is accepted.
//
// The resource must exist in the emulator (NOT_FOUND otherwise, as Google
// documents for TestIamPermissions on a missing instance), and its policy
// goes with it: DropDatabase drops a database's, DeleteInstance the
// instance's and its databases'. Policies are kept in memory, as the
// emulator keeps its instances. The front's backups (backups.go) and
// database roles (roles.go) have policies too, and a backup's goes with it.

var (
	iamInstanceRe = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+$`)
	iamDatabaseRe = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+/databases/[^/]+$`)
	iamBackupRe   = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+/backups/[^/]+$`)
	iamRoleRe     = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+/databases/[^/]+/databaseRoles/[^/]+$`)

	// restIAM is POST /v1/{resource}:{verb} on an instance or database.
	restIAM = regexp.MustCompile(`^/v1/(projects/[^/:]+/instances/[^/:]+(?:/(?:databases|backups)/[^/:]+(?:/databaseRoles/[^/:]+)?)?):(getIamPolicy|setIamPolicy|testIamPermissions)$`)
)

var iamMethods = map[string]bool{"GetIamPolicy": true, "SetIamPolicy": true, "TestIamPermissions": true}

// iamTarget checks that resource exists, for a method of the instance
// (database false) or database admin service.
func (s *Instances) iamTarget(ctx context.Context, resource string, database bool) error {
	switch {
	case !database && iamInstanceRe.MatchString(resource):
		_, err := s.emu.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: resource})
		return err
	case database && iamDatabaseRe.MatchString(resource):
		_, err := s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: resource})
		return err
	case database && iamBackupRe.MatchString(resource):
		if _, _, ok := s.backupCopy(resource); !ok {
			return backupNotFound(resource)
		}
		return nil
	case database && iamRoleRe.MatchString(resource):
		return s.roleExists(ctx, resource)
	case database:
		return status.Errorf(codes.InvalidArgument, "Invalid resource %q: want projects/{project}/instances/{instance}/databases/{database}.", resource)
	}
	return status.Errorf(codes.InvalidArgument, "Invalid resource %q: want projects/{project}/instances/{instance}.", resource)
}

func (s *Instances) getIAM(ctx context.Context, req *iampb.GetIamPolicyRequest, database bool) (*iampb.Policy, error) {
	if err := s.iamTarget(ctx, req.GetResource(), database); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := iampolicy.Get(s.policies[req.GetResource()], req.GetOptions())
	return p, apierror.Wrap(err)
}

func (s *Instances) setIAM(ctx context.Context, req *iampb.SetIamPolicyRequest, database bool) (*iampb.Policy, error) {
	if err := s.iamTarget(ctx, req.GetResource(), database); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := iampolicy.Set(s.policies[req.GetResource()], req)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.policies[req.GetResource()] = &next
	return next.Proto(), nil
}

func (s *Instances) testIAM(ctx context.Context, req *iampb.TestIamPermissionsRequest, database bool) (*iampb.TestIamPermissionsResponse, error) {
	if err := s.iamTarget(ctx, req.GetResource(), database); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// dropPolicies forgets the policy of resource and of everything under it.
func (s *Instances) dropPolicies(resource string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.policies {
		if name == resource || strings.HasPrefix(name, resource+"/") {
			delete(s.policies, name)
		}
	}
}

func (s *Instances) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return s.getIAM(ctx, req, false)
}

func (s *Instances) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return s.setIAM(ctx, req, false)
}

func (s *Instances) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return s.testIAM(ctx, req, false)
}

func (d databases) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return d.s.getIAM(ctx, req, true)
}

func (d databases) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return d.s.setIAM(ctx, req, true)
}

func (d databases) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return d.s.testIAM(ctx, req, true)
}

// serveRESTIAM is POST /v1/{resource}:{verb}, whose body and answer are the
// protos' JSON.
func (s *Instances) serveRESTIAM(w http.ResponseWriter, r *http.Request) {
	m := restIAM.FindStringSubmatch(r.URL.Path)
	resource, verb := m[1], m[2]
	database := strings.Contains(resource, "/databases/") || strings.Contains(resource, "/backups/")
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, status.Errorf(codes.InvalidArgument, "Invalid JSON payload received. %v", err))
		return
	}
	read := func(msg proto.Message) bool {
		if len(b) == 0 {
			return true
		}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, msg); err != nil {
			writeError(w, status.Errorf(codes.InvalidArgument, "Invalid JSON payload received. %v", err))
			return false
		}
		return true
	}
	var resp proto.Message
	switch verb {
	case "getIamPolicy":
		req := &iampb.GetIamPolicyRequest{}
		if !read(req) {
			return
		}
		req.Resource = resource
		resp, err = s.getIAM(r.Context(), req, database)
	case "setIamPolicy":
		req := &iampb.SetIamPolicyRequest{}
		if !read(req) {
			return
		}
		req.Resource = resource
		resp, err = s.setIAM(r.Context(), req, database)
	default:
		req := &iampb.TestIamPermissionsRequest{}
		if !read(req) {
			return
		}
		req.Resource = resource
		resp, err = s.testIAM(r.Context(), req, database)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	out, err := protojson.Marshal(resp)
	if err != nil {
		writeError(w, status.Errorf(codes.Internal, "encoding the response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(out)
}
