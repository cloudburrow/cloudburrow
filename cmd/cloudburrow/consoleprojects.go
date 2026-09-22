package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/identity-wael/cloudburrow/internal/console"
	"github.com/identity-wael/cloudburrow/internal/service/resourcemanager"
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
			Columns:     []string{"Name", "Created"},
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
				"Created": created,
			},
		})
	}

	return console.Listing{
		// The identifier is the ID, and the display name is a separate
		// column: labelling both "Name" put the same header on two columns.
		NameColumn: "ID",
		Columns:    []string{"Name", "Created"},
		Noun:       "projects",
		Items:      out,
		Total:      len(out),
		Note: "Projects are a local registry. Deleting one removes the registration, " +
			"not the resources created under it — those live in the services that own them.",
	}, nil
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
			{Label: "Resource name", Value: project.Name},
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
			"Absent: organisations, folders, liens, IAM policies and billing. " +
			"Each returns UNIMPLEMENTED rather than a plausible answer.\n\n" +
			"Deleting this project removes the registration only. Resources " +
			"created under the identifier live in the services that own them, " +
			"and are not reached by a delete here.",
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Project ID", Value: project.ProjectID},
			{Label: "Project name", Value: project.DisplayName},
			{Label: "State", Value: string(project.State)},
			{Label: "Created", Value: created},
		},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups},
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
func (projectsProvider) CreateForm() (string, []console.Field) {
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
	}
}

// Create implements console.Creator.
func (p projectsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	id := strings.TrimSpace(values["projectId"])
	created, err := p.registry.Create(resourcemanager.Project{
		ProjectID:   id,
		DisplayName: strings.TrimSpace(values["displayName"]),
	})
	if err != nil {
		return "", err
	}
	return created.ProjectID, nil
}

// Delete implements console.Deleter.
func (p projectsProvider) Delete(ctx context.Context, project, name string) error {
	return p.registry.Delete(name)
}

var _ console.Creator = projectsProvider{}
var _ console.Deleter = projectsProvider{}
var _ console.Driller = projectsProvider{}
var _ console.Editor = projectsProvider{}
