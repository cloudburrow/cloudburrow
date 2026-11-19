package main

// The Organization Policies page's Edit policy (#1889), as Google's console
// lays it out (https://docs.cloud.google.com/resource-manager/docs/
// organization-policy/creating-managing-policies): Manage policy on a
// constraint opens Edit policy, whose Policy source is Inherit parent's
// policy, Google-managed default (spec.reset) or Override parent's policy;
// a list constraint's Policy enforcement is Merge with parent
// (inheritFromParent) or Replace; Add a rule adds a rule, Enforcement On or
// Off for a boolean constraint, Policy values Allow all, Deny all or Custom
// (Policy type Allow or Deny, the custom values) for a list one, each with
// an optional condition (a CEL expression over resource tags, such as
// resource.matchTag('123456789012/env', 'prod')); Set policy saves it. The
// same form edits the dry-run policy (dryRunSpec), as Google's Edit dry run
// policy does. Every call is one of the Org Policy v2 registry methods
// orgpolicy.googleapis.com serves (internal/service/resourcemanager/
// orgpolicyv2.go), so what the page saves is what the orgpolicy/apiv2
// client and gcloud org-policies read back, and what the enforced
// constraints act on.
//
//	policy-get  {constraint, spec}: the constraint, the project's policy
//	            as the form shows it, and the effective policy
//	policy-set  {constraint, spec, etag, source, merge, rules}

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The Policy source choices.
const (
	orgPolicySourceInherit  = "inherit"
	orgPolicySourceDefault  = "default"
	orgPolicySourceOverride = "override"
)

type orgPolicyEditRule struct {
	// Enforce is a boolean constraint's Enforcement.
	Enforce bool `json:"enforce"`
	// Values is a list constraint's Policy values: allowAll, denyAll or
	// custom; Type is custom's Policy type, allow or deny.
	Values string   `json:"values,omitempty"`
	Type   string   `json:"type,omitempty"`
	Custom []string `json:"custom,omitempty"`
	// Condition is the rule's condition, nil for none.
	Condition *denyCondition `json:"condition,omitempty"`
}

type orgPolicyEditSpec struct {
	Source string              `json:"source"`
	Merge  bool                `json:"merge"`
	Rules  []orgPolicyEditRule `json:"rules"`
}

type orgPolicyEditView struct {
	Constraint  string            `json:"constraint"`
	DisplayName string            `json:"displayName"`
	Description string            `json:"description"`
	Boolean     bool              `json:"boolean"`
	DryRun      bool              `json:"supportsDryRun"`
	Resource    string            `json:"resource"`
	Etag        string            `json:"etag"`
	Spec        orgPolicyEditSpec `json:"spec"`
	DryRunSpec  orgPolicyEditSpec `json:"dryRunSpec"`
	Effective   orgPolicyEditSpec `json:"effective"`
	Note        string            `json:"note"`
}

type orgPolicyEditChange struct {
	Constraint string `json:"constraint"`
	// DryRun saves the dry-run policy rather than the live one.
	DryRun bool   `json:"dryRun"`
	Etag   string `json:"etag"`
	orgPolicyEditSpec
}

// shortConstraint is constraints/x or x as x.
func shortConstraint(c string) string {
	return strings.TrimPrefix(strings.TrimSpace(c), "constraints/")
}

func editSpecOf(s *orgpolicypb.PolicySpec) orgPolicyEditSpec {
	out := orgPolicyEditSpec{Source: orgPolicySourceInherit, Rules: []orgPolicyEditRule{}}
	if s == nil {
		return out
	}
	if s.GetReset_() {
		out.Source = orgPolicySourceDefault
		return out
	}
	out.Source, out.Merge = orgPolicySourceOverride, s.GetInheritFromParent()
	for _, r := range s.GetRules() {
		e := orgPolicyEditRule{Enforce: r.GetEnforce()}
		switch {
		case r.GetAllowAll():
			e.Values = "allowAll"
		case r.GetDenyAll():
			e.Values = "denyAll"
		case r.GetValues() != nil:
			e.Values = "custom"
			if v := r.GetValues(); len(v.GetDeniedValues()) > 0 {
				e.Type, e.Custom = "deny", v.GetDeniedValues()
			} else {
				e.Type, e.Custom = "allow", v.GetAllowedValues()
			}
		}
		if c := r.GetCondition(); c != nil && c.GetExpression() != "" {
			e.Condition = &denyCondition{Title: c.GetTitle(), Description: c.GetDescription(), Expression: c.GetExpression()}
		}
		out.Rules = append(out.Rules, e)
	}
	return out
}

// specOf is the form's spec as the API takes it; nil for Inherit parent's
// policy.
func specOf(e orgPolicyEditSpec, boolean bool) (*orgpolicypb.PolicySpec, error) {
	switch e.Source {
	case orgPolicySourceInherit, "":
		return nil, nil
	case orgPolicySourceDefault:
		return &orgpolicypb.PolicySpec{Reset_: true}, nil
	case orgPolicySourceOverride:
	default:
		return nil, fmt.Errorf("policy source %q: choose inherit, default or override", e.Source)
	}
	if len(e.Rules) == 0 {
		return nil, errors.New("add a rule")
	}
	s := &orgpolicypb.PolicySpec{InheritFromParent: e.Merge && !boolean}
	for i, r := range e.Rules {
		rule := &orgpolicypb.PolicySpec_PolicyRule{}
		if boolean {
			rule.Kind = &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: r.Enforce}
		} else {
			switch r.Values {
			case "allowAll":
				rule.Kind = &orgpolicypb.PolicySpec_PolicyRule_AllowAll{AllowAll: true}
			case "denyAll":
				rule.Kind = &orgpolicypb.PolicySpec_PolicyRule_DenyAll{DenyAll: true}
			case "custom":
				vals := trimmed(r.Custom)
				if len(vals) == 0 {
					return nil, fmt.Errorf("rule %d: add a custom value", i+1)
				}
				sv := &orgpolicypb.PolicySpec_PolicyRule_StringValues{}
				if r.Type == "deny" {
					sv.DeniedValues = vals
				} else {
					sv.AllowedValues = vals
				}
				rule.Kind = &orgpolicypb.PolicySpec_PolicyRule_Values{Values: sv}
			default:
				return nil, fmt.Errorf("rule %d: choose Allow all, Deny all or Custom", i+1)
			}
		}
		if c := r.Condition; c != nil && strings.TrimSpace(c.Expression) != "" {
			rule.Condition = &expr.Expr{Title: strings.TrimSpace(c.Title), Description: strings.TrimSpace(c.Description), Expression: strings.TrimSpace(c.Expression)}
		}
		s.Rules = append(s.Rules, rule)
	}
	return s, nil
}

func orgPolicyNotFound(err error) bool {
	return status.Code(err) == codes.NotFound || strings.Contains(strings.ToLower(err.Error()), "not found")
}

// constraintV2 is constraint c as the project's ListConstraints lists it.
func (p iamOrgPoliciesProvider) constraintV2(resource, c string) (*orgpolicypb.Constraint, error) {
	all, err := p.registry.ConstraintsV2(resource)
	if err != nil {
		return nil, err
	}
	for _, x := range all {
		if shortConstraint(x.GetName()[strings.LastIndex(x.GetName(), "/")+1:]) == c {
			return x, nil
		}
	}
	return nil, fmt.Errorf("no constraint constraints/%s", c)
}

func (p iamOrgPoliciesProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	if strings.HasPrefix(call, "custom-") {
		return p.customCall(ctx, call, body) // consoleorgpolicycustom.go
	}
	if project == "" {
		return nil, errors.New("choose a project in the toolbar")
	}
	var c orgPolicyEditChange
	if len(body) > 0 {
		if err := json.Unmarshal(body, &c); err != nil {
			return nil, fmt.Errorf("malformed request: %v", err)
		}
	}
	resource := "projects/" + project
	short := shortConstraint(c.Constraint)
	if short == "" {
		return nil, errors.New("name a constraint")
	}
	con, err := p.constraintV2(resource, short)
	if err != nil {
		return nil, err
	}
	boolean := con.GetBooleanConstraint() != nil
	name := resource + "/policies/" + short
	switch call {
	case "policy-get":
		out := orgPolicyEditView{Constraint: "constraints/" + short, DisplayName: con.GetDisplayName(), Description: con.GetDescription(),
			Boolean: boolean, DryRun: con.GetSupportsDryRun(), Resource: resource, Note: orgPolicyNote,
			Spec: editSpecOf(nil), DryRunSpec: editSpecOf(nil), Effective: editSpecOf(nil)}
		pol, err := p.registry.PolicyV2(name)
		switch {
		case err == nil:
			out.Etag, out.Spec, out.DryRunSpec = pol.GetEtag(), editSpecOf(pol.GetSpec()), editSpecOf(pol.GetDryRunSpec())
		case !orgPolicyNotFound(err):
			return nil, err
		}
		if eff, err := p.registry.EffectivePolicyV2(name); err == nil {
			out.Effective = editSpecOf(eff.GetSpec())
		}
		return out, nil
	case "policy-set":
		spec, err := specOf(c.orgPolicyEditSpec, boolean)
		if err != nil {
			return nil, err
		}
		cur, err := p.registry.PolicyV2(name)
		if err != nil && !orgPolicyNotFound(err) {
			return nil, err
		}
		next := &orgpolicypb.Policy{Name: name, Etag: c.Etag}
		if cur != nil {
			next.Spec, next.DryRunSpec = cur.GetSpec(), cur.GetDryRunSpec()
		}
		if c.DryRun {
			next.DryRunSpec = spec
		} else {
			next.Spec = spec
		}
		if next.Spec == nil && next.DryRunSpec == nil {
			if cur == nil {
				return map[string]string{"applied": "inherit"}, nil
			}
			if err := p.registry.DeletePolicyV2(name, c.Etag); err != nil {
				return nil, err
			}
			return map[string]string{"applied": "inherit"}, nil
		}
		if _, err := p.registry.SetPolicyV2(next, true); err != nil {
			return nil, err
		}
		return map[string]string{"applied": "set"}, nil
	}
	return nil, fmt.Errorf("no such call %q", call)
}
