package main

import (
	"encoding/json"

	ptb "google.golang.org/api/policytroubleshooter/v3beta"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
)

// Policy Troubleshooter v3beta's pabPolicyExplanation (#2057), from the
// PAB policies internal/iameval evaluated for the check, in the shape of
// Google's v3beta reference
// (https://docs.cloud.google.com/policy-intelligence/docs/reference/policytroubleshooter/rest/v3beta/iam/troubleshoot,
// read 2026-11-20):
//
//   - principalAccessBoundaryAccessState is PAB_ACCESS_STATE_NOT_ALLOWED
//     when the boundary blocks the permission, PAB_ACCESS_STATE_ALLOWED when
//     a policy that applies makes the resource eligible, and
//     PAB_ACCESS_STATE_NOT_ENFORCED otherwise (no policy bound, the
//     permission outside every bound policy's enforcement version, or every
//     binding's condition false);
//   - explainedBindingsAndPolicies holds one entry per binding of a policy
//     bound to a principal set the principal is in: the binding
//     (policyBindingState ENFORCED unless its condition is false, with the
//     condition's value), and the policy with its version
//     (PAB_POLICY_ENFORCEMENT_STATE_ENFORCED when the version covers the
//     permission), its rules and each rule resource's
//     RESOURCE_INCLUSION_STATE (the resource, or one of its ancestors);
//   - a state that decides the overall state is HEURISTIC_RELEVANCE_HIGH,
//     anything else NORMAL.
//
// The local caller can read every policy, so explainedPolicy is never
// omitted and no state is UNKNOWN_INFO.

// withPAB is out with v3beta's pabPolicyExplanation added. v3's and
// v3beta's other fields are the same JSON.
func withPAB(out any, pab *ptb.GoogleCloudPolicytroubleshooterIamV3betaPABPolicyExplanation) any {
	b, err := json.Marshal(out)
	if err != nil {
		return out
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		return out
	}
	if p, err := json.Marshal(pab); err == nil {
		m["pabPolicyExplanation"] = p
	}
	return m
}

// ptPAB explains d's principal access boundary policies.
func ptPAB(d iameval.Decision) *ptb.GoogleCloudPolicytroubleshooterIamV3betaPABPolicyExplanation {
	overall := iameval.PABNotEnforced
	switch {
	case d.Blocked:
		overall = iameval.PABNotAllowed
	default:
		for _, r := range d.Boundary {
			if r.State == iameval.PABAllowed {
				overall = iameval.PABAllowed
			}
		}
	}
	out := &ptb.GoogleCloudPolicytroubleshooterIamV3betaPABPolicyExplanation{
		PrincipalAccessBoundaryAccessState: overall, Relevance: relevance(overall != iameval.PABNotEnforced)}
	for _, r := range d.Boundary {
		decides := r.State != iameval.PABNotEnforced && r.State == overall
		e := &ptb.GoogleCloudPolicytroubleshooterIamV3betaExplainedPABBindingAndPolicy{
			BindingAndPolicyAccessState: r.State, Relevance: relevance(decides),
			ExplainedPolicyBinding: ptPABBinding(r, decides),
			ExplainedPolicy:        ptPABPolicy(r, decides)}
		out.ExplainedBindingsAndPolicies = append(out.ExplainedBindingsAndPolicies, e)
	}
	return out
}

func ptPABBinding(r iameval.BoundaryResult, decides bool) *ptb.GoogleCloudPolicytroubleshooterIamV3betaExplainedPolicyBinding {
	out := &ptb.GoogleCloudPolicytroubleshooterIamV3betaExplainedPolicyBinding{
		PolicyBindingState: "POLICY_BINDING_STATE_ENFORCED", Relevance: relevance(decides)}
	var pb ptb.GoogleIamV3PolicyBinding
	if fromProto(r.Binding, &pb) {
		out.PolicyBinding = &pb
	}
	switch r.Condition {
	case "true":
		out.ConditionExplanation = &ptb.GoogleCloudPolicytroubleshooterIamV3betaConditionExplanation{Value: true}
	case "false":
		out.PolicyBindingState, out.Relevance = "POLICY_BINDING_STATE_NOT_ENFORCED", relevance(false)
		out.ConditionExplanation = &ptb.GoogleCloudPolicytroubleshooterIamV3betaConditionExplanation{Value: false}
	case "error":
		// A condition that cannot be evaluated enforces the policy (PAB
		// policies fail closed).
		out.ConditionExplanation = &ptb.GoogleCloudPolicytroubleshooterIamV3betaConditionExplanation{
			Errors: []*ptb.GoogleRpcStatus{{Code: 3, Message: "the condition cannot be evaluated"}}}
	}
	return out
}

func ptPABPolicy(r iameval.BoundaryResult, decides bool) *ptb.GoogleCloudPolicytroubleshooterIamV3betaExplainedPABPolicy {
	version, _ := iameval.EnforcementVersion(r.Policy.GetDetails().GetEnforcementVersion())
	out := &ptb.GoogleCloudPolicytroubleshooterIamV3betaExplainedPABPolicy{PolicyAccessState: r.State, Relevance: relevance(decides),
		PolicyVersion: &ptb.GoogleCloudPolicytroubleshooterIamV3betaExplainedPABPolicyVersion{
			Version: int64(version), EnforcementState: "PAB_POLICY_ENFORCEMENT_STATE_NOT_ENFORCED"}}
	if r.Blockable {
		out.PolicyVersion.EnforcementState = "PAB_POLICY_ENFORCEMENT_STATE_ENFORCED"
	}
	var p ptb.GoogleIamV3PrincipalAccessBoundaryPolicy
	if fromProto(r.Policy, &p) {
		out.Policy = &p
	}
	enforced := r.State != iameval.PABNotEnforced
	for _, rule := range r.Policy.GetDetails().GetRules() {
		er := &ptb.GoogleCloudPolicytroubleshooterIamV3betaExplainedPABRule{Effect: rule.GetEffect().String(),
			CombinedResourceInclusionState: "RESOURCE_INCLUSION_STATE_NOT_INCLUDED", RuleAccessState: iameval.PABNotEnforced}
		included := false
		for _, res := range rule.GetResources() {
			st := "RESOURCE_INCLUSION_STATE_NOT_INCLUDED"
			if r.Includes[res] {
				st, included = "RESOURCE_INCLUSION_STATE_INCLUDED", true
			}
			er.ExplainedResources = append(er.ExplainedResources, &ptb.GoogleCloudPolicytroubleshooterIamV3betaExplainedPABRuleExplainedResource{
				Resource: res, ResourceInclusionState: st, Relevance: relevance(enforced && r.Includes[res])})
		}
		if included {
			er.CombinedResourceInclusionState = "RESOURCE_INCLUSION_STATE_INCLUDED"
		}
		if enforced {
			er.RuleAccessState = iameval.PABNotAllowed
			if included {
				er.RuleAccessState = iameval.PABAllowed
			}
		}
		er.CombinedResourceRelevance = relevance(enforced && decides)
		er.Relevance = relevance(enforced && decides && (included || r.State == iameval.PABNotAllowed))
		out.ExplainedRules = append(out.ExplainedRules, er)
	}
	return out
}

// fromProto copies a google.iam.v3 message into its REST type.
func fromProto(m proto.Message, into any) bool {
	b, err := protojson.Marshal(m)
	return err == nil && json.Unmarshal(b, into) == nil
}
