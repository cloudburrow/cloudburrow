package bigqueryfront

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// expiryEmulator is resultsEmulator that also answers tables.delete (204,
// or failing with fail) and jobs.getQueryResults (200), counting the
// deletions.
type expiryEmulator struct {
	resultsEmulator
	deleted []string
	fail    int
}

func (e *expiryEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/datasets/"+resultsDataset+"/tables/"):
		if e.fail != 0 {
			writeError(w, e.fail, "internalError", "failed")
			return
		}
		e.deleted = append(e.deleted, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/queries/"):
		writeJSON(w, 200, map[string]any{"kind": "bigquery#getQueryResultsResponse", "jobComplete": true})
	default:
		e.resultsEmulator.ServeHTTP(w, r)
	}
}

// expiryClock is a clock the test moves.
type expiryClock struct{ t time.Time }

func (c *expiryClock) now() time.Time { return c.t }

func newExpiryResults(emu http.Handler, c *expiryClock) *queryResults {
	q := Results(emu)
	q.expiry.now = c.now
	q.expiry.maxTables = 5
	q.expiry.hardCap = 10
	q.expiry.batch = 2
	return q
}

func runJobs(t *testing.T, q http.Handler, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if code, got := do(t, q, "POST", base+"/jobs", queryJob(id, "SELECT 1 AS x", "")); code != 200 {
			t.Fatalf("job %s: %d %v", id, code, got)
		}
	}
}

// TestResultsExpireAfterADay (#1059): a result table older than 24 hours
// is deleted, only while the instance is idle and in batches; the job's
// getQueryResults is then 404 notFound, a newer job's is answered.
func TestResultsExpireAfterADay(t *testing.T) {
	emu := &expiryEmulator{}
	c := &expiryClock{t: time.Unix(1_000_000, 0)}
	q := newExpiryResults(emu, c)
	runJobs(t, q, "old1", "old2", "old3")
	c.t = c.t.Add(12 * time.Hour)
	runJobs(t, q, "new1")
	if due := q.expiry.due(-1, false); len(due) != 0 {
		t.Fatalf("before a day: due %v", due)
	}
	c.t = c.t.Add(12*time.Hour + time.Second) // old* are a day old, new1 half a day
	// Not idle: a request is in flight.
	q.expiry.begin()
	if due := q.expiry.due(-1, false); len(due) != 0 {
		t.Errorf("with a request in flight: due %v", due)
	}
	q.expiry.end()
	if due := q.expiry.due(-1, false); len(due) != 0 {
		t.Errorf("a request just now: due %v", due)
	}
	c.t = c.t.Add(resultsIdle)
	due := q.expiry.due(-1, false)
	if len(due) != 2 || due[0].job != "old1" || due[1].job != "old2" {
		t.Fatalf("idle: due %v, want old1 and old2 (a batch of 2)", due)
	}
	q.deleteTables(context.Background(), due, true)
	q.deleteTables(context.Background(), q.expiry.due(-1, false), true)
	if fmt.Sprint(emu.deleted) != "[old1 old2 old3]" {
		t.Errorf("deleted %v, want [old1 old2 old3]", emu.deleted)
	}
	if due := q.expiry.due(-1, false); len(due) != 0 {
		t.Errorf("after: due %v", due)
	}
	for _, c := range []struct {
		job  string
		code int
	}{{"old1", 404}, {"old3", 404}, {"new1", 200}, {"other", 200}} {
		code, got := do(t, q, "GET", base+"/queries/"+c.job, "")
		if code != c.code {
			t.Errorf("getQueryResults %s: %d %v, want %d", c.job, code, got, c.code)
		}
		if c.code == 404 {
			e, _ := got["error"].(map[string]any)
			errs, _ := e["errors"].([]any)
			first, _ := errs[0].(map[string]any)
			if first["reason"] != "notFound" || !strings.Contains(fmt.Sprint(e["message"]), "Not found: Table p:"+resultsDataset+"."+c.job) {
				t.Errorf("getQueryResults %s: %v", c.job, got)
			}
		}
	}
}

// TestResultsExpireOverTheCap (#1059): more than maxTables tables are cut
// to maxTables, oldest first, while idle; more than hardCap are cut
// without waiting for idle time; a batch stops at a request.
func TestResultsExpireOverTheCap(t *testing.T) {
	emu := &expiryEmulator{}
	c := &expiryClock{t: time.Unix(1_000_000, 0)}
	q := newExpiryResults(emu, c)
	runJobs(t, q, "j1", "j2", "j3", "j4", "j5", "j6", "j7")
	if due := q.expiry.due(-1, false); len(due) != 0 {
		t.Fatalf("not idle, 7 of a cap of 5: due %v", due)
	}
	c.t = c.t.Add(resultsIdle)
	if due := q.expiry.due(-1, false); len(due) != 2 || due[0].job != "j1" || due[1].job != "j2" {
		t.Fatalf("idle, 7 of 5: due %v, want j1, j2", due)
	}
	runJobs(t, q, "j8", "j9", "j10", "j11") // 11 > 2×5
	if due := q.expiry.due(-1, false); len(due) != 2 || due[0].job != "j1" {
		t.Errorf("not idle, 11 of 5: due %v, want a batch of 2 anyway", due)
	}
	if !q.expiry.overCap() {
		t.Error("11 of 5: not over the cap")
	}
	// A batch stops at a request that comes in.
	busy := &expiryEmulator{}
	q2 := newExpiryResults(busy, c)
	runJobs(t, q2, "a", "b")
	stop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		busy.ServeHTTP(w, r)
		c.t = c.t.Add(time.Second)
		q2.expiry.begin() // a client's request arrives
		q2.expiry.end()
	})
	q2.next = stop
	c.t = c.t.Add(time.Minute)
	if n := q2.deleteTables(context.Background(), q2.expiry.due(0, true), true); n != 1 {
		t.Errorf("deleted %d with a request after the first, want 1", n)
	}
}

// TestExpireResultsPath (#1059): the front's path deletes all but the
// newest keep at once; a deletion the emulator fails stops it and leaves
// the job readable.
func TestExpireResultsPath(t *testing.T) {
	emu := &expiryEmulator{}
	c := &expiryClock{t: time.Unix(1_000_000, 0)}
	q := newExpiryResults(emu, c)
	runJobs(t, q, "j1", "j2", "j3")
	if code, got := do(t, q, "GET", ExpireResultsPath, ""); code != 405 {
		t.Errorf("GET: %d %v", code, got)
	}
	if code, got := do(t, q, "POST", ExpireResultsPath+"?keep=x", ""); code != 400 {
		t.Errorf("keep=x: %d %v", code, got)
	}
	code, got := do(t, q, "POST", ExpireResultsPath+"?keep=1", "")
	if code != 200 || got["deleted"] != float64(2) || got["kept"] != float64(1) {
		t.Errorf("keep=1: %d %v", code, got)
	}
	if code, _ := do(t, q, "GET", base+"/queries/j2", ""); code != 404 {
		t.Errorf("j2 after: %d", code)
	}
	if code, _ := do(t, q, "GET", base+"/queries/j3", ""); code != 200 {
		t.Errorf("j3 after: %d", code)
	}
	runJobs(t, q, "j4", "j5")
	if code, got := do(t, q, "POST", ExpireResultsPath+"?job=j4&job=nope", ""); code != 200 || got["deleted"] != float64(1) || got["kept"] != float64(2) {
		t.Errorf("job=j4: %d %v", code, got)
	}
	if code, _ := do(t, q, "GET", base+"/queries/j4", ""); code != 404 {
		t.Errorf("j4 after: %d", code)
	}
	do(t, q, "POST", ExpireResultsPath+"?keep=1", "") // j5 kept
	emu.fail = 500
	if code, got := do(t, q, "POST", ExpireResultsPath, ""); code != 200 || got["deleted"] != float64(0) || got["kept"] != float64(1) {
		t.Errorf("failing: %d %v", code, got)
	}
	if code, _ := do(t, q, "GET", base+"/queries/j5", ""); code != 200 {
		t.Errorf("j5 after a failed deletion: %d", code)
	}
	// A restart forgets them: a job of an old ID is answered by the emulator.
	q.reset()
	if code, _ := do(t, q, "GET", base+"/queries/j1", ""); code != 200 {
		t.Errorf("j1 after a restart: %d", code)
	}
}

// TestScratchTablesInResultsDataset (#1057): a request that makes a
// scratch table in resultsDataset has the dataset made first; the
// front's tables.delete of one is answered 204 and carried out at the
// next idle time, before the query results due, once.
func TestScratchTablesInResultsDataset(t *testing.T) {
	emu := &expiryEmulator{}
	c := &expiryClock{t: time.Unix(1_000_000, 0)}
	q := newExpiryResults(emu, c)
	scratch := "_cloudburrow_replace_0123abcd"
	body := `{"query":"CREATE TABLE ` + "`" + resultsDataset + "." + scratch + "`" + ` AS SELECT 1 AS a"}`
	do(t, q, "POST", base+"/queries", body)
	if n := emu.datasetInserts(); n != 1 || !emu.datasets[resultsDataset] {
		t.Fatalf("before the scratch table: the dataset made %d times", n)
	}
	emu.log = nil
	do(t, q, "POST", base+"/queries", `{"query":"SELECT 1"}`)
	do(t, q, "POST", base+"/datasets/"+resultsDataset+"/tables", `{"tableReference":{"tableId":"`+scratch+`2"}}`)
	if n := emu.datasetInserts(); n != 0 {
		t.Errorf("made again: %d", n)
	}
	for _, name := range []string{scratch, scratch, scratch + "2"} {
		if code, _ := do(t, q, "DELETE", base+"/datasets/"+resultsDataset+"/tables/"+name, ""); code != 204 {
			t.Errorf("tables.delete of %s: %d", name, code)
		}
	}
	if len(emu.deleted) != 0 {
		t.Fatalf("deleted at once: %v", emu.deleted)
	}
	// A table of a client's in the dataset is deleted at once.
	if code, _ := do(t, q, "DELETE", base+"/datasets/"+resultsDataset+"/tables/mine", ""); code != 204 || fmt.Sprint(emu.deleted) != "[mine]" {
		t.Errorf("a client's table: %d, deleted %v", code, emu.deleted)
	}
	emu.deleted = nil
	runJobs(t, q, "j1")
	if due := q.expiry.due(-1, false); len(due) != 0 {
		t.Errorf("not idle: due %v", due)
	}
	c.t = c.t.Add(resultsIdle)
	q.deleteTables(context.Background(), q.expiry.due(-1, false), true)
	if fmt.Sprint(emu.deleted) != "["+scratch+" "+scratch+"2]" {
		t.Errorf("idle: deleted %v, want the two scratch tables, and not j1", emu.deleted)
	}
	if code, _ := do(t, q, "GET", base+"/queries/"+scratch, ""); code != 200 {
		t.Errorf("a scratch table's name is no expired job: %d", code)
	}
}

// TestUnscratch (#1057): an error naming a scratch table in
// resultsDataset names the client's dataset instead.
func TestUnscratch(t *testing.T) {
	got := string(unscratch([]byte("Already Exists: Table p:"+resultsDataset+".s1; "+resultsDataset+".s1"), "s1", "ds"))
	if got != "Already Exists: Table p:ds.s1; ds.s1" {
		t.Errorf("got %q", got)
	}
	if got := string(unscratch([]byte("x "+resultsDataset+".s1"), "s1", "")); got != "x s1" {
		t.Errorf("without a dataset: %q", got)
	}
}

// TestScratchFunctionDroppedLater (#1057): the front's DROP FUNCTION of a
// scratch function in resultsDataset is answered at once and sent at the
// next idle time; other DROP FUNCTIONs are sent as they are.
func TestScratchFunctionDroppedLater(t *testing.T) {
	emu := &expiryEmulator{}
	c := &expiryClock{t: time.Unix(1_000_000, 0)}
	q := newExpiryResults(emu, c)
	drop := "DROP FUNCTION IF EXISTS `p`.`" + resultsDataset + "`.`_cloudburrow_replace_00ff`"
	body := func(sql string) string { b, _ := json.Marshal(map[string]any{"query": sql}); return string(b) }
	if code, got := do(t, q, "POST", base+"/queries", body(drop)); code != 200 || got["jobComplete"] != true {
		t.Errorf("the scratch DROP FUNCTION: %d %v", code, got)
	}
	do(t, q, "POST", base+"/queries", body("DROP FUNCTION IF EXISTS `ds`.`f`"))
	if n := len(emu.log); n != 1 || !strings.Contains(emu.log[0], "`ds`.`f`") {
		t.Fatalf("sent %v, want only the client's DROP FUNCTION", emu.log)
	}
	emu.log = nil
	c.t = c.t.Add(resultsIdle)
	q.deleteTables(context.Background(), q.expiry.due(-1, false), true)
	if len(emu.log) != 1 || !strings.HasPrefix(emu.log[0], "POST "+base+"/queries ") || !strings.Contains(emu.log[0], "_cloudburrow_replace_00ff") {
		t.Errorf("idle: sent %v, want the DROP FUNCTION", emu.log)
	}
	if due := q.expiry.due(-1, false); len(due) != 0 {
		t.Errorf("after: due %v", due)
	}
}

// TestLivenessProbeIsIdle (#1059): the emulator's liveness probe, which
// the front answers every 5 s, is no client request: the instance stays
// idle for the expiry.
func TestLivenessProbeIsIdle(t *testing.T) {
	emu := &expiryEmulator{}
	c := &expiryClock{t: time.Unix(1_000_000, 0)}
	q := newExpiryResults(emu, c)
	runJobs(t, q, "j1", "j2", "j3", "j4", "j5", "j6")
	c.t = c.t.Add(resultsIdle)
	do(t, q, "GET", EngineLivenessPath, "")
	if due := q.expiry.due(-1, false); len(due) != 1 || due[0].job != "j1" {
		t.Errorf("after a probe: due %v, want j1", due)
	}
}
