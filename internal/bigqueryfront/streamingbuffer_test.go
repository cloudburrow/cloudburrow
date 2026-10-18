package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

// TestColumnStreamingBuffer (#1273): a table partitioned by a column marks
// its streamed rows: a daily table's near today stay in their partition's
// buffer, others go to __UNPARTITIONED__; a decorator reads accordingly;
// a value outside 10 years back and 1 year ahead is refused.
func TestColumnStreamingBuffer(t *testing.T) {
	now := time.Date(2026, 10, 15, 13, 45, 0, 0, time.UTC)
	m := func(unit string) partitionMeta {
		var p partitionMeta
		p.TimePartitioning = &struct {
			Type  string `json:"type"`
			Field string `json:"field"`
		}{Type: unit, Field: "d"}
		p.Schema.Fields = []field{{Name: "d", Type: "DATE"}}
		return p
	}
	if got := bufferedMark(m("HOUR"), now); got != "TRUE" {
		t.Errorf("hourly mark = %s, want TRUE", got)
	}
	want := "IF(TIMESTAMP(`d`) >= TIMESTAMP '2026-10-08 00:00:00' AND TIMESTAMP(`d`) < TIMESTAMP '2026-10-19 00:00:00', FALSE, TRUE)"
	if got := bufferedMark(m("DAY"), now); got != want {
		t.Errorf("daily mark = %s, want %s", got, want)
	}
	day := m("DAY")
	day.buffered = true
	if f, _, _ := partitionFilter(day, "__UNPARTITIONED__", "t"); !strings.Contains(f, "OR IFNULL(_PARTITION_BUFFERED, FALSE)") {
		t.Errorf("__UNPARTITIONED__ filter = %s", f)
	}
	if f, _, _ := partitionFilter(day, "20261015", "t"); !strings.Contains(f, "AND NOT IFNULL(_PARTITION_BUFFERED, FALSE)") {
		t.Errorf("partition filter = %s", f)
	}
	if f, _, _ := partitionFilter(m("DAY"), "20261015", "t"); strings.Contains(f, bufferedRows) {
		t.Errorf("filter without a buffer = %s", f)
	}
	for v, bad := range map[any]bool{"2026-10-15": false, "2010-01-01": true, "2028-01-01": true,
		"2026-10-15 10:00:00": false, json.Number("1760000000"): false, json.Number("1000000000"): true, "x": false} {
		if got := streamBounds(day, v, now) != ""; got != bad {
			t.Errorf("streamBounds(%v) refused = %v, want %v", v, got, bad)
		}
	}
	if w := (streamWindow{Column: true}); w.cond() != "_PARTITION_BUFFERED IS NOT NULL" {
		t.Errorf("cond = %s", w.cond())
	}
}

// TestBufferedMarksMatchTheSQL (#1624): the marks the front gives the rows
// of a table partitioned by a column are bufferedMark's, read in Go; a
// value it cannot read sends the rows through a scratch table.
func TestBufferedMarksMatchTheSQL(t *testing.T) {
	now := time.Date(2026, 10, 15, 13, 45, 0, 0, time.UTC)
	var m partitionMeta
	m.TimePartitioning = &struct {
		Type  string `json:"type"`
		Field string `json:"field"`
	}{Type: "DAY", Field: "d"}
	out := `{"rows":[{"json":{"d":"2026-10-08"}},{"json":{"d":"2026-10-07T23:59:59Z"}},{"json":{"d":"2026-10-18 23:59:59"}},` +
		`{"json":{"d":"2026-10-19"}},{"json":{}},{"json":{"D":1792071900}},{"json":{"d":"2001-01-01"}}]}`
	got, ok := bufferedMarks(m, []byte(out), now)
	want := []bool{false, true, false, true, true, false, true}
	if !ok || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("bufferedMarks = %v, %v, want %v", got, ok, want)
	}
	if _, ok := bufferedMarks(m, []byte(`{"rows":[{"json":{"d":"next week"}}]}`), now); ok {
		t.Error("a value bufferedMarks cannot read was read")
	}
	m.TimePartitioning.Type = "HOUR"
	if got, ok := bufferedMarks(m, []byte(`{"rows":[{"json":{"d":"2026-10-15"}}]}`), now); !ok || fmt.Sprint(got) != "[true]" {
		t.Errorf("hourly bufferedMarks = %v, %v, want [true]", got, ok)
	}
}

// TestStreamSendsTheColumns (#1624): a streamed insert into a table is one
// tabledata.insertAll of the table that names the rows' pseudo-columns,
// with no scratch table; a client's own header is not forwarded.
func TestStreamSendsTheColumns(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get(streamColumnsHeader))
		mu.Unlock()
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"type":"TABLE","schema":{"fields":[{"name":"n","type":"INTEGER"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"kind":"bigquery#tableDataInsertAllResponse"}`)
	})
	h := Wrap(emu, func(o *options) { o.streamTimes = true })
	r := httptest.NewRequest(http.MethodPost, base+"/datasets/d/tables/t/insertAll", strings.NewReader(`{"rows":[{"json":{"n":1}}]}`))
	r.Header.Set(streamColumnsHeader, `{"_PARTITIONTIME":null}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("insertAll: %d %s", w.Code, w.Body)
	}
	var posts []string
	for _, s := range seen {
		if !strings.HasPrefix(s, "GET ") {
			posts = append(posts, s)
		}
	}
	if len(posts) != 1 || !strings.Contains(posts[0], "/tables/t/insertAll {\"_PARTITION_STREAMED\":") ||
		strings.Contains(posts[0], "_PARTITIONTIME") {
		t.Errorf("sent %q, want one insertAll of t naming _PARTITION_STREAMED alone", seen)
	}
}
