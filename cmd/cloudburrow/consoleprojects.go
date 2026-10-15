package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// projectsProvider is the Resource Manager screen.
//
// It is the one screen that is not scoped to a project, because it is the
// screen that decides which projects exist. Listing it per project would be
// circular.
type projectsProvider struct {
	registry *resourcemanager.Registry
}

func (projectsProvider) ID() string    { return "projects" }
func (projectsProvider) Title() string { return "Resource Manager" }

func (p projectsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	items, err := p.registry.List()
	if err != nil {
		return console.Listing{
			NameColumn:  "ID",
			Columns:     []string{"Name", "Number", "Created"},
			Noun:        "projects",
			Unavailable: err.Error(),
		}, nil
	}

	out := make([]console.Resource, 0, len(items))
	for _, it := range items {
		created := it.CreateTime
		if t, err := time.Parse(time.RFC3339, it.CreateTime); err == nil {
			created = t.Local().Format("2006-01-02 15:04")
		}
		out = append(out, console.Resource{
			Name:   it.ProjectID,
			Status: string(it.State),
			Fields: map[string]string{
				"Name":    it.DisplayName,
				"Number":  projectNumberText(it.ProjectID),
				"Created": created,
			},
		})
	}

	return console.Listing{
		// The identifier is the ID, and the display name is a separate
		// column: labelling both "Name" put the same header on two columns.
		NameColumn: "ID",
		Columns:    []string{"Name", "Number", "Created"},
		Noun:       "projects",
		Items:      out,
		Total:      len(out),
		Note: "Projects are a local registry. Deleting one marks it DELETE_REQUESTED for 30 days, " +
			"when Undelete project on its page restores it, and then removes the registration; " +
			"never the resources created under it, which live in the services that own them.",
	}, nil
}

// projectTarget is a project's full resource name, as a tag binding names
// it.
func projectTarget(id string) string { return resourcemanager.CRMPrefix + resourcemanager.V3Name(id) }

// projectNumberText is a project's number, resource.ProjectNumber: the one
// the metadata server, Resource Manager v1 and v3 and the service agents'
// emails carry (#1211).
func projectNumberText(id string) string {
	return strconv.FormatInt(resource.ProjectNumber(id), 10)
}

// Detail implements console.Driller for one project.
//
// A project row was the end of the road: the screen that decides which projects
// exist could not say anything about one. Its identifier, its display name, its
// labels and the fact that it is a local registration rather than a Google
// project all belong on a page of its own.
func (p projectsProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	project, err := p.registry.Get(path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the project: " + err.Error()}, nil
	}

	created := project.CreateTime
	if t, err := time.Parse(time.RFC3339, project.CreateTime); err == nil {
		created = t.Local().Format("2006-01-02 15:04")
	}

	groups := []console.PropertyGroup{{
		Heading: "Project",
		Properties: []console.Property{
			{Label: "Project ID", Value: project.ProjectID},
			{Label: "Project name", Value: project.DisplayName},
			{Label: "Project number", Value: projectNumberText(project.ProjectID)},
			{Label: "Resource name", Value: resourcemanager.V3Name(project.ProjectID)},
			{Label: "Parent", Value: p.parentText(project.Parent)},
			{Label: "State", Value: string(project.State)},
			{Label: "Created", Value: created},
		},
	}}
	if len(project.Labels) > 0 {
		groups = append(groups, console.PropertyGroup{
			Heading: "Labels", Properties: sortedPairs(project.Labels),
		})
	}

	// What Resource Manager has and this does not. Said on the page rather than
	// only in the docs: an operator looking for a folder or an IAM policy should
	// find out here, not by concluding the console is broken.
	scope := console.Section{
		ID: "scope", Label: "Scope", Kind: console.KindText,
		Text: "CloudBurrow keeps a local project registry, not Resource Manager.\n\n" +
			"Present: project identifiers, display names, labels, and the state " +
			"every service uses to keep one project's resources apart from " +
			"another's.\n\n" +
			"One fixed organization, " + resourcemanager.OrganizationName + ", and the " +
			"folders under it (Resource hierarchy), which a project can be moved into " +
			"with Move project.\n\n" +
			"Tags (Tags screen; bound and inherited ones under Tags here) and " +
			"liens: a lien restricting resourcemanager.projects.delete makes " +
			"deleting this project fail, as on Google. Tags are stored, not enforced.\n\n" +
			"Absent: IAM policies and billing. Each returns UNIMPLEMENTED " +
			"rather than a plausible answer.\n\n" +
			"Deleting this project marks it DELETE_REQUESTED for 30 days, as Google " +
			"does, and Undelete project restores it until then. It changes the " +
			"registration only: resources created under the identifier live in the " +
			"services that own them, and are not reached by a delete here.",
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Project ID", Value: project.ProjectID},
			{Label: "Project name", Value: project.DisplayName},
			{Label: "Project number", Value: projectNumberText(project.ProjectID)},
			{Label: "State", Value: string(project.State)},
			{Label: "Created", Value: created},
		},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups},
			{ID: "org-policies", Label: "Organization policies", Listing: p.orgPolicies(path[0])},
			tagSection(p.registry, projectTarget(project.ProjectID)),
			lienSection(p.registry, project.ProjectID),
			scope,
		},
		Edit: &console.EditForm{
			Label: "Edit project",
			Fields: []console.Field{
				{Name: "projectId", Label: "Project ID", Type: "text",
					Default: project.ProjectID, Immutable: true,
					Help: "A project ID cannot be changed after creation."},
				{Name: "displayName", Label: "Project name", Type: "text",
					Default: project.DisplayName,
					Help:    "Clearing it falls back to the project ID."},
				{Name: "labels", Label: "Labels", Type: "map",
					Default: console.FormatMap(project.Labels),
					Help:    "One key=value per line. Replaces the whole set."},
			},
			Note: "The identifier, the state and the creation time are not settings, " +
				"so they are not editable — which is also what Google's " +
				"UpdateProject accepts.",
		},
	}, nil
}

// Edit implements console.Editor.
func (p projectsProvider) Edit(_ context.Context, _ string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("only a project can be edited")
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	_, err = p.registry.Update(path[0], values["displayName"], labels)
	return err
}

// CreateForm implements console.Creator.
//
// The pattern is the identifier rule the API enforces, so the form refuses
// what the API would refuse instead of letting a round trip discover it.
func (p projectsProvider) CreateForm() (string, []console.Field) {
	return "Create project", []console.Field{
		{
			Name: "projectId", Label: "Project ID", Type: "text", Required: true,
			Help: "6–30 characters: lowercase letters, digits and hyphens, " +
				"starting with a letter and not ending with one. It cannot be changed later.",
			Pattern: `^[a-z][a-z0-9\-]{4,28}[a-z0-9]$`,
		},
		{
			Name: "displayName", Label: "Project name", Type: "text",
			Help: "Optional. Defaults to the project ID.",
		},
		{
			Name: "parent", Label: "Parent", Type: "select", Options: p.parentOptions(),
			Help: "Optional: the organization or a folder (Resource hierarchy). None leaves the project top-level.",
		},
	}
}

// parentOptions are the parents a project can be created under or moved to:
// none, the organization, or an active folder (#1294).
func (p projectsProvider) parentOptions() []string {
	if p.registry == nil {
		return []string{""}
	}
	return append([]string{""}, hierarchyProvider{registry: p.registry}.parentOptions("")...)
}

// parentText names a project's parent.
func (p projectsProvider) parentText(parent string) string {
	if parent == "" {
		return "None (top-level)"
	}
	if p.registry == nil {
		return parent
	}
	byName, _, _ := hierarchyProvider{registry: p.registry}.folders()
	return parentText(byName, parent)
}

// Create implements console.Creator.
func (p projectsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	id := strings.TrimSpace(values["projectId"])
	created, err := p.registry.Create(resourcemanager.Project{
		ProjectID:   id,
		DisplayName: strings.TrimSpace(values["displayName"]),
		Parent:      values["parent"],
	})
	if err != nil {
		return "", err
	}
	return created.ProjectID, nil
}

// Delete implements console.Deleter.
func (p projectsProvider) Delete(ctx context.Context, project, name string) error {
	_, err := p.registry.Delete(name)
	return err
}

// projectUndelete is the Undelete project action on a deleted project.
const projectUndelete = "undelete"

// DetailActions implements console.PathActor: Undelete project, on a project
// pending deletion only (#1216).
func (p projectsProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	project, err := p.registry.Get(path[0])
	if err != nil {
		return nil
	}
	if project.State == resourcemanager.StateDeleteRequested {
		return []console.Action{{ID: projectUndelete, Label: "Undelete project"}}
	}
	// The org policy actions (#1296), over the registry the v1 methods use.
	names := make([]string, 0, len(resourcemanager.Constraints))
	for _, c := range resourcemanager.Constraints {
		names = append(names, c.Name)
	}
	out := []console.Action{{ID: projectMove, Label: "Move project", Fields: []console.Field{
		{Name: "parent", Label: "Destination", Type: "select", Required: true, Options: p.parentOptions()[1:], Default: project.Parent,
			Help: "The organization or a folder (#1294)."},
	}}, {ID: projectSetPolicy, Label: "Set organization policy", Fields: []console.Field{
		{Name: "constraint", Label: "Constraint", Type: "select", Required: true, Options: names, Default: names[0]},
		{Name: "enforced", Label: "Enforce (boolean constraints)", Type: "checkbox"},
		{Name: "allowed", Label: "Allowed values (list constraints)", Type: "text",
			Help: "Comma-separated, e.g. in:eu-locations. Leave empty with Deny all unset to allow all."},
		{Name: "denyAll", Label: "Deny all values (list constraints)", Type: "checkbox"},
	}}}
	if set := p.orgPolicies(path[0]).Items; len(set) > 0 {
		ids := make([]string, 0, len(set))
		for _, r := range set {
			ids = append(ids, r.Name)
		}
		out = append(out, console.Action{ID: projectClearPolicy, Label: "Clear organization policy", Destructive: true,
			Fields: []console.Field{{Name: "constraint", Label: "Constraint", Type: "select", Required: true, Options: ids, Default: ids[0]}}})
	}
	// Tags and liens (#1295).
	out = append(out, tagActions(p.registry, projectTarget(project.ProjectID))...)
	return append(out, lienActions(p.registry, project.ProjectID)...)
}

const (
	projectMove        = "move"
	projectSetPolicy   = "set-org-policy"
	projectClearPolicy = "clear-org-policy"
)

// orgPolicies is the project's organization policies, as listOrgPolicies
// answers them (#1296).
func (p projectsProvider) orgPolicies(project string) console.Listing {
	l := console.Listing{Columns: []string{"Policy", "Updated"}, NameColumn: "Constraint", Noun: "policies",
		Note: "Stored, never enforced, as IAM policies are; a project has no organization above it, so its effective policy is its own."}
	all, err := p.registry.OrgPolicies(project)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l
	}
	for _, op := range all {
		l.Items = append(l.Items, console.Resource{Name: op.Constraint, Fields: map[string]string{
			"Policy": orgPolicyText(op), "Updated": op.UpdateTime}})
	}
	l.Total = len(l.Items)
	return l
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
		return strings.Join(parts, "; ")
	case op.RestoreDefault != nil:
		return "Restore default"
	}
	return "-"
}

// ActAt implements console.PathActor through the registry's Undelete, the
// one UndeleteProject calls.
func (p projectsProvider) ActAt(_ context.Context, _ string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	if ok, err := tagAct(p.registry, projectTarget(path[0]), action, values); ok {
		return err
	}
	if ok, err := lienAct(p.registry, path[0], action, values); ok {
		return err
	}
	switch action {
	case projectUndelete:
		_, err := p.registry.Undelete(path[0])
		return err
	case projectMove:
		_, err := p.registry.Move(path[0], values["parent"])
		return err
	case projectSetPolicy:
		op := &crmv1.OrgPolicy{Constraint: values["constraint"]}
		c := resourcemanager.KnownConstraint(op.Constraint)
		switch {
		case c != nil && c.BooleanConstraint != nil:
			op.BooleanPolicy = &crmv1.BooleanPolicy{Enforced: values["enforced"] == "true"}
		case values["denyAll"] == "true":
			op.ListPolicy = &crmv1.ListPolicy{AllValues: "DENY"}
		default:
			lp := &crmv1.ListPolicy{}
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
		_, err := p.registry.SetOrgPolicy(path[0], op)
		return err
	case projectClearPolicy:
		return p.registry.ClearOrgPolicy(path[0], values["constraint"], "")
	}
	return fmt.Errorf("unknown action %q", action)
}

var _ console.PathActor = projectsProvider{}
var _ console.Creator = projectsProvider{}
var _ console.Deleter = projectsProvider{}
var _ console.Driller = projectsProvider{}
var _ console.Editor = projectsProvider{}
