package sqladmin

import (
	"context"
	"io"
	"net/http"
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

func TestOrgPolicyLocationsOnCloudSQL(t *testing.T) {
	useOrgPolicies(t)
	ts, _ := fieldsServer(t)
	resp, err := http.Post(ts.URL+"/v1/projects/proj/instances", "application/json",
		strings.NewReader(`{"name":"in-eu","databaseVersion":"POSTGRES_17","region":"europe-west1","settings":{"tier":"db-custom-1-3840"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed || !strings.Contains(string(b), "Location REGION:europe-west1 violates constraint constraints/gcp.resourceLocations on the resource projects/proj.") {
		t.Fatalf("instances.insert in europe-west1 = %d %s, want 412", resp.StatusCode, b)
	}
}
