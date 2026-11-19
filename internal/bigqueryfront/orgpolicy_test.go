package bigqueryfront

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"

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

func TestOrgPolicyLocationsOnDatasets(t *testing.T) {
	useOrgPolicies(t)
	h := Wrap(&datasetEmulator{datasets: map[string]string{}})
	code, got := do(t, h, "POST", base+"/datasets", `{"datasetReference":{"datasetId":"eu","projectId":"p"},"location":"EU"}`)
	if code != 412 || !strings.Contains(fmt.Sprint(got), "Location MULTI_REGION:EU violates constraint constraints/gcp.resourceLocations on the resource projects/p.") {
		t.Fatalf("datasets.insert in EU = %d %v, want 412", code, got)
	}
	// No location is BigQuery's default, US, which the policy allows.
	if code, got := do(t, h, "POST", base+"/datasets", `{"datasetReference":{"datasetId":"us","projectId":"p"}}`); code != 200 {
		t.Fatalf("datasets.insert with the default location = %d %v", code, got)
	}
}
