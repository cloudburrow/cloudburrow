package run

import (
	"context"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/api/annotations"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// Secure Source Manager IAM policies (#1527), stored and never enforced,
// under ADR-0006 as amended by #1527: a repository's through the service's
// own GetIamPolicyRepo, SetIamPolicyRepo and TestIamPermissionsRepo, and an
// instance's through the google.iam.v1.IAMPolicy mixin at the instance's
// v1 :getIamPolicy, :setIamPolicy and :testIamPermissions paths, on the
// JSON API only (the Run port's gRPC IAMPolicy is Cloud Functions').
// Terraform's google_secure_source_manager_instance_iam_* and
// _repository_iam_*, and gcloud source-manager's get-iam-policy,
// set-iam-policy and add-iam-policy-binding, read and write them. A policy
// is saved with the instances and repositories and removed with its
// resource, so one made again under the same name starts with none.

// ssmIAMMixins bind the IAMPolicy mixin at an instance's paths, beside the
// Operations mixin the operations are polled through.
var ssmIAMMixins = append(append([]*annotations.HttpRule{}, sourceDeployMixins...),
	&annotations.HttpRule{Selector: "google.iam.v1.IAMPolicy.GetIamPolicy",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{resource=projects/*/locations/*/instances/*}:getIamPolicy"}},
	&annotations.HttpRule{Selector: "google.iam.v1.IAMPolicy.SetIamPolicy",
		Pattern: &annotations.HttpRule_Post{Post: "/v1/{resource=projects/*/locations/*/instances/*}:setIamPolicy"}, Body: "*"},
	&annotations.HttpRule{Selector: "google.iam.v1.IAMPolicy.TestIamPermissions",
		Pattern: &annotations.HttpRule_Post{Post: "/v1/{resource=projects/*/locations/*/instances/*}:testIamPermissions"}, Body: "*"},
)

// ssmResourceForIAMLocked checks that name is an instance or repository
// that exists.
func ssmResourceForIAMLocked(name, collection string) error {
	if _, _, err := parseSSMName(name, collection); err != nil {
		return err
	}
	found := false
	if collection == "instances" {
		_, found = ssmState.instances[name]
	} else {
		_, found = ssmState.repos[name]
	}
	if !found {
		return apierror.NotFound("%s %q not found", collection[:len(collection)-1], name)
	}
	return nil
}

func ssmGetPolicy(collection string, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if err := ssmResourceForIAMLocked(req.GetResource(), collection); err != nil {
		return nil, err
	}
	var cur *iampolicy.Stored
	if st, ok := ssmState.policies[req.GetResource()]; ok {
		cur = &st
	}
	return iampolicy.Get(cur, req.GetOptions())
}

func ssmSetPolicy(collection string, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if err := ssmResourceForIAMLocked(req.GetResource(), collection); err != nil {
		return nil, err
	}
	var cur *iampolicy.Stored
	if st, ok := ssmState.policies[req.GetResource()]; ok {
		cur = &st
	}
	next, err := iampolicy.Set(cur, req)
	if err != nil {
		return nil, err
	}
	ssmState.policies[req.GetResource()] = next
	saveSSMLocked()
	return next.Proto(), nil
}

func ssmTestPermissions(collection string, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	ssmState.mu.Lock()
	err := ssmResourceForIAMLocked(req.GetResource(), collection)
	ssmState.mu.Unlock()
	return testPermissions(err, req)
}

// GetIamPolicyRepo is a repository's stored policy; nothing enforces it.
func (m *SecureSourceManagerServer) GetIamPolicyRepo(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return ssmGetPolicy("repositories", req)
}

// SetIamPolicyRepo stores a repository's policy with a new etag; a stale
// etag is ABORTED.
func (m *SecureSourceManagerServer) SetIamPolicyRepo(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return ssmSetPolicy("repositories", req)
}

// TestIamPermissionsRepo grants every permission asked about on a
// repository that exists, and none on one that does not.
func (m *SecureSourceManagerServer) TestIamPermissionsRepo(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return ssmTestPermissions("repositories", req)
}

// isSSMInstanceName reports whether name is a Secure Source Manager
// instance's, projects/*/locations/*/instances/*.
func isSSMInstanceName(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 6 && parts[0] == "projects" && parts[2] == "locations" && parts[4] == "instances" &&
		parts[1] != "" && parts[3] != "" && parts[5] != ""
}

// ssmInstanceIAM is the IAMPolicy mixin on instances.
type ssmInstanceIAM struct {
	iampb.UnimplementedIAMPolicyServer
}

func (ssmInstanceIAM) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return ssmGetPolicy("instances", req)
}

func (ssmInstanceIAM) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return ssmSetPolicy("instances", req)
}

func (ssmInstanceIAM) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return ssmTestPermissions("instances", req)
}

// SourceManagerInstanceIAM is the instances' IAM methods, for the
// console's Permissions tab.
func SourceManagerInstanceIAM() iampb.IAMPolicyServer { return ssmInstanceIAM{} }

// SourceManagerRepositoryIAM is the repositories' IAM methods
// (GetIamPolicyRepo and the rest) under IAMPolicy's names, for the
// console's Permissions tab.
func SourceManagerRepositoryIAM() iampb.IAMPolicyServer { return ssmRepoIAM{} }

type ssmRepoIAM struct {
	iampb.UnimplementedIAMPolicyServer
}

func (ssmRepoIAM) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return ssmGetPolicy("repositories", req)
}

func (ssmRepoIAM) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return ssmSetPolicy("repositories", req)
}

func (ssmRepoIAM) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return ssmTestPermissions("repositories", req)
}
