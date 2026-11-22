//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	iam "cloud.google.com/go/iam/apiv3"
	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestIAMTroubleshooterExplainsPAB (#2057): Policy Troubleshooter shows
// the principal access boundary policies bound to a principal. A service
// account of one project is bound, through its project's principal set, to
// a PAB policy listing only another project; checking
// resourcemanager.projects.get on its own project shows "Access not
// granted", the boundary blocking it, and a row naming the policy, the
// binding, the principal set and the listed resource as not including it.
// The form filled from ?principal=&resource=&permission=, with its
// results, passes the console's WCAG 2.1 AA audit.
func TestIAMTroubleshooterExplainsPAB(t *testing.T) {
	needService(t, "iam-principal-access-boundary")
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(endpoint(t, envResourceManager)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	pc, err := iam.NewPrincipalAccessBoundaryPoliciesClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	bc, err := iam.NewPolicyBindingsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	project := uniqueProject(t)
	elsewhere := uniqueProject(t)
	id := fmt.Sprintf("tsboundary-%d", time.Now().UnixNano()%1e9)
	op, err := pc.CreatePrincipalAccessBoundaryPolicy(ctx, &iamv3.CreatePrincipalAccessBoundaryPolicyRequest{
		Parent: "organizations/100000000001/locations/global", PrincipalAccessBoundaryPolicyId: id,
		PrincipalAccessBoundaryPolicy: &iamv3.PrincipalAccessBoundaryPolicy{Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{
			Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{{Effect: iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW,
				Resources: []string{"//cloudresourcemanager.googleapis.com/projects/" + elsewhere}}}}}})
	if err != nil {
		t.Fatalf("CreatePrincipalAccessBoundaryPolicy: %v", err)
	}
	policy, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreatePrincipalAccessBoundaryPolicy: %v", err)
	}
	t.Cleanup(func() {
		if op, err := pc.DeletePrincipalAccessBoundaryPolicy(context.Background(), &iamv3.DeletePrincipalAccessBoundaryPolicyRequest{Name: policy.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	set := "//cloudresourcemanager.googleapis.com/projects/" + project
	bop, err := bc.CreatePolicyBinding(ctx, &iamv3.CreatePolicyBindingRequest{Parent: "projects/" + project + "/locations/global", PolicyBindingId: "ts-binding",
		PolicyBinding: &iamv3.PolicyBinding{Target: &iamv3.PolicyBinding_Target{Target: &iamv3.PolicyBinding_Target_PrincipalSet{PrincipalSet: set}},
			PolicyKind: iamv3.PolicyBinding_PRINCIPAL_ACCESS_BOUNDARY, Policy: policy.GetName()}})
	if err != nil {
		t.Fatalf("CreatePolicyBinding: %v", err)
	}
	binding, err := bop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreatePolicyBinding: %v", err)
	}

	q := url.Values{"principal": {"worker@" + project + ".iam.gserviceaccount.com"}, "resource": {set},
		"permission": {"resourcemanager.projects.get"}}
	path := "/iam-admin/troubleshooter?" + q.Encode()
	p := open(t)
	p.navigate(path + "&project=" + project)
	p.waitFor(`document.querySelector("#troubleshoot-pab tbody tr") !== null`)
	var verdict, state, row string
	p.eval(`document.querySelector("#troubleshoot-verdict").textContent`, &verdict)
	p.eval(`document.querySelector("#troubleshoot-pab-state").textContent`, &state)
	p.eval(`[...document.querySelectorAll("#troubleshoot-pab tbody tr td")].map((c) => c.textContent).join(" | ")`, &row)
	if verdict != "Access not granted" || state != "Principal access boundary: Blocks access." {
		t.Errorf("verdict %q, boundary %q; want Access not granted, Blocks access", verdict, state)
	}
	for _, want := range []string{policy.GetName(), binding.GetName(), set, "//cloudresourcemanager.googleapis.com/projects/" + elsewhere + " (does not include it)", "Blocks access"} {
		if !strings.Contains(row, want) {
			t.Errorf("the PAB row %q misses %q", row, want)
		}
	}

	auditScreens(t, []a11yPage{{"Policy Troubleshooter with a PAB explanation", path + "&project=" + url.QueryEscape(project),
		settled + ` && document.querySelector("#troubleshoot-pab tbody tr") !== null`, ""}})
}
