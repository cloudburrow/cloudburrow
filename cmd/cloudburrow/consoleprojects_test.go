package main

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestProjectUndeleteIsOfferedOnlyWhenDeleted (#1216): a project the console
// deletes is kept in DELETE_REQUESTED, its page offers Undelete project, and
// the action restores it; an active project's page offers no Undelete (only
// the org policy actions, #1296).
func TestProjectUndeleteIsOfferedOnlyWhenDeleted(t *testing.T) {
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	p := projectsProvider{registry: reg}
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "undelete-me"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range p.DetailActions(ctx, "", []string{"undelete-me"}) {
		if a.ID == projectUndelete {
			t.Errorf("an active project's page offers %+v", a)
		}
	}
	if err := p.Delete(ctx, "", "undelete-me"); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(ctx, "")
	if err != nil || len(l.Items) != 1 || l.Items[0].Status != string(resourcemanager.StateDeleteRequested) {
		t.Fatalf("list after delete = %+v, %v; want the project DELETE_REQUESTED", l.Items, err)
	}
	a := p.DetailActions(ctx, "", []string{"undelete-me"})
	if len(a) != 1 || a[0].ID != projectUndelete || a[0].Label != "Undelete project" {
		t.Fatalf("a deleted project's page offers %+v, want Undelete project", a)
	}
	if err := p.ActAt(ctx, "", []string{"undelete-me"}, projectUndelete, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := reg.Get("undelete-me"); err != nil || got.State != resourcemanager.StateActive {
		t.Errorf("after Undelete project = %+v, %v; want ACTIVE", got, err)
	}
	if err := p.ActAt(ctx, "", []string{"undelete-me"}, projectUndelete, nil); err == nil {
		t.Error("undeleting an active project succeeded")
	}
}

// TestConsoleProjectOrgPolicies (#1296): Set organization policy stores a
// boolean and a list policy through the registry the v1 methods use, the
// project's page lists them, and Clear organization policy unsets one.
func TestConsoleProjectOrgPolicies(t *testing.T) {
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	p := projectsProvider{registry: reg}
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "policy-proj"}); err != nil {
		t.Fatal(err)
	}
	path := []string{"policy-proj"}
	if a := p.DetailActions(ctx, "", path); len(a) != 2 || a[0].ID != projectMove || a[1].ID != projectSetPolicy {
		t.Fatalf("a project with no policies offers %+v; want Set organization policy", a)
	}
	if err := p.ActAt(ctx, "", path, projectSetPolicy, map[string]string{"constraint": "constraints/storage.uniformBucketLevelAccess", "enforced": "true"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", path, projectSetPolicy, map[string]string{"constraint": "constraints/gcp.resourceLocations", "allowed": "in:eu-locations, in:us-locations"}); err != nil {
		t.Fatal(err)
	}
	got, err := reg.OrgPolicy("policy-proj", "constraints/gcp.resourceLocations")
	if err != nil || got.ListPolicy == nil || len(got.ListPolicy.AllowedValues) != 2 {
		t.Fatalf("the registry reads %+v, %v", got, err)
	}
	l := p.orgPolicies("policy-proj")
	if len(l.Items) != 2 || l.Items[1].Fields["Policy"] != "Enforced" || l.Items[0].Fields["Policy"] != "allow in:eu-locations, in:us-locations" {
		t.Fatalf("the Organization policies section = %+v", l.Items)
	}
	a := p.DetailActions(ctx, "", path)
	if len(a) != 3 || a[2].ID != projectClearPolicy {
		t.Fatalf("actions = %+v; want Move, Set and Clear", a)
	}
	if err := p.ActAt(ctx, "", path, projectClearPolicy, map[string]string{"constraint": "constraints/storage.uniformBucketLevelAccess"}); err != nil {
		t.Fatal(err)
	}
	if all, _ := reg.OrgPolicies("policy-proj"); len(all) != 1 {
		t.Errorf("after Clear, the policies are %+v", all)
	}
}

// TestConsoleResourceHierarchy (#1294): Create folder under the organization
// and under a folder, a project created in a folder, Move folder and Move
// project, Rename folder, Delete refused while the folder holds a project,
// then Delete and Undelete folder, all through the registry the v3 Folders
// API uses.
func TestConsoleResourceHierarchy(t *testing.T) {
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	h := hierarchyProvider{registry: reg}
	p := projectsProvider{registry: reg}
	eng, err := h.Create(ctx, "", map[string]string{"displayName": "Engineering", "parent": resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	team, err := h.Create(ctx, "", map[string]string{"displayName": "Team A", "parent": "folders/" + eng})
	if err != nil {
		t.Fatal(err)
	}
	if _, fields := h.CreateForm(); len(fields[1].Options) != 3 {
		t.Errorf("Create folder's parents = %v; want the organization and both folders", fields[1].Options)
	}
	if _, err := p.Create(ctx, "", map[string]string{"projectId": "in-team-a", "parent": "folders/" + team}); err != nil {
		t.Fatal(err)
	}
	l, _ := h.List(ctx, "")
	if l.Total != 2 {
		t.Fatalf("folders = %+v", l.Items)
	}
	for _, it := range l.Items {
		if it.Name == team && it.Fields["Path"] != resourcemanager.OrganizationDomain+" / Engineering / Team A" {
			t.Errorf("Team A's path = %q", it.Fields["Path"])
		}
	}
	d, _ := h.Detail(ctx, "", []string{team})
	if d.Sections[2].Listing.Total != 1 {
		t.Errorf("Team A's projects = %+v", d.Sections[2].Listing.Items)
	}
	// Move folder offers neither the folder itself nor one under it.
	moves := h.DetailActions(ctx, "", []string{eng})
	if len(moves) != 1 || len(moves[0].Fields[0].Options) != 1 {
		t.Errorf("Engineering's Move folder = %+v; want only the organization", moves)
	}
	if err := h.ActAt(ctx, "", []string{team}, folderMove, map[string]string{"parent": resourcemanager.OrganizationName}); err != nil {
		t.Fatal(err)
	}
	if err := h.Edit(ctx, "", []string{team}, map[string]string{"displayName": "Team B"}); err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(ctx, "", team); err == nil {
		t.Error("a folder holding a project was deleted")
	}
	if err := p.ActAt(ctx, "", []string{"in-team-a"}, projectMove, map[string]string{"parent": "folders/" + eng}); err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(ctx, "", team); err != nil {
		t.Fatal(err)
	}
	if a := h.DetailActions(ctx, "", []string{team}); len(a) != 1 || a[0].ID != folderUndelete {
		t.Fatalf("a deleted folder offers %+v; want Undelete folder", a)
	}
	if err := h.ActAt(ctx, "", []string{team}, folderUndelete, nil); err != nil {
		t.Fatal(err)
	}
	if f, err := reg.Folder(team); err != nil || f.State != resourcemanager.StateActive || f.DisplayName != "Team B" || f.Parent != resourcemanager.OrganizationName {
		t.Errorf("Team B after undelete = %+v, %v", f, err)
	}
}
