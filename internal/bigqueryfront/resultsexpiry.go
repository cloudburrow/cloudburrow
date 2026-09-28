package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Expiring query results (#1059).
//
// BigQuery keeps the temporary table a query job's result is written to
// for up to 24 hours after the query, best-effort, and "may be
// invalidated sooner":
// https://cloud.google.com/bigquery/docs/writing-results#temporary_and_permanent_tables
// https://cloud.google.com/bigquery/docs/cached-results
//
// The front writes those results to tables in resultsDataset (results.go)
// and, since this, deletes a table once it is older than
// resultsMaxAge, or once there are more than resultsMaxTables of them
// (the oldest first). Deleting one is a DROP TABLE, which the pinned
// emulator's SQL engine answers by building every catalog again
// (googlesqlite v0.3.1, internal/catalog.go, resetCatalog), so the
// deletions are made in batches of at most resultsBatch while the
// instance is idle (no request for resultsIdle, and none in flight), and
// a batch stops at the first request that comes in; only past
// resultsHardCap tables are they made without waiting for idle time, and
// not stopped by requests. The cost of a DROP TABLE grows with the tables
// the engine has: measured with scripts/bigquery-engine-soak.sh, about
// 0.5 s with 300 result tables and 4 s with 1,000, the memory flat;
// deleting them while busy, past 400, made 600 query jobs (a CREATE and a
// DROP TABLE every 5) take 454 s rather than 308 s (docs/compatibility.md,
// "Query results").
//
// A job whose table was deleted is then answered as BigQuery answers a
// reference to a table that does not exist, 404 notFound
// (https://cloud.google.com/bigquery/docs/error-messages), at
// jobs.getQueryResults (which the emulator would otherwise answer from the
// job it keeps) and, by the emulator, at tabledata.list; jobs.get still
// answers the job, which still names its destination table, as BigQuery's
// does.
//
// ExpireResultsPath does the same at once, for a test or to free the
// engine before a long run: a POST with ?job=ID (repeated) deletes those
// jobs' tables, else ?keep=N (default 0) all but the newest N; it answers
// {"deleted":D,"kept":K}.

// ExpireResultsPath is the front's path that deletes the query results
// but the newest ?keep= of them at once (above).
const ExpireResultsPath = "/cloudburrow/bigquery-expire-query-results"

const (
	resultsMaxAge    = 24 * time.Hour
	resultsMaxTables = 200
	// resultsHardCap is how many tables make the front delete while busy.
	resultsHardCap = 1000
	resultsBatch   = 20
	resultsIdle    = 30 * time.Second
	resultsTick    = 5 * time.Second
	// resultsForgetAfter bounds the expired jobs remembered, per front.
	resultsForgetAfter = 100000
)

// resultTable is one table in resultsDataset: a job's (job is its ID), or
// a scratch table (job is its name).
type resultTable struct {
	project, job, base string
	made               time.Time
	scratch            bool
	// drop is the statement that drops a scratch function, or "".
	drop string
}

// resultsExpiry is what queryResults keeps to expire its tables.
type resultsExpiry struct {
	mu        sync.Mutex
	now       func() time.Time
	maxAge    time.Duration
	maxTables int
	hardCap   int
	batch     int
	idle      time.Duration
	// tables are the tables made, oldest first.
	tables []resultTable
	// expired are the jobs whose table was deleted, by project and job,
	// and in the order they were (to forget the oldest).
	expired      map[string]bool
	expiredOrder []string
	inflight     int
	last         time.Time
}

func (e *resultsExpiry) init() {
	if e.now == nil {
		e.now = time.Now
	}
	if e.maxAge == 0 {
		e.maxAge = resultsMaxAge
	}
	if e.maxTables == 0 {
		e.maxTables = resultsMaxTables
	}
	if e.hardCap == 0 {
		e.hardCap = max(resultsHardCap, e.maxTables)
	}
	if e.batch == 0 {
		e.batch = resultsBatch
	}
	if e.idle == 0 {
		e.idle = resultsIdle
	}
}

func expiryKey(project, job string) string { return project + "\x00" + job }

// begin and end bracket a request, for idleness.
func (e *resultsExpiry) begin() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	e.inflight++
	e.last = e.now()
}

func (e *resultsExpiry) end() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inflight--
	e.last = e.now()
}

// made notes a job's table written, or (scratch) a scratch table the
// front is done with.
func (e *resultsExpiry) made(project, job, base string, scratch bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	if !scratch {
		delete(e.expired, expiryKey(project, job))
	} else {
		for _, t := range e.tables {
			if t.scratch && t.project == project && t.job == job {
				return // deleted twice
			}
		}
	}
	e.tables = append(e.tables, resultTable{project: project, job: job, base: base, made: e.now(), scratch: scratch})
}

// dropLater notes a scratch function the front is done with, which drop
// drops.
func (e *resultsExpiry) dropLater(project, name, base, drop string) {
	e.made(project, name, base, true)
	e.mu.Lock()
	defer e.mu.Unlock()
	if n := len(e.tables); n > 0 && e.tables[n-1].job == name {
		e.tables[n-1].drop = drop
	}
}

// isExpired reports whether the job's table was deleted.
func (e *resultsExpiry) isExpired(project, job string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.expired[expiryKey(project, job)]
}

// reset forgets every table: the emulator restarted, empty.
func (e *resultsExpiry) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tables = nil
	e.expired = nil
	e.expiredOrder = nil
}

// due returns the tables to delete now, oldest first, at most batch:
// with keep >= 0, all but the newest keep, at once (force); else, when the
// instance is idle, or has more than hardCap, the scratch tables
// and those past maxAge or beyond maxTables.
func (e *resultsExpiry) due(keep int, force bool) []resultTable {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	n := len(e.tables)
	if keep >= 0 {
		return append([]resultTable(nil), e.tables[:max(n-keep, 0)]...)
	}
	now := e.now()
	idle := e.inflight == 0 && now.Sub(e.last) >= e.idle
	if !(force || idle || n > e.hardCap) {
		return nil
	}
	var out []resultTable
	for i, t := range e.tables {
		if len(out) == e.batch {
			break
		}
		if t.scratch || now.Sub(t.made) >= e.maxAge || i < n-e.maxTables {
			out = append(out, t)
		}
	}
	return out
}

// jobs returns the query results tables of the jobs with these IDs.
func (e *resultsExpiry) jobs(ids []string) []resultTable {
	e.mu.Lock()
	defer e.mu.Unlock()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []resultTable
	for _, t := range e.tables {
		if !t.scratch && want[t.job] {
			out = append(out, t)
		}
	}
	return out
}

// overCap reports whether there are more than hardCap tables.
func (e *resultsExpiry) overCap() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	return len(e.tables) > e.hardCap
}

// busy reports whether a request came in since started.
func (e *resultsExpiry) busy(started time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inflight > 0 || e.last.After(started)
}

// gone notes t deleted (or found gone).
func (e *resultsExpiry) gone(t resultTable) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, u := range e.tables {
		if u.project == t.project && u.job == t.job {
			e.tables = append(e.tables[:i:i], e.tables[i+1:]...)
			break
		}
	}
	if t.scratch {
		return
	}
	if e.expired == nil {
		e.expired = map[string]bool{}
	}
	k := expiryKey(t.project, t.job)
	if !e.expired[k] {
		e.expired[k] = true
		e.expiredOrder = append(e.expiredOrder, k)
	}
	if len(e.expiredOrder) > resultsForgetAfter {
		old := e.expiredOrder[:len(e.expiredOrder)-resultsForgetAfter]
		for _, k := range old {
			delete(e.expired, k)
		}
		e.expiredOrder = append([]string(nil), e.expiredOrder[len(old):]...)
	}
}

// deleteTables deletes tables through next, oldest first, stopping at a
// request that comes in when stopWhenBusy, and returns how many it
// deleted. A table that is not there counts as deleted; any other answer
// stops the batch (the emulator is failing, or has restarted).
func (q *queryResults) deleteTables(ctx context.Context, tables []resultTable, stopWhenBusy bool) int {
	started := q.expiry.now()
	deleted := 0
	for _, t := range tables {
		if stopWhenBusy && q.expiry.busy(started) {
			break
		}
		p := t.base + "/datasets/" + resultsDataset + "/tables/" + url.PathEscape(t.job)
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p, nil)
		if t.drop != "" {
			b, _ := json.Marshal(map[string]any{"query": t.drop, "useLegacySql": false})
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/queries", bytes.NewReader(b))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
			}
		}
		if err != nil {
			break
		}
		rec := newRecorder()
		q.next.ServeHTTP(rec, req)
		if rec.status >= 300 && rec.status != http.StatusNotFound {
			break
		}
		q.expiry.gone(t)
		deleted++
	}
	return deleted
}

// expire deletes the tables due (above) every resultsTick until ctx ends.
func (q *queryResults) expire(ctx context.Context) {
	tick := time.NewTicker(resultsTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if due := q.expiry.due(-1, false); len(due) > 0 {
				// Past the hard cap, a batch goes on while busy.
				q.deleteTables(ctx, due, !q.expiry.overCap())
			}
		}
	}
}

// serveExpire answers ExpireResultsPath (above).
func (q *queryResults) serveExpire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "badRequest", "POST "+ExpireResultsPath+"?keep=N")
		return
	}
	keep := 0
	if s := r.URL.Query().Get("keep"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid", "keep is a number of tables, 0 or more")
			return
		}
		keep = n
	}
	due := q.expiry.due(keep, true)
	if jobs := r.URL.Query()["job"]; len(jobs) > 0 {
		due = q.expiry.jobs(jobs)
	}
	deleted := q.deleteTables(r.Context(), due, false)
	q.expiry.mu.Lock()
	kept := len(q.expiry.tables)
	q.expiry.mu.Unlock()
	writeRaw(w, http.StatusOK, []byte(fmt.Sprintf(`{"deleted":%d,"kept":%d}`+"\n", deleted, kept)))
}

// serveExpired answers jobs.getQueryResults of a job whose table was
// deleted (above).
func serveExpired(w http.ResponseWriter, project, job string) {
	writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Table %s:%s.%s (the query's temporary "+
		"result table, which CloudBurrow keeps up to 24 hours, as BigQuery does, or fewer when there are more than %d)",
		project, resultsDataset, job, resultsMaxTables))
}
