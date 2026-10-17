package bigqueryfront

import (
	"encoding/json"
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
// changed since, and deletes those of jobs gone; a filtered one reads a
// literal of the jobs it keeps.
func TestJobsTableWritesOnlyNewRows(t *testing.T) {
	emu := tableEmulator{&jobsEmulator{tables: map[string]string{}}}
	h := Wrap(emu)
	for _, q := range []string{"SELECT 1", "SELECT 2", "SELECT 3"} {
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+q+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", q, code, got)
		}
	}
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
	sent = run(count + " WHERE job_id = 'auto1'")
	if len(sent) != 1 || strings.Contains(sent[0], table) || !strings.Contains(sent[0], "UNNEST(ARRAY<") {
		t.Fatalf("filtered JOBS query sent %q, want a literal", sent)
	}
	// A job that changed, and one that is gone.
	emu.jobs["auto1"]["status"] = map[string]any{"state": "DONE", "errorResult": map[string]any{"reason": "x", "message": "y"}}
	delete(emu.jobs, "auto2")
	emu.order = append(emu.order[:1], emu.order[2:]...)
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
// left to the engine; one that keeps fewer reads a literal of them.
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
	for _, where := range []string{" WHERE job_type = 'QUERY'", " WHERE creation_time > TIMESTAMP '2000-01-01'"} {
		sent := run(count + where)
		last := sent[len(sent)-1]
		if last != "SELECT COUNT(*) FROM (SELECT * FROM "+table+") AS JOBS"+where {
			t.Fatalf("%s: sent %q, want the table read", where, sent)
		}
	}
	sent := run(count + " WHERE job_id IN ('auto1', 'auto2')")
	if len(sent) != 1 || strings.Contains(sent[0], table) || !strings.Contains(sent[0], "UNNEST(ARRAY<") {
		t.Fatalf("JOBS query of two jobs sent %q, want a literal", sent)
	}
}

// TestJobsTableBackgroundWaitsAfterASlowSync (#1525): a background sync
// runs no sooner than jobsTableIdle times as long as the last one took
// after it ended, so syncs that list many jobs never run back to back.
func TestJobsTableBackgroundWaitsAfterASlowSync(t *testing.T) {
	saved := jobsTableDelay
	jobsTableDelay = time.Millisecond
	t.Cleanup(func() { jobsTableDelay = saved })
	var j jobsTable
	j.setMade("p", true)
	const took = 40 * time.Millisecond
	ran := make(chan [2]time.Time, 2)
	sync := func() {
		start := time.Now()
		time.Sleep(took)
		ran <- [2]time.Time{start, time.Now()}
	}
	j.soon("p", sync)
	first := <-ran
	time.Sleep(5 * time.Millisecond) // the first sync's wait is recorded
	j.soon("p", sync)
	second := <-ran
	if gap := second[0].Sub(first[1]); gap < jobsTableIdle*took {
		t.Errorf("the second sync started %v after the first ended, want at least %v", gap, jobsTableIdle*took)
	}
}
