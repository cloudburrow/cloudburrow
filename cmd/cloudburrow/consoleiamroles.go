package main

// IAM & Admin > Roles (#1743): the selected project's custom roles and the
// organization's, as Google's console lists them, with Create role, Edit
// role, Delete and Undelete, over the same registry the IAM API's
// projects.roles and organizations.roles serve, so a role made here is one
// gcloud iam roles and Terraform see, and the API's refusals (an invalid
// permission, a taken ID) are the form's. Predefined roles are not listed:
// CloudBurrow carries no catalog of them.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type iamRolesProvider struct {
	registry *resourcemanager.Registry
}

func (iamRolesProvider) ID() string    { return "iam-roles" }
func (iamRolesProvider) Title() string { return "Roles" }

const iamRolesNote = "Custom roles of this project and of the organization (" + resourcemanager.OrganizationName + "). " +
	"A role is granted on the IAM page; Firestore with MongoDB compatibility and Memorystore's IAM auth evaluate its permissions. " +
	"A deleted role grants nothing, can be undeleted for 7 days and is gone after 44. Predefined roles are not listed."

// orgRolePrefix names an organization role's row, so a project role and
// an organization role of the same ID are told apart; a role ID has no colon.
const orgRolePrefix = "organization:"

const roleIDPattern = `^[a-zA-Z0-9_.]{3,64}$`

var roleStages = []string{"ALPHA", "BETA", "GA", "EAP", "DEPRECATED", "DISABLED"}

// roleName is a row's role name.
func roleName(project, row string) (string, error) {
	if id, ok := strings.CutPrefix(row, orgRolePrefix); ok {
		return resourcemanager.OrganizationName + "/roles/" + id, nil
	}
	if project == "" {
		return "", errors.New("choose a project in the toolbar")
	}
	return "projects/" + project + "/roles/" + row, nil
}

func roleRow(r *adminpb.Role) string {
	id := r.GetName()[strings.LastIndex(r.GetName(), "/")+1:]
	if strings.HasPrefix(r.GetName(), "organizations/") {
		return orgRolePrefix + id
	}
	return id
}

func roleStatus(r *adminpb.Role) string {
	if r.GetDeleted() {
		return "DELETED"
	}
	if r.GetStage() == adminpb.Role_DISABLED {
		return "DISABLED"
	}
	return "ENABLED"
}

func (p iamRolesProvider) roles(project string) ([]*adminpb.Role, error) {
	parents := []string{resourcemanager.OrganizationName}
	if project != "" {
		parents = append([]string{"projects/" + project}, parents...)
	}
	var out []*adminpb.Role
	for _, parent := range parents {
		resp, err := p.registry.ListRoles(&adminpb.ListRolesRequest{Parent: parent, View: adminpb.RoleView_FULL, ShowDeleted: true, PageSize: 1000})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetRoles()...)
	}
	return out, nil
}

func (p iamRolesProvider) List(_ context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Role ID", Columns: []string{"Title", "Type", "Stage", "Permissions"}, Noun: "custom roles", Note: iamRolesNote}
	roles, err := p.roles(project)
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	for _, r := range roles {
		kind := "Custom (project)"
		if strings.HasPrefix(r.GetName(), "organizations/") {
			kind = "Custom (organization)"
		}
		row := console.Resource{Name: roleRow(r), Status: roleStatus(r), Fields: map[string]string{
			"Title": r.GetTitle(), "Type": kind, "Stage": r.GetStage().String(), "Permissions": strconv.Itoa(len(r.GetIncludedPermissions()))}}
		row.Actions = p.Actions(row)
		l.Items = append(l.Items, row)
	}
	l.Total = len(l.Items)
	return l, nil
}

// permissionsOf splits a form's permissions, one per line or comma-separated.
func permissionsOf(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' }) {
		out = append(out, f)
	}
	return out
}

func roleFields(r *adminpb.Role) []console.Field {
	stage, title, desc, perms := "ALPHA", "", "", ""
	if r != nil {
		stage, title, desc, perms = r.GetStage().String(), r.GetTitle(), r.GetDescription(), strings.Join(r.GetIncludedPermissions(), "\n")
	}
	return []console.Field{
		{Name: "title", Label: "Title", Type: "text", Default: title, Help: "At most 100 bytes."},
		{Name: "description", Label: "Description", Type: "text", Default: desc, Help: "At most 300 bytes."},
		{Name: "stage", Label: "Role launch stage", Type: "select", Options: roleStages, Default: stage,
			Help: "Informational, except DISABLED: a disabled role stays in policies and grants nothing."},
		{Name: "permissions", Label: "Permissions", Type: "textarea", Default: perms,
			Help: "One per line, or comma-separated, such as datastore.entities.get. Each must be one Google supports in custom roles; " +
				"a project role cannot hold an organization or folder permission. At most 3,000."},
	}
}

func roleFromForm(values map[string]string) *adminpb.Role {
	return &adminpb.Role{Title: values["title"], Description: values["description"],
		Stage:               adminpb.Role_RoleLaunchStage(adminpb.Role_RoleLaunchStage_value[values["stage"]]),
		IncludedPermissions: permissionsOf(values["permissions"])}
}

func (p iamRolesProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "parent", Label: "Create in", Type: "select", Options: []string{"This project", "The organization"}, Default: "This project",
			Help: "A project role is grantable in its project; an organization role in the organization's projects."},
		{Name: "roleId", Label: "ID", Type: "text", Required: true, Pattern: roleIDPattern,
			Help: "3 to 64 letters, digits, underscores and periods. It cannot change, and is not reused for 44 days after the role is deleted."},
	}
	return "Create role", append(fields, roleFields(nil)...)
}

func (iamRolesProvider) CreateOnPage() bool { return true }

func (p iamRolesProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	parent := resourcemanager.OrganizationName
	if values["parent"] != "The organization" {
		if project == "" {
			return "", errors.New("choose a project in the toolbar, or create the role in the organization")
		}
		parent = "projects/" + project
	}
	r, err := p.registry.CreateRole(parent, strings.TrimSpace(values["roleId"]), roleFromForm(values))
	if err != nil {
		return "", err
	}
	return roleRow(r), nil
}

func (p iamRolesProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	name, err := roleName(project, path[0])
	if err != nil {
		return console.Detail{Prompt: err.Error()}, nil
	}
	r, err := p.registry.GetRole(name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the role: " + consoleErr(err)}, nil
	}
	props := []console.Property{
		{Label: "Title", Value: orDash(r.GetTitle())},
		{Label: "Resource name", Value: r.GetName()},
		{Label: "Description", Value: orDash(r.GetDescription())},
		{Label: "Role launch stage", Value: r.GetStage().String()},
		{Label: "Status", Value: roleStatus(r)},
		{Label: "Permissions", Value: strconv.Itoa(len(r.GetIncludedPermissions()))},
	}
	perms := console.Listing{NameColumn: "Permission", Noun: "permissions"}
	ps := append([]string(nil), r.GetIncludedPermissions()...)
	sort.Strings(ps)
	for _, p := range ps {
		perms.Items = append(perms.Items, console.Resource{Name: p})
	}
	perms.Total = len(perms.Items)
	d := console.Detail{Summary: props[:2], Sections: []console.Section{
		{ID: "permissions", Label: "Permissions", Listing: perms},
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Role", Properties: props}}},
	}}
	if !r.GetDeleted() {
		d.Edit = &console.EditForm{Label: "Edit role", Fields: roleFields(r), Note: "The ID cannot change, as on Google."}
	}
	return d, nil
}

func (p iamRolesProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("only a role can be edited")
	}
	name, err := roleName(project, path[0])
	if err != nil {
		return err
	}
	_, err = p.registry.UpdateRole(name, roleFromForm(values), []string{"title", "description", "stage", "included_permissions"})
	return err
}

func (p iamRolesProvider) Delete(_ context.Context, project, row string) error {
	name, err := roleName(project, row)
	if err != nil {
		return err
	}
	_, err = p.registry.DeleteRole(name, nil)
	return err
}

// Actions offers Undelete on a deleted role; Delete is the list's own.
func (iamRolesProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "DELETED" {
		return []console.Action{{ID: "undelete", Label: "Undelete"}}
	}
	return nil
}

func (p iamRolesProvider) Act(_ context.Context, project, row, action string) error {
	if action != "undelete" {
		return fmt.Errorf("unknown action %q", action)
	}
	name, err := roleName(project, row)
	if err != nil {
		return err
	}
	_, err = p.registry.UndeleteRole(name, nil)
	return err
}

var _ console.Creator = iamRolesProvider{}
var _ console.PageCreator = iamRolesProvider{}
var _ console.Deleter = iamRolesProvider{}
var _ console.Driller = iamRolesProvider{}
var _ console.Editor = iamRolesProvider{}
var _ console.Actor = iamRolesProvider{}
