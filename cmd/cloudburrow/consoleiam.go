package main

// IAM & Admin > IAM (#1368): the selected project's IAM policy, as Google's
// console has it, with Grant access, Remove principal and Test permissions
// on the Permissions tab of the project's row. Every read and write is
// Resource Manager's own Projects GetIamPolicy, SetIamPolicy and
// TestIamPermissions, the in-process server the API ports serve, so a stale
// etag or an invalid condition is refused with the API's message. Grant
// access takes an IAM Condition, and a conditional row's Edit condition
// changes it (#1742); the policy is read as version 3, so a condition is
// shown with its binding.
//
// The policy is stored and never enforced (ADR-0006), and nothing inherits
// it: a bucket's or a topic's own Permissions tab still shows only its own
// bindings. The page says both.

import (
	"context"
	"fmt"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

const projectIAMLink = compatibilityDoc + "resource-manager--googlecloudresourcemanagerv3-projects-folders-organizations-tags-liens"

const projectIAMNote = "The project's IAM policy, stored and never enforced (ADR-0006). Nothing inherits it: " +
	"a resource's own Permissions tab shows only that resource's bindings, and a binding here grants nothing on the resources in the project."

type projectIAMProvider struct {
	registry *resourcemanager.Registry
	// iam answers Policy Troubleshooter and Policy Analyzer (#1879).
	iam *iameval.Enforcer
	// assetOut are analyzeIamPolicyLongrunning's destinations (#2112).
	assetOut *assetOutputs
}

func (projectIAMProvider) ID() string    { return "iam" }
func (projectIAMProvider) Title() string { return "IAM" }

func (p projectIAMProvider) api() iamServer { return resourcemanager.NewProjectsServer(p.registry) }

// List holds the selected project's one policy, with what it binds.
func (p projectIAMProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{NameColumn: "Project", Columns: []string{"Principals", "Roles"}, Noun: "IAM policies", Note: projectIAMNote}
	sel, scoped, err := consoleResourceScope(ctx, p.registry)
	if err != nil {
		out.Unavailable = err.Error()
		return out, nil
	}
	name, api := resourcemanager.V3Name(project), p.api()
	if scoped {
		// The folder or organization chosen in the toolbar (#2023).
		out.NameColumn = sel.Kind
		name, api, project = sel.Name, iamServerOf(p.registry, sel.Name), idOfName(sel.Name)
	} else if project == "" {
		out.Prompt = "IAM is per project. Choose one in the toolbar."
		return out, nil
	}
	pol, err := getProtoPolicy(ctx, api, name)
	if err != nil {
		out.Unavailable = "cannot read the project's IAM policy: " + err.Error()
		return out, nil
	}
	principals := map[string]bool{}
	for _, b := range pol.Bindings {
		for _, m := range b.Members {
			principals[m] = true
		}
	}
	out.Items = []console.Resource{{Name: project, Fields: map[string]string{
		"Principals": strconv.Itoa(len(principals)), "Roles": strconv.Itoa(len(pol.Bindings))}}}
	out.Total = 1
	return out, nil
}

// Detail is the project's policy by principal, as Google's IAM page lists
// it; the Permissions tab beside it changes it.
func (p projectIAMProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	sel, scoped, err := consoleResourceScope(ctx, p.registry)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	title := "IAM for project " + path[0]
	if scoped {
		if path[0] != idOfName(sel.Name) {
			return console.Detail{Unavailable: fmt.Sprintf("%s is not the selected %s %s", path[0], strings.ToLower(sel.Kind), sel.Name)}, nil
		}
		title = "IAM for " + strings.ToLower(sel.Kind) + " " + sel.Display
	} else if project != "" && path[0] != project {
		return console.Detail{Unavailable: fmt.Sprintf("%s is not the selected project %s", path[0], project)}, nil
	}
	api, name, err := p.policyTarget(ctx, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	pol, err := getProtoPolicy(ctx, api, name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the project's IAM policy: " + err.Error()}, nil
	}
	byPrincipal := map[string][]string{}
	for _, b := range pol.Bindings {
		role := b.Role
		if b.Condition != "" {
			role += " (condition: " + b.Condition + ")"
		}
		for _, m := range b.Members {
			byPrincipal[m] = append(byPrincipal[m], role)
		}
	}
	names := make([]string, 0, len(byPrincipal))
	for m := range byPrincipal {
		names = append(names, m)
	}
	sort.Strings(names)
	props := make([]console.Property, 0, len(names))
	for _, m := range names {
		roles := byPrincipal[m]
		sort.Strings(roles)
		props = append(props, console.Property{Label: m, Value: strings.Join(roles, ", ")})
	}
	sections := []console.Section{{ID: "scope", Label: "About", Kind: console.KindText, Text: projectIAMNote}}
	if len(props) > 0 {
		sections = append([]console.Section{{ID: "principals", Label: "Principals", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "View by principals", Properties: props}}}}, sections...)
	}
	return console.Detail{Title: title, Sections: sections}, nil
}

func (projectIAMProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: projectIAMLink, RoleHelp: roleHelp("Resource Manager", "roles/viewer"), Conditions: true}
}

func (p projectIAMProvider) GetPolicy(ctx context.Context, _ string, path []string) (console.Policy, error) {
	api, name, err := p.policyTarget(ctx, path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, api, name)
}

func (p projectIAMProvider) SetPolicy(ctx context.Context, _ string, path []string, pol console.Policy) error {
	api, name, err := p.policyTarget(ctx, path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, api, name, pol)
}

func (p projectIAMProvider) TestPermissions(ctx context.Context, _ string, path []string, perms []string) ([]string, error) {
	api, name, err := p.policyTarget(ctx, path)
	if err != nil {
		return nil, err
	}
	return testProtoPermissions(ctx, api, name, perms)
}

// policyTarget is the API and resource name of the policy path names: the
// project, or the folder or organization chosen in the toolbar when path
// names it by its number (#2023).
func (p projectIAMProvider) policyTarget(ctx context.Context, path []string) (iamServer, string, error) {
	sel, scoped, err := consoleResourceScope(ctx, p.registry)
	if err != nil {
		return nil, "", err
	}
	if scoped && len(path) == 1 && path[0] == idOfName(sel.Name) {
		return iamServerOf(p.registry, sel.Name), sel.Name, nil
	}
	return p.api(), resourcemanager.V3Name(path[0]), nil
}

func idOfName(name string) string { return name[strings.LastIndex(name, "/")+1:] }
