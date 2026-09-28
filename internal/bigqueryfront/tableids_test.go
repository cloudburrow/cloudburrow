package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// pagingEmulator is the emulator's REST API for tabledata.list of a.t,
// whose ID b has too: its metadata, datasets.list and tables.list (the
// reads counted), and queries of a.t by LIMIT and OFFSET, answered from
// its rows (the queries kept).
type pagingEmulator struct {
	mu      sync.Mutex
	rows    []string
	tables  map[string][]string
	reads   int
	queries []string
}

var limitOffset = regexp.MustCompile("^SELECT \\* FROM `a\\.t`(?: LIMIT (\\d+) OFFSET (\\d+))?$")

func (e *pagingEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	p := strings.TrimPrefix(r.URL.Path, base)
	switch {
	case r.Method == http.MethodGet && p == "/datasets/a/tables/t":
		writeJSON(w, 200, map[string]any{"type": "TABLE", "numRows": strconv.Itoa(len(e.rows))})
	case r.Method == http.MethodGet && p == "/datasets":
		e.reads++
		var list []map[string]any
		for _, ds := range []string{"a", "b", resultsDataset} {
			list = append(list, map[string]any{"datasetReference": map[string]string{"datasetId": ds}})
		}
		writeJSON(w, 200, map[string]any{"datasets": list})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/tables"):
		e.reads++
		ds := strings.TrimSuffix(strings.TrimPrefix(p, "/datasets/"), "/tables")
		if ds == resultsDataset {
			http.Error(w, "the results dataset was listed", http.StatusTeapot)
			return
		}
		var list []map[string]any
		for _, id := range e.tables[ds] {
			list = append(list, map[string]any{"tableReference": map[string]string{"tableId": id}})
		}
		writeJSON(w, 200, map[string]any{"tables": list})
	case r.Method == http.MethodPost && p == "/queries":
		var q struct{ Query string }
		_ = json.Unmarshal(b, &q)
		e.queries = append(e.queries, q.Query)
		m := limitOffset.FindStringSubmatch(q.Query)
		if m == nil {
			writeJSON(w, 200, map[string]any{"rows": []any{}})
			return
		}
		rows := e.rows
		if m[1] != "" {
			n, _ := strconv.Atoi(m[1])
			off, _ := strconv.Atoi(m[2])
			rows = rows[min(off, len(rows)):min(off+n, len(rows))]
		}
		var out []map[string]any
		for _, v := range rows {
			out = append(out, map[string]any{"f": []map[string]string{{"v": v}}})
		}
		writeJSON(w, 200, map[string]any{"rows": out, "totalRows": strconv.Itoa(len(rows))})
	default:
		writeJSON(w, 200, map[string]any{})
	}
}

// TestTableDataListPagesByQueryAndKeepsTheSharedIDs (#1063): tabledata.list
// of a table whose ID another dataset has reads each page by LIMIT and
// OFFSET, never the whole table, with the table's numRows as totalRows;
// which IDs are shared is read once, with datasets.list and tables.list of
// each dataset but the results dataset, and kept for every page, until a
// request that may change the tables (not a lone SELECT), or the emulator
// restarting.
func TestTableDataListPagesByQueryAndKeepsTheSharedIDs(t *testing.T) {
	e := &pagingEmulator{rows: []string{"1", "2", "3", "4", "5"}, tables: map[string][]string{"a": {"t"}, "b": {"T"}}}
	watch := &emulatorWatch{}
	h := Wrap(e, func(o *options) { o.restarts = watch })
	page := func(q string) ([]string, string, string) {
		t.Helper()
		code, got := do(t, h, "GET", base+"/datasets/a/tables/t/data"+q, "")
		if code != 200 {
			t.Fatalf("%s: %d %v", q, code, got)
		}
		var vals []string
		rows, _ := got["rows"].([]any)
		for _, r := range rows {
			vals = append(vals, r.(map[string]any)["f"].([]any)[0].(map[string]any)["v"].(string))
		}
		token, _ := got["pageToken"].(string)
		return vals, token, fmt.Sprint(got["totalRows"])
	}
	var all []string
	token := ""
	for i := 0; i < 5; i++ {
		q := "?maxResults=2"
		if token != "" {
			q += "&pageToken=" + token
		}
		vals, next, total := page(q)
		if total != "5" {
			t.Errorf("page %d: totalRows %s", i, total)
		}
		all = append(all, vals...)
		if token = next; token == "" {
			break
		}
	}
	if strings.Join(all, ",") != "1,2,3,4,5" {
		t.Errorf("the pages read %v", all)
	}
	wantQueries := []string{"SELECT * FROM `a.t` LIMIT 2 OFFSET 0", "SELECT * FROM `a.t` LIMIT 2 OFFSET 2", "SELECT * FROM `a.t` LIMIT 1 OFFSET 4"}
	if strings.Join(e.queries, "\n") != strings.Join(wantQueries, "\n") {
		t.Errorf("the queries sent: %q, want %q", e.queries, wantQueries)
	}
	// datasets.list and a tables.list of a and b, once for the three pages.
	if e.reads != 3 {
		t.Errorf("the table IDs were read with %d requests, want 3", e.reads)
	}
	if vals, next, _ := page("?startIndex=3"); strings.Join(vals, ",") != "4,5" || next != "" {
		t.Errorf("startIndex=3: %v %q", vals, next)
	}
	if vals, next, _ := page("?startIndex=9&maxResults=2"); len(vals) != 0 || next != "" {
		t.Errorf("startIndex=9: %v %q", vals, next)
	}

	// A lone SELECT changes no table: the IDs are kept.
	do(t, h, "POST", base+"/queries", `{"query":"SELECT 1"}`)
	page("?maxResults=1")
	if e.reads != 3 {
		t.Errorf("after a SELECT the IDs were read again (%d requests)", e.reads)
	}
	// A request that may: read again, and b's table dropped makes a.t's
	// ID its own, read by the emulator itself.
	e.tables["b"] = nil
	do(t, h, "POST", base+"/queries", `{"query":"DROP TABLE b.T"}`)
	queries := len(e.queries)
	page("?maxResults=1")
	if e.reads != 6 || len(e.queries) != queries {
		t.Errorf("after DROP TABLE: %d reads, queries %q", e.reads, e.queries[queries:])
	}
	// The emulator restarting drops them too.
	page("")
	watch.fire()
	page("")
	if e.reads != 9 {
		t.Errorf("after a restart: %d reads, want 9", e.reads)
	}
}

// TestTableIDsMayChange (#1063): which requests drop the table IDs kept.
func TestTableIDsMayChange(t *testing.T) {
	for _, c := range []struct {
		method, path, body string
		want               bool
	}{
		{"GET", base + "/datasets/a/tables/t/data", "", false},
		{"POST", base + "/queries", `{"query":"SELECT * FROM a.t"}`, false},
		{"POST", base + "/queries", `{"query":"WITH x AS (SELECT 1) SELECT * FROM x;"}`, false},
		{"POST", base + "/queries", `{"query":"CREATE TABLE a.u (x INT64)"}`, true},
		{"POST", base + "/queries", `{"query":"SELECT 1; DROP TABLE a.t"}`, true},
		{"POST", base + "/jobs", `{"configuration":{"query":{"query":"SELECT 1"}}}`, false},
		{"POST", base + "/jobs", `{"configuration":{"query":{"query":"SELECT 1","destinationTable":{"tableId":"x"}}}}`, true},
		{"POST", base + "/jobs", `{"configuration":{"load":{}}}`, true},
		{"POST", base + "/datasets/a/tables", `{}`, true},
		{"DELETE", base + "/datasets/a/tables/t", "", true},
		{"PATCH", base + "/datasets/a/tables/t", `{}`, true},
		{"POST", base + "/datasets/a/tables/t/insertAll", `{}`, true},
		{"POST", "/upload" + base + "/jobs", `{}`, true},
	} {
		r, _ := http.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if got := tableIDsMayChange(r); got != c.want {
			t.Errorf("%s %s %s: %v, want %v", c.method, c.path, c.body, got, c.want)
		}
		if b, _ := io.ReadAll(r.Body); string(b) != c.body {
			t.Errorf("%s %s: the body is %q after the check", c.method, c.path, b)
		}
	}
}

// TestTableIDsAreNotKeptAcrossAChange (#1063): IDs read while a request
// that may change the tables is in flight are not kept.
func TestTableIDsAreNotKeptAcrossAChange(t *testing.T) {
	x := &tableIDs{}
	reads := 0
	read := func() (map[string][]string, bool) { reads++; return map[string][]string{"t": {"a"}}, true }
	done := x.change()
	x.lookup("p", read)
	x.lookup("p", read)
	done()
	x.lookup("p", read)
	x.lookup("p", read)
	if reads != 3 {
		t.Errorf("%d reads, want 3", reads)
	}
	// A change that begins while IDs are read: they are not kept.
	x.reset()
	x.lookup("p", func() (map[string][]string, bool) { x.change()(); return read() })
	x.lookup("p", read)
	if reads != 5 {
		t.Errorf("%d reads, want 5", reads)
	}
	var none *tableIDs
	none.lookup("p", read)
	none.change()()
	none.reset()
	if reads != 6 {
		t.Errorf("a nil tableIDs: %d reads, want 6", reads)
	}
}
