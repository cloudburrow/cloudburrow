package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// hierarchyProvider is the Resource hierarchy screen (#1294): the local
// organization, its folders and the folders under them, over the registry
// the v3 Folders API and v2 folders use. Like the project list it is not
// scoped to a project: it is the tree projects sit in.
type hierarchyProvider struct {
	registry *resourcemanager.Registry
}

func (hierarchyProvider) ID() string    { return "hierarchy" }
func (hierarchyProvider) Title() string { return "Resource hierarchy" }

func consoleTime(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02 15:04")
	}
	return s
}

// rmFolderPath is a folder's place in the tree, from the organization down:
// "cloudburrow.local / Engineering / Team A".
func rmFolderPath(byName map[string]resourcemanager.Folder, f resourcemanager.Folder) string {
	parts := []string{f.DisplayName}
	for p, i := f.Parent, 0; strings.HasPrefix(p, "folders/") && i <= resourcemanager.MaxFolderDepth; i++ {
		pf, ok := byName[p]
		if !ok {
			break
		}
		parts = append([]string{pf.DisplayName}, parts...)
		p = pf.Parent
	}
	return strings.Join(append([]string{resourcemanager.OrganizationDomain}, parts...), " / ")
}

// parentText names a parent for a person: the organization's domain, or a
// folder's display name and number.
func parentText(byName map[string]resourcemanager.Folder, parent string) string {
	switch {
	case parent == "":
		return "-"
	case parent == resourcemanager.OrganizationName:
		return resourcemanager.OrganizationDomain + " (" + parent + ")"
	}
	if f, ok := byName[parent]; ok {
		return f.DisplayName + " (" + parent + ")"
	}
	return parent
}

func (p hierarchyProvider) folders() (map[string]resourcemanager.Folder, []resourcemanager.Folder, error) {
	all, err := p.registry.Folders()
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]resourcemanager.Folder{}
	for _, f := range all {
		byName[f.Name] = f
	}
	return byName, all, nil
}

const hierarchyNote = "One local organization, " + resourcemanager.OrganizationName + " (" + resourcemanager.OrganizationDomain +
	"), fixed on every instance; folders nest under it up to 10 deep. A folder is deleted only when it is empty, " +
	"and is then kept DELETE_REQUESTED for 30 days, when Undelete folder restores it. Folder and organization IAM policies are stored, never enforced and inherited by nothing (ADR-0006, #1368), and read through the API, gcloud and Terraform."

func (p hierarchyProvider) List(context.Context, string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Folder ID", Columns: []string{"Name", "Path", "Parent", "Created"}, Noun: "folders", Note: hierarchyNote}
	byName, all, err := p.folders()
	if err != nil {
		l.Unavailable = err.Error()
		return l, nil
	}
	for _, f := range all {
		l.Items = append(l.Items, console.Resource{Name: f.ID(), Status: string(f.State), Fields: map[string]string{
			"Name": f.DisplayName, "Path": rmFolderPath(byName, f), "Parent": parentText(byName, f.Parent), "Created": consoleTime(f.CreateTime)}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// parentOptions are the parents a folder or project can be placed under: the
// organization, then every active folder except skip and its descendants.
func (p hierarchyProvider) parentOptions(skip string) []string {
	out := []string{resourcemanager.OrganizationName}
	byName, all, err := p.folders()
	if err != nil {
		return out
	}
	under := func(f resourcemanager.Folder) bool {
		for n, i := f.Name, 0; strings.HasPrefix(n, "folders/") && i <= resourcemanager.MaxFolderDepth; i++ {
			if n == skip {
				return true
			}
			n = byName[n].Parent
		}
		return false
	}
	for _, f := range all {
		if f.State == resourcemanager.StateActive && (skip == "" || !under(f)) {
			out = append(out, f.Name)
		}
	}
	return out
}

func (p hierarchyProvider) CreateForm() (string, []console.Field) {
	return "Create folder", []console.Field{
		{Name: "displayName", Label: "Folder name", Type: "text", Required: true,
			Help:    "1 to 30 letters, digits, spaces, hyphens and underscores, starting and ending with a letter or digit; unique among its siblings.",
			Pattern: `^[\p{L}\p{N}]([\p{L}\p{N} _\-]{0,28}[\p{L}\p{N}])?$`},
		{Name: "parent", Label: "Parent", Type: "select", Required: true, Options: p.parentOptions(""), Default: resourcemanager.OrganizationName,
			Help: "The organization, or a folder."},
	}
}

func (p hierarchyProvider) Create(_ context.Context, _ string, values map[string]string) (string, error) {
	f, err := p.registry.CreateFolder(values["parent"], strings.TrimSpace(values["displayName"]))
	if err != nil {
		return "", err
	}
	return f.ID(), nil
}

func (p hierarchyProvider) Delete(_ context.Context, _ string, name string) error {
	_, err := p.registry.DeleteFolder(name)
	return err
}

func (p hierarchyProvider) Detail(_ context.Context, _ string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	byName, all, err := p.folders()
	if err != nil {
		return console.Detail{Unavailable: "cannot read the folders: " + err.Error()}, nil
	}
	f, err := p.registry.Folder(path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the folder: " + err.Error()}, nil
	}
	props := []console.Property{
		{Label: "Folder name", Value: f.DisplayName},
		{Label: "Resource name", Value: f.Name},
		{Label: "Path", Value: rmFolderPath(byName, f)},
		{Label: "Parent", Value: parentText(byName, f.Parent)},
		{Label: "State", Value: string(f.State)},
		{Label: "Created", Value: consoleTime(f.CreateTime)},
	}
	if f.DeleteTime != "" {
		props = append(props, console.Property{Label: "Deleted", Value: consoleTime(f.DeleteTime)})
	}
	children := console.Listing{NameColumn: "Folder ID", Columns: []string{"Name"}, Noun: "folders"}
	for _, c := range all {
		if c.Parent == f.Name && c.State == resourcemanager.StateActive {
			children.Items = append(children.Items, console.Resource{Name: c.ID(), Status: string(c.State), Fields: map[string]string{"Name": c.DisplayName}})
		}
	}
	children.Total = len(children.Items)
	projects := console.Listing{NameColumn: "ID", Columns: []string{"Name"}, Noun: "projects"}
	if ps, err := p.registry.Active(); err == nil {
		for _, pr := range ps {
			if pr.Parent == f.Name {
				projects.Items = append(projects.Items, console.Resource{Name: pr.ProjectID, Status: string(pr.State), Fields: map[string]string{"Name": pr.DisplayName}})
			}
		}
	}
	projects.Total = len(projects.Items)
	d := console.Detail{
		Summary: props[:2],
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Folder", Properties: props}}},
			{ID: "folders", Label: "Folders", Listing: children},
			{ID: "projects", Label: "Projects", Listing: projects},
			tagSection(p.registry, resourcemanager.CRMPrefix+f.Name),
		},
	}
	if f.State == resourcemanager.StateActive {
		d.Edit = &console.EditForm{Label: "Rename folder", Fields: []console.Field{
			{Name: "displayName", Label: "Folder name", Type: "text", Required: true, Default: f.DisplayName,
				Pattern: `^[\p{L}\p{N}]([\p{L}\p{N} _\-]{0,28}[\p{L}\p{N}])?$`},
		}, Note: "Move folder changes its parent."}
	}
	return d, nil
}

func (p hierarchyProvider) Edit(_ context.Context, _ string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("only a folder can be edited")
	}
	_, err := p.registry.RenameFolder(path[0], strings.TrimSpace(values["displayName"]))
	return err
}

const (
	folderMove     = "move"
	folderUndelete = "undelete"
)

// DetailActions offers Move folder on an active folder and Undelete folder
// on one pending deletion.
func (p hierarchyProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	f, err := p.registry.Folder(path[0])
	if err != nil {
		return nil
	}
	if f.State == resourcemanager.StateDeleteRequested {
		return []console.Action{{ID: folderUndelete, Label: "Undelete folder"}}
	}
	return append([]console.Action{{ID: folderMove, Label: "Move folder", Fields: []console.Field{
		{Name: "parent", Label: "Destination", Type: "select", Required: true, Options: p.parentOptions(f.Name), Default: f.Parent,
			Help: "The organization, or a folder that is not this one or under it."},
	}}}, tagActions(p.registry, resourcemanager.CRMPrefix+f.Name)...)
}

func (p hierarchyProvider) ActAt(_ context.Context, _ string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	if ok, err := tagAct(p.registry, resourcemanager.CRMPrefix+"folders/"+strings.TrimPrefix(path[0], "folders/"), action, values); ok {
		return err
	}
	var err error
	switch action {
	case folderMove:
		_, err = p.registry.MoveFolder(path[0], values["parent"])
	case folderUndelete:
		_, err = p.registry.UndeleteFolder(path[0])
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	return err
}

var _ console.PathActor = hierarchyProvider{}
var _ console.Creator = hierarchyProvider{}
var _ console.Deleter = hierarchyProvider{}
var _ console.Driller = hierarchyProvider{}
var _ console.Editor = hierarchyProvider{}
