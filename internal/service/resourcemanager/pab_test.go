package resourcemanager

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	iam "cloud.google.com/go/iam/apiv3"
	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestPABThroughTheOfficialClient (#2057): IAM v3 principal access
// boundary policies and policy bindings over gRPC and REST with
// cloud.google.com/go/iam/apiv3: create, get, list, patch, search,
// delete (refused while bound unless forced), and validation.
func TestPABThroughTheOfficialClient(t *testing.T) {
	reg, srv, _ := startAPI(t)
	ctx := context.Background()
	if _, err := reg.Create(Project{ProjectID: "pab-proj", Parent: OrganizationName}); err != nil {
		t.Fatal(err)
	}
	num := strconv.FormatInt(reg.projectNumber("pab-proj"), 10)
	for _, rest := range []bool{false, true} {
		var pc *iam.PrincipalAccessBoundaryPoliciesClient
		var bc *iam.PolicyBindingsClient
		var err error
		if rest {
			opts := []option.ClientOption{option.WithEndpoint("http://" + srv.Addr()), option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{})}
			pc, err = iam.NewPrincipalAccessBoundaryPoliciesRESTClient(ctx, opts...)
			if err == nil {
				bc, err = iam.NewPolicyBindingsRESTClient(ctx, opts...)
			}
		} else {
			opts := []option.ClientOption{option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
				option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
			pc, err = iam.NewPrincipalAccessBoundaryPoliciesClient(ctx, opts...)
			if err == nil {
				bc, err = iam.NewPolicyBindingsClient(ctx, opts...)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		id := map[bool]string{false: "grpc-pab", true: "rest-pab"}[rest]
		parent := OrganizationName + "/locations/global"
		op, err := pc.CreatePrincipalAccessBoundaryPolicy(ctx, &iamv3.CreatePrincipalAccessBoundaryPolicyRequest{Parent: parent, PrincipalAccessBoundaryPolicyId: id,
			PrincipalAccessBoundaryPolicy: &iamv3.PrincipalAccessBoundaryPolicy{DisplayName: "only pab-proj", Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{
				Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{{Resources: []string{"//cloudresourcemanager.googleapis.com/projects/pab-proj"}, Effect: iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW}}}}})
		if err != nil {
			t.Fatalf("rest=%v create: %v", rest, err)
		}
		p, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p.GetName() != parent+"/principalAccessBoundaryPolicies/"+id || p.GetDetails().GetEnforcementVersion() != "4" || p.GetUid() == "" || p.GetEtag() == "" {
			t.Errorf("created %v", p)
		}
		if got, err := pc.GetPrincipalAccessBoundaryPolicy(ctx, &iamv3.GetPrincipalAccessBoundaryPolicyRequest{Name: p.GetName()}); err != nil || got.GetDisplayName() != "only pab-proj" {
			t.Errorf("get %v %v", got, err)
		}
		n := 0
		it := pc.ListPrincipalAccessBoundaryPolicies(ctx, &iamv3.ListPrincipalAccessBoundaryPoliciesRequest{Parent: parent})
		for {
			_, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			n++
		}
		if n == 0 {
			t.Error("list found nothing")
		}
		uop, err := pc.UpdatePrincipalAccessBoundaryPolicy(ctx, &iamv3.UpdatePrincipalAccessBoundaryPolicyRequest{
			PrincipalAccessBoundaryPolicy: &iamv3.PrincipalAccessBoundaryPolicy{Name: p.GetName(), DisplayName: "renamed",
				Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{EnforcementVersion: "1"}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "details.enforcement_version"}}})
		if err != nil {
			t.Fatal(err)
		}
		if u, err := uop.Wait(ctx); err != nil || u.GetDisplayName() != "renamed" || u.GetDetails().GetEnforcementVersion() != "1" || len(u.GetDetails().GetRules()) != 1 {
			t.Errorf("update %v %v", u, err)
		}
		// Bindings: the project's own set, by ID, named by number.
		bid := id + "-b"
		bop, err := bc.CreatePolicyBinding(ctx, &iamv3.CreatePolicyBindingRequest{Parent: "projects/pab-proj/locations/global", PolicyBindingId: bid,
			PolicyBinding: &iamv3.PolicyBinding{Target: &iamv3.PolicyBinding_Target{Target: &iamv3.PolicyBinding_Target_PrincipalSet{PrincipalSet: "//cloudresourcemanager.googleapis.com/projects/pab-proj"}},
				Policy: p.GetName(), Condition: &expr.Expr{Expression: "principal.type == 'iam.googleapis.com/ServiceAccount'"}}})
		if err != nil {
			t.Fatal(err)
		}
		b, err := bop.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if b.GetName() != "projects/"+num+"/locations/global/policyBindings/"+bid || b.GetPolicyKind() != iamv3.PolicyBinding_PRINCIPAL_ACCESS_BOUNDARY || b.GetPolicyUid() != p.GetUid() {
			t.Errorf("binding %v", b)
		}
		if bs := reg.BoundaryBindings("//cloudresourcemanager.googleapis.com/projects/" + num); len(bs) != 1 || bs[0].Policy == nil {
			t.Errorf("boundary bindings %v", bs)
		}
		sit := pc.SearchPrincipalAccessBoundaryPolicyBindings(ctx, &iamv3.SearchPrincipalAccessBoundaryPolicyBindingsRequest{Name: p.GetName()})
		if got, err := sit.Next(); err != nil || got.GetName() != b.GetName() {
			t.Errorf("search %v %v", got, err)
		}
		tit := bc.SearchTargetPolicyBindings(ctx, &iamv3.SearchTargetPolicyBindingsRequest{Parent: "projects/pab-proj/locations/global", Target: "//cloudresourcemanager.googleapis.com/projects/" + num})
		if got, err := tit.Next(); err != nil || got.GetName() != b.GetName() {
			t.Errorf("search target %v %v", got, err)
		}
		// A bound policy cannot be deleted without force.
		dop, err := pc.DeletePrincipalAccessBoundaryPolicy(ctx, &iamv3.DeletePrincipalAccessBoundaryPolicyRequest{Name: p.GetName()})
		if err == nil || !rest && status.Code(err) != codes.FailedPrecondition {
			t.Errorf("delete while bound: %v %v", dop, err)
		}
		if _, err := bc.CreatePolicyBinding(ctx, &iamv3.CreatePolicyBindingRequest{Parent: "projects/pab-proj/locations/global", PolicyBindingId: "bad-target",
			PolicyBinding: &iamv3.PolicyBinding{Target: &iamv3.PolicyBinding_Target{Target: &iamv3.PolicyBinding_Target_PrincipalSet{PrincipalSet: "//cloudresourcemanager.googleapis.com/" + OrganizationName}},
				Policy: p.GetName()}}); err == nil || !rest && status.Code(err) != codes.InvalidArgument {
			t.Errorf("an organization set under a project: %v", err)
		}
		if _, err := bc.CreatePolicyBinding(ctx, &iamv3.CreatePolicyBindingRequest{Parent: "projects/pab-proj/locations/global", PolicyBindingId: "bad-cond",
			PolicyBinding: &iamv3.PolicyBinding{Target: &iamv3.PolicyBinding_Target{Target: &iamv3.PolicyBinding_Target_PrincipalSet{PrincipalSet: "//cloudresourcemanager.googleapis.com/projects/pab-proj"}},
				Policy: p.GetName(), Condition: &expr.Expr{Expression: "resource.name == 'x'"}}}); err == nil || !rest && status.Code(err) != codes.InvalidArgument {
			t.Errorf("a resource condition: %v", err)
		}
		delB, err := bc.DeletePolicyBinding(ctx, &iamv3.DeletePolicyBindingRequest{Name: b.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if err := delB.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		dop, err = pc.DeletePrincipalAccessBoundaryPolicy(ctx, &iamv3.DeletePrincipalAccessBoundaryPolicyRequest{Name: p.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if err := dop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := pc.GetPrincipalAccessBoundaryPolicy(ctx, &iamv3.GetPrincipalAccessBoundaryPolicyRequest{Name: p.GetName()}); err == nil || !rest && status.Code(err) != codes.NotFound {
			t.Errorf("get after delete: %v", err)
		}
		_ = pc.Close()
		_ = bc.Close()
	}
	// Validation.
	for _, bad := range []*iamv3.PrincipalAccessBoundaryPolicy{
		{Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{}},
		{Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{EnforcementVersion: "9", Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{{Resources: []string{"//cloudresourcemanager.googleapis.com/projects/p"}, Effect: iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW}}}},
		{Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{{Resources: []string{"//storage.googleapis.com/buckets/b"}, Effect: iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW}}}},
		{Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{{Resources: []string{"//cloudresourcemanager.googleapis.com/projects/p"}}}}},
	} {
		if _, err := reg.CreatePABPolicy(OrganizationName+"/locations/global", "bad", bad, false); err == nil {
			t.Errorf("created %v", bad)
		}
	}
	if _, err := reg.CreatePABPolicy("projects/pab-proj/locations/global", "abc", nil, false); err == nil {
		t.Error("a policy in a project")
	}
}
