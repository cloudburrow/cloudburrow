package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// IAM & Admin > Roles (#1743): Create role, the list, the role's page, Edit
// role, Delete and Undelete, over the registry the IAM API serves, with the
// API's refusals.
func TestConsoleIAMRoles(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "console-roles"}); err != nil {
		t.Fatal(err)
	}
	p := iamRolesProvider{registry: reg}
	ctx := context.Background()
	row, err := p.Create(ctx, "console-roles", map[string]string{"parent": "This project", "roleId": "consoleRole", "title": "Console",
		"stage": "BETA", "permissions": "datastore.entities.get\ndatastore.entities.list"})
	if err != nil || row != "consoleRole" {
		t.Fatalf("Create = %q, %v", row, err)
	}
	if _, err := p.Create(ctx, "console-roles", map[string]string{"roleId": "bad", "permissions": "no.such.permission"}); err == nil ||
		!strings.Contains(err.Error(), "is not valid") {
		t.Errorf("an invalid permission = %v", err)
	}
	org, err := p.Create(ctx, "console-roles", map[string]string{"parent": "The organization", "roleId": "orgRole",
		"permissions": "resourcemanager.organizations.get"})
	if err != nil || org != orgRolePrefix+"orgRole" {
		t.Fatalf("Create in the organization = %q, %v", org, err)
	}
	l, _ := p.List(ctx, "console-roles")
	if len(l.Items) < 2400 || l.Items[0].Fields["Permissions"] != "2" || l.Items[0].Fields["Stage"] != "BETA" || l.Items[2].Fields["Type"] != "Predefined" {
		t.Fatalf("List = %+v", l.Items[:3])
	}
	// A predefined role (#1746): its page lists its permissions, read-only.
	pd, _ := p.Detail(ctx, "console-roles", []string{"predefined:storage.objectViewer"})
	if pd.Unavailable != "" || pd.Edit != nil || pd.Summary[0].Value != "Storage Object Viewer" || len(pd.Sections[0].Listing.Items) == 0 {
		t.Fatalf("a predefined role's Detail = %+v", pd)
	}
	if err := p.Delete(ctx, "console-roles", "predefined:storage.objectViewer"); err == nil {
		t.Error("deleted a predefined role")
	}
	d, _ := p.Detail(ctx, "console-roles", []string{"consoleRole"})
	if d.Unavailable != "" || d.Edit == nil || len(d.Sections[0].Listing.Items) != 2 {
		t.Fatalf("Detail = %+v", d)
	}
	if err := p.Edit(ctx, "console-roles", []string{"consoleRole"}, map[string]string{"title": "Renamed", "stage": "DISABLED",
		"permissions": "datastore.entities.get"}); err != nil {
		t.Fatal(err)
	}
	if perms, _ := reg.RolePermissions("projects/console-roles/roles/consoleRole"); len(perms) != 0 {
		t.Errorf("a DISABLED role grants %v", perms)
	}
	if err := p.Delete(ctx, "console-roles", "consoleRole"); err != nil {
		t.Fatal(err)
	}
	l, _ = p.List(ctx, "console-roles")
	if l.Items[0].Status != "DELETED" || len(l.Items[0].Actions) != 1 {
		t.Fatalf("after Delete = %+v", l.Items[0])
	}
	if err := p.Act(ctx, "console-roles", "consoleRole", "undelete"); err != nil {
		t.Fatal(err)
	}
	if r, err := reg.GetRole("projects/console-roles/roles/consoleRole"); err != nil || r.GetDeleted() || r.GetTitle() != "Renamed" {
		t.Errorf("after Undelete = %v, %v", r, err)
	}
}
