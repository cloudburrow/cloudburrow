package bigqueryfront

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestStreamTarget (#1273): the partition a streamed row goes to, by
// the table's unit or a daily table's decorator, within the bounds
// BigQuery documents.
func TestStreamTarget(t *testing.T) {
	now := time.Date(2026, 10, 15, 13, 45, 0, 0, time.UTC)
	m := func(unit string) partitionMeta {
		var p partitionMeta
		p.TimePartitioning = &struct {
			Type  string `json:"type"`
			Field string `json:"field"`
		}{Type: unit}
		return p
	}
	for _, c := range []struct {
		unit, dec string
		want      string
		code      int
	}{
		{"DAY", "", "2026-10-15 00:00:00", 0},
		{"HOUR", "", "2026-10-15 13:00:00", 0},
		{"MONTH", "", "2026-10-01 00:00:00", 0},
		{"YEAR", "", "2026-01-01 00:00:00", 0},
		{"DAY", "20260914", "2026-09-14 00:00:00", 0},
		{"DAY", "20260913", "", http.StatusBadRequest},
		{"DAY", "20261031", "2026-10-31 00:00:00", 0},
		{"DAY", "20261101", "", http.StatusBadRequest},
		{"DAY", "2026101", "", http.StatusBadRequest},
		{"HOUR", "2026101513", "", http.StatusBadRequest},
	} {
		got, code, _ := streamTarget(m(c.unit), c.dec, "p:d.t", now)
		if code != c.code || code == 0 && got.Format(sqlTimestamp) != c.want {
			t.Errorf("%s %q: %v %d, want %s %d", c.unit, c.dec, got, code, c.want, c.code)
		}
	}
}

// TestStreamBuffersKept (#1273): the windows outlive a restart of the
// front, in its state directory, and the emulator's restart empties them.
func TestStreamBuffersKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "streambuffers.json")
	a := &streamBuffers{}
	a.keep(path, nil)
	a.mu.Lock()
	a.wins = map[string]*streamWindow{streamKey("p", "d", "t"): {Base: "/projects/p", Dataset: "d", Table: "t", Unit: "DAY",
		Target: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), Oldest: time.Now().UTC(), Rows: 2}}
	a.saveLocked()
	a.mu.Unlock()
	b := &streamBuffers{}
	b.keep(path, nil)
	if w, ok := b.window("p", "d", "t"); !ok || w.Rows != 2 || w.Table != "t" {
		t.Fatalf("restored window: %+v %v", w, ok)
	}
	b.reset()
	c := &streamBuffers{}
	c.keep(path, nil)
	if _, ok := c.window("p", "d", "t"); ok {
		t.Error("a window survived reset")
	}
}
