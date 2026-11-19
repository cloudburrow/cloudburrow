// Package orgpolicy evaluates organization policies (#1890) where the
// services that act on them ask: a constraint's effective policy on a
// project, folder or the organization, evaluated down the hierarchy as
// "Understanding hierarchy evaluation" describes it
// (https://docs.cloud.google.com/organization-policy/understand-hierarchy-evaluation),
// and the checks each enforced constraint makes.
//
// Organization policies are not IAM: they are enforced whether or not
// `up --iam-enforcement` is on, as on Google Cloud. A resource with no
// policy set on it or above it (every test that sets none) is unaffected:
// every constraint CloudBurrow enforces defaults to not enforced, or to
// all values allowed.
package orgpolicy

import (
	"strings"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

// Merge is the effective spec of a resource whose own spec is child, below
// a resource whose effective spec is parent (nil: the constraint's
// default). list is whether the constraint is a list constraint.
//
//   - no own spec, or one with neither rules nor reset, inherits parent;
//   - reset restores the constraint's default (nil), whatever is above;
//   - inheritFromParent on a list constraint merges the rules: an
//     allowAll or denyAll of the child's own replaces what is above; an
//     allowAll above keeps only the child's denied values beside it; a
//     denyAll above gives way to the child's allowed values; otherwise
//     the values of both apply (and a denied value wins, at evaluation);
//   - anything else replaces parent.
func Merge(parent, child *orgpolicypb.PolicySpec, list bool) *orgpolicypb.PolicySpec {
	if child == nil {
		return parent
	}
	if child.GetReset_() {
		return nil
	}
	if len(child.GetRules()) == 0 {
		return parent
	}
	if !list || !child.GetInheritFromParent() || parent == nil {
		return child
	}
	for _, r := range child.GetRules() {
		if r.GetAllowAll() || r.GetDenyAll() {
			return child
		}
	}
	out := &orgpolicypb.PolicySpec{InheritFromParent: true, UpdateTime: child.GetUpdateTime(), Etag: child.GetEtag()}
	for _, r := range parent.GetRules() {
		switch {
		case r.GetAllowAll():
			c := proto.Clone(r).(*orgpolicypb.PolicySpec_PolicyRule)
			out.Rules = append(out.Rules, c)
		case r.GetDenyAll():
			// The child's allowed values replace a denyAll above.
		default:
			out.Rules = append(out.Rules, proto.Clone(r).(*orgpolicypb.PolicySpec_PolicyRule))
		}
	}
	for _, r := range child.GetRules() {
		out.Rules = append(out.Rules, proto.Clone(r).(*orgpolicypb.PolicySpec_PolicyRule))
	}
	return out
}

// applies reports whether rule r applies to a resource with these tags:
// it has no condition, or its condition is true. Conditions read the
// resource's tags (resource.matchTag, resource.matchTagId,
// resource.hasTagKey...), as Google documents for organization policy
// conditions.
func applies(r *orgpolicypb.PolicySpec_PolicyRule, tags []iamcond.Tag) bool {
	expr := strings.TrimSpace(r.GetCondition().GetExpression())
	if expr == "" {
		return true
	}
	return iamcond.Eval(expr, iamcond.Attributes{Tags: tags, TagsKnown: true})
}

// BoolEnforced is a boolean constraint's verdict under spec: a
// conditional rule whose condition holds decides first, then the
// unconditional rule; no rule is the default, not enforced.
func BoolEnforced(spec *orgpolicypb.PolicySpec, tags []iamcond.Tag) bool {
	for _, r := range spec.GetRules() {
		if r.GetCondition().GetExpression() != "" && applies(r, tags) {
			return r.GetEnforce()
		}
	}
	for _, r := range spec.GetRules() {
		if r.GetCondition().GetExpression() == "" {
			return r.GetEnforce()
		}
	}
	return false
}

// ListAllows is a list constraint's verdict on value under spec, match
// deciding whether a policy value covers it: of the rules that apply, a
// denyAll or a matching denied value refuses it, then an allowAll or a
// matching allowed value allows it; a value no rule names is refused
// when some rule lists allowed values, else allowed (the default).
func ListAllows(spec *orgpolicypb.PolicySpec, tags []iamcond.Tag, value string, match func(policyValue, value string) bool) bool {
	var rules []*orgpolicypb.PolicySpec_PolicyRule
	for _, r := range spec.GetRules() {
		if applies(r, tags) {
			rules = append(rules, r)
		}
	}
	for _, r := range rules {
		if r.GetDenyAll() {
			return false
		}
		for _, d := range r.GetValues().GetDeniedValues() {
			if match(d, value) {
				return false
			}
		}
	}
	restricts := false
	for _, r := range rules {
		if r.GetAllowAll() {
			return true
		}
		for _, a := range r.GetValues().GetAllowedValues() {
			if match(a, value) {
				return true
			}
		}
		if len(r.GetValues().GetAllowedValues()) > 0 {
			restricts = true
		}
	}
	return !restricts
}
