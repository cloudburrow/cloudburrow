package run

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
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

func TestOrgPoliciesOnRun(t *testing.T) {
	ctx := context.Background()
	if err := checkOrgLocation(ctx, "projects/my-project/locations/europe-west1"); err != nil {
		t.Fatalf("no policy: %v", err)
	}
	useOrgPolicies(t)
	if err := checkOrgLocation(ctx, "projects/my-project/locations/us-central1"); err != nil {
		t.Errorf("us-central1: %v", err)
	}
	err := checkOrgLocation(ctx, "projects/my-project/locations/europe-west1")
	if apierror.From(err).Code != codes.FailedPrecondition || !strings.Contains(err.Error(), "Location REGION:europe-west1 violates constraint constraints/gcp.resourceLocations on the resource projects/my-project.") {
		t.Errorf("europe-west1: %v", err)
	}
	pol := func(m ...string) *iampb.Policy {
		return &iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/run.invoker", Members: m}}}
	}
	if err := checkOrgMembers(ctx, "projects/my-project/locations/l/services/s", nil, pol("serviceAccount:a@p.iam.gserviceaccount.com")); err != nil {
		t.Errorf("a service account: %v", err)
	}
	if err := checkOrgMembers(ctx, "projects/my-project/locations/l/services/s", nil, pol("allUsers")); apierror.From(err).Code != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), orgpolicy.MembersMessage) {
		t.Errorf("allUsers: %v", err)
	}
	if err := checkOrgMembers(ctx, "projects/my-project/locations/l/services/s", pol("allUsers"), pol("allUsers")); err != nil {
		t.Errorf("allUsers already granted: %v", err)
	}
}
