package resourcemanager

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	iam "cloud.google.com/go/iam/apiv2"
	iamv2 "cloud.google.com/go/iam/apiv2/iampb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestDenyPoliciesThroughTheOfficialClient (#1886): IAM v2 Policies over
// gRPC and REST with cloud.google.com/go/iam/apiv2: create (named by the
// project's ID, answered with its number), get, list without rules,
// update with a stale etag ABORTED, delete, and the validation of rules.
func TestDenyPoliciesThroughTheOfficialClient(t *testing.T) {
	reg, srv, _ := startAPI(t)
	ctx := context.Background()
	if _, err := reg.Create(Project{ProjectID: "deny-p"}); err != nil {
		t.Fatal(err)
	}
	num := strconv.FormatInt(reg.projectNumber("deny-p"), 10)
	grpcC, err := iam.NewPoliciesClient(ctx, option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = grpcC.Close() })
	restC, err := iam.NewPoliciesRESTClient(ctx, option.WithEndpoint("http://"+srv.Addr()), option.WithoutAuthentication(),
		option.WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restC.Close() })
	parent := "policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/projects/deny-p") + "/denypolicies"
	wantName := "policies/cloudresourcemanager.googleapis.com%2Fprojects%2F" + num + "/denypolicies/"
	rule := func() []*iamv2.PolicyRule {
		return []*iamv2.PolicyRule{{Description: "no secrets", Kind: &iamv2.PolicyRule_DenyRule{DenyRule: &iamv2.DenyRule{
			DeniedPrincipals:  []string{"principalSet://goog/public:all"},
			DeniedPermissions: []string{"secretmanager.googleapis.com/versions.access"},
			DenialCondition:   &expr.Expr{Expression: `resource.matchTag("123/env", "prod")`},
		}}}}
	}
	for name, c := range map[string]*iam.PoliciesClient{"grpc": grpcC, "rest": restC} {
		id := "deny-" + name
		op, err := c.CreatePolicy(ctx, &iamv2.CreatePolicyRequest{Parent: parent, PolicyId: id,
			Policy: &iamv2.Policy{DisplayName: "My deny policy.", Rules: rule()}})
		if err != nil {
			t.Fatalf("%s create: %v", name, err)
		}
		p, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("%s create wait: %v", name, err)
		}
		if p.GetName() != wantName+id || p.GetKind() != "DenyPolicy" || p.GetUid() == "" || p.GetEtag() == "" || p.GetCreateTime() == nil {
			t.Fatalf("%s created %v", name, p)
		}
		got, err := c.GetPolicy(ctx, &iamv2.GetPolicyRequest{Name: p.GetName()})
		if err != nil || len(got.GetRules()) != 1 || got.GetEtag() != p.GetEtag() {
			t.Fatalf("%s get: %v %v", name, got, err)
		}
		var listed []*iamv2.Policy
		it := c.ListPolicies(ctx, &iamv2.ListPoliciesRequest{Parent: parent})
		for {
			x, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("%s list: %v", name, err)
			}
			listed = append(listed, x)
		}
		found := false
		for _, x := range listed {
			if x.GetName() == p.GetName() {
				found = true
				if len(x.GetRules()) != 0 {
					t.Errorf("%s list carries rules", name)
				}
			}
		}
		if !found {
			t.Fatalf("%s list: %v", name, listed)
		}
		stale := proto2(got)
		stale.Etag = "stale"
		uop, err := c.UpdatePolicy(ctx, &iamv2.UpdatePolicyRequest{Policy: stale})
		if err == nil {
			_, err = uop.Wait(ctx)
		}
		if status.Code(err) != codes.Aborted {
			t.Fatalf("%s update with a stale etag: %v", name, err)
		}
		got.DisplayName = "renamed"
		uop, err = c.UpdatePolicy(ctx, &iamv2.UpdatePolicyRequest{Policy: got})
		if err != nil {
			t.Fatalf("%s update: %v", name, err)
		}
		up, err := uop.Wait(ctx)
		if err != nil || up.GetDisplayName() != "renamed" || up.GetEtag() == got.GetEtag() {
			t.Fatalf("%s updated %v %v", name, up, err)
		}
		dop, err := c.DeletePolicy(ctx, &iamv2.DeletePolicyRequest{Name: p.GetName()})
		if err != nil {
			t.Fatalf("%s delete: %v", name, err)
		}
		if dp, err := dop.Wait(ctx); err != nil || dp.GetDeleteTime() == nil {
			t.Fatalf("%s deleted %v %v", name, dp, err)
		}
		if _, err := c.GetPolicy(ctx, &iamv2.GetPolicyRequest{Name: p.GetName()}); status.Code(err) != codes.NotFound {
			t.Fatalf("%s get after delete: %v", name, err)
		}
	}
	bad := map[string]*iamv2.DenyRule{
		"v1 permission":        {DeniedPrincipals: []string{"principalSet://goog/public:all"}, DeniedPermissions: []string{"secretmanager.versions.access"}},
		"v1 member":            {DeniedPrincipals: []string{"user:a@example.com"}, DeniedPermissions: []string{"secretmanager.googleapis.com/versions.access"}},
		"public exception":     {DeniedPrincipals: []string{"principal://goog/subject/a@example.com"}, ExceptionPrincipals: []string{"principalSet://goog/public:all"}, DeniedPermissions: []string{"secretmanager.googleapis.com/versions.access"}},
		"non-tag condition":    {DeniedPrincipals: []string{"principalSet://goog/public:all"}, DeniedPermissions: []string{"secretmanager.googleapis.com/versions.access"}, DenialCondition: &expr.Expr{Expression: `request.time < timestamp("2030-01-01T00:00:00Z")`}},
		"no denied principals": {DeniedPermissions: []string{"secretmanager.googleapis.com/versions.access"}},
	}
	for what, r := range bad {
		_, err := grpcC.CreatePolicy(ctx, &iamv2.CreatePolicyRequest{Parent: parent, PolicyId: "bad-rule",
			Policy: &iamv2.Policy{Rules: []*iamv2.PolicyRule{{Kind: &iamv2.PolicyRule_DenyRule{DenyRule: r}}}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", what, err)
		}
	}
	for _, id := range []string{"ab", "Upper", "1abc", strings.Repeat("a", 64)} {
		if _, err := grpcC.CreatePolicy(ctx, &iamv2.CreatePolicyRequest{Parent: parent, PolicyId: id, Policy: &iamv2.Policy{Rules: rule()}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("policy ID %q: %v", id, err)
		}
	}
	if _, err := grpcC.CreatePolicy(ctx, &iamv2.CreatePolicyRequest{Parent: "policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/projects/nope") + "/denypolicies",
		PolicyId: "abc", Policy: &iamv2.Policy{Rules: rule()}}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown project: %v", err)
	}
	// The resource's limit of 500 deny rules across its policies.
	many := make([]*iamv2.PolicyRule, 0, 300)
	for range 300 {
		many = append(many, rule()[0])
	}
	if _, err := grpcC.CreatePolicy(ctx, &iamv2.CreatePolicyRequest{Parent: parent, PolicyId: "many-1", Policy: &iamv2.Policy{Rules: many}}); err != nil {
		t.Fatal(err)
	}
	if _, err := grpcC.CreatePolicy(ctx, &iamv2.CreatePolicyRequest{Parent: parent, PolicyId: "many-2", Policy: &iamv2.Policy{Rules: many}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("601 rules on a resource: %v", err)
	}
	if got := reg.DenyPolicies("projects/deny-p"); len(got) != 1 {
		t.Errorf("DenyPolicies by project ID: %d", len(got))
	}
}

func proto2(p *iamv2.Policy) *iamv2.Policy {
	return &iamv2.Policy{Name: p.GetName(), Etag: p.GetEtag(), DisplayName: p.GetDisplayName(), Rules: p.GetRules()}
}
