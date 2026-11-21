package main

import (
	"encoding/json"
	"testing"

	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
)

// TestPABExplanation (#2057): v3beta's pabPolicyExplanation of a blocked
// check names the binding and the policy, with the rule's resource not
// included; an allowed one is relevant; a false condition is not enforced.
func TestPABExplanation(t *testing.T) {
	res := "//cloudresourcemanager.googleapis.com/projects/home"
	pol := &iamv3.PrincipalAccessBoundaryPolicy{Name: "organizations/1/locations/global/principalAccessBoundaryPolicies/p",
		Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{{
			Effect: iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW, Resources: []string{res}}}}}
	b := &iamv3.PolicyBinding{Name: "projects/1/locations/global/policyBindings/b", Policy: pol.Name,
		Condition: &expr.Expr{Expression: "principal.type == 'iam.googleapis.com/ServiceAccount'"}}
	result := func(state, cond string, included bool) iameval.BoundaryResult {
		return iameval.BoundaryResult{Binding: b, Policy: pol, State: state, Condition: cond, Blockable: true, Includes: map[string]bool{res: included}}
	}

	got := ptPAB(iameval.Decision{Blocked: true, Boundary: []iameval.BoundaryResult{result(iameval.PABNotAllowed, "true", false)}})
	if got.PrincipalAccessBoundaryAccessState != iameval.PABNotAllowed || got.Relevance != "HEURISTIC_RELEVANCE_HIGH" || len(got.ExplainedBindingsAndPolicies) != 1 {
		t.Fatalf("blocked = %+v", got)
	}
	e := got.ExplainedBindingsAndPolicies[0]
	if e.ExplainedPolicyBinding.PolicyBinding.Name != b.Name || e.ExplainedPolicy.Policy.Name != pol.Name ||
		e.ExplainedPolicyBinding.ConditionExplanation.Value != true || e.ExplainedPolicy.PolicyVersion.Version != 4 ||
		e.ExplainedPolicy.ExplainedRules[0].ExplainedResources[0].ResourceInclusionState != "RESOURCE_INCLUSION_STATE_NOT_INCLUDED" ||
		e.ExplainedPolicy.ExplainedRules[0].RuleAccessState != iameval.PABNotAllowed || e.Relevance != "HEURISTIC_RELEVANCE_HIGH" {
		t.Errorf("blocked entry = %+v / %+v / %+v", e, e.ExplainedPolicyBinding, e.ExplainedPolicy)
	}

	got = ptPAB(iameval.Decision{Boundary: []iameval.BoundaryResult{result(iameval.PABAllowed, "", true)}})
	if got.PrincipalAccessBoundaryAccessState != iameval.PABAllowed || got.Relevance != "HEURISTIC_RELEVANCE_HIGH" ||
		got.ExplainedBindingsAndPolicies[0].ExplainedPolicy.ExplainedRules[0].CombinedResourceInclusionState != "RESOURCE_INCLUSION_STATE_INCLUDED" {
		t.Errorf("allowed = %+v", got)
	}

	got = ptPAB(iameval.Decision{Boundary: []iameval.BoundaryResult{result(iameval.PABNotEnforced, "false", false)}})
	e = got.ExplainedBindingsAndPolicies[0]
	if got.PrincipalAccessBoundaryAccessState != iameval.PABNotEnforced || got.Relevance != "HEURISTIC_RELEVANCE_NORMAL" ||
		e.ExplainedPolicyBinding.PolicyBindingState != "POLICY_BINDING_STATE_NOT_ENFORCED" || e.ExplainedPolicy.ExplainedRules[0].RuleAccessState != iameval.PABNotEnforced {
		t.Errorf("not enforced = %+v", got)
	}

	// The v3 fields stay as they are next to it.
	var m map[string]any
	raw, _ := json.Marshal(withPAB(map[string]string{"overallAccessState": "CANNOT_ACCESS"}, got))
	if err := json.Unmarshal(raw, &m); err != nil || m["overallAccessState"] != "CANNOT_ACCESS" || m["pabPolicyExplanation"] == nil {
		t.Errorf("withPAB = %s, %v", raw, err)
	}
}
