package pubsubfront

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
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

func TestOrgPoliciesOnPubSub(t *testing.T) {
	ctx := context.Background()
	topic := func(r ...string) *pubsubpb.Topic {
		return &pubsubpb.Topic{Name: "projects/p/topics/t", MessageStoragePolicy: &pubsubpb.MessageStoragePolicy{AllowedPersistenceRegions: r}}
	}
	if err := checkTopicRegions(ctx, "projects/p/topics/t", topic("europe-west1")); err != nil {
		t.Fatalf("no policy: %v", err)
	}
	useOrgPolicies(t)
	if err := checkTopicRegions(ctx, "projects/p/topics/t", topic("us-east1", "us-central1")); err != nil {
		t.Errorf("US regions: %v", err)
	}
	if err := checkTopicRegions(ctx, "projects/p/topics/t", topic("us-east1", "europe-west1")); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("europe-west1 = %v, want FAILED_PRECONDITION", err)
	}
	r := httptest.NewRequest(http.MethodPatch, "/v1/projects/p/topics/t",
		strings.NewReader(`{"topic":{"messageStoragePolicy":{"allowedPersistenceRegions":["asia-east1"]}},"updateMask":"messageStoragePolicy"}`))
	if err := checkRESTTopic(r, "projects/p/topics/t"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("REST UpdateTopic to asia-east1 = %v", err)
	}
	r = httptest.NewRequest(http.MethodPatch, "/v1/projects/p/topics/t", strings.NewReader(`{"topic":{"labels":{"a":"b"}},"updateMask":"labels"}`))
	if err := checkRESTTopic(r, "projects/p/topics/t"); err != nil {
		t.Errorf("REST UpdateTopic of labels = %v", err)
	}
	after := &iampolicy.Stored{Bindings: []iampolicy.Binding{{Role: "roles/pubsub.subscriber", Members: []string{"allAuthenticatedUsers"}}}}
	if err := checkOrgMembers(ctx, "projects/p/topics/t", nil, after); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("allAuthenticatedUsers = %v", err)
	}
}
