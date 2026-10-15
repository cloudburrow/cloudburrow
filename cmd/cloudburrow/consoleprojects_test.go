package main

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestProjectUndeleteIsOfferedOnlyWhenDeleted (#1216): a project the console
// deletes is kept in DELETE_REQUESTED, its page offers Undelete project, and
// the action restores it; an active project's page offers nothing.
func TestProjectUndeleteIsOfferedOnlyWhenDeleted(t *testing.T) {
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	p := projectsProvider{registry: reg}
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "undelete-me"}); err != nil {
		t.Fatal(err)
	}
	if a := p.DetailActions(ctx, "", []string{"undelete-me"}); len(a) != 0 {
		t.Errorf("an active project's page offers %+v", a)
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
