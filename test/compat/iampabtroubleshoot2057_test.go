//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	iam "cloud.google.com/go/iam/apiv3"
	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	ptiam "cloud.google.com/go/policytroubleshooter/iam/apiv3"
	ptiampb "cloud.google.com/go/policytroubleshooter/iam/apiv3/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"google.golang.org/api/option"
	ptb "google.golang.org/api/policytroubleshooter/v3beta"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestIAMEnforcementPABTroubleshooter (#2057): Policy Troubleshooter
// explains principal access boundary policies. A service account holding
// Browser on its own project and on another is bound, through its
// project's principal set, to a PAB policy listing only its own project.
// v3beta's iam:troubleshoot (google.golang.org/api/policytroubleshooter/v3beta,
// the version Google documents pabPolicyExplanation in) answers, for
// resourcemanager.projects.get on the other project, CANNOT_ACCESS with
// the allow policy granting and pabPolicyExplanation
// PAB_ACCESS_STATE_NOT_ALLOWED, naming the binding and the policy and the
// rule's resource as not included; on its own project CAN_ACCESS with
// the PAB state ALLOWED, relevant (HIGH), and the resource included. The
// official cloud.google.com/go/policytroubleshooter/iam/apiv3 client
// (REST), whose v3 response has no PAB field, gets the same overall
// states. A forced delete of the bound policy removes its binding.
func TestIAMEnforcementPABTroubleshooter(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	home := iamProject(t, h, "pabth-")
	other := iamProject(t, h, "pabto-")
	p := newIAMPrincipal(t, h, home, "pabt", "roles/browser")
	projects, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	undo := bindOn(t, projects.GetIamPolicy, projects.SetIamPolicy, "projects/"+other,
		&iampb.Binding{Role: "roles/browser", Members: []string{"serviceAccount:" + p.email}})
	t.Cleanup(undo)

	pc, err := iam.NewPrincipalAccessBoundaryPoliciesClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	bc, err := iam.NewPolicyBindingsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	homeFull := "//cloudresourcemanager.googleapis.com/projects/" + home
	otherFull := "//cloudresourcemanager.googleapis.com/projects/" + other
	op, err := pc.CreatePrincipalAccessBoundaryPolicy(ctx, &iamv3.CreatePrincipalAccessBoundaryPolicyRequest{Parent: localOrg + "/locations/global",
		PrincipalAccessBoundaryPolicyId: home, PrincipalAccessBoundaryPolicy: &iamv3.PrincipalAccessBoundaryPolicy{DisplayName: "home only",
			Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{EnforcementVersion: "latest", Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{{
				Resources: []string{homeFull}, Effect: iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW}}}}})
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
	bop, err := bc.CreatePolicyBinding(ctx, &iamv3.CreatePolicyBindingRequest{Parent: "projects/" + home + "/locations/global", PolicyBindingId: "pabt-" + home[len(home)-8:],
		PolicyBinding: &iamv3.PolicyBinding{
			Target:     &iamv3.PolicyBinding_Target{Target: &iamv3.PolicyBinding_Target_PrincipalSet{PrincipalSet: homeFull}},
			PolicyKind: iamv3.PolicyBinding_PRINCIPAL_ACCESS_BOUNDARY, Policy: policy.GetName(),
			Condition: &expr.Expr{Expression: "principal.subject == '" + p.email + "'"}}})
	if err != nil {
		t.Fatalf("CreatePolicyBinding: %v", err)
	}
	binding, err := bop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreatePolicyBinding: %v", err)
	}

	base := "http://" + strings.TrimPrefix(rmEndpoint, "http://") + "/"
	beta, err := ptb.NewService(ctx, option.WithEndpoint(base), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	troubleshoot := func(full string) *ptb.GoogleCloudPolicytroubleshooterIamV3betaTroubleshootIamPolicyResponse {
		t.Helper()
		resp, err := beta.Iam.Troubleshoot(&ptb.GoogleCloudPolicytroubleshooterIamV3betaTroubleshootIamPolicyRequest{
			AccessTuple: &ptb.GoogleCloudPolicytroubleshooterIamV3betaAccessTuple{Principal: p.email, FullResourceName: full,
				Permission: "resourcemanager.projects.get"}}).Context(ctx).Do()
		if err != nil {
			t.Fatalf("v3beta iam:troubleshoot %s: %v", full, err)
		}
		return resp
	}
	explained := func(what string, pab *ptb.GoogleCloudPolicytroubleshooterIamV3betaPABPolicyExplanation, state, inclusion string) {
		t.Helper()
		if pab == nil || pab.PrincipalAccessBoundaryAccessState != state || pab.Relevance != "HEURISTIC_RELEVANCE_HIGH" || len(pab.ExplainedBindingsAndPolicies) != 1 {
			t.Fatalf("%s: pabPolicyExplanation = %+v; want %s, HIGH, one binding", what, pab, state)
		}
		e := pab.ExplainedBindingsAndPolicies[0]
		if e.BindingAndPolicyAccessState != state || e.Relevance != "HEURISTIC_RELEVANCE_HIGH" {
			t.Errorf("%s: binding and policy = %s / %s; want %s / HIGH", what, e.BindingAndPolicyAccessState, e.Relevance, state)
		}
		b := e.ExplainedPolicyBinding
		if b == nil || b.PolicyBinding == nil || b.PolicyBinding.Name != binding.GetName() || b.PolicyBinding.Policy != policy.GetName() ||
			b.PolicyBindingState != "POLICY_BINDING_STATE_ENFORCED" || b.ConditionExplanation == nil || b.ConditionExplanation.Value != true {
			t.Errorf("%s: explainedPolicyBinding = %+v; want %s, enforced, its condition true", what, b, binding.GetName())
		}
		ep := e.ExplainedPolicy
		if ep == nil || ep.Policy == nil || ep.Policy.Name != policy.GetName() || ep.PolicyAccessState != state ||
			ep.PolicyVersion == nil || ep.PolicyVersion.Version != 4 || ep.PolicyVersion.EnforcementState != "PAB_POLICY_ENFORCEMENT_STATE_ENFORCED" {
			t.Fatalf("%s: explainedPolicy = %+v; want %s at version 4, enforced", what, ep, policy.GetName())
		}
		if len(ep.ExplainedRules) != 1 || len(ep.ExplainedRules[0].ExplainedResources) != 1 ||
			ep.ExplainedRules[0].ExplainedResources[0].Resource != homeFull ||
			ep.ExplainedRules[0].ExplainedResources[0].ResourceInclusionState != inclusion ||
			ep.ExplainedRules[0].RuleAccessState != state || ep.ExplainedRules[0].Effect != "ALLOW" {
			t.Errorf("%s: explainedRules = %+v; want %s %s", what, ep.ExplainedRules, homeFull, inclusion)
		}
	}

	// Outside the boundary: the allow policy grants, the PAB policy blocks.
	resp := troubleshoot(otherFull)
	if resp.OverallAccessState != "CANNOT_ACCESS" || resp.AllowPolicyExplanation == nil || resp.AllowPolicyExplanation.AllowAccessState != "ALLOW_ACCESS_STATE_GRANTED" {
		t.Errorf("other project: overall %s, allow %+v; want CANNOT_ACCESS with the allow policy granting", resp.OverallAccessState, resp.AllowPolicyExplanation)
	}
	explained("other project", resp.PabPolicyExplanation, "PAB_ACCESS_STATE_NOT_ALLOWED", "RESOURCE_INCLUSION_STATE_NOT_INCLUDED")

	// Inside it: the PAB policy is relevant and allows.
	resp = troubleshoot(homeFull)
	if resp.OverallAccessState != "CAN_ACCESS" {
		t.Errorf("home project: overall %s; want CAN_ACCESS", resp.OverallAccessState)
	}
	explained("home project", resp.PabPolicyExplanation, "PAB_ACCESS_STATE_ALLOWED", "RESOURCE_INCLUSION_STATE_INCLUDED")

	// The official v3 client gets the same overall states.
	v3, err := ptiam.NewPolicyTroubleshooterRESTClient(ctx, option.WithEndpoint(base), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v3.Close() })
	for full, want := range map[string]ptiampb.TroubleshootIamPolicyResponse_OverallAccessState{
		otherFull: ptiampb.TroubleshootIamPolicyResponse_CANNOT_ACCESS, homeFull: ptiampb.TroubleshootIamPolicyResponse_CAN_ACCESS} {
		got, err := v3.TroubleshootIamPolicy(ctx, &ptiampb.TroubleshootIamPolicyRequest{AccessTuple: &ptiampb.AccessTuple{
			Principal: p.email, FullResourceName: full, Permission: "resourcemanager.projects.get"}})
		if err != nil || got.GetOverallAccessState() != want {
			t.Errorf("v3 TroubleshootIamPolicy %s = %v, %v; want %s", full, got.GetOverallAccessState(), err, want)
		}
	}

	// A forced delete removes the policy's bindings too.
	if _, err := pc.DeletePrincipalAccessBoundaryPolicy(ctx, &iamv3.DeletePrincipalAccessBoundaryPolicyRequest{Name: policy.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeletePrincipalAccessBoundaryPolicy while bound = %v; want FAILED_PRECONDITION", err)
	}
	dop, err := pc.DeletePrincipalAccessBoundaryPolicy(ctx, &iamv3.DeletePrincipalAccessBoundaryPolicyRequest{Name: policy.GetName(), Force: true})
	if err == nil {
		err = dop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("DeletePrincipalAccessBoundaryPolicy force: %v", err)
	}
	if _, err := bc.GetPolicyBinding(ctx, &iamv3.GetPolicyBindingRequest{Name: binding.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetPolicyBinding after the forced delete = %v; want NOT_FOUND", err)
	}
	if resp := troubleshoot(otherFull); resp.OverallAccessState != "CAN_ACCESS" ||
		resp.PabPolicyExplanation == nil || resp.PabPolicyExplanation.PrincipalAccessBoundaryAccessState != "PAB_ACCESS_STATE_NOT_ENFORCED" {
		t.Errorf("other project with no boundary: %s, %+v; want CAN_ACCESS, NOT_ENFORCED", resp.OverallAccessState, resp.PabPolicyExplanation)
	}
}
