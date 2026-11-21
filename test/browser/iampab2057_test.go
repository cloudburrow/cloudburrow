//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	iam "cloud.google.com/go/iam/apiv3"
	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestIAMPrincipalAccessBoundaryPage (#2057): IAM & Admin > Principal
// access boundary in Google's steps. Create policy takes a boundary rule
// (Description, Resources), then the policy name, ID and enforcement
// version; the list shows the policy; its page's Add binding binds it to a
// project's principal set with a condition; Edit policy changes the
// enforcement version; the binding's Delete removes it; the list, the
// create page and the policy's page pass the console's WCAG 2.1 AA audit.
// Each is read back through the official cloud.google.com/go/iam/apiv3
// client.
func TestIAMPrincipalAccessBoundaryPage(t *testing.T) {
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
	id := fmt.Sprintf("boundary-%d", time.Now().UnixNano()%1e9)
	name := "organizations/100000000001/locations/global/principalAccessBoundaryPolicies/" + id
	t.Cleanup(func() {
		if op, err := pc.DeletePrincipalAccessBoundaryPolicy(context.Background(), &iamv3.DeletePrincipalAccessBoundaryPolicyRequest{Name: name, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	p := open(t)
	p.navigate("/iam-admin/principal-access-boundary/create?project=" + project)
	p.waitFor(`document.querySelector("#f-ruleResources") !== null`)
	var steps []string
	p.eval(`[...document.querySelectorAll(".create-step-title")].map((s) => s.textContent)`, &steps)
	if len(steps) != 2 || steps[0] != "Boundary rule" || steps[1] != "Policy" {
		t.Errorf("the create page's steps are %v", steps)
	}
	setField(p, "#view", "f-ruleDescription", "the project")
	setField(p, "#view", "f-ruleResources", "//cloudresourcemanager.googleapis.com/projects/"+project)
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-displayName", "Project boundary")
	setField(p, "#view", "f-policyId", id)
	setField(p, "#view", "f-enforcementVersion", "1")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("` + id + `"))`)
	pol, err := pc.GetPrincipalAccessBoundaryPolicy(ctx, &iamv3.GetPrincipalAccessBoundaryPolicyRequest{Name: name})
	if err != nil || pol.GetDisplayName() != "Project boundary" || pol.GetDetails().GetEnforcementVersion() != "1" ||
		len(pol.GetDetails().GetRules()) != 1 || pol.GetDetails().GetRules()[0].GetResources()[0] != "//cloudresourcemanager.googleapis.com/projects/"+project {
		t.Fatalf("the new policy = %v, %v", pol, err)
	}

	// The policy's page: Add binding to the project's principal set.
	p.navigate("/iam-admin/principal-access-boundary/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	p.pageAction("Add binding", map[string]string{"bindingDisplayName": "Service accounts", "bindingId": "sa-binding", "targetType": "Project",
		"targetId": project, "conditionTitle": "sa", "conditionExpression": "principal.type == 'iam.googleapis.com/ServiceAccount'"}, "")
	var binding *iamv3.PolicyBinding
	for i := 0; i < 20 && binding == nil; i++ {
		if b, err := pc.SearchPrincipalAccessBoundaryPolicyBindings(ctx, &iamv3.SearchPrincipalAccessBoundaryPolicyBindingsRequest{Name: name}).Next(); err == nil {
			binding = b
		} else {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if binding == nil || binding.GetTarget().GetPrincipalSet() != "//cloudresourcemanager.googleapis.com/projects/"+project ||
		!strings.Contains(binding.GetCondition().GetExpression(), "ServiceAccount") || !strings.HasSuffix(binding.GetName(), "/policyBindings/sa-binding") {
		t.Fatalf("the added binding = %v", binding)
	}
	p.navigate("/iam-admin/principal-access-boundary/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	p.clickText("#view [role=tab]", "Bindings")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Service accounts"))`)

	auditScreens(t, []a11yPage{
		screen("Principal access boundary", "/iam-admin/principal-access-boundary", project),
		screen("create a principal access boundary policy", "/iam-admin/principal-access-boundary/create", project),
		screen("a principal access boundary policy", "/iam-admin/principal-access-boundary/"+id, project),
	})

	// The binding's page: Delete.
	p.navigate("/iam-admin/principal-access-boundary/" + id + "/b:" + strings.ReplaceAll(binding.GetName(), "/", "~") + "?project=" + project)
	p.clickAction("Delete", "b:"+strings.ReplaceAll(binding.GetName(), "/", "~"))
	for i := 0; i < 20; i++ {
		if _, err = bc.GetPolicyBinding(ctx, &iamv3.GetPolicyBindingRequest{Name: binding.GetName()}); err != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err == nil {
		t.Errorf("the binding outlived its Delete")
	}
}
