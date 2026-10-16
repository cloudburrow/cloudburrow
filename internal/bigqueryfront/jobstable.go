package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// INFORMATION_SCHEMA.JOBS rows in a table (#1378).
//
// A JOBS query the front cannot filter (pushedJobsFilter, #1369) read a
// literal row per job (jobsRows, rowsSubquery), which the engine compiles
// anew for each query: measured on one instance with 5,004 jobs
// (--services bigquery,storage, 2026-10-16), an unfiltered COUNT(*) took
// 22.65 s the first time and 7.19 s the second. The rows depend on what
// only the front knows (the times it gave, the failures, the client's
// texts, its own jobs), so the front keeps them itself, in a hidden table
// of resultsDataset (jobsTableName), and before each such query writes
// only the rows that are new or changed since the last (an INSERT of
// their literal) and deletes those of jobs gone (jobs.delete). The query
// then reads the table, and the engine runs its WHERE. The rows are the
// ones jobRow writes, so the answer is the one the literal gave.
//
// The table is made again (dropped first) the first time a project's
// rows are asked for after the front starts, as the front no longer knows
// what it holds, and forgotten when the emulator restarts (restart.go),
// as the emulator then has neither the table nor the jobs. A filtered
// query keeps reading a literal of the jobs it keeps, which is smaller.
// The table is left out of INFORMATION_SCHEMA's TABLES and the like
// (tablesOf), and the statements that write it are the front's own,
// which jobs.list leaves out (front.send). When the table cannot be
// written, the query reads the literal, as before.
//
// Written in the background too (#1419). Measured on one instance
// (--services bigquery,storage, arm64, 2026-10-16), the first unfiltered
// COUNT(*) after 5,000 jobs were made took 25.1 s: 16.6 s to list them
// (a jobs.get for each jobs.query job, whose configuration the front had
// not seen, joblist.go) and 8.5 s for five INSERTs of 1,000 rows (1.65 ms
// a row, whatever the batch); the queries after it took 0.24 to 0.27 s.
// The engine now lists each job with its configuration (bigquery-emulator
// patch 0022), so no jobs.get is sent. And once a project's table is
// made, each job made through the front (jobs.insert, jobs.query) has the
// table brought up to date in the background, jobsTableDelay later, one
// sync at a time and one waiting at most: the INSERTs happen as the jobs
// are made, and a query writes only what the last second added. A query
// still syncs first, so it reads every job, as before. The background
// never makes a table: one dropped (the emulator restarted, a write
// failed) waits for the next query.

// jobsTableName is the table of JOBS rows in resultsDataset. It does not
// begin with scratchPrefix, so queryResults takes it for no scratch table
// or result of its own to expire, and is unlike a client's table ID, as
// the emulator may read a table by its bare ID (#1015).
const jobsTableName = "cloudburrow_information_schema_jobs"

// jobsTableBatch is how many rows one INSERT writes.
const jobsTableBatch = 1000

// jobsTableDelay is how long after a job is made the background sync
// (above) runs: the jobs made meanwhile are written with it. A sync takes
// every job not yet written, so the table never falls behind by more than
// the jobs of one delay and one sync. (Waiting twice as long as the last
// sync took, measured, fell behind: each sync took longer, the next
// waited longer, and the first query after 5,000 jobs took 20 s.)
var jobsTableDelay = time.Second

// jobsTable is what the front wrote to each project's table of JOBS rows.
type jobsTable struct {
	mu sync.Mutex
	// rows are the rows written, by project and job ID; a project with
	// none is one whose table the front has not made since it started
	// or the emulator restarted.
	rows map[string]map[string]string

	// bg guards queued and made, apart from mu, which a sync holds
	// while it writes.
	bg sync.Mutex
	// queued are the projects a background sync waits to run for.
	queued map[string]bool
	// made are the projects whose table is made (rows has them).
	made map[string]bool
}

// reset forgets the tables: the emulator restarted, and has none.
func (j *jobsTable) reset() {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.rows = nil
	j.mu.Unlock()
	j.bg.Lock()
	j.made = nil
	j.bg.Unlock()
}

// setMade records whether the project's table is made.
func (j *jobsTable) setMade(project string, made bool) {
	j.bg.Lock()
	defer j.bg.Unlock()
	if !made {
		delete(j.made, project)
		return
	}
	if j.made == nil {
		j.made = map[string]bool{}
	}
	j.made[project] = true
}

// soon runs sync jobsTableDelay from now, unless the project has no
// table or a sync already waits to run for it.
func (j *jobsTable) soon(project string, sync func()) {
	if j == nil {
		return
	}
	j.bg.Lock()
	if !j.made[project] || j.queued[project] {
		j.bg.Unlock()
		return
	}
	if j.queued == nil {
		j.queued = map[string]bool{}
	}
	j.queued[project] = true
	j.bg.Unlock()
	time.AfterFunc(jobsTableDelay, func() {
		j.bg.Lock()
		delete(j.queued, project)
		j.bg.Unlock()
		sync()
	})
}

// jobsTableColumns is jobsColumns as a CREATE TABLE's column list.
func jobsTableColumns() string {
	return "(" + strings.TrimSuffix(strings.TrimPrefix(jobsColumns, "STRUCT<"), ">") + ")"
}

// jobsTablePath is the table as SQL.
func jobsTablePath() string {
	return "`" + resultsDataset + "`.`" + jobsTableName + "`"
}

// sync writes the project's table so that it holds rows (job ID to the
// row's SQL), new ones in the order of ids, and reports whether it does;
// a table not made is made only when create is true. No order of a
// query's rows is promised without ORDER BY.
func (j *jobsTable) sync(f front, r *http.Request, project string, ids []string, rows map[string]string, create bool) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	run := func(sql string) bool {
		body, _ := json.Marshal(map[string]any{"query": sql, "useLegacySql": false})
		st, _ := f.send(r, http.MethodPost, "/queries", body)
		return st == http.StatusOK
	}
	have, made := j.rows[project]
	if !made && !create {
		return false
	}
	if !made {
		// The dataset may not be there yet (queryResults makes it for the
		// first query result); 409 if it is.
		body, _ := json.Marshal(map[string]any{"datasetReference": map[string]string{"projectId": project, "datasetId": resultsDataset}})
		f.send(r, http.MethodPost, "/datasets", body)
		if !run("DROP TABLE IF EXISTS "+jobsTablePath()) || !run("CREATE TABLE "+jobsTablePath()+" "+jobsTableColumns()) {
			return false
		}
		have = map[string]string{}
		if j.rows == nil {
			j.rows = map[string]map[string]string{}
		}
		j.rows[project] = have
		j.setMade(project, true)
	}
	var gone []string
	var add []string
	for id, row := range have {
		if now, ok := rows[id]; !ok || now != row {
			gone = append(gone, id)
		}
	}
	for _, id := range ids {
		if row, ok := have[id]; !ok || row != rows[id] {
			add = append(add, id)
		}
	}
	fail := func() bool {
		delete(j.rows, project) // made again by the next query
		j.setMade(project, false)
		return false
	}
	for len(gone) > 0 {
		n := min(len(gone), jobsTableBatch)
		ids := make([]string, n)
		for i, id := range gone[:n] {
			ids[i] = sqlString(id)
		}
		if !run("DELETE FROM " + jobsTablePath() + " WHERE job_id IN (" + strings.Join(ids, ", ") + ")") {
			return fail()
		}
		for _, id := range gone[:n] {
			delete(have, id)
		}
		gone = gone[n:]
	}
	for len(add) > 0 {
		n := min(len(add), jobsTableBatch)
		lits := make([]string, n)
		for i, id := range add[:n] {
			lits[i] = rows[id]
		}
		if !run("INSERT INTO " + jobsTablePath() + " SELECT * FROM " + literalRows(jobsColumns, lits)) {
			return fail()
		}
		for _, id := range add[:n] {
			have[id] = rows[id]
		}
		add = add[n:]
	}
	return true
}

// tableJobsRows returns the subquery of the project's jobs as the table
// (above), written from jobs (made first when create is true), or ""
// when it could not be written.
func (f front) tableJobsRows(r *http.Request, project string, jobs []map[string]any, create bool) string {
	if f.jobsTable == nil {
		return ""
	}
	rows := make(map[string]string, len(jobs))
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		_, id := jobRef(job)
		if _, dup := rows[id]; id == "" || dup {
			return ""
		}
		rows[id] = "(" + strings.Join(jobRow(project, job), ", ") + ")"
		ids = append(ids, id)
	}
	if !f.jobsTable.sync(f, r, project, ids, rows, create) {
		return ""
	}
	return "(SELECT * FROM " + jobsTablePath() + ")"
}

// syncJobsTableSoon brings the table of the project of r, a job made
// through the front, up to date in the background (above), when it is
// made.
func (f front) syncJobsTableSoon(r *http.Request) {
	if f.jobsTable == nil || f.listJobs == nil {
		return
	}
	project := projectOf(f.base)
	// The request has been answered: its context ends, its body is read.
	req := r.Clone(context.WithoutCancel(r.Context()))
	req.Body, req.ContentLength = http.NoBody, 0
	f.jobsTable.soon(project, func() {
		ctx, cancel := context.WithTimeout(req.Context(), jobsTableSyncTimeout)
		defer cancel()
		r := req.WithContext(ctx)
		if jobs, status, _ := f.allJobs(r, nil); status == 0 {
			f.tableJobsRows(r, project, jobs, false)
		}
	})
}

// jobsTableSyncTimeout bounds a background sync.
const jobsTableSyncTimeout = 2 * time.Minute
