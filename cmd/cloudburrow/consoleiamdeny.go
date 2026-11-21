package main

// The IAM page's Deny tab (#1889), as Google's console lays it out
// (https://docs.cloud.google.com/iam/docs/deny-access): for the selected
// project, or a folder or the organization above it, the deny policies
// attached to it, listed by Policy ID with their display name, rule count
// and last update; Create deny policy (a display name that fills in the
// ID, then deny rules, each with denied principals, exception principals,
// denied permissions, exception permissions and an optional denial
// condition), a policy's details, Edit (Save) and Delete. Each is one call
// of the registry IAM v2's Policies server uses (internal/service/
// resourcemanager/denypolicies.go), so what the page saves is what
// cloud.google.com/go/iam/apiv2 and gcloud iam policies read back, with
// the API's own validation and etags.
//
//	deny-list    {resource}: the attachment points the page offers and the
//	             resource's policies
//	deny-get     {name}: one policy with its rules
//	deny-create  {resource, id, displayName, rules}
//	deny-update  {name, etag, displayName, rules}
//	deny-delete  {name, etag}

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	iamv2 "cloud.google.com/go/iam/apiv2/iampb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// denyResource is one attachment point the Deny tab's selector offers.
type denyResource struct {
	Resource string `json:"resource"`
	Display  string `json:"display"`
	Kind     string `json:"kind"`
}

type denyCondition struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Expression  string `json:"expression"`
}

type denyPageRule struct {
	Description          string         `json:"description,omitempty"`
	DeniedPrincipals     []string       `json:"deniedPrincipals"`
	ExceptionPrincipals  []string       `json:"exceptionPrincipals"`
	DeniedPermissions    []string       `json:"deniedPermissions"`
	ExceptionPermissions []string       `json:"exceptionPermissions"`
	Condition            *denyCondition `json:"condition,omitempty"`
}

type denyPagePolicy struct {
	Name        string         `json:"name"`
	ID          string         `json:"id"`
	DisplayName string         `json:"displayName"`
	Etag        string         `json:"etag"`
	Created     string         `json:"created"`
	Updated     string         `json:"updated"`
	RuleCount   int            `json:"ruleCount"`
	Rules       []denyPageRule `json:"rules,omitempty"`
}

type denyPageList struct {
	Resource  string           `json:"resource"`
	Resources []denyResource   `json:"resources"`
	Policies  []denyPagePolicy `json:"policies"`
}

type denyPageChange struct {
	Resource    string         `json:"resource"`
	Name        string         `json:"name"`
	ID          string         `json:"id"`
	Etag        string         `json:"etag"`
	DisplayName string         `json:"displayName"`
	Rules       []denyPageRule `json:"rules"`
}

// denyParent is the deny policies' parent of resource (projects/ID,
// folders/N or organizations/N).
func denyParent(resource string) string {
	return "policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/"+resource) + "/denypolicies"
}

// denyResources are the project, the folders above it and the
// organization, which Google's resource selector offers whether or not the
// project is under it.
//
// With a folder or the organization chosen in the toolbar (sel, #2023),
// they are that resource and the folders and organization above it.
func (p projectIAMProvider) denyResources(project string, sel *selectedResource) []denyResource {
	if sel != nil {
		out := []denyResource{{Resource: sel.Name, Display: sel.Display, Kind: sel.Kind}}
		for _, a := range ancestorsOf(p.registry, sel.Name) {
			out = append(out, denyResource{Resource: a.Name, Display: a.Display, Kind: a.Kind})
		}
		return out
	}
	out := []denyResource{{Resource: "projects/" + project, Display: project, Kind: "Project"}}
	org := denyResource{Resource: resourcemanager.OrganizationName, Display: resourcemanager.OrganizationName, Kind: "Organization"}
	chain, err := p.registry.Ancestry(project)
	if err != nil || len(chain) < 2 {
		return append(out, org)
	}
	for _, a := range chain[1:] {
		switch a.Type {
		case "folder":
			d := denyResource{Resource: "folders/" + a.ID, Display: "folders/" + a.ID, Kind: "Folder"}
			if f, err := p.registry.Folder(d.Resource); err == nil && f.DisplayName != "" {
				d.Display = f.DisplayName
			}
			out = append(out, d)
		}
	}
	return append(out, org)
}

// denyScope is resource checked against what the page offers for project.
func (p projectIAMProvider) denyScope(project string, sel *selectedResource, resource string) (string, []denyResource, error) {
	offered := p.denyResources(project, sel)
	if resource == "" {
		return offered[0].Resource, offered, nil
	}
	for _, r := range offered {
		if r.Resource == resource {
			return resource, offered, nil
		}
	}
	if sel != nil {
		return "", nil, fmt.Errorf("%s is not %s or a folder or organization above it", resource, sel.Name)
	}
	return "", nil, fmt.Errorf("%s is not project %s or a folder or organization above it", resource, project)
}

func denyPolicyID(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}

func toDenyPage(pol *iamv2.Policy) denyPagePolicy {
	out := denyPagePolicy{Name: pol.GetName(), ID: denyPolicyID(pol.GetName()), DisplayName: pol.GetDisplayName(), Etag: pol.GetEtag(),
		RuleCount: len(pol.GetRules())}
	if t := pol.GetCreateTime(); t != nil {
		out.Created = t.AsTime().UTC().Format("2006-01-02T15:04:05Z")
	}
	if t := pol.GetUpdateTime(); t != nil {
		out.Updated = t.AsTime().UTC().Format("2006-01-02T15:04:05Z")
	}
	for _, r := range pol.GetRules() {
		d := r.GetDenyRule()
		if d == nil {
			continue
		}
		pr := denyPageRule{Description: r.GetDescription(), DeniedPrincipals: nonNil(d.GetDeniedPrincipals()),
			ExceptionPrincipals: nonNil(d.GetExceptionPrincipals()), DeniedPermissions: nonNil(d.GetDeniedPermissions()),
			ExceptionPermissions: nonNil(d.GetExceptionPermissions())}
		if c := d.GetDenialCondition(); c != nil && c.GetExpression() != "" {
			pr.Condition = &denyCondition{Title: c.GetTitle(), Description: c.GetDescription(), Expression: c.GetExpression()}
		}
		out.Rules = append(out.Rules, pr)
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func trimmed(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// denyRules are the page's rules as the API takes them.
func denyRules(in []denyPageRule) ([]*iamv2.PolicyRule, error) {
	if len(in) == 0 {
		return nil, errors.New("add at least one deny rule")
	}
	var out []*iamv2.PolicyRule
	for i, r := range in {
		d := &iamv2.DenyRule{DeniedPrincipals: trimmed(r.DeniedPrincipals), ExceptionPrincipals: trimmed(r.ExceptionPrincipals),
			DeniedPermissions: trimmed(r.DeniedPermissions), ExceptionPermissions: trimmed(r.ExceptionPermissions)}
		if len(d.DeniedPrincipals) == 0 {
			return nil, fmt.Errorf("deny rule %d: add at least one denied principal", i+1)
		}
		if len(d.DeniedPermissions) == 0 {
			return nil, fmt.Errorf("deny rule %d: add at least one denied permission", i+1)
		}
		if c := r.Condition; c != nil && strings.TrimSpace(c.Expression) != "" {
			d.DenialCondition = &expr.Expr{Title: strings.TrimSpace(c.Title), Description: strings.TrimSpace(c.Description), Expression: strings.TrimSpace(c.Expression)}
		}
		out = append(out, &iamv2.PolicyRule{Description: strings.TrimSpace(r.Description), Kind: &iamv2.PolicyRule_DenyRule{DenyRule: d}})
	}
	return out, nil
}

// denyCall serves the Deny tab's calls.
func (p projectIAMProvider) denyCall(project string, sel *selectedResource, call string, body json.RawMessage) (any, error) {
	var c denyPageChange
	if len(body) > 0 {
		if err := json.Unmarshal(body, &c); err != nil {
			return nil, fmt.Errorf("malformed request: %v", err)
		}
	}
	switch call {
	case "deny-list":
		resource, offered, err := p.denyScope(project, sel, c.Resource)
		if err != nil {
			return nil, err
		}
		list, err := p.registry.ListDenyPolicies(denyParent(resource))
		if err != nil {
			return nil, err
		}
		out := denyPageList{Resource: resource, Resources: offered, Policies: []denyPagePolicy{}}
		for _, pol := range list {
			full, err := p.registry.GetDenyPolicy(pol.GetName())
			if err != nil {
				return nil, err
			}
			d := toDenyPage(full)
			d.Rules = nil
			out.Policies = append(out.Policies, d)
		}
		return out, nil
	case "deny-get":
		pol, err := p.registry.GetDenyPolicy(c.Name)
		if err != nil {
			return nil, err
		}
		return toDenyPage(pol), nil
	case "deny-create":
		resource, _, err := p.denyScope(project, sel, c.Resource)
		if err != nil {
			return nil, err
		}
		id := strings.TrimSpace(c.ID)
		if id == "" {
			return nil, errors.New("enter a policy ID")
		}
		rules, err := denyRules(c.Rules)
		if err != nil {
			return nil, err
		}
		pol, err := p.registry.CreateDenyPolicy(denyParent(resource), id, &iamv2.Policy{DisplayName: strings.TrimSpace(c.DisplayName), Rules: rules})
		if err != nil {
			return nil, err
		}
		return toDenyPage(pol), nil
	case "deny-update":
		if c.Etag == "" {
			return nil, errors.New("etag is required: send the one the policy was read with")
		}
		rules, err := denyRules(c.Rules)
		if err != nil {
			return nil, err
		}
		pol, err := p.registry.UpdateDenyPolicy(&iamv2.Policy{Name: c.Name, Etag: c.Etag, DisplayName: strings.TrimSpace(c.DisplayName), Rules: rules})
		if err != nil {
			return nil, err
		}
		return toDenyPage(pol), nil
	case "deny-delete":
		if _, err := p.registry.DeleteDenyPolicy(c.Name, c.Etag); err != nil {
			return nil, err
		}
		return map[string]string{"deleted": c.Name}, nil
	}
	return nil, fmt.Errorf("no such call %q", call)
}
