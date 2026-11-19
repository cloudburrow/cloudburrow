package resourcemanager

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	orgpolicy "cloud.google.com/go/orgpolicy/apiv2"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	opol "github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

func TestOrgPolicyCustomerMatchesTheOrganization(t *testing.T) {
	if opol.Customer.ID != OrganizationCustomerID || opol.Customer.OrgNumber != OrganizationNumber || opol.Customer.Domain != OrganizationDomain {
		t.Fatalf("orgpolicy.Customer = %+v, want the local organization's", opol.Customer)
	}
}

func values(allowed, denied []string) *orgpolicypb.PolicySpec_PolicyRule {
	return &orgpolicypb.PolicySpec_PolicyRule{Kind: &orgpolicypb.PolicySpec_PolicyRule_Values{
		Values: &orgpolicypb.PolicySpec_PolicyRule_StringValues{AllowedValues: allowed, DeniedValues: denied}}}
}

func enforce(on bool) *orgpolicypb.PolicySpec_PolicyRule {
	return &orgpolicypb.PolicySpec_PolicyRule{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: on}}
}

// TestOrgPolicyV2 (#1890) through the official orgpolicy/apiv2 client
// over gRPC and JSON: create, get, update with an etag, list, effective
// down the hierarchy (inherit, merge, reset), v1 and v2 reading each
// other's policies, custom constraints, and the source the services ask.
func TestOrgPolicyV2(t *testing.T) {
	reg, srv, _ := startAPI(t)
	ctx := context.Background()
	g, err := orgpolicy.NewClient(ctx, option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	j, err := orgpolicy.NewRESTClient(ctx, option.WithEndpoint("http://"+srv.Addr()), option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	f, err := reg.CreateFolder(OrganizationName, "Apps")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(Project{ProjectID: "op2-proj", Parent: f.Name}); err != nil {
		t.Fatal(err)
	}
	num := strconv.FormatInt(reg.projectNumber("op2-proj"), 10)
	const loc = "gcp.resourceLocations"

	for _, c := range []*orgpolicy.Client{g, j} {
		name := "projects/op2-proj/policies/iam.disableServiceAccountKeyCreation"
		p, err := c.CreatePolicy(ctx, &orgpolicypb.CreatePolicyRequest{Parent: "projects/op2-proj",
			Policy: &orgpolicypb.Policy{Name: name, Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{enforce(true)}}}})
		if err != nil {
			t.Fatalf("CreatePolicy: %v", err)
		}
		if p.GetName() != "projects/"+num+"/policies/iam.disableServiceAccountKeyCreation" || p.GetEtag() == "" || p.GetSpec().GetUpdateTime() == nil {
			t.Fatalf("CreatePolicy = %v", p)
		}
		if _, err := c.CreatePolicy(ctx, &orgpolicypb.CreatePolicyRequest{Parent: "projects/op2-proj", Policy: p}); errCode(err) != codes.AlreadyExists {
			t.Fatalf("CreatePolicy again = %v, want ALREADY_EXISTS", err)
		}
		v1, err := reg.OrgPolicy("op2-proj", "constraints/iam.disableServiceAccountKeyCreation")
		if err != nil || v1.BooleanPolicy == nil || !v1.BooleanPolicy.Enforced {
			t.Fatalf("v1 read of a v2 policy = %+v, %v", v1, err)
		}
		stale := &orgpolicypb.Policy{Name: name, Etag: "AAAAAAAAAAA=", Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{enforce(false)}}}
		if _, err := c.UpdatePolicy(ctx, &orgpolicypb.UpdatePolicyRequest{Policy: stale}); errCode(err) != codes.Aborted {
			t.Fatalf("UpdatePolicy with a stale etag = %v, want ABORTED", err)
		}
		stale.Etag = p.GetEtag()
		if _, err := c.UpdatePolicy(ctx, &orgpolicypb.UpdatePolicyRequest{Policy: stale}); err != nil {
			t.Fatalf("UpdatePolicy: %v", err)
		}
		it := c.ListPolicies(ctx, &orgpolicypb.ListPoliciesRequest{Parent: "projects/" + num})
		n := 0
		for {
			_, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListPolicies: %v", err)
			}
			n++
		}
		if n != 1 {
			t.Fatalf("ListPolicies = %d policies, want 1", n)
		}
		if err := c.DeletePolicy(ctx, &orgpolicypb.DeletePolicyRequest{Name: name}); err != nil {
			t.Fatalf("DeletePolicy: %v", err)
		}
		if _, err := c.GetPolicy(ctx, &orgpolicypb.GetPolicyRequest{Name: name}); errCode(err) != codes.NotFound {
			t.Fatalf("GetPolicy after delete = %v, want NOT_FOUND", err)
		}
		if _, err := c.UpdatePolicy(ctx, &orgpolicypb.UpdatePolicyRequest{Policy: stale}); errCode(err) != codes.NotFound {
			t.Fatalf("UpdatePolicy of none = %v, want NOT_FOUND", err)
		}
		// A boolean constraint takes enforce rules only.
		bad := &orgpolicypb.Policy{Name: name, Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{values([]string{"x"}, nil)}}}
		if _, err := c.CreatePolicy(ctx, &orgpolicypb.CreatePolicyRequest{Parent: "projects/op2-proj", Policy: bad}); errCode(err) != codes.InvalidArgument {
			t.Fatalf("values on a boolean constraint = %v, want INVALID_ARGUMENT", err)
		}
	}

	// Hierarchy: the organization allows the US, the folder merges in
	// europe-west1, the project inherits; a reset restores the default.
	mk := func(parent string, spec *orgpolicypb.PolicySpec) {
		t.Helper()
		if _, err := g.CreatePolicy(ctx, &orgpolicypb.CreatePolicyRequest{Parent: parent,
			Policy: &orgpolicypb.Policy{Name: parent + "/policies/" + loc, Spec: spec}}); err != nil {
			t.Fatalf("CreatePolicy on %s: %v", parent, err)
		}
	}
	mk(OrganizationName, &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{values([]string{"in:us-locations"}, nil)}})
	mk(f.Name, &orgpolicypb.PolicySpec{InheritFromParent: true, Rules: []*orgpolicypb.PolicySpec_PolicyRule{values([]string{"europe-west1"}, []string{"us-east1"})}})
	if _, err := g.CreatePolicy(ctx, &orgpolicypb.CreatePolicyRequest{Parent: f.Name, Policy: &orgpolicypb.Policy{Name: f.Name + "/policies/" + loc,
		Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{values([]string{"in:nowhere-locations"}, nil)}}}}); errCode(err) == codes.OK {
		t.Fatal("an unknown value group was accepted")
	}
	eff, err := j.GetEffectivePolicy(ctx, &orgpolicypb.GetEffectivePolicyRequest{Name: "projects/op2-proj/policies/" + loc})
	if err != nil || len(eff.GetSpec().GetRules()) != 2 {
		t.Fatalf("GetEffectivePolicy = %v, %v; want the organization's and the folder's rules", eff, err)
	}
	src := OrgPolicySource{Reg: func() *Registry { return reg }}
	opol.Use(src)
	t.Cleanup(func() { opol.Use(nil) })
	for loc, ok := range map[string]bool{"us-central1": true, "US": true, "nam4": true, "us-central1-a": true, "europe-west1": true, "us-east1": false, "asia-east1": false, "global": true} {
		err := opol.CheckLocations(ctx, "projects/op2-proj", "projects/op2-proj", loc)
		if (err == nil) != ok {
			t.Errorf("CheckLocations(%s) = %v, want allowed %v", loc, err, ok)
		}
	}
	if err := opol.CheckLocations(ctx, "projects/op2-proj", "projects/op2-proj", "asia-east1"); err == nil ||
		err.Error() != "Location REGION:asia-east1 violates constraint constraints/gcp.resourceLocations on the resource projects/op2-proj." {
		t.Errorf("CheckLocations message = %v", err)
	}
	mk("projects/op2-proj", &orgpolicypb.PolicySpec{Reset_: true})
	if err := opol.CheckLocations(ctx, "projects/op2-proj", "projects/op2-proj", "asia-east1"); err != nil {
		t.Errorf("CheckLocations under a reset = %v, want allowed", err)
	}

	// Domain restricted sharing, with a tag condition: allowed customers
	// everywhere, but allowAll on resources tagged env=sandbox.
	dom := "projects/op2-proj/policies/iam.allowedPolicyMemberDomains"
	if _, err := g.CreatePolicy(ctx, &orgpolicypb.CreatePolicyRequest{Parent: "projects/op2-proj", Policy: &orgpolicypb.Policy{Name: dom,
		Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{values([]string{"is:" + OrganizationCustomerID}, nil),
			{Kind: &orgpolicypb.PolicySpec_PolicyRule_AllowAll{AllowAll: true}, Condition: &expr.Expr{Expression: "resource.matchTag('" + OrganizationNumber + "/env', 'sandbox')"}}}}}}); err != nil {
		t.Fatalf("CreatePolicy %s: %v", dom, err)
	}
	if err := opol.CheckMembers(ctx, "projects/op2-proj", nil, []string{"user:a@" + OrganizationDomain, "serviceAccount:x@op2-proj.iam.gserviceaccount.com"}); err != nil {
		t.Errorf("CheckMembers of the customer's members = %v", err)
	}
	for _, m := range []string{"user:a@example.com", "allUsers", "allAuthenticatedUsers"} {
		if err := opol.CheckMembers(ctx, "projects/op2-proj", nil, []string{m}); err == nil || err.Error() != opol.MembersMessage {
			t.Errorf("CheckMembers(%s) = %v, want refused", m, err)
		}
	}
	if err := opol.CheckMembers(ctx, "projects/op2-proj", []string{"user:a@example.com"}, []string{"user:a@example.com"}); err != nil {
		t.Errorf("CheckMembers of a member already granted = %v, want allowed", err)
	}

	// v1 sees v2's policies, and a v1 set replaces a v2 policy.
	all, err := reg.OrgPolicies("op2-proj")
	if err != nil || len(all) != 2 {
		t.Fatalf("v1 OrgPolicies = %v, %v; want 2", all, err)
	}
	if _, err := reg.SetOrgPolicy("op2-proj", &crmv1.OrgPolicy{Constraint: "constraints/" + loc, ListPolicy: &crmv1.ListPolicy{AllowedValues: []string{"asia-east1"}}}); err != nil {
		t.Fatal(err)
	}
	p, err := reg.PolicyV2("projects/op2-proj/policies/" + loc)
	if err != nil || len(p.GetSpec().GetRules()) != 1 || p.GetSpec().GetRules()[0].GetValues().GetAllowedValues()[0] != "asia-east1" {
		t.Fatalf("v2 read of a v1 policy = %v, %v", p, err)
	}

	// Custom constraints and the constraint list.
	cc := &orgpolicypb.CustomConstraint{Name: OrganizationName + "/customConstraints/custom.noPublic", ResourceTypes: []string{"storage.googleapis.com/Bucket"},
		MethodTypes: []orgpolicypb.CustomConstraint_MethodType{orgpolicypb.CustomConstraint_CREATE}, Condition: "resource.name.startsWith('x')",
		ActionType: orgpolicypb.CustomConstraint_DENY, DisplayName: "No x"}
	if _, err := j.CreateCustomConstraint(ctx, &orgpolicypb.CreateCustomConstraintRequest{Parent: OrganizationName, CustomConstraint: cc}); err != nil {
		t.Fatalf("CreateCustomConstraint: %v", err)
	}
	if got, err := g.GetCustomConstraint(ctx, &orgpolicypb.GetCustomConstraintRequest{Name: cc.Name}); err != nil || got.GetDisplayName() != "No x" {
		t.Fatalf("GetCustomConstraint = %v, %v", got, err)
	}
	found := false
	cit := g.ListConstraints(ctx, &orgpolicypb.ListConstraintsRequest{Parent: "projects/op2-proj"})
	for {
		c, err := cit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListConstraints: %v", err)
		}
		found = found || c.GetName() == "projects/"+num+"/constraints/custom.noPublic"
	}
	if !found {
		t.Fatal("ListConstraints does not list the custom constraint")
	}
	if err := g.DeleteCustomConstraint(ctx, &orgpolicypb.DeleteCustomConstraintRequest{Name: cc.Name}); err != nil {
		t.Fatalf("DeleteCustomConstraint: %v", err)
	}
}

// errCode is err's code over gRPC, or over JSON its HTTP status's (409 is
// ALREADY_EXISTS or ABORTED, which the caller tells apart).
func errCode(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case 400:
			return codes.InvalidArgument
		case 404:
			return codes.NotFound
		case 409:
			if strings.Contains(err.Error(), "etag") {
				return codes.Aborted
			}
			return codes.AlreadyExists
		}
		return codes.Unknown
	}
	return status.Code(err)
}
