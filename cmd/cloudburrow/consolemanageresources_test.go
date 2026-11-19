package main

import (
	"context"
	"encoding/json"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func mrDo(t *testing.T, p projectsProvider, call string, body any) any {
	t.Helper()
	raw, _ := json.Marshal(body)
	out, err := p.StudioCall(context.Background(), "", call, raw)
	if err != nil {
		t.Fatalf("%s %v: %v", call, body, err)
	}
	return out
}

func mrFind(t *testing.T, p projectsProvider, kind, id string) mrNode {
	t.Helper()
	tree := mrDo(t, p, "tree", nil).(mrTree)
	for _, n := range tree.Nodes {
		if n.Kind == kind && n.ID == id {
			return n
		}
	}
	t.Fatalf("the tree has no %s %s: %+v", kind, id, tree.Nodes)
	return mrNode{}
}

// TestManageResourcesStudio (#1865): the tree holds the organization, a
// folder made with Create folder and a project under it; Move takes the
// project to the organization; labels and a tag are set and removed;
// Delete shuts the project down (pending deletion, with its purge time)
// and Restore brings it back.
func TestManageResourcesStudio(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	p := projectsProvider{registry: reg}
	ctx := context.Background()
	folder := mrDo(t, p, "folder", mrRequest{DisplayName: "Engineering", Parent: resourcemanager.OrganizationName}).(map[string]string)["name"]
	if _, err := p.Create(ctx, "", map[string]string{"projectId": "mr-proj-1", "parent": folder}); err != nil {
		t.Fatal(err)
	}
	org := mrFind(t, p, "organization", resourcemanager.OrganizationNumber)
	if org.Name != resourcemanager.OrganizationDomain {
		t.Errorf("the organization row = %+v", org)
	}
	f := mrFind(t, p, "folder", folder[len("folders/"):])
	if f.Name != "Engineering" || f.Parent != resourcemanager.OrganizationName {
		t.Errorf("the folder row = %+v", f)
	}
	if n := mrFind(t, p, "project", "mr-proj-1"); n.Parent != folder || n.State != "ACTIVE" || n.Number == "" {
		t.Errorf("the project row = %+v", n)
	}
	mrDo(t, p, "move", mrRequest{Kind: "project", ID: "mr-proj-1", Parent: resourcemanager.OrganizationName})
	if n := mrFind(t, p, "project", "mr-proj-1"); n.Parent != resourcemanager.OrganizationName {
		t.Errorf("after Move the project's parent is %q", n.Parent)
	}
	mrDo(t, p, "labels", mrRequest{ID: "mr-proj-1", Labels: map[string]string{"env": "dev"}})
	if n := mrFind(t, p, "project", "mr-proj-1"); n.Labels["env"] != "dev" {
		t.Errorf("after labels the project's labels are %v", n.Labels)
	}
	tg := tagsProvider{registry: reg}
	if _, err := tg.Create(ctx, "", map[string]string{"parent": resourcemanager.OrganizationName, "shortName": "env",
		"values": `{"prod":"Serving traffic","dev":"Development"}`}); err != nil {
		t.Fatal(err)
	}
	tree := mrDo(t, p, "tree", nil).(mrTree)
	want := resourcemanager.OrganizationNumber + "/env/prod"
	if len(tree.TagValues) != 2 || tree.TagValues[1] != want {
		t.Fatalf("the tag values offered = %v; want the key's two values, made with it", tree.TagValues)
	}
	mrDo(t, p, "tag", mrRequest{Kind: "folder", ID: f.ID, Value: want})
	if n := mrFind(t, p, "folder", f.ID); len(n.Tags) != 1 || n.Tags[0] != want {
		t.Errorf("after Add tag the folder's tags are %v", n.Tags)
	}
	mrDo(t, p, "untag", mrRequest{Kind: "folder", ID: f.ID, Value: want})
	if n := mrFind(t, p, "folder", f.ID); len(n.Tags) != 0 {
		t.Errorf("after Remove the folder's tags are %v", n.Tags)
	}
	mrDo(t, p, "delete", mrRequest{Kind: "project", ID: "mr-proj-1"})
	if n := mrFind(t, p, "project", "mr-proj-1"); n.State != string(resourcemanager.StateDeleteRequested) || n.PurgeTime == "" {
		t.Errorf("after Delete the project row = %+v; want DELETE_REQUESTED with its deletion date", n)
	}
	mrDo(t, p, "restore", mrRequest{Kind: "project", ID: "mr-proj-1"})
	if n := mrFind(t, p, "project", "mr-proj-1"); n.State != "ACTIVE" {
		t.Errorf("after Restore the project is %s", n.State)
	}
	if _, err := p.StudioCall(ctx, "", "delete", json.RawMessage(`{"kind":"organization"}`)); err == nil {
		t.Error("deleting the organization was accepted")
	}
}

// TestTagKeyCreateForm (#1865): Google's Create tag key: Tag key, Tag key
// description, For use with network firewall (GCE_FIREWALL with its
// network) and the Tag values made with the key; a bad value makes
// nothing.
func TestTagKeyCreateForm(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	tg := tagsProvider{registry: reg}
	ctx := context.Background()
	_, fields := tg.CreateForm()
	var labels []string
	for _, f := range fields {
		labels = append(labels, f.Label)
	}
	if got := labels[:3]; got[0] != "Tag key" || got[1] != "Tag key description" || got[2] != "For use with network firewall" {
		t.Errorf("Create tag key's fields begin %q", got)
	}
	id, err := tg.Create(ctx, "", map[string]string{"parent": resourcemanager.OrganizationName, "shortName": "fw",
		"purpose": "true", "network": "net-proj/default"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := reg.TagKey("tagKeys/" + id)
	if err != nil {
		t.Fatal(err)
	}
	if k.GetPurpose() != rmpb.Purpose_GCE_FIREWALL || k.GetPurposeData()["network"] != "net-proj/default" {
		t.Errorf("the firewall key = %v", k)
	}
	if _, err := tg.Create(ctx, "", map[string]string{"parent": resourcemanager.OrganizationName, "shortName": "bad",
		"values": `{"-nope":"x"}`}); err == nil {
		t.Error("a key with a bad value was created")
	}
	if _, err := reg.NamespacedTagKey(resourcemanager.OrganizationNumber + "/bad"); err == nil {
		t.Error("a refused key was left behind")
	}
}

// TestSettingsShutDown (#1865): Google's Settings page has Shut down,
// which schedules the project for deletion.
func TestSettingsShutDown(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	ctx := context.Background()
	if _, err := (projectsProvider{registry: reg}).Create(ctx, "", map[string]string{"projectId": "settings-shut"}); err != nil {
		t.Fatal(err)
	}
	s := iamSettingsProvider{registry: reg}
	acts := s.DetailActions(ctx, "", []string{"settings-shut"})
	if len(acts) != 1 || acts[0].Label != "Shut down" || !acts[0].Destructive {
		t.Fatalf("Settings' actions = %+v; want Shut down", acts)
	}
	if err := s.ActAt(ctx, "", []string{"settings-shut"}, acts[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	if pr, _ := reg.Get("settings-shut"); pr.State != resourcemanager.StateDeleteRequested {
		t.Errorf("after Shut down the project is %s", pr.State)
	}
	if acts := s.DetailActions(ctx, "", []string{"settings-shut"}); len(acts) != 0 {
		t.Errorf("a project pending deletion offers %+v", acts)
	}
}
