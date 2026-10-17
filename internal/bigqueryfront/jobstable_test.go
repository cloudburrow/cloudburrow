package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() {
	// The fake emulators of this package's tests serve one request at a
	// time: no background sync (jobstable.go, #1419) runs in them, but in
	// TestJobsTableSyncsInTheBackground, which sets it short.
	jobsTableDelay = time.Hour
}

// tableEmulator is a jobsEmulator that makes datasets.
type tableEmulator struct{ *jobsEmulator }

func (e tableEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/datasets") {
		e.log = append(e.log, "datasets")
		w.WriteHeader(http.StatusConflict)
		return
	}
	e.jobsEmulator.ServeHTTP(w, r)
}

// sentSince returns the query texts sent from the log's entry from on.
func (e tableEmulator) sentSince(from int) []string {
	var out []string
	for _, l := range e.log[from:] {
		var body struct {
			Query string `json:"query"`
		}
		if _, b, ok := strings.Cut(l, " "); ok && json.Unmarshal([]byte(b), &body) == nil {
			out = append(out, body.Query)
		}
	}
	return out
}

// TestJobsTableWritesOnlyNewRows (#1378): an unfiltered JOBS query makes
// the table of rows and reads it; the next writes only the rows new or
// changed since, and deletes those of jobs gone; a filtered one then
// reads the table too (#1564).
func TestJobsTableWritesOnlyNewRows(t *testing.T) {
	emu := tableEmulator{&jobsEmulator{tables: map[string]string{}}}
	h := Wrap(emu)
	for _, q := range []string{"SELECT 1", "SELECT 2", "SELECT 3"} {
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+q+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", q, code, got)
		}
	}
	// auto1 is still running when its row is first written.
	emu.jobs["auto1"]["status"] = map[string]any{"state": "RUNNING"}
	const count = "SELECT COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS"
	table := "`" + resultsDataset + "`.`" + jobsTableName + "`"
	run := func(sql string) []string {
		t.Helper()
		from := len(emu.log)
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+sql+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", sql, code, got)
		}
		return emu.sentSince(from)
	}
	rowsIn := func(insert string) int { return strings.Count(insert, "'auto") }

	sent := run(count)
	if len(sent) != 4 || sent[0] != "DROP TABLE IF EXISTS "+table || !strings.HasPrefix(sent[1], "CREATE TABLE "+table+" (cache_hit BOOL") ||
		!strings.HasPrefix(sent[2], "INSERT INTO "+table+" SELECT * FROM (SELECT * FROM UNNEST(") || rowsIn(sent[2]) != 3 ||
		sent[3] != "SELECT COUNT(*) FROM (SELECT * FROM "+table+") AS JOBS" {
		t.Fatalf("first JOBS query sent %q", sent)
	}
	sent = run(count)
	if len(sent) != 2 || !strings.HasPrefix(sent[0], "INSERT INTO "+table) || rowsIn(sent[0]) != 1 || !strings.Contains(sent[1], table) {
		t.Fatalf("second JOBS query sent %q, want the one new row written", sent)
	}
	// Once the table is made, a filtered query reads it too, brought up
	// to date as an unfiltered one does (#1564).
	sent = run(count + " WHERE job_id = 'auto1'")
	if len(sent) != 2 || !strings.HasPrefix(sent[0], "INSERT INTO "+table) || rowsIn(sent[0]) != 1 ||
		sent[1] != "SELECT COUNT(*) FROM (SELECT * FROM "+table+") AS JOBS WHERE job_id = 'auto1'" {
		t.Fatalf("filtered JOBS query sent %q, want the new row written and the table read", sent)
	}
	// A job that finished since (its row was not DONE), and one deleted
	// through the front (noteMade): the query lists only those and the
	// new ones (#1559).
	emu.jobs["auto1"]["status"] = map[string]any{"state": "DONE", "errorResult": map[string]any{"reason": "x", "message": "y"}}
	delete(emu.jobs, "auto2")
	emu.order = append(emu.order[:1], emu.order[2:]...)
	do(t, h, "DELETE", base+"/jobs/auto2/delete", "")
	sent = run(count)
	if len(sent) != 3 || !strings.HasPrefix(sent[0], "DELETE FROM "+table+" WHERE job_id IN (") ||
		!strings.Contains(sent[0], "'auto1'") || !strings.Contains(sent[0], "'auto2'") ||
		!strings.HasPrefix(sent[1], "INSERT INTO "+table) || !strings.Contains(sent[1], "'y'") {
		t.Fatalf("JOBS query after a change sent %q", sent)
	}
}

// lockedEmulator serves one request at a time, for a test whose front
// sends requests in the background.
type lockedEmulator struct {
	mu sync.Mutex
	e  tableEmulator
	h  http.Handler // when set, served in place of e
}

func (l *lockedEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.h != nil {
		l.h.ServeHTTP(w, r)
		return
	}
	l.e.ServeHTTP(w, r)
}

// TestJobsTableSyncsInTheBackground (#1419): once a project's table of JOBS
// rows is made, the jobs made after it are written to it in the background,
// so the next unfiltered JOBS query writes nothing before reading it.
func TestJobsTableSyncsInTheBackground(t *testing.T) {
	saved := jobsTableDelay
	jobsTableDelay = 10 * time.Millisecond
	t.Cleanup(func() { jobsTableDelay = saved })
	emu := &lockedEmulator{e: tableEmulator{&jobsEmulator{tables: map[string]string{}}}}
	h := Wrap(emu)
	query := func(sql string) {
		t.Helper()
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+sql+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", sql, code, got)
		}
	}
	const count = "SELECT COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS"
	table := "`" + resultsDataset + "`.`" + jobsTableName + "`"
	query("SELECT 1")
	query(count) // makes the table
	for _, q := range []string{"SELECT 4", "SELECT 5", "SELECT 6"} {
		query(q)
	}
	written := func() bool {
		emu.mu.Lock()
		defer emu.mu.Unlock()
		for _, q := range emu.e.sentSince(0) {
			if strings.HasPrefix(q, "INSERT INTO "+table) && strings.Contains(q, "SELECT 6") {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(5 * time.Second); !written(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the jobs made after the table were not written to it in the background")
		}
	}
	emu.mu.Lock()
	from := len(emu.e.log)
	emu.mu.Unlock()
	query(count)
	emu.mu.Lock()
	sent := emu.e.sentSince(from)
	emu.mu.Unlock()
	if len(sent) != 1 || sent[0] != "SELECT COUNT(*) FROM (SELECT * FROM "+table+") AS JOBS" {
		t.Fatalf("the JOBS query after the background sync sent %q, want only its read of the table", sent)
	}
}

// TestJobsTableBackgroundMakesNoTable (#1419): a project whose table is not
// made gets none from the background.
func TestJobsTableBackgroundMakesNoTable(t *testing.T) {
	saved := jobsTableDelay
	jobsTableDelay = time.Millisecond
	t.Cleanup(func() { jobsTableDelay = saved })
	emu := &lockedEmulator{e: tableEmulator{&jobsEmulator{tables: map[string]string{}}}}
	h := Wrap(emu)
	for _, q := range []string{"SELECT 1", "SELECT 2"} {
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+q+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", q, code, got)
		}
	}
	time.Sleep(50 * time.Millisecond)
	emu.mu.Lock()
	defer emu.mu.Unlock()
	for _, q := range emu.e.sentSince(0) {
		if strings.Contains(q, jobsTableName) {
			t.Fatalf("sent %q for a project that read no JOBS", q)
		}
	}
}

// TestJobsTableReadByFilteredQueries (#1515): a filtered JOBS query that
// keeps more than jobsLiteralMax jobs reads the table of rows, its WHERE
// left to the engine; one that keeps fewer reads a literal of them, until
// the table is made (#1564).
func TestJobsTableReadByFilteredQueries(t *testing.T) {
	saved := jobsLiteralMax
	jobsLiteralMax = 2
	t.Cleanup(func() { jobsLiteralMax = saved })
	emu := tableEmulator{&jobsEmulator{tables: map[string]string{}}}
	h := Wrap(emu)
	for _, q := range []string{"SELECT 1", "SELECT 2", "SELECT 3"} {
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+q+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", q, code, got)
		}
	}
	table := "`" + resultsDataset + "`.`" + jobsTableName + "`"
	run := func(sql string) []string {
		t.Helper()
		from := len(emu.log)
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+sql+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", sql, code, got)
		}
		return emu.sentSince(from)
	}
	const count = "SELECT COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS"
	// Before the table is made, a query that keeps few jobs reads a
	// literal of them.
	sent := run(count + " WHERE job_id IN ('auto1', 'auto2')")
	if len(sent) != 1 || strings.Contains(sent[0], table) || !strings.Contains(sent[0], "UNNEST(ARRAY<") {
		t.Fatalf("JOBS query of two jobs sent %q, want a literal", sent)
	}
	for _, where := range []string{" WHERE job_type = 'QUERY'", " WHERE creation_time > TIMESTAMP '2000-01-01'"} {
		sent := run(count + where)
		last := sent[len(sent)-1]
		if last != "SELECT COUNT(*) FROM (SELECT * FROM "+table+") AS JOBS"+where {
			t.Fatalf("%s: sent %q, want the table read", where, sent)
		}
	}
	// Once it is, the table, whatever the query keeps (#1564): a literal
	// is cut from a list of every job.
	sent = run(count + " WHERE job_id IN ('auto1', 'auto2')")
	if last := sent[len(sent)-1]; last != "SELECT COUNT(*) FROM (SELECT * FROM "+table+") AS JOBS WHERE job_id IN ('auto1', 'auto2')" {
		t.Fatalf("JOBS query of two jobs sent %q, want the table read", sent)
	}
}

// TestJobsTableSyncsIncrementally (#1528): a background sync lists only
// the jobs made since the last and those not DONE then (bigquery-emulator
// patch 0064's parameters), never every job, and leaves the table as a
// full rebuild writes it: after jobs made, one RUNNING that then finished, and one
// deleted, the next unfiltered JOBS query, which lists every job and
// writes every row that differs, writes nothing.
func TestJobsTableSyncsIncrementally(t *testing.T) {
	saved := jobsTableDelay
	jobsTableDelay = 10 * time.Millisecond
	t.Cleanup(func() { jobsTableDelay = saved })
	emu := &lockedEmulator{e: tableEmulator{&jobsEmulator{tables: map[string]string{}}}}
	var full, some, uncut int // jobs.list of every job, of some, of some with no cursor
	marked := false
	emu.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs") {
			if r.URL.Query().Has("cloudburrowAfterJobId") {
				some++
				if r.URL.Query().Get("cloudburrowAfterJobId") == "" {
					uncut++
				}
			} else {
				full++
			}
		}
		emu.e.ServeHTTP(w, r)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/queries") {
			for _, job := range emu.e.jobs {
				conf, _ := job["configuration"].(map[string]any)
				qc, _ := conf["query"].(map[string]any)
				if qc["query"] == "SELECT 'running'" && !marked {
					job["status"] = map[string]any{"state": "RUNNING"}
					marked = true
				}
			}
		}
	})
	h := Wrap(emu)
	query := func(sql string) {
		t.Helper()
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+sql+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", sql, code, got)
		}
	}
	const count = "SELECT COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS"
	table := "`" + resultsDataset + "`.`" + jobsTableName + "`"
	query("SELECT 1")
	query("SELECT 2")
	query(count) // makes the table
	locked := func(f func()) {
		emu.mu.Lock()
		defer emu.mu.Unlock()
		f()
	}
	waitFor := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			ok := false
			locked(func() { ok = done() })
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("no background sync %s", what)
			}
		}
	}
	insertOf := func(text string) func() bool {
		return func() bool {
			for _, q := range emu.e.sentSince(0) {
				if strings.HasPrefix(q, "INSERT INTO "+table) && strings.Contains(q, text) {
					return true
				}
			}
			return false
		}
	}
	locked(func() { full, some = 0, 0 })
	query("SELECT 'running'")
	query("SELECT 4")
	waitFor("wrote the new jobs", insertOf("SELECT 4"))
	var running string
	locked(func() {
		if full != 0 || some == 0 || uncut != 0 {
			t.Errorf("the background sync sent %d jobs.list of every job and %d of some, %d with no job to list after, want only some after one", full, some, uncut)
		}
		for id, job := range emu.e.jobs {
			if job["status"].(map[string]any)["state"] == "RUNNING" {
				running = id
				job["status"] = map[string]any{"state": "DONE", "errorResult": map[string]any{"reason": "stopped", "message": "finished late"}}
			}
		}
		// A job deleted (jobs.delete).
		delete(emu.e.jobs, "auto2")
		for i, id := range emu.e.order {
			if id == "auto2" {
				emu.e.order = append(emu.e.order[:i:i], emu.e.order[i+1:]...)
				break
			}
		}
	})
	if running == "" {
		t.Fatal("no job was RUNNING when the background synced")
	}
	if code, got := do(t, h, "DELETE", base+"/jobs/auto2/delete", ""); code/100 != 2 {
		t.Fatalf("jobs.delete: %d %v", code, got)
	}
	query("SELECT 5")
	waitFor("wrote the job that finished", insertOf("finished late"))
	waitFor("deleted the job gone", func() bool {
		for _, q := range emu.e.sentSince(0) {
			if strings.HasPrefix(q, "DELETE FROM "+table) && strings.Contains(q, "'auto2'") {
				return true
			}
		}
		return false
	})
	waitFor("wrote the last job", insertOf("SELECT 5"))
	from := 0
	locked(func() { from = len(emu.e.log) })
	query(count)
	var sent []string
	locked(func() { sent = emu.e.sentSince(from) })
	if len(sent) != 1 || sent[0] != "SELECT COUNT(*) FROM (SELECT * FROM "+table+") AS JOBS" {
		t.Fatalf("the JOBS query after the background syncs sent %q, want only its read of the table: its rows differ from a full rebuild", sent)
	}
}

// TestJobsTableStatementsAreNoJobsAfterARestart (#1553): the statements
// that write the table of JOBS rows are the front's own, and are no
// client's jobs, in jobs.list or in JOBS, once the front has restarted
// too, when it no longer remembers running them; each was a row of the
// next INSERT, whose text was written into the INSERT after it, until a
// JOBS query no longer fit the engine.
func TestJobsTableStatementsAreNoJobsAfterARestart(t *testing.T) {
	emu := tableEmulator{&jobsEmulator{tables: map[string]string{}}}
	for _, q := range []string{"SELECT 1", "SELECT 2"} {
		if code, got := do(t, Wrap(emu), "POST", base+"/queries", `{"query":"`+q+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", q, code, got)
		}
	}
	const count = "SELECT COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS"
	table := "`" + resultsDataset + "`.`" + jobsTableName + "`"
	if code, got := do(t, Wrap(emu), "POST", base+"/queries", `{"query":"`+count+`"}`); code != 200 {
		t.Fatalf("JOBS: %d %v", code, got)
	}
	h := Wrap(emu) // a front that has restarted
	code, got := do(t, h, "GET", base+"/jobs?allUsers=true&projection=full", "")
	if code != 200 {
		t.Fatalf("jobs.list: %d %v", code, got)
	}
	jobs, _ := got["jobs"].([]any)
	for _, j := range jobs {
		job, _ := j.(map[string]any)
		if internalText(job) {
			t.Errorf("jobs.list listed the front's own %v", job["configuration"])
		}
	}
	if len(jobs) != 3 { // the two SELECTs and the JOBS query
		t.Errorf("jobs.list listed %d jobs, want 3", len(jobs))
	}
	from := len(emu.log)
	if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+count+`"}`); code != 200 {
		t.Fatalf("JOBS after the restart: %d %v", code, got)
	}
	for _, s := range emu.sentSince(from) {
		if strings.HasPrefix(s, "INSERT INTO "+table) && strings.Contains(s, "INSERT INTO "+strings.ReplaceAll(table, "`", `\`+"`")) {
			t.Errorf("the INSERT after the restart carries an INSERT's text: %.200s", s)
		}
		if strings.HasPrefix(s, "INSERT INTO "+table) && strings.Count(s, "'auto") != 3 {
			t.Errorf("the INSERT after the restart wrote %d rows, want 3: %.300s", strings.Count(s, "'auto"), s)
		}
	}
}

// TestJobsBatchBoundsAnInsert (#1553): an INSERT of the table writes at
// most jobsTableBatch rows and, past the first, jobsTableBatchBytes of
// them.
func TestJobsBatchBoundsAnInsert(t *testing.T) {
	small := make([]string, 2500)
	for i := range small {
		small[i] = "(x)"
	}
	big := strings.Repeat("x", jobsTableBatchBytes/3)
	for _, c := range []struct {
		rows []string
		want int
	}{
		{nil, 0},
		{small, jobsTableBatch},
		{[]string{big, big, big, big, big}, 3},
		{[]string{strings.Repeat("x", 2*jobsTableBatchBytes), "(x)"}, 1},
	} {
		if got := jobsBatch(c.rows); got != c.want {
			t.Errorf("jobsBatch of %d rows: %d, want %d", len(c.rows), got, c.want)
		}
	}
}

// TestJobsTableOverflowListsEveryJob (#1559): once pending has dropped a
// change (jobsPendingMax), the next query of every job lists them all.
func TestJobsTableOverflowListsEveryJob(t *testing.T) {
	var j jobsTable
	if j.incremental("p") {
		t.Fatal("a table not made is synced incrementally")
	}
	j.setMade("p", true)
	if !j.incremental("p") {
		t.Fatal("a made table is not synced incrementally")
	}
	for i := range jobsPendingMax + 1 {
		j.noteMade("p", fmt.Sprint("job", i))
	}
	if j.incremental("p") {
		t.Fatal("a table whose pending overflowed is synced incrementally")
	}
	if !j.incremental("p") {
		t.Fatal("the overflow outlived the query that lists every job")
	}
}
