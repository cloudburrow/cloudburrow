package bigqueryfront

import (
	"path/filepath"
	"testing"
)

// TestDroppedStoreForgetsAPurgedProject: a purge (#1540) forgets every
// deletion kept of the project, on disk too, and returns their copies; another
// project's stay.
func TestDroppedStoreForgetsAPurgedProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dropped.json")
	s := &droppedStore{}
	s.keep(path, nil)
	s.add([]droppedTable{{Project: "gone", Dataset: "d", Table: "t", Copy: "c1", Deleted: 1000, Hours: 48},
		{Project: "gone", Dataset: "d", Table: "v", Deleted: 1000, Hours: 48},
		{Project: "kept", Dataset: "d", Table: "t", Copy: "c2", Deleted: 1000, Hours: 48}},
		&droppedDataset{Project: "gone", Dataset: "e", Deleted: 1000, Hours: 48})
	if got := s.forgetProject("gone"); len(got) != 1 || got[0] != "c1" {
		t.Errorf("forgetProject = %v, want [c1]", got)
	}
	again := &droppedStore{}
	again.keep(path, nil)
	if _, _, deleted := again.table("gone", "d", "v", -1); deleted {
		t.Error("the purged project's view is still kept")
	}
	if _, _, ok := again.takeDataset("gone", "e", 0); ok {
		t.Error("the purged project's dataset is still kept")
	}
	if _, found, _ := again.table("kept", "d", "t", -1); !found {
		t.Error("another project's deletion was forgotten")
	}
}
