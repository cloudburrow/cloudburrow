package bigqueryfront

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDroppedStoreKeepsAndExpires: a kept deletion is found at a time the
// table existed, an undelete takes the dataset with the tables deleted
// with it, a restart keeps them, and once the window has passed they
// expire with their copies (#1460).
func TestDroppedStoreKeepsAndExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dropped.json")
	s := &droppedStore{}
	s.keep(path, nil)
	hour := int64(time.Hour / time.Millisecond)
	s.add([]droppedTable{{Project: "p", Dataset: "d", Table: "t", Copy: "c1", Created: 100, Deleted: 1000, Hours: 48}}, nil)
	s.add([]droppedTable{{Project: "p", Dataset: "e", Table: "u", Copy: "c2", Created: 100, Deleted: 2000, Hours: 48, WithDataset: true}},
		&droppedDataset{Project: "p", Dataset: "e", Deleted: 2000, Hours: 48})
	if _, found, _ := s.table("p", "d", "t", 500); !found {
		t.Error("d.t at 500 not found")
	}
	if _, found, deleted := s.table("p", "d", "t", 1500); found || !deleted {
		t.Errorf("d.t after its deletion: found %v, deleted %v; want not found, deleted", found, deleted)
	}
	again := &droppedStore{}
	again.keep(path, nil)
	if got := again.expire(1000 + 48*hour - 1); len(got) != 0 {
		t.Errorf("expired before the window passed: %v", got)
	}
	if got := again.expire(1000 + 48*hour + 1); len(got) != 1 || got[0] != "c1" {
		t.Errorf("expired %v, want [c1]", got)
	}
	d, tables, ok := again.takeDataset("p", "e", 0)
	if !ok || d.Dataset != "e" || len(tables) != 1 || tables[0].Copy != "c2" {
		t.Errorf("takeDataset = %+v %+v %v", d, tables, ok)
	}
	if _, _, ok := again.takeDataset("p", "e", 0); ok {
		t.Error("the dataset was taken twice")
	}
}
