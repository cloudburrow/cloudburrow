package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestBackupDRHierarchy: a folder's projects include its subfolders', the
// organization's every project under it, and a project with no parent is
// under neither (#1536).
func TestBackupDRHierarchy(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	top, err := reg.CreateFolder(resourcemanager.OrganizationName, "Top")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.CreateFolder(top.Name, "Sub")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []resourcemanager.Project{{ProjectID: "in-top", Parent: top.Name}, {ProjectID: "in-sub", Parent: sub.Name},
		{ProjectID: "in-org", Parent: resourcemanager.OrganizationName}, {ProjectID: "no-parent"}} {
		if _, err := reg.Create(p); err != nil {
			t.Fatal(err)
		}
	}
	h := backupDRHierarchy{reg: reg}
	for parent, want := range map[string]string{top.Name: "in-sub in-top", sub.Name: "in-sub",
		resourcemanager.OrganizationName: "in-org in-sub in-top"} {
		got, err := h.ProjectsUnder(parent)
		if err != nil {
			t.Fatalf("%s: %v", parent, err)
		}
		set := map[string]bool{}
		for _, id := range got {
			set[id] = true
		}
		for _, id := range strings.Fields(want) {
			if !set[id] {
				t.Errorf("%s: %v, want %s", parent, got, want)
			}
		}
		if len(got) != len(strings.Fields(want)) {
			t.Errorf("%s: %v, want %s", parent, got, want)
		}
	}
	for _, bad := range []string{"folders/999", "organizations/42", "projects/in-top"} {
		if _, err := h.ProjectsUnder(bad); !errors.Is(err, backupdr.ErrNoParent) {
			t.Errorf("%s: %v, want ErrNoParent", bad, err)
		}
	}
}
