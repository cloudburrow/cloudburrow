package resourcemanager

import (
	"slices"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestOnCreated: each project and folder made is passed to the OnCreated
// hooks, after the registry's lock is released (a hook can read it).
func TestOnCreated(t *testing.T) {
	r := New(store.NewMemory())
	var got []string
	r.OnCreated(func(name string) {
		if _, err := r.Folder("folders/none"); err == nil {
			t.Error("folders/none exists")
		}
		got = append(got, name)
	})
	f, err := r.CreateFolder(OrganizationName, "Team")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(Project{ProjectID: "made-here", Parent: f.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(Project{ProjectID: "made-here", Parent: f.Name}); err == nil {
		t.Fatal("a second made-here was created")
	}
	if want := []string{f.Name, "projects/made-here"}; !slices.Equal(got, want) {
		t.Errorf("OnCreated saw %v; want %v", got, want)
	}
}
