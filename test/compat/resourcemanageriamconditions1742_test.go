//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestResourceManagerIAMConditions (#1742): IAM Conditions on a project's
// policy through the official v3 ProjectsClient and v1 REST client, as
// Google's "Policy versions" documents them. A version 3 policy with a
// conditional binding is stored and read back with its condition when
// version 3 is requested; a conditional binding in a version 1 (or
// unversioned) policy, a condition without a title or an expression, and
// an expression that does not compile are INVALID_ARGUMENT; a version 1
// read renders ROLE_withcond_HASH without the condition, a version 1 set
// of that rendering keeps the condition, and a mix of conditions and
// hashes is refused with Google's message. Folders and the organization
// take the same policy code.
// covers: google.cloud.resourcemanager.v3.Projects/GetIamPolicy, google.cloud.resourcemanager.v3.Projects/SetIamPolicy, cloudresourcemanager.projects.getIamPolicy, cloudresourcemanager.projects.setIamPolicy
func TestResourceManagerIAMConditions(t *testing.T) {
	h := New(t)
	ep := h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	id := iamProject(t, h, "iamcond-")
	c, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name := "projects/" + id
	cond := &expr.Expr{Title: "one database", Description: "the app's database",
		Expression: `resource.name == "projects/` + id + `/databases/app" && request.time < timestamp("2099-01-01T00:00:00Z")`}
	set := func(p *iampb.Policy) (*iampb.Policy, error) {
		return c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: p})
	}
	binding := func(c *expr.Expr) []*iampb.Binding {
		return []*iampb.Binding{{Role: "roles/datastore.user", Members: []string{"user:ada@example.com"}, Condition: c}}
	}
	for what, p := range map[string]*iampb.Policy{
		"a conditional binding in a version 1 policy": {Version: 1, Bindings: binding(cond)},
		"a conditional binding with no version":       {Bindings: binding(cond)},
		"a condition without a title":                 {Version: 3, Bindings: binding(&expr.Expr{Expression: "true"})},
		"a condition without an expression":           {Version: 3, Bindings: binding(&expr.Expr{Title: "t"})},
		"an expression that does not compile":         {Version: 3, Bindings: binding(&expr.Expr{Title: "t", Expression: `resource.name ==`})},
		"an expression that is not a boolean":         {Version: 3, Bindings: binding(&expr.Expr{Title: "t", Expression: `resource.name`})},
	} {
		if _, err := set(p); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: SetIamPolicy = %v; want INVALID_ARGUMENT", what, err)
		}
	}
	got, err := set(&iampb.Policy{Version: 3, Bindings: append(binding(cond),
		&iampb.Binding{Role: "roles/viewer", Members: []string{"user:ada@example.com"}})})
	if err != nil || got.GetVersion() != 3 || got.GetBindings()[0].GetCondition().GetTitle() != cond.Title {
		t.Fatalf("SetIamPolicy version 3 = %v, %v", got, err)
	}
	v3, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil || v3.GetVersion() != 3 || len(v3.GetBindings()) != 2 {
		t.Fatalf("GetIamPolicy version 3 = %v, %v", v3, err)
	}
	if g := v3.GetBindings()[0].GetCondition(); g.GetTitle() != cond.Title || g.GetDescription() != cond.Description || g.GetExpression() != cond.Expression {
		t.Errorf("the condition read back = %v; want %v", g, cond)
	}

	// Version 1, through the v1 REST client gcloud and Terraform call.
	v1 := rmV1(t, h)
	p1, err := v1.Projects.GetIamPolicy(id, &crmv1.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil || p1.Version != 1 {
		t.Fatalf("v1 getIamPolicy = %+v, %v", p1, err)
	}
	var hashed *crmv1.Binding
	for _, b := range p1.Bindings {
		if strings.HasPrefix(b.Role, "roles/datastore.user_withcond_") && b.Condition == nil {
			hashed = b
		}
	}
	if hashed == nil {
		t.Fatalf("v1 getIamPolicy bindings = %+v; want roles/datastore.user_withcond_HASH without its condition", p1.Bindings)
	}
	mixed := &crmv1.Policy{Version: 3, Etag: p1.Etag, Bindings: append(append([]*crmv1.Binding(nil), p1.Bindings...),
		&crmv1.Binding{Role: "roles/owner", Members: []string{"user:bob@example.com"}, Condition: &crmv1.Expr{Title: "t", Expression: "true"}})}
	if _, err := v1.Projects.SetIamPolicy(id, &crmv1.SetIamPolicyRequest{Policy: mixed}).Context(ctx).Do(); err == nil ||
		!strings.Contains(err.Error(), "Policy cannot contain both conditions and condition hashes") {
		t.Errorf("v1 setIamPolicy with conditions and hashes = %v", err)
	}
	hashed.Members = append(hashed.Members, "user:bob@example.com")
	if _, err := v1.Projects.SetIamPolicy(id, &crmv1.SetIamPolicyRequest{Policy: p1}).Context(ctx).Do(); err != nil {
		t.Fatalf("v1 setIamPolicy of the version 1 rendering: %v", err)
	}
	after, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil {
		t.Fatal(err)
	}
	var kept bool
	for _, b := range after.GetBindings() {
		if b.GetRole() == "roles/datastore.user" && b.GetCondition().GetExpression() == cond.Expression && len(b.GetMembers()) == 2 {
			kept = true
		}
	}
	if !kept {
		t.Errorf("after a version 1 set of the rendering = %v; want the condition kept, with bob added", after.GetBindings())
	}
}
