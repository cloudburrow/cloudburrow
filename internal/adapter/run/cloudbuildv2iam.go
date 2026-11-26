package run

import (
	"context"

	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"google.golang.org/genproto/googleapis/api/annotations"
)

// A Cloud Build connection's IAM policy (#2197), stored and never
// enforced, under ADR-0006 as Secure Source Manager's are
// (securesourcemanageriam.go): the google.iam.v1.IAMPolicy mixin at the
// connection's v2 :getIamPolicy, :setIamPolicy and :testIamPermissions
// paths (cloudbuild_v2.yaml), and over gRPC through the Run port's one
// IAMPolicy service, which hands a connection's name here
// (functions.go). Terraform's google_cloudbuildv2_connection_iam_* and
// gcloud builds connections get-iam-policy and set-iam-policy read and
// write it. It is removed with its connection.

var cb2IAMMixins = []*annotations.HttpRule{
	{Selector: "google.iam.v1.IAMPolicy.GetIamPolicy",
		Pattern: &annotations.HttpRule_Get{Get: "/v2/{resource=projects/*/locations/*/connections/*}:getIamPolicy"}},
	{Selector: "google.iam.v1.IAMPolicy.SetIamPolicy",
		Pattern: &annotations.HttpRule_Post{Post: "/v2/{resource=projects/*/locations/*/connections/*}:setIamPolicy"}, Body: "*"},
	{Selector: "google.iam.v1.IAMPolicy.TestIamPermissions",
		Pattern: &annotations.HttpRule_Post{Post: "/v2/{resource=projects/*/locations/*/connections/*}:testIamPermissions"}, Body: "*"},
}

// cb2Policies are the connections' policies, under ssmState.mu, saved in
// cloudburrow state with the connections.
var cb2Policies = map[string]iampolicy.Stored{}

// isCB2Connection reports whether name is a Cloud Build connection this
// instance holds (a Developer Connect connection has the same shape).
func isCB2Connection(name string) bool {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadCB2Locked()
	_, ok := cb2State.connections[name]
	return ok
}

func cb2ConnectionForIAMLocked(name string) error {
	if err := checkConnectionName(name); err != nil {
		return err
	}
	loadCB2Locked()
	if _, ok := cb2State.connections[name]; !ok {
		return apierror.NotFound("connection %q not found", name)
	}
	return nil
}

type cb2ConnectionIAM struct {
	iampb.UnimplementedIAMPolicyServer
}

// CloudBuildConnectionIAM is the connections' IAM methods, for the console.
func CloudBuildConnectionIAM() iampb.IAMPolicyServer { return cb2ConnectionIAM{} }

func (cb2ConnectionIAM) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if err := cb2ConnectionForIAMLocked(req.GetResource()); err != nil {
		return nil, err
	}
	var cur *iampolicy.Stored
	if st, ok := cb2Policies[req.GetResource()]; ok {
		cur = &st
	}
	return iampolicy.Get(cur, req.GetOptions())
}

func (cb2ConnectionIAM) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if err := cb2ConnectionForIAMLocked(req.GetResource()); err != nil {
		return nil, err
	}
	var cur *iampolicy.Stored
	if st, ok := cb2Policies[req.GetResource()]; ok {
		cur = &st
	}
	next, err := iampolicy.Set(cur, req)
	if err != nil {
		return nil, err
	}
	if err := checkOrgMembers(ctx, req.GetResource(), storedProto(cur), next.Proto()); err != nil {
		return nil, err
	}
	cb2Policies[req.GetResource()] = next
	saveCB2Locked()
	return next.Proto(), nil
}

func (cb2ConnectionIAM) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	ssmState.mu.Lock()
	err := cb2ConnectionForIAMLocked(req.GetResource())
	ssmState.mu.Unlock()
	return testPermissions(err, req)
}
