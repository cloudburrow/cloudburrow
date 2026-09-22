package main

import (
	"context"
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
			Columns:     []string{"Name", "ID", "State", "Created"},
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
				"ID":      it.ProjectID,
				"Created": created,
			},
		})
	}

	return console.Listing{
		Columns: []string{"Name", "ID", "Created"},
		Items:   out,
		Total:   len(out),
		Note: "Projects are a local registry. Deleting one removes the registration, " +
			"not the resources created under it — those live in the services that own them.",
	}, nil
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
