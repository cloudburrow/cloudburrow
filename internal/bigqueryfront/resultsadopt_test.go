package bigqueryfront

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// resultTablesEmulator is expiryEmulator that keeps the tables of
// resultsDataset the jobs write, with the clock's creationTime and rows
// (rows, by job, default 0), and answers projects.list, tables.list and
// tables.get of them, as the emulator does across a restart of the front.
type resultTablesEmulator struct {
	expiryEmulator
	c      *expiryClock
	rows   map[string]int64
	schema []field
	// tables are the tables' creationTime, by ID.
	tables map[string]int64
	gets   int
}

func (e *resultTablesEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if e.tables == nil {
		e.tables = map[string]int64{}
	}
	path := strings.TrimPrefix(r.URL.Path, "/bigquery/v2")
	dir := "/projects/p/datasets/" + resultsDataset + "/tables"
	switch {
	case r.Method == http.MethodGet && path == "/projects":
		writeJSON(w, 200, map[string]any{"projects": []any{map[string]any{"id": "p"}}})
	case r.Method == http.MethodGet && path == dir:
		var list []any
		for id, ms := range e.tables {
			list = append(list, map[string]any{"tableReference": map[string]any{"projectId": "p", "datasetId": resultsDataset, "tableId": id},
				"creationTime": strconv.FormatInt(ms, 10)})
		}
		writeJSON(w, 200, map[string]any{"tables": list})
	case r.Method == http.MethodGet && strings.HasPrefix(path, dir+"/"):
		e.gets++
		id := strings.TrimPrefix(path, dir+"/")
		if _, ok := e.tables[id]; !ok {
			writeError(w, 404, "notFound", "Not found: Table "+id)
			return
		}
		writeJSON(w, 200, map[string]any{"numRows": strconv.FormatInt(e.rows[id], 10), "numBytes": "0",
			"schema": map[string]any{"fields": e.schema}})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/jobs"):
		b, _ := io.ReadAll(r.Body)
		var job jobBody
		_ = json.Unmarshal(b, &job)
		if q := job.Configuration.Query; q != nil && q.DestinationTable != nil && q.DestinationTable.DatasetID == resultsDataset {
			e.tables[q.DestinationTable.TableID] = e.c.t.UnixMilli()
		}
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		e.expiryEmulator.ServeHTTP(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, dir+"/"):
		delete(e.tables, strings.TrimPrefix(path, dir+"/"))
		e.expiryEmulator.ServeHTTP(w, r)
	default:
		e.expiryEmulator.ServeHTTP(w, r)
	}
}

func (e *resultTablesEmulator) ids() string {
	var ids []string
	for id := range e.tables {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return fmt.Sprint(ids)
}

// expireAll deletes the tables due while idle until none is.
func expireAll(t *testing.T, q *queryResults, c *expiryClock) {
	t.Helper()
	c.t = c.t.Add(resultsIdle)
	for i := 0; i < 100; i++ {
		due := q.expiry.due(-1, false)
		if len(due) == 0 {
			return
		}
		q.deleteTables(context.Background(), due, true)
	}
	t.Fatal("tables still due after 100 batches")
}

// TestResultsAdoptedAfterARestart (#1660): a front that starts adopts the
// jobs' tables already in resultsDataset, made by the front before it,
// oldest first with their creationTime, and not a scratch table nor the
// JOBS table; they are then cut to maxTables, and expired after a day, as
// the tables it makes are.
func TestResultsAdoptedAfterARestart(t *testing.T) {
	c := &expiryClock{t: time.Unix(1_000_000, 0)}
	emu := &resultTablesEmulator{c: c}
	q1 := newExpiryResults(emu, c)
	for _, id := range []string{"j1", "j2", "j3", "j4", "j5", "j6", "j7"} {
		runJobs(t, q1, id)
		c.t = c.t.Add(time.Minute)
	}
	emu.tables[scratchPrefix+"replace_0a1b"] = c.t.UnixMilli()
	emu.tables[jobsTableName] = c.t.UnixMilli()
	// The front restarts, before it deleted any: the new one knew none.
	q2 := newExpiryResults(emu, c)
	expireAll(t, q2, c)
	if got := emu.ids(); got != "[_cloudburrow_replace_0a1b cloudburrow_information_schema_jobs j1 j2 j3 j4 j5 j6 j7]" {
		t.Fatalf("before adopting: %s", got)
	}
	runJobs(t, q2, "k1")
	if err := q2.adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tb := range q2.expiry.tables {
		got = append(got, tb.job+"@"+strconv.FormatInt(tb.made.Sub(time.Unix(1_000_000, 0)).Milliseconds()/60000, 10))
	}
	if fmt.Sprint(got) != "[j1@0 j2@1 j3@2 j4@3 j5@4 j6@5 j7@6 k1@7]" {
		t.Fatalf("adopted (job@minute): %v", got)
	}
	if !q2.isMade("p") {
		t.Error("the dataset is not noted made")
	}
	// Adopting twice adds nothing.
	if err := q2.adopt(context.Background()); err != nil || len(q2.expiry.tables) != 8 {
		t.Fatalf("adopting again: %v, %d tables", err, len(q2.expiry.tables))
	}
	expireAll(t, q2, c)
	if got := emu.ids(); got != "[_cloudburrow_replace_0a1b cloudburrow_information_schema_jobs j4 j5 j6 j7 k1]" {
		t.Errorf("after the count cap of 5: %s", got)
	}
	c.t = c.t.Add(resultsMaxAge - 2*time.Minute) // j7 is a day old, k1 not
	expireAll(t, q2, c)
	if got := emu.ids(); got != "[_cloudburrow_replace_0a1b cloudburrow_information_schema_jobs k1]" {
		t.Errorf("a day after j7: %s", got)
	}
	if code, _ := do(t, q2, "GET", base+"/queries/j7", ""); code != 404 {
		t.Errorf("getQueryResults of an adopted job expired: %d, want 404", code)
	}
}

// TestResultsAdoptFails (#1660): an emulator that does not answer leaves
// nothing adopted, to try again.
func TestResultsAdoptFails(t *testing.T) {
	q := Results(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadGateway, "backendError", "no emulator")
	}))
	if err := q.adopt(context.Background()); err == nil {
		t.Error("adopted from an emulator that fails")
	}
	// A project with no resultsDataset has nothing to adopt.
	q = Results(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects" {
			writeJSON(w, 200, map[string]any{"projects": []any{map[string]any{"id": "p"}}})
			return
		}
		writeError(w, http.StatusNotFound, "notFound", "Not found: Dataset p:"+resultsDataset)
	}))
	if err := q.adopt(context.Background()); err != nil || len(q.expiry.tables) != 0 {
		t.Errorf("no dataset: %v, %v", err, q.expiry.tables)
	}
}

// TestResultsBoundedByRowsAndBytes (#1662): the tables' rows and bytes,
// read once each, are cut to maxRows and maxBytes, the oldest first and
// the newest kept, while idle; past hardRows or hardBytes, while busy too.
func TestResultsBoundedByRowsAndBytes(t *testing.T) {
	c := &expiryClock{t: time.Unix(1_000_000, 0)}
	emu := &resultTablesEmulator{c: c, rows: map[string]int64{"j1": 40, "j2": 40, "j3": 40, "j4": 40, "big": 500}}
	emu.schema = []field{{Name: "n", Type: "INT64"}, {Name: "s", Type: "STRING"}, {Name: "r", Type: "RECORD", Fields: []field{{Name: "b", Type: "BOOL"}}}}
	q := newExpiryResults(emu, c)
	q.expiry.maxTables, q.expiry.hardCap = 100, 100
	q.expiry.maxRows, q.expiry.hardRows = 100, 150
	q.expiry.maxBytes, q.expiry.hardBytes = 1<<40, 1<<40
	runJobs(t, q, "j1", "j2", "j3")
	q.sizeTables(context.Background())
	q.sizeTables(context.Background())
	if emu.gets != 3 {
		t.Errorf("tables.get %d times, want 3 (once each)", emu.gets)
	}
	if w := rowWidth(emu.schema); w != 8+resultsVarBytes+1 {
		t.Errorf("rowWidth %d", w)
	}
	if tb := q.expiry.tables[0]; !tb.sized || tb.rows != 40 || tb.bytes != 40*(8+resultsVarBytes+1) {
		t.Errorf("j1 sized %+v", tb)
	}
	// 120 rows of 100, not idle and not over 150: kept.
	if due := q.expiry.due(-1, false); len(due) != 0 || q.expiry.overCap() {
		t.Fatalf("120 rows, busy: due %v", due)
	}
	runJobs(t, q, "j4")
	q.sizeTables(context.Background())
	// 160 rows, over 150: due while busy, down to 100.
	if due := q.expiry.due(-1, false); len(due) != 2 || due[0].job != "j1" || due[1].job != "j2" || !q.expiry.overCap() {
		t.Fatalf("160 rows, busy: due %v", due)
	}
	expireAll(t, q, c)
	if got := emu.ids(); got != "[j3 j4]" {
		t.Errorf("after: %s", got)
	}
	// The newest is kept whatever its size.
	runJobs(t, q, "big")
	q.sizeTables(context.Background())
	expireAll(t, q, c)
	if got := emu.ids(); got != "[big]" {
		t.Errorf("after a table of 500 rows: %s", got)
	}
	// Bytes the same way.
	emu.rows = map[string]int64{"b1": 10, "b2": 10, "b3": 10}
	q.expiry.maxRows, q.expiry.hardRows = 1<<40, 1<<40
	q.expiry.maxBytes = 25 * (8 + resultsVarBytes + 1)
	runJobs(t, q, "b1", "b2", "b3")
	q.sizeTables(context.Background())
	expireAll(t, q, c)
	if got := emu.ids(); got != "[b2 b3]" {
		t.Errorf("after a byte budget of 25 rows: %s", got)
	}
}
