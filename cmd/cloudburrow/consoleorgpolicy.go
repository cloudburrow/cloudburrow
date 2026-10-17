package main

import (
	"sort"
	"strings"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// Organization policies on a project, a folder and the organization (#1296,
// #1534), as Google's console's Organization policies page shows them for
// whichever resource is selected: each constraint set on the resource or
// above it, the resource's own policy, and the effective one
// getEffectiveOrgPolicy evaluates. Over the registry the v1 methods use.

const (
	orgPolicySet   = "set-org-policy"
	orgPolicyClear = "clear-org-policy"
)

const orgPolicyNote = "Stored, never enforced, as IAM policies are. Effective is the policy evaluated from the organization down " +
	"through the folders: a policy below replaces the one above, unless it is a list policy that inherits from its parent."

// orgPolicyListing is resource's Organization policies listing.
func orgPolicyListing(reg *resourcemanager.Registry, resource string) console.Listing {
	l := console.Listing{Columns: []string{"Policy", "Effective", "Updated"}, NameColumn: "Constraint", Noun: "policies", Note: orgPolicyNote}
	chain, err := reg.OrgPolicyChain(resource)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l
	}
	own := map[string]*crmv1.OrgPolicy{}
	set := map[string]bool{}
	for _, res := range chain {
		all, err := reg.OrgPolicies(res)
		if err != nil {
			l.Unavailable = consoleErr(err)
			return l
		}
		for _, op := range all {
			set[op.Constraint] = true
			if res == chain[len(chain)-1] {
				own[op.Constraint] = op
			}
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		policy, updated := "Inherited", ""
		if op := own[n]; op != nil {
			policy, updated = orgPolicyText(op), op.UpdateTime
		}
		effective := "-"
		if eff, err := reg.EffectiveOrgPolicy(resource, n); err == nil {
			effective = orgPolicyText(eff)
		}
		l.Items = append(l.Items, console.Resource{Name: n, Fields: map[string]string{
			"Policy": policy, "Effective": effective, "Updated": updated}})
	}
	l.Total = len(l.Items)
	return l
}

// orgPolicyActions are Set organization policy and, when resource has a
// policy of its own, Clear organization policy.
func orgPolicyActions(reg *resourcemanager.Registry, resource, on string) []console.Action {
	names := make([]string, 0, len(resourcemanager.Constraints))
	for _, c := range resourcemanager.Constraints {
		names = append(names, c.Name)
	}
	out := []console.Action{{ID: orgPolicySet, Label: "Set organization policy" + on, Fields: []console.Field{
		{Name: "constraint", Label: "Constraint", Type: "select", Required: true, Options: names, Default: names[0]},
		{Name: "enforced", Label: "Enforce (boolean constraints)", Type: "checkbox"},
		{Name: "allowed", Label: "Allowed values (list constraints)", Type: "text",
			Help: "Comma-separated, e.g. in:eu-locations. Leave empty with Deny all unset to allow all."},
		{Name: "denyAll", Label: "Deny all values (list constraints)", Type: "checkbox"},
		{Name: "inherit", Label: "Merge with parent (list constraints)", Type: "checkbox",
			Help: "inheritFromParent: add these values to the policy above instead of replacing it."},
	}}}
	if own, err := reg.OrgPolicies(resource); err == nil && len(own) > 0 {
		ids := make([]string, 0, len(own))
		for _, op := range own {
			ids = append(ids, op.Constraint)
		}
		out = append(out, console.Action{ID: orgPolicyClear, Label: "Clear organization policy" + on, Destructive: true,
			Fields: []console.Field{{Name: "constraint", Label: "Constraint", Type: "select", Required: true, Options: ids, Default: ids[0]}}})
	}
	return out
}

// orgPolicyAct performs an orgPolicyActions action on resource; ok is false
// for any other action.
func orgPolicyAct(reg *resourcemanager.Registry, resource, action string, values map[string]string) (ok bool, err error) {
	switch action {
	case orgPolicySet:
		op := &crmv1.OrgPolicy{Constraint: values["constraint"]}
		c := resourcemanager.KnownConstraint(op.Constraint)
		switch {
		case c != nil && c.BooleanConstraint != nil:
			op.BooleanPolicy = &crmv1.BooleanPolicy{Enforced: values["enforced"] == "true"}
		case values["denyAll"] == "true":
			op.ListPolicy = &crmv1.ListPolicy{AllValues: "DENY"}
		default:
			lp := &crmv1.ListPolicy{InheritFromParent: values["inherit"] == "true"}
			for _, v := range strings.Split(values["allowed"], ",") {
				if v = strings.TrimSpace(v); v != "" {
					lp.AllowedValues = append(lp.AllowedValues, v)
				}
			}
			if len(lp.AllowedValues) == 0 {
				lp.AllValues = "ALLOW"
			}
			op.ListPolicy = lp
		}
		_, err := reg.SetOrgPolicy(resource, op)
		return true, err
	case orgPolicyClear:
		return true, reg.ClearOrgPolicy(resource, values["constraint"], "")
	}
	return false, nil
}

func orgPolicyText(op *crmv1.OrgPolicy) string {
	switch {
	case op.BooleanPolicy != nil && op.BooleanPolicy.Enforced:
		return "Enforced"
	case op.BooleanPolicy != nil:
		return "Not enforced"
	case op.ListPolicy != nil && op.ListPolicy.AllValues != "":
		return op.ListPolicy.AllValues + " all"
	case op.ListPolicy != nil:
		parts := []string{}
		if len(op.ListPolicy.AllowedValues) > 0 {
			parts = append(parts, "allow "+strings.Join(op.ListPolicy.AllowedValues, ", "))
		}
		if len(op.ListPolicy.DeniedValues) > 0 {
			parts = append(parts, "deny "+strings.Join(op.ListPolicy.DeniedValues, ", "))
		}
		if op.ListPolicy.InheritFromParent {
			parts = append(parts, "merged with parent")
		}
		return strings.Join(parts, "; ")
	case op.RestoreDefault != nil:
		return "Restore default"
	}
	return "-"
}
