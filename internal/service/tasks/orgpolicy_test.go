package tasks

import (
	"context"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

type orgPolicySource map[string]*orgpolicypb.PolicySpec

func (s orgPolicySource) Effective(_ context.Context, _, constraint string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{Spec: s[constraint]}, nil
}

// TestOrgPolicyMembersOnQueues (#1890): a queue's setIamPolicy refuses a
// member iam.allowedPolicyMemberDomains does not permit.
func TestOrgPolicyMembersOnQueues(t *testing.T) {
	s := NewStore(store.NewMemory())
	name := "projects/my-project/locations/us-central1/queues/q"
	if _, err := s.CreateQueue(Queue{Name: name}); err != nil {
		t.Fatal(err)
	}
	orgpolicy.Use(orgPolicySource{orgpolicy.AllowedPolicyMemberDomains: {Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Values{
		Values: &orgpolicypb.PolicySpec_PolicyRule_StringValues{AllowedValues: []string{"is:" + orgpolicy.Customer.ID}}}}}}})
	t.Cleanup(func() { orgpolicy.Use(nil) })
	req := func(m string) *iampb.SetIamPolicyRequest {
		return &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/cloudtasks.viewer", Members: []string{m}}}}}
	}
	if _, err := s.SetIamPolicy(name, req("user:x@example.com")); apierror.From(err).Code != codes.FailedPrecondition {
		t.Fatalf("a member outside the customer = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := s.SetIamPolicy(name, req("user:x@"+orgpolicy.Customer.Domain)); err != nil {
		t.Fatalf("the customer's member: %v", err)
	}
}
