package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
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
	if len(a) != 2 || a[0].ID != projectUndelete || a[0].Label != "Undelete project" || a[1].ID != projectPurge || !a[1].Destructive {
		t.Fatalf("a deleted project's page offers %+v, want Undelete project and Purge project now", a)
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
	// Purge project now (#1537): refused on an active project, and on a
	// deleted one frees the ID.
	if err := p.ActAt(ctx, "", []string{"undelete-me"}, projectPurge, nil); err == nil {
		t.Error("purging an active project succeeded")
	}
	if err := p.Delete(ctx, "", "undelete-me"); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", []string{"undelete-me"}, projectPurge, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "undelete-me"}); err != nil {
		t.Errorf("the purged project's ID is not free: %v", err)
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
	if a := p.DetailActions(ctx, "", path); len(a) < 2 || a[0].ID != projectMove || a[1].ID != projectSetPolicy {
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
	if len(a) < 3 || a[2].ID != projectClearPolicy { // then the lien and tag actions (#1295)
		t.Fatalf("actions = %+v; want Move, Set and Clear", a)
	}
	if err := p.ActAt(ctx, "", path, projectClearPolicy, map[string]string{"constraint": "constraints/storage.uniformBucketLevelAccess"}); err != nil {
		t.Fatal(err)
	}
	if all, _ := reg.OrgPolicies("policy-proj"); len(all) != 1 {
		t.Errorf("after Clear, the policies are %+v", all)
	}
}

// TestConsoleFolderAndOrganizationOrgPolicies (#1534): Set organization
// policy on the organization (on its page, #1554) and on a folder, through the registry the v1 methods use; the folder's
// and a project's pages show the inherited policy and the effective one, and
// Clear organization policy on the folder unsets its own.
func TestConsoleFolderAndOrganizationOrgPolicies(t *testing.T) {
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	h := hierarchyProvider{registry: reg}
	p := projectsProvider{registry: reg}
	eng, err := h.Create(ctx, "", map[string]string{"displayName": "Engineering", "parent": resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "", map[string]string{"projectId": "eng-proj", "parent": "folders/" + eng}); err != nil {
		t.Fatal(err)
	}
	const boolC, listC = "constraints/compute.requireOsLogin", "constraints/gcp.resourceLocations"
	org := []string{organizationSegment}
	if a := h.DetailActions(ctx, "", org); len(a) != 1 || a[0].ID != orgPolicySet {
		t.Fatalf("the organization's page offers %+v; want Set organization policy", a)
	}
	if err := h.ActAt(ctx, "", org, orgPolicySet, map[string]string{"constraint": boolC, "enforced": "true"}); err != nil {
		t.Fatal(err)
	}
	if err := h.ActAt(ctx, "", org, orgPolicySet, map[string]string{"constraint": listC, "allowed": "in:eu-locations"}); err != nil {
		t.Fatal(err)
	}
	if got, err := reg.OrgPolicy(resourcemanager.OrganizationName, boolC); err != nil || got.BooleanPolicy == nil || !got.BooleanPolicy.Enforced {
		t.Fatalf("the organization's %s = %+v, %v", boolC, got, err)
	}
	od, _ := h.Detail(ctx, "", org)
	for _, sec := range od.Sections {
		if sec.ID == "org-policies" && (sec.Listing.Total != 2 || sec.Listing.Items[0].Fields["Policy"] != "Enforced") {
			t.Errorf("the organization's Organization policies = %+v", sec.Listing.Items)
		}
	}
	if a := h.DetailActions(ctx, "", org); len(a) != 2 || a[1].ID != orgPolicyClear {
		t.Errorf("with policies set, the organization's page offers %+v; want Set and Clear", a)
	}
	if err := h.ActAt(ctx, "", []string{eng}, orgPolicySet, map[string]string{"constraint": listC, "allowed": "in:us-locations", "inherit": "true"}); err != nil {
		t.Fatal(err)
	}
	d, _ := h.Detail(ctx, "", []string{eng})
	var sec *console.Listing
	for i := range d.Sections {
		if d.Sections[i].ID == "org-policies" {
			sec = &d.Sections[i].Listing
		}
	}
	if sec == nil || sec.Total != 2 {
		t.Fatalf("the folder's Organization policies = %+v", sec)
	}
	byName := map[string]map[string]string{}
	for _, it := range sec.Items {
		byName[it.Name] = it.Fields
	}
	if f := byName[boolC]; f["Policy"] != "Inherited" || f["Effective"] != "Enforced" {
		t.Errorf("the folder's %s row = %v", boolC, f)
	}
	if f := byName[listC]; f["Effective"] != "allow in:eu-locations, in:us-locations" {
		t.Errorf("the folder's %s row = %v", listC, f)
	}
	if pl := p.orgPolicies("eng-proj"); pl.Total != 2 {
		t.Errorf("the project's Organization policies = %+v; want both inherited", pl.Items)
	}
	if err := h.ActAt(ctx, "", []string{eng}, orgPolicyClear, map[string]string{"constraint": listC}); err != nil {
		t.Fatal(err)
	}
	if all, _ := reg.OrgPolicies("folders/" + eng); len(all) != 0 {
		t.Errorf("after Clear on the folder, its policies are %+v", all)
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
	if len(moves) == 0 || moves[0].ID != folderMove || len(moves[0].Fields[0].Options) != 1 {
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

// TestConsoleTagsAndLiens (#1295): Create tag key under the organization and
// a project, Create tag value, Edit description, Bind tag on a folder and a
// project, the project's Tags showing the inherited one, Unbind tag, Delete
// refused while bound, then Delete tag value and Delete; Add lien, Delete
// project refused FAILED_PRECONDITION, Remove lien, Delete project.
func TestConsoleTagsAndLiens(t *testing.T) {
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	tg := tagsProvider{registry: reg}
	h := hierarchyProvider{registry: reg}
	p := projectsProvider{registry: reg}
	eng, err := h.Create(ctx, "", map[string]string{"displayName": "Engineering", "parent": resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "", map[string]string{"projectId": "tagged-proj", "parent": "folders/" + eng}); err != nil {
		t.Fatal(err)
	}
	if _, fields := tg.CreateForm(); len(fields[0].Options) != 2 {
		t.Errorf("Create tag key's parents = %v; want the organization and the project", fields[0].Options)
	}
	env, err := tg.Create(ctx, "", map[string]string{"parent": resourcemanager.OrganizationName, "shortName": "env", "description": "Environment"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.Create(ctx, "", map[string]string{"parent": "projects/tagged-proj", "shortName": "team"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := tg.List(ctx, ""); l.Total != 2 {
		t.Fatalf("tag keys = %+v", l.Items)
	}
	for _, v := range []string{"prod", "dev"} {
		if err := tg.ActAt(ctx, "", []string{env}, tagCreateValue, map[string]string{"shortName": v}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tg.Edit(ctx, "", []string{env}, map[string]string{"description": "Deployment environment"}); err != nil {
		t.Fatal(err)
	}
	d, _ := tg.Detail(ctx, "", []string{env})
	if d.Sections[0].Listing.Total != 2 {
		t.Fatalf("env's values = %+v", d.Sections[0].Listing.Items)
	}
	prod := d.Sections[0].Listing.Items[1].Name
	if d.Sections[0].Listing.Items[1].Fields["Short name"] != "prod" {
		prod = d.Sections[0].Listing.Items[0].Name
	}
	if err := tg.Edit(ctx, "", []string{env, prod}, map[string]string{"description": "Production"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := reg.TagValue("tagValues/" + prod); v.GetDescription() != "Production" {
		t.Errorf("prod's description = %q", v.GetDescription())
	}

	// Bind env/prod on the folder; the project inherits it.
	ns := resourcemanager.OrganizationNumber + "/env/prod"
	if err := h.ActAt(ctx, "", []string{eng}, tagBind, map[string]string{"value": ns}); err != nil {
		t.Fatal(err)
	}
	pd, _ := p.Detail(ctx, "", []string{"tagged-proj"})
	var tags, liens console.Section
	for _, s := range pd.Sections {
		switch s.ID {
		case "tags":
			tags = s
		case "liens":
			liens = s
		}
	}
	if tags.Listing.Total != 1 || tags.Listing.Items[0].Fields["Source"] != "Inherited" {
		t.Errorf("the project's tags = %+v; want env/prod inherited", tags.Listing.Items)
	}
	// Bound on the project, dev replaces the inherited prod.
	if err := p.ActAt(ctx, "", []string{"tagged-proj"}, tagBind, map[string]string{"value": resourcemanager.OrganizationNumber + "/env/dev"}); err != nil {
		t.Fatal(err)
	}
	if all, _ := reg.EffectiveTags(projectTarget("tagged-proj")); len(all) != 1 || all[0].GetInherited() || all[0].GetNamespacedTagValue() != resourcemanager.OrganizationNumber+"/env/dev" {
		t.Errorf("effective tags after binding dev = %+v", all)
	}
	if err := tg.ActAt(ctx, "", []string{env}, tagDeleteValue, map[string]string{"value": "prod"}); err == nil {
		t.Error("a bound tag value was deleted")
	}
	if err := h.ActAt(ctx, "", []string{eng}, tagUnbind, map[string]string{"value": ns}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", []string{"tagged-proj"}, tagUnbind, map[string]string{"value": resourcemanager.OrganizationNumber + "/env/dev"}); err != nil {
		t.Fatal(err)
	}
	if err := tg.Delete(ctx, "", env); err == nil {
		t.Error("a tag key with values was deleted")
	}
	if err := tg.ActAt(ctx, "", []string{env}, tagDeleteValue, map[string]string{"value": "prod"}); err != nil {
		t.Fatal(err)
	}
	if err := tg.ActAt(ctx, "", []string{env}, tagDeleteValue, map[string]string{"value": "dev"}); err != nil {
		t.Fatal(err)
	}
	if err := tg.Delete(ctx, "", env); err != nil {
		t.Fatal(err)
	}

	// Liens.
	if liens.Listing.Total != 0 {
		t.Errorf("liens before any = %+v", liens.Listing.Items)
	}
	if err := p.ActAt(ctx, "", []string{"tagged-proj"}, lienAdd, map[string]string{
		"restrictions": resourcemanager.ProjectDeleteRestriction, "reason": "production", "origin": "cloudburrow-console"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, "", "tagged-proj"); err == nil || !strings.Contains(err.Error(), "lien") {
		t.Fatalf("Delete with a lien = %v; want the lien's FAILED_PRECONDITION", err)
	}
	var remove *console.Action
	for _, a := range p.DetailActions(ctx, "", []string{"tagged-proj"}) {
		if a.ID == lienRemove {
			remove = &a
		}
	}
	if remove == nil {
		t.Fatal("no Remove lien on a project with a lien")
	}
	if err := p.ActAt(ctx, "", []string{"tagged-proj"}, lienRemove, map[string]string{"lien": remove.Fields[0].Default}); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, "", "tagged-proj"); err != nil {
		t.Fatal(err)
	}
}
