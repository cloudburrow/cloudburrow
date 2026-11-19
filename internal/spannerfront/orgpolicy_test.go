package spannerfront

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// orgPolicySource answers the constraints it holds, for every resource.
type orgPolicySource map[string]*orgpolicypb.PolicySpec

func (s orgPolicySource) Effective(_ context.Context, _, constraint string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{Spec: s[constraint]}, nil
}

// useOrgPolicies makes gcp.resourceLocations allow in:us-locations only,
// and iam.allowedPolicyMemberDomains the local customer only, for t.
func useOrgPolicies(t *testing.T) {
	t.Helper()
	vals := func(v string) *orgpolicypb.PolicySpec {
		return &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Values{
			Values: &orgpolicypb.PolicySpec_PolicyRule_StringValues{AllowedValues: []string{v}}}}}}
	}
	orgpolicy.Use(orgPolicySource{orgpolicy.ResourceLocations: vals("in:us-locations"),
		orgpolicy.AllowedPolicyMemberDomains: vals("is:" + orgpolicy.Customer.ID)})
	t.Cleanup(func() { orgpolicy.Use(nil) })
}

func TestOrgPoliciesOnSpanner(t *testing.T) {
	s := &Instances{configs: map[string]*instancepb.InstanceConfig{
		"projects/p/instanceConfigs/custom-eu": {Name: "projects/p/instanceConfigs/custom-eu", BaseConfig: "projects/p/instanceConfigs/eur3"}}}
	for cfg, want := range map[string]string{"projects/p/instanceConfigs/regional-us-central1": "us-central1",
		"projects/p/instanceConfigs/nam3": "nam3", "projects/p/instanceConfigs/custom-eu": "eur3", "projects/p/instanceConfigs/emulator-config": ""} {
		if got := s.configLocation(cfg); got != want {
			t.Errorf("configLocation(%s) = %q, want %q", cfg, got, want)
		}
	}
	useOrgPolicies(t)
	_, err := s.CreateInstance(context.Background(), &instancepb.CreateInstanceRequest{Parent: "projects/p", InstanceId: "i",
		Instance: &instancepb.Instance{Config: "projects/p/instanceConfigs/regional-europe-west1"}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Location REGION:europe-west1 violates constraint constraints/gcp.resourceLocations on the resource projects/p.") {
		t.Errorf("CreateInstance in europe-west1 = %v", err)
	}
	if err := checkOrgMembers(context.Background(), "projects/p/instances/i", nil,
		&iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/spanner.databaseReader", Members: []string{"user:x@example.com"}}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a member outside the customer = %v", err)
	}
}
