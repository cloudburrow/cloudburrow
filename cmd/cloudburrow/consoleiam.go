package main

// IAM & Admin > IAM (#1368): the selected project's IAM policy, as Google's
// console has it, with Grant access, Remove principal and Test permissions
// on the Permissions tab of the project's row. Every read and write is
// Resource Manager's own Projects GetIamPolicy, SetIamPolicy and
// TestIamPermissions, the in-process server the API ports serve, so a stale
// etag or a conditional binding is refused with the API's message.
//
// The policy is stored and never enforced (ADR-0006), and nothing inherits
// it: a bucket's or a topic's own Permissions tab still shows only its own
// bindings. The page says both.

import (
	"context"
	"fmt"
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
}

func (projectIAMProvider) ID() string    { return "iam" }
func (projectIAMProvider) Title() string { return "IAM" }

func (p projectIAMProvider) api() iamServer { return resourcemanager.NewProjectsServer(p.registry) }

// List holds the selected project's one policy, with what it binds.
func (p projectIAMProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{NameColumn: "Project", Columns: []string{"Principals", "Roles"}, Noun: "IAM policies", Note: projectIAMNote}
	if project == "" {
		out.Prompt = "IAM is per project. Choose one in the toolbar."
		return out, nil
	}
	pol, err := getProtoPolicy(ctx, p.api(), resourcemanager.V3Name(project))
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
	if project != "" && path[0] != project {
		return console.Detail{Unavailable: fmt.Sprintf("%s is not the selected project %s", path[0], project)}, nil
	}
	pol, err := getProtoPolicy(ctx, p.api(), resourcemanager.V3Name(path[0]))
	if err != nil {
		return console.Detail{Unavailable: "cannot read the project's IAM policy: " + err.Error()}, nil
	}
	byPrincipal := map[string][]string{}
	for _, b := range pol.Bindings {
		for _, m := range b.Members {
			byPrincipal[m] = append(byPrincipal[m], b.Role)
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
	return console.Detail{Title: "IAM for project " + path[0], Sections: sections}, nil
}

func (projectIAMProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: projectIAMLink, RoleHelp: roleHelp("Resource Manager", "roles/viewer")}
}

func (p projectIAMProvider) GetPolicy(ctx context.Context, _ string, path []string) (console.Policy, error) {
	return getProtoPolicy(ctx, p.api(), resourcemanager.V3Name(path[0]))
}

func (p projectIAMProvider) SetPolicy(ctx context.Context, _ string, path []string, pol console.Policy) error {
	return setProtoPolicy(ctx, p.api(), resourcemanager.V3Name(path[0]), pol)
}

func (p projectIAMProvider) TestPermissions(ctx context.Context, _ string, path []string, perms []string) ([]string, error) {
	return testProtoPermissions(ctx, p.api(), resourcemanager.V3Name(path[0]), perms)
}
