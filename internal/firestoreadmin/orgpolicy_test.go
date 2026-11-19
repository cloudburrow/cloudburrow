package firestoreadmin_test

import (
	"context"
	"strings"
	"testing"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	"cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
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

func TestOrgPolicyLocationsOnFirestore(t *testing.T) {
	useOrgPolicies(t)
	addr, _ := front(t)
	ctx := context.Background()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := admin.NewFirestoreAdminClient(ctx, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/p", DatabaseId: "in-eu",
		Database: &adminpb.Database{LocationId: "eur3", Type: adminpb.Database_FIRESTORE_NATIVE}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Location MULTI_REGION:eur3 violates constraint constraints/gcp.resourceLocations on the resource projects/p.") {
		t.Fatalf("CreateDatabase in eur3 = %v, want FAILED_PRECONDITION", err)
	}
	op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/p", DatabaseId: "in-us",
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE}})
	if err != nil {
		t.Fatalf("CreateDatabase in nam5: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
