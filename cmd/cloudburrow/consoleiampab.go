package main

// IAM & Admin > Principal access boundary (#2057), as Google's console lays
// it out ("Create and apply principal access boundary policies",
// https://docs.cloud.google.com/iam/docs/principal-access-boundary-policies-create,
// read 2026-11-20): the organization's principal access boundary policies,
// listed by policy ID with their name, enforcement version and rules; Create
// policy (Policy name, Enforcement version, then a boundary rule's
// Description and Resources); a policy's page with its Details, its boundary
// rules (Add boundary rule, Delete rule) and its Bindings tab (Add binding:
// Display name, Binding ID, Target principal set, and an optional condition's
// Title, Description and Expression), Edit policy, Delete, and a binding's
// Edit and Delete. Each is one call of the registry IAM v3's servers use
// (internal/service/resourcemanager/pab.go), so what the page saves is what
// cloud.google.com/go/iam/apiv3 and gcloud iam principal-access-boundary-policies
// read back, with the API's validation. Policies are the organization's: the
// page shows the instance's organization whatever project the toolbar has.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type iamPABProvider struct {
	registry *resourcemanager.Registry
}

func (iamPABProvider) ID() string    { return "iam-principal-access-boundary" }
func (iamPABProvider) Title() string { return "Principal access boundary" }

const (
	pabParent      = resourcemanager.OrganizationName + "/locations/global"
	pabIDPattern   = `^[a-z][a-z0-9.\-]{2,62}$`
	pabBindingRow  = "b:"
	pabRuleRow     = "r:"
	pabVersionHelp = "Which permissions the policy can block. latest follows the default version (4), and might block new permissions as Google adds them."
	pabNote        = "Principal access boundary policies of the organization " + resourcemanager.OrganizationName + ". A principal bound to one or more of them " +
		"can only access the resources they list (and their descendants), for the permissions their enforcement versions cover; they never grant access."
)

var pabVersions = []string{"4", "3", "2", "1", "latest"}

func (p iamPABProvider) policyName(id string) string {
	return pabParent + "/principalAccessBoundaryPolicies/" + id
}

func pabRuleCount(pol *iamv3.PrincipalAccessBoundaryPolicy) string {
	return strconv.Itoa(len(pol.GetDetails().GetRules()))
}

func (p iamPABProvider) List(_ context.Context, _ string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Policy ID", Columns: []string{"Policy name", "Enforcement version", "Rules", "Last updated"}, Noun: "principal access boundary policies", Note: pabNote}
	list, _, err := p.registry.ListPABPolicies(pabParent, 1000, "")
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, pol := range list {
		updated := ""
		if t := pol.GetUpdateTime(); t != nil {
			updated = t.AsTime().UTC().Format("2006-01-02 15:04:05 UTC")
		}
		l.Items = append(l.Items, console.Resource{Name: lastSegment(pol.GetName()),
			Fields: map[string]string{"Policy name": pol.GetDisplayName(), "Enforcement version": pol.GetDetails().GetEnforcementVersion(),
				"Rules": pabRuleCount(pol), "Last updated": updated}})
	}
	l.Total = len(l.Items)
	return l, nil
}

func pabRuleFields(section string) []console.Field {
	return []console.Field{
		{Name: "ruleDescription", Label: "Description", Type: "text", Section: section, Help: "At most 256 characters."},
		{Name: "ruleResources", Label: "Resources", Type: "textarea", Section: section, Required: true,
			Help: "One per line: projects, folders or organizations, such as //cloudresourcemanager.googleapis.com/projects/PROJECT_ID. Each includes its descendants."},
	}
}

func pabRuleFromForm(v map[string]string) *iamv3.PrincipalAccessBoundaryPolicyRule {
	var res []string
	for _, line := range strings.FieldsFunc(v["ruleResources"], func(r rune) bool { return r == '\n' || r == ',' }) {
		if s := strings.TrimSpace(line); s != "" {
			res = append(res, s)
		}
	}
	return &iamv3.PrincipalAccessBoundaryPolicyRule{Description: strings.TrimSpace(v["ruleDescription"]), Resources: res,
		Effect: iamv3.PrincipalAccessBoundaryPolicyRule_ALLOW}
}

// CreateForm is Google's Create policy: a first boundary rule, the policy
// name and its enforcement version.
func (iamPABProvider) CreateForm() (string, []console.Field) {
	const rule = "Boundary rule"
	const pol = "Policy"
	fields := append(pabRuleFields(rule),
		console.Field{Name: "displayName", Label: "Policy name", Type: "text", Section: pol, Help: "At most 63 characters."},
		console.Field{Name: "policyId", Label: "Policy ID", Type: "text", Required: true, Pattern: pabIDPattern, Section: pol, DeriveFrom: "displayName",
			Help: "3 to 63 lowercase letters, digits, hyphens and dots, starting with a letter. It cannot be changed."},
		console.Field{Name: "enforcementVersion", Label: "Enforcement version", Type: "select", Section: pol, Options: pabVersions, Default: "4", Help: pabVersionHelp})
	return "Create policy", fields
}

func (iamPABProvider) CreateTitle() string { return "Create principal access boundary policy" }

func (iamPABProvider) CreateOnPage() bool { return true }

func (p iamPABProvider) Create(_ context.Context, _ string, v map[string]string) (string, error) {
	id := strings.TrimSpace(v["policyId"])
	_, err := p.registry.CreatePABPolicy(pabParent, id, &iamv3.PrincipalAccessBoundaryPolicy{DisplayName: strings.TrimSpace(v["displayName"]),
		Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{EnforcementVersion: v["enforcementVersion"],
			Rules: []*iamv3.PrincipalAccessBoundaryPolicyRule{pabRuleFromForm(v)}}}, false)
	return id, err
}

func pabBindingKey(name string) string { return pabBindingRow + strings.ReplaceAll(name, "/", "~") }

func pabBindingName(row string) (string, bool) {
	s, ok := strings.CutPrefix(row, pabBindingRow)
	return strings.ReplaceAll(s, "~", "/"), ok
}

func (p iamPABProvider) bindings(policy string) ([]*iamv3.PolicyBinding, error) {
	var out []*iamv3.PolicyBinding
	token := ""
	for {
		page, next, err := p.registry.SearchPABPolicyBindings(policy, 1000, token)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		token = next
	}
}

func (p iamPABProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	pol, err := p.registry.GetPABPolicy(p.policyName(path[0]))
	if err != nil {
		return console.Detail{Unavailable: "cannot read the policy: " + consoleErr(err)}, nil
	}
	if len(path) == 2 {
		if name, ok := pabBindingName(path[1]); ok {
			return p.bindingDetail(name)
		}
		return console.DeeperThan(1, path), nil
	}
	created, updated := "", ""
	if t := pol.GetCreateTime(); t != nil {
		created = t.AsTime().UTC().Format("2006-01-02 15:04:05 UTC")
	}
	if t := pol.GetUpdateTime(); t != nil {
		updated = t.AsTime().UTC().Format("2006-01-02 15:04:05 UTC")
	}
	props := []console.Property{
		{Label: "Policy ID", Value: path[0]},
		{Label: "Policy name", Value: orDash(pol.GetDisplayName())},
		{Label: "Enforcement version", Value: pol.GetDetails().GetEnforcementVersion()},
		{Label: "Organization", Value: resourcemanager.OrganizationName},
		{Label: "Created", Value: created},
		{Label: "Last updated", Value: updated},
		{Label: "Resource name", Value: pol.GetName()},
	}
	rules := console.Listing{NameColumn: "Rule", Columns: []string{"Description", "Resources", "Effect"}, Noun: "boundary rules"}
	for i, r := range pol.GetDetails().GetRules() {
		row := console.Resource{Name: pabRuleRow + strconv.Itoa(i+1),
			Fields: map[string]string{"Description": orDash(r.GetDescription()), "Resources": strings.Join(r.GetResources(), ", "), "Effect": r.GetEffect().String()}}
		if len(pol.GetDetails().GetRules()) > 1 {
			row.Actions = []console.Action{{ID: "delete-rule-" + strconv.Itoa(i+1), Label: "Delete rule", Destructive: true}}
		}
		rules.Items = append(rules.Items, row)
	}
	rules.Total = len(rules.Items)
	bl := console.Listing{NameColumn: "Binding", Columns: []string{"Display name", "Target principal set", "Condition"}, Noun: "bindings",
		Note: "A binding applies the policy to every principal in its target principal set for which its condition, if any, is true or cannot be evaluated."}
	bs, err := p.bindings(pol.GetName())
	if err != nil {
		bl.Unavailable = consoleErr(err)
	}
	for _, b := range bs {
		row := console.Resource{Name: pabBindingKey(b.GetName()),
			Fields: map[string]string{"Display name": orDash(b.GetDisplayName()), "Target principal set": b.GetTarget().GetPrincipalSet(),
				"Condition": orDash(b.GetCondition().GetExpression())}}
		row.Actions = []console.Action{{ID: "delete-binding", Label: "Delete", Destructive: true}}
		bl.Items = append(bl.Items, row)
	}
	bl.Total = len(bl.Items)
	d := console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Policy", Properties: props}}},
		{ID: "rules", Label: "Boundary rules", Listing: rules},
		{ID: "bindings", Label: "Bindings", Listing: bl},
	}}
	d.Edit = &console.EditForm{Label: "Edit policy", Note: "The policy ID cannot change, as on Google.", Fields: []console.Field{
		{Name: "displayName", Label: "Policy name", Type: "text", Default: pol.GetDisplayName(), Help: "At most 63 characters."},
		{Name: "enforcementVersion", Label: "Enforcement version", Type: "select", Options: pabVersions, Default: pol.GetDetails().GetEnforcementVersion(), Help: pabVersionHelp},
	}}
	return d, nil
}

func (p iamPABProvider) bindingDetail(name string) (console.Detail, error) {
	b, err := p.registry.GetPolicyBinding(name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the binding: " + consoleErr(err)}, nil
	}
	c := b.GetCondition()
	props := []console.Property{
		{Label: "Binding ID", Value: lastSegment(b.GetName())},
		{Label: "Display name", Value: orDash(b.GetDisplayName())},
		{Label: "Target principal set", Value: b.GetTarget().GetPrincipalSet()},
		{Label: "Policy kind", Value: b.GetPolicyKind().String()},
		{Label: "Policy", Value: b.GetPolicy()},
		{Label: "Condition title", Value: orDash(c.GetTitle())},
		{Label: "Condition expression", Value: orDash(c.GetExpression())},
		{Label: "Resource name", Value: b.GetName()},
	}
	d := console.Detail{Summary: props[:3], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Binding", Properties: props}}}}}
	d.Edit = &console.EditForm{Label: "Edit binding", Note: "The target principal set and the policy cannot change, as on Google.", Fields: pabConditionFields(b.GetDisplayName(), c)}
	return d, nil
}

func pabConditionFields(display string, c *expr.Expr) []console.Field {
	return []console.Field{
		{Name: "bindingDisplayName", Label: "Display name", Type: "text", Default: display, Help: "At most 63 characters."},
		{Name: "conditionTitle", Label: "Condition title", Type: "text", Default: c.GetTitle(), Section: "Condition (optional)"},
		{Name: "conditionDescription", Label: "Condition description", Type: "text", Default: c.GetDescription(), Section: "Condition (optional)"},
		{Name: "conditionExpression", Label: "Expression", Type: "textarea", Default: c.GetExpression(), Section: "Condition (optional)",
			Help: "principal.type and principal.subject only, such as principal.type == 'iam.googleapis.com/ServiceAccount'; at most 10 logical operators and 250 characters."},
	}
}

func pabConditionFromForm(v map[string]string) *expr.Expr {
	e := strings.TrimSpace(v["conditionExpression"])
	if e == "" {
		return nil
	}
	return &expr.Expr{Title: strings.TrimSpace(v["conditionTitle"]), Description: strings.TrimSpace(v["conditionDescription"]), Expression: e}
}

func (p iamPABProvider) Edit(_ context.Context, _ string, path []string, v map[string]string) error {
	switch len(path) {
	case 1:
		_, err := p.registry.UpdatePABPolicy(&iamv3.PrincipalAccessBoundaryPolicy{Name: p.policyName(path[0]), DisplayName: strings.TrimSpace(v["displayName"]),
			Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{EnforcementVersion: v["enforcementVersion"]}},
			&fieldmaskpb.FieldMask{Paths: []string{"display_name", "details.enforcement_version"}}, false)
		return err
	case 2:
		name, ok := pabBindingName(path[1])
		if !ok {
			break
		}
		_, err := p.registry.UpdatePolicyBinding(&iamv3.PolicyBinding{Name: name, DisplayName: strings.TrimSpace(v["bindingDisplayName"]), Condition: pabConditionFromForm(v)},
			&fieldmaskpb.FieldMask{Paths: []string{"display_name", "condition"}}, false)
		return err
	}
	return fmt.Errorf("only a policy or a binding can be edited")
}

// Delete deletes a policy; one still bound is refused, as the API refuses
// it without force.
func (p iamPABProvider) Delete(_ context.Context, _, row string) error {
	_, err := p.registry.DeletePABPolicy(p.policyName(row), "", false, false)
	return err
}

// Actions are a policy row's: none beyond Delete.
func (iamPABProvider) Actions(console.Resource) []console.Action { return nil }

func (p iamPABProvider) Act(ctx context.Context, project, row, action string) error {
	return p.ActAt(ctx, project, []string{row}, action, nil)
}

// pabBindingFields are Add binding's fields.
func pabBindingFields() []console.Field {
	return append([]console.Field{
		{Name: "bindingId", Label: "Binding ID", Type: "text", Required: true, Pattern: pabIDPattern, DeriveFrom: "bindingDisplayName",
			Help: "3 to 63 lowercase letters, digits, hyphens and dots, starting with a letter."},
		{Name: "targetType", Label: "Target principal set type", Type: "select", Required: true,
			Options: []string{"Organization", "Folder", "Project", "Workforce identity pool", "Workload identity pool", "Google Workspace"}},
		{Name: "targetId", Label: "Target principal set ID", Type: "text", Required: true,
			Help: "The organization, folder or project ID; a workforce pool's ID; a workload pool's projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/POOL_ID; the Workspace customer ID. It cannot be changed."},
	}, pabConditionFields("", nil)...)
}

// pabTarget is the binding parent and principal set a form names.
func (p iamPABProvider) pabTarget(typ, id string) (parent, set string, err error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", fmt.Errorf("enter the target principal set's ID")
	}
	crm := "//cloudresourcemanager.googleapis.com/"
	switch typ {
	case "Organization":
		id = strings.TrimPrefix(id, "organizations/")
		return "organizations/" + id, crm + "organizations/" + id, nil
	case "Folder":
		id = strings.TrimPrefix(id, "folders/")
		return "folders/" + id, crm + "folders/" + id, nil
	case "Project":
		id = strings.TrimPrefix(id, "projects/")
		return "projects/" + id, crm + "projects/" + id, nil
	case "Workforce identity pool":
		return resourcemanager.OrganizationName, "//iam.googleapis.com/locations/global/workforcePools/" + id, nil
	case "Google Workspace":
		return resourcemanager.OrganizationName, "//iam.googleapis.com/locations/global/workspace/" + id, nil
	case "Workload identity pool":
		id = strings.TrimPrefix(id, "//iam.googleapis.com/")
		parts := strings.Split(id, "/")
		if len(parts) != 6 || parts[0] != "projects" || parts[4] != "workloadIdentityPools" {
			return "", "", fmt.Errorf("a workload identity pool is projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/POOL_ID")
		}
		return "projects/" + parts[1], "//iam.googleapis.com/" + id, nil
	}
	return "", "", fmt.Errorf("choose a target principal set type")
}

// DetailActions are a policy's Add binding and Add boundary rule, and a
// binding's Delete.
func (p iamPABProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	switch len(path) {
	case 1:
		return []console.Action{
			{ID: "add-binding", Label: "Add binding", Fields: pabBindingFields()},
			{ID: "add-rule", Label: "Add boundary rule", Fields: pabRuleFields("Boundary rule")},
		}
	case 2:
		if _, ok := pabBindingName(path[1]); ok {
			return []console.Action{{ID: "delete-binding", Label: "Delete", Destructive: true}}
		}
	}
	return nil
}

func (p iamPABProvider) ActAt(_ context.Context, _ string, path []string, action string, v map[string]string) error {
	if len(path) == 0 {
		return fmt.Errorf("unknown action %q", action)
	}
	policy := p.policyName(path[0])
	switch {
	case len(path) == 1 && action == "add-binding":
		parent, set, err := p.pabTarget(v["targetType"], v["targetId"])
		if err != nil {
			return err
		}
		_, err = p.registry.CreatePolicyBinding(parent+"/locations/global", strings.TrimSpace(v["bindingId"]), &iamv3.PolicyBinding{
			DisplayName: strings.TrimSpace(v["bindingDisplayName"]), Policy: policy, PolicyKind: iamv3.PolicyBinding_PRINCIPAL_ACCESS_BOUNDARY,
			Target:    &iamv3.PolicyBinding_Target{Target: &iamv3.PolicyBinding_Target_PrincipalSet{PrincipalSet: set}},
			Condition: pabConditionFromForm(v)}, false)
		return err
	case len(path) == 1 && action == "add-rule":
		cur, err := p.registry.GetPABPolicy(policy)
		if err != nil {
			return err
		}
		rules := append(append([]*iamv3.PrincipalAccessBoundaryPolicyRule(nil), cur.GetDetails().GetRules()...), pabRuleFromForm(v))
		_, err = p.registry.UpdatePABPolicy(&iamv3.PrincipalAccessBoundaryPolicy{Name: policy, Etag: cur.GetEtag(),
			Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{Rules: rules}}, &fieldmaskpb.FieldMask{Paths: []string{"details.rules"}}, false)
		return err
	case len(path) == 2 && action == "delete-binding":
		name, ok := pabBindingName(path[1])
		if !ok {
			break
		}
		_, err := p.registry.DeletePolicyBinding(name, "", false)
		return err
	case len(path) == 2 && strings.HasPrefix(action, "delete-rule-"):
		n, err := strconv.Atoi(strings.TrimPrefix(action, "delete-rule-"))
		if err != nil {
			break
		}
		cur, err := p.registry.GetPABPolicy(policy)
		if err != nil {
			return err
		}
		old := cur.GetDetails().GetRules()
		if n < 1 || n > len(old) {
			return fmt.Errorf("no rule %d", n)
		}
		rules := append(append([]*iamv3.PrincipalAccessBoundaryPolicyRule(nil), old[:n-1]...), old[n:]...)
		_, err = p.registry.UpdatePABPolicy(&iamv3.PrincipalAccessBoundaryPolicy{Name: policy, Etag: cur.GetEtag(),
			Details: &iamv3.PrincipalAccessBoundaryPolicyDetails{Rules: rules}}, &fieldmaskpb.FieldMask{Paths: []string{"details.rules"}}, false)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var _ console.Creator = iamPABProvider{}
var _ console.PageCreator = iamPABProvider{}
var _ console.Deleter = iamPABProvider{}
var _ console.Driller = iamPABProvider{}
var _ console.Editor = iamPABProvider{}
var _ console.Actor = iamPABProvider{}
