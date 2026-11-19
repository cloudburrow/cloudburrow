package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func orgPolicyEditCall(t *testing.T, p iamOrgPoliciesProvider, call string, body any) (any, error) {
	t.Helper()
	raw, _ := json.Marshal(body)
	return p.StudioCall(context.Background(), "iam-page", call, raw)
}

func orgPolicyEditRead(t *testing.T, p iamOrgPoliciesProvider, c string) orgPolicyEditView {
	t.Helper()
	out, err := orgPolicyEditCall(t, p, "policy-get", map[string]string{"constraint": c})
	if err != nil {
		t.Fatalf("policy-get %s: %v", c, err)
	}
	return out.(orgPolicyEditView)
}

// The Organization Policies page's Edit policy (#1889): Override parent's
// policy with Replace and a conditional custom Allow rule, read back by the
// form and through the Org Policy v2 registry; Merge with parent; the dry
// run's own spec; Google-managed default (reset); a boolean constraint's
// Enforcement; Inherit parent's policy deleting the policy; a rule without
// values refused.
func TestConsoleOrgPolicyEdit(t *testing.T) {
	reg := iamPageRegistry(t)
	p := iamOrgPoliciesProvider{registry: reg}
	const list, boolean = "constraints/gcp.resourceLocations", "constraints/compute.requireOsLogin"
	v := orgPolicyEditRead(t, p, list)
	if v.Boolean || v.Spec.Source != orgPolicySourceInherit || v.Etag != "" {
		t.Fatalf("an unset list constraint reads %+v", v)
	}
	rule := orgPolicyEditRule{Values: "custom", Type: "allow", Custom: []string{"in:us-locations", " europe-west1 "},
		Condition: &denyCondition{Title: "prod", Expression: "resource.matchTag('123456789012/env', 'prod')"}}
	set := func(c orgPolicyEditChange) error {
		_, err := orgPolicyEditCall(t, p, "policy-set", c)
		return err
	}
	if err := set(orgPolicyEditChange{Constraint: list, orgPolicyEditSpec: orgPolicyEditSpec{Source: "override", Rules: []orgPolicyEditRule{rule}}}); err != nil {
		t.Fatalf("Set policy: %v", err)
	}
	pol, err := reg.PolicyV2("projects/iam-page/policies/gcp.resourceLocations")
	if err != nil {
		t.Fatalf("PolicyV2: %v", err)
	}
	r := pol.GetSpec().GetRules()
	if len(r) != 1 || strings.Join(r[0].GetValues().GetAllowedValues(), ",") != "in:us-locations,europe-west1" || r[0].GetCondition().GetTitle() != "prod" {
		t.Fatalf("the stored spec is %v", pol.GetSpec())
	}
	v = orgPolicyEditRead(t, p, list)
	if v.Spec.Source != orgPolicySourceOverride || v.Spec.Merge || len(v.Spec.Rules) != 1 || v.Spec.Rules[0].Type != "allow" || v.Etag == "" {
		t.Fatalf("Edit policy reads back %+v", v.Spec)
	}
	if err := set(orgPolicyEditChange{Constraint: list, Etag: v.Etag, orgPolicyEditSpec: orgPolicyEditSpec{Source: "override", Merge: true, Rules: v.Spec.Rules}}); err != nil {
		t.Fatalf("Merge with parent: %v", err)
	}
	if pol, _ := reg.PolicyV2("projects/iam-page/policies/gcp.resourceLocations"); !pol.GetSpec().GetInheritFromParent() {
		t.Error("Merge with parent did not set inheritFromParent")
	}
	if err := set(orgPolicyEditChange{Constraint: list, Etag: v.Etag, orgPolicyEditSpec: orgPolicyEditSpec{Source: "default"}}); err == nil {
		t.Error("a stale etag was accepted")
	}
	if err := set(orgPolicyEditChange{Constraint: list, DryRun: true, orgPolicyEditSpec: orgPolicyEditSpec{Source: "override",
		Rules: []orgPolicyEditRule{{Values: "denyAll"}}}}); err != nil {
		t.Fatalf("Set dry run policy: %v", err)
	}
	v = orgPolicyEditRead(t, p, list)
	if !v.Spec.Merge || len(v.DryRunSpec.Rules) != 1 || v.DryRunSpec.Rules[0].Values != "denyAll" {
		t.Fatalf("after the dry run Edit policy reads %+v / %+v", v.Spec, v.DryRunSpec)
	}
	if err := set(orgPolicyEditChange{Constraint: list, orgPolicyEditSpec: orgPolicyEditSpec{Source: "default"}}); err != nil {
		t.Fatalf("Google-managed default: %v", err)
	}
	if pol, _ := reg.PolicyV2("projects/iam-page/policies/gcp.resourceLocations"); !pol.GetSpec().GetReset_() {
		t.Error("Google-managed default did not set reset")
	}
	if err := set(orgPolicyEditChange{Constraint: list, orgPolicyEditSpec: orgPolicyEditSpec{Source: "override", Rules: []orgPolicyEditRule{{Values: "custom"}}}}); err == nil {
		t.Error("a custom rule without values was accepted")
	}
	// Inherit parent's policy for both specs deletes the policy.
	if err := set(orgPolicyEditChange{Constraint: list, DryRun: true, orgPolicyEditSpec: orgPolicyEditSpec{Source: "inherit"}}); err != nil {
		t.Fatal(err)
	}
	if err := set(orgPolicyEditChange{Constraint: list, orgPolicyEditSpec: orgPolicyEditSpec{Source: "inherit"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.PolicyV2("projects/iam-page/policies/gcp.resourceLocations"); err == nil {
		t.Error("Inherit parent's policy left the policy")
	}
	// A boolean constraint.
	if v := orgPolicyEditRead(t, p, boolean); !v.Boolean {
		t.Fatalf("%s is not read as boolean", boolean)
	}
	if err := set(orgPolicyEditChange{Constraint: boolean, orgPolicyEditSpec: orgPolicyEditSpec{Source: "override", Merge: true, Rules: []orgPolicyEditRule{{Enforce: true}}}}); err != nil {
		t.Fatalf("Enforcement On: %v", err)
	}
	pol, err = reg.PolicyV2("projects/iam-page/policies/compute.requireOsLogin")
	if err != nil || pol.GetSpec().GetInheritFromParent() || !pol.GetSpec().GetRules()[0].GetEnforce() {
		t.Fatalf("the boolean policy is %v, %v", pol, err)
	}
}
