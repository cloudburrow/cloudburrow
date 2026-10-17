package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
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
// query keeps reading a literal of the jobs it keeps, which is smaller,
// unless it keeps more than jobsLiteralMax, when it reads the table too
// (#1515).
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
//
// A background sync listed every job, which costs the engine more with
// each job made (#1525): measured on one instance with 2,100 jobs, a
// list took 0.33 s, and one is sent each second jobs are made. Waiting
// after each sync four times as long as it took kept the engine free,
// but let the table fall behind in a burst, and the first query after
// one wrote what the background had not (the first COUNT(*) after 3,273
// jobs made in 54 s took 2.0 s). So a background sync is incremental
// (#1528), and runs jobsTableDelay after a job is made again: it lists
// only the jobs the engine added after the last one the previous list of
// the table ended with (the cursor), and the jobs the front was told of
// since (jobs.insert, jobs.query, a script's parent and children, the
// front's own jobs, jobs.cancel and jobs.delete: noteMade) or whose row
// was not DONE when written, which may have finished since, with one
// jobs.list of bigquery-emulator patch 0064 (engineJobs). The jobs.list
// path gives them their times, failures, configuration and texts as it
// gives every job's, and lists the front's own jobs whole, so each row is
// the one a full list writes; a job asked for and not listed is gone, and
// its row deleted. A sync then costs what the jobs new or changed since
// the last do.
//
// A query of every job wrote the table the same way, from a list of every
// job, which cost it what the list did (#1559): measured on one instance
// late in the CI emulators shard's run (5,287 jobs, the kind node at two
// CPUs, 2026-10-17), a jobs.list of them took 0.55 s of an unfiltered
// COUNT(*)'s 0.55 to 0.70 s, and on GitHub's runners the COUNT(*) took
// 1.36 to 1.44 s. So a query brings the table up to date as a background
// sync does, incrementally, while every change of a job since the table
// was last written whole is one the front was told of (noteMade) or one
// of a job not DONE: it lists every job again only when the table is not
// made, or when pending reached jobsPendingMax since (overflow), so that
// a change was dropped. The syncs of a project's table, a query's and the
// background's, run one at a time (inc), so a query waits for the
// background's sync of the jobs it took, and reads them.
//
// An incremental sync still cost what every job did (#1564): the engine
// read its job order as GoogleSQL, whose WHERE visits every row, so a
// list of one job took 70 ms at 5,000 jobs, 138 ms at 10,000 and 274 ms
// at 20,000 (the kind node at two CPUs, 2026-10-17); and sync compared
// every row written. The engine now reads the order by its keys
// (bigquery-emulator patch 0060), and sync compares only the rows asked
// for and listed. A filtered query read a list of every job too, which
// the engine did not cut by creation time (1.0 s at 10,000 jobs, 2.3 s
// at 20,000): once the table is made it reads the table as an unfiltered
// one does (jobsRows), and the engine cuts a list by minCreationTime and
// maxCreationTime (patch 0060), which the front reads again.

// jobsTableName is the table of JOBS rows in resultsDataset. It does not
// begin with scratchPrefix, so queryResults takes it for no scratch table
// or result of its own to expire, and is unlike a client's table ID, as
// the emulator may read a table by its bare ID (#1015).
const jobsTableName = "cloudburrow_information_schema_jobs"

// jobsTableBatch is how many rows one INSERT writes at most, and
// jobsTableBatchBytes how many bytes of their literals, past the first
// row: a row's query text may be long (BigQuery takes up to 1 MB of
// text), and the engine compiles a statement in its WebAssembly heap,
// whose size is fixed, at about three bytes of SQLite for each byte of
// GoogleSQL (#1553).
const (
	jobsTableBatch      = 1000
	jobsTableBatchBytes = 1 << 20
)

// jobsBatch returns how many of rows, from the first, one INSERT writes
// (jobsTableBatch, jobsTableBatchBytes); one at least.
func jobsBatch(rows []string) int {
	n, size := 0, 0
	for n < len(rows) && n < jobsTableBatch {
		size += len(rows[n])
		if n > 0 && size > jobsTableBatchBytes {
			break
		}
		n++
	}
	return n
}

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

	// open are the IDs of the rows written of jobs not DONE, by
	// project, which a background sync reads again.
	open map[string]map[string]bool
	// cursor is the ID of the last job the engine listed when each
	// project's table was last written: the next background sync asks
	// for the jobs added after it.
	cursor map[string]string

	// bg guards queued, made and pending, apart from mu, which a sync
	// holds while it writes.
	bg sync.Mutex
	// queued are the projects a background sync waits to run for.
	queued map[string]bool
	// made are the projects whose table is made (rows has them).
	made map[string]bool
	// pending are the IDs of the jobs made since the last background
	// sync, by project, for those whose table is made.
	pending map[string]map[string]bool
	// overflow are the projects whose pending reached jobsPendingMax
	// since their table was last written from a list of every job: the
	// next query lists every job (above).
	overflow map[string]bool

	// inc is held through each sync of a table, from its list of jobs to
	// its last write (above).
	inc sync.Mutex
}

// jobsPendingMax is the most job IDs pending keeps of a project, which a
// background sync names in its jobs.list's URL; past it the background
// leaves the changes of jobs it already wrote to the next query, which
// lists every job (the new ones it lists after the cursor).
const jobsPendingMax = 2000

// noteMade records that the job was made or changed, for the next
// background sync of its project's table, when it is made.
func (j *jobsTable) noteMade(project, id string) {
	if j == nil || id == "" {
		return
	}
	j.bg.Lock()
	defer j.bg.Unlock()
	if !j.made[project] {
		return
	}
	if j.pending == nil {
		j.pending = map[string]map[string]bool{}
	}
	p := j.pending[project]
	if p == nil {
		p = map[string]bool{}
		j.pending[project] = p
	}
	if len(p) < jobsPendingMax {
		p[id] = true
		return
	}
	j.overflowLocked(project)
}

// overflowLocked records that a change of the project's jobs was dropped
// (overflow). The caller holds bg.
func (j *jobsTable) overflowLocked(project string) {
	if j.overflow == nil {
		j.overflow = map[string]bool{}
	}
	j.overflow[project] = true
}

// incremental reports whether the project's table is made and every
// change since it was last written whole is pending or open (above), and
// forgets the overflow when it is not: the caller lists every job.
func (j *jobsTable) incremental(project string) bool {
	j.bg.Lock()
	defer j.bg.Unlock()
	if !j.made[project] || j.overflow[project] {
		delete(j.overflow, project)
		return false
	}
	return true
}

// isMade reports whether the project's table is made.
func (j *jobsTable) isMade(project string) bool {
	if j == nil {
		return false
	}
	j.bg.Lock()
	defer j.bg.Unlock()
	return j.made[project]
}

// takePending returns and forgets the project's pending IDs.
func (j *jobsTable) takePending(project string) map[string]bool {
	j.bg.Lock()
	defer j.bg.Unlock()
	p := j.pending[project]
	delete(j.pending, project)
	return p
}

// reset forgets the tables: the emulator restarted, and has none.
func (j *jobsTable) reset() {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.rows, j.open, j.cursor = nil, nil, nil
	j.mu.Unlock()
	j.bg.Lock()
	j.made, j.pending, j.overflow = nil, nil, nil
	j.bg.Unlock()
}

// setMade records whether the project's table is made.
func (j *jobsTable) setMade(project string, made bool) {
	j.bg.Lock()
	defer j.bg.Unlock()
	if !made {
		delete(j.made, project)
		delete(j.pending, project)
		delete(j.overflow, project)
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

// jobsTableStatements are how each statement sync sends begins.
var jobsTableStatements = []string{"DROP TABLE IF EXISTS ", "CREATE TABLE ", "DELETE FROM ", "INSERT INTO "}

// isJobsTableStatement reports whether sql is one of the statements sync
// sends, which write the table (internalText).
func isJobsTableStatement(sql string) bool {
	for _, s := range jobsTableStatements {
		if strings.HasPrefix(sql, s+jobsTablePath()) {
			return true
		}
	}
	return false
}

// sync writes the project's table so that it holds rows (job ID to the
// row's SQL), new ones in the order of ids, and reports whether it does;
// a table not made is made only when create is true. open are the IDs of
// rows of jobs not DONE, last the job the engine's list ended with. With
// asked nil, rows are every job's; else they are some jobs', those asked
// for among them if they are still there: the row of a job asked for and
// not in rows is deleted, and the others are left as they are. No order of a query's rows is
// promised without ORDER BY.
func (j *jobsTable) sync(f front, r *http.Request, project string, ids []string, rows map[string]string, open, asked map[string]bool, last string, create bool) bool {
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
	// Of an incremental sync, only the rows asked for and listed can have
	// changed (#1564): reading every row written cost each sync what the
	// project's jobs did.
	check := func(id string) {
		if row, had := have[id]; had {
			if now, ok := rows[id]; ok && now != row || !ok && (asked == nil || asked[id]) {
				gone = append(gone, id)
			}
		}
	}
	if asked == nil {
		for id := range have {
			check(id)
		}
	} else {
		for id := range asked {
			check(id)
		}
		for id := range rows {
			if !asked[id] {
				check(id)
			}
		}
	}
	for _, id := range ids {
		if row, ok := have[id]; !ok || row != rows[id] {
			add = append(add, id)
		}
	}
	fail := func() bool {
		delete(j.rows, project) // made again by the next query
		delete(j.open, project)
		delete(j.cursor, project)
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
		lits := make([]string, min(len(add), jobsTableBatch))
		for i, id := range add[:len(lits)] {
			lits[i] = rows[id]
		}
		n := jobsBatch(lits)
		lits = lits[:n]
		if !run("INSERT INTO " + jobsTablePath() + " SELECT * FROM " + literalRows(jobsColumns, lits)) {
			return fail()
		}
		for _, id := range add[:n] {
			have[id] = rows[id]
		}
		add = add[n:]
	}
	if j.open == nil {
		j.open = map[string]map[string]bool{}
	}
	was := j.open[project]
	if asked == nil || was == nil {
		was = map[string]bool{}
	}
	for id := range asked {
		delete(was, id)
	}
	for id := range open {
		was[id] = true
	}
	j.open[project] = was
	if j.cursor == nil {
		j.cursor = map[string]string{}
	}
	j.cursor[project] = last
	return true
}

// cursorOf returns the ID of the last job the engine listed for the
// project's table.
func (j *jobsTable) cursorOf(project string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cursor[project]
}

// openIDs returns the IDs of the project's rows of jobs not DONE.
func (j *jobsTable) openIDs(project string) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var ids []string
	for id := range j.open[project] {
		ids = append(ids, id)
	}
	return ids
}

// tableJobsRows returns the subquery of the project's jobs as the table
// (above), written from jobs (made first when create is true), or ""
// when it could not be written. jobs are every job of the project, or,
// with asked set, those the engine added after the job its last list
// ended with and those asked for (sync); last is the job this list ended
// with.
func (f front) tableJobsRows(r *http.Request, project string, jobs []map[string]any, last string, asked map[string]bool, create bool) string {
	if f.jobsTable == nil {
		return ""
	}
	rows := make(map[string]string, len(jobs))
	ids := make([]string, 0, len(jobs))
	open := map[string]bool{}
	for _, job := range jobs {
		_, id := jobRef(job)
		if _, dup := rows[id]; id == "" || dup {
			return ""
		}
		rows[id] = "(" + strings.Join(jobRow(project, job), ", ") + ")"
		ids = append(ids, id)
		if jobStateOf(job) != "DONE" {
			open[id] = true
		}
	}
	if !f.jobsTable.sync(f, r, project, ids, rows, open, asked, last, create) {
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
		f.jobsTable.inc.Lock()
		defer f.jobsTable.inc.Unlock()
		f.syncJobsTable(req.WithContext(ctx), project)
	})
}

// syncJobsTable writes the rows of the project's jobs made since the last
// sync (those the engine added after the last it listed, and those
// noteMade was told of) and of those not DONE then (above) to its table,
// if it is made, and reports whether it did. The caller holds inc.
func (f front) syncJobsTable(r *http.Request, project string) bool {
	pending := f.jobsTable.takePending(project)
	asked := map[string]bool{}
	ids := make([]string, 0, len(pending))
	for id := range pending {
		asked[id] = true
		ids = append(ids, id)
	}
	for _, id := range f.jobsTable.openIDs(project) {
		if !asked[id] {
			asked[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	l := &engineList{some: true, after: f.jobsTable.cursorOf(project), ids: ids}
	jobs, status, _ := f.listedJobs(r, nil, l)
	if status != 0 || f.tableJobsRows(r, project, jobs, l.last, asked, false) == "" {
		for id := range pending { // for the next sync
			f.jobsTable.noteMade(project, id)
		}
		return false
	}
	return true
}

// everyJobRows is the subquery of the project's table of every job's
// rows, brought up to date incrementally when it can be (above), else
// from a list of every job (made first if need be), or "" when it could
// not be written.
func (f front) everyJobRows(r *http.Request, project string) string {
	if f.jobsTable == nil {
		return ""
	}
	f.jobsTable.inc.Lock()
	defer f.jobsTable.inc.Unlock()
	if f.jobsTable.incremental(project) && f.syncJobsTable(r, project) {
		return "(SELECT * FROM " + jobsTablePath() + ")"
	}
	listed := &engineList{}
	jobs, status, _ := f.listedJobs(r, nil, listed)
	if status != 0 {
		f.jobsTable.bg.Lock()
		if f.jobsTable.made[project] {
			f.jobsTable.overflowLocked(project) // still to be listed whole
		}
		f.jobsTable.bg.Unlock()
		return ""
	}
	return f.tableRowsOrRetry(r, project, jobs, listed.last)
}

// lockedTableRows is tableRowsOrRetry, holding inc.
func (f front) lockedTableRows(r *http.Request, project string, jobs []map[string]any, last string) string {
	if f.jobsTable == nil {
		return ""
	}
	f.jobsTable.inc.Lock()
	defer f.jobsTable.inc.Unlock()
	return f.tableRowsOrRetry(r, project, jobs, last)
}

// engineListKey is the context key of the engineList of a jobs.list the
// front sends itself (engineJobs).
type engineListKey struct{}

// engineList is what a jobs.list the front sends itself asks the engine
// for, and what it was answered.
type engineList struct {
	// some, when set, asks for the jobs added after the job after (every
	// job when after is "") and the jobs of ids alone (bigquery-emulator
	// patch 0064); otherwise every job is listed.
	some  bool
	after string
	ids   []string
	// last is the ID of the last job the engine listed, or after when it
	// listed none.
	last string
}

// engineJobs is next, the engine, but for a jobs.list with an engineList
// in its context (above), which it sends with the parameters of patch
// 0064 when some is set, and whose last job it records, for the jobs.list
// path to serve as it serves the engine's whole list.
func engineJobs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l, ok := r.Context().Value(engineListKey{}).(*engineList)
		if !ok || l == nil {
			next.ServeHTTP(w, r)
			return
		}
		if l.some {
			q := r.URL.Query()
			q.Set("cloudburrowAfterJobId", l.after)
			q.Set("cloudburrowJobIds", strings.Join(l.ids, ","))
			u := *r.URL
			u.RawQuery = q.Encode()
			r = r.Clone(r.Context())
			r.URL = &u
		}
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		var list struct {
			Jobs []struct {
				JobReference struct {
					JobID string `json:"jobId"`
				} `json:"jobReference"`
			} `json:"jobs"`
		}
		l.last = l.after
		if (rec.status == 0 || rec.status == http.StatusOK) && json.Unmarshal(rec.body.Bytes(), &list) == nil && len(list.Jobs) > 0 {
			l.last = list.Jobs[len(list.Jobs)-1].JobReference.JobID
		}
		rec.copyTo(w)
	})
}

// jobsTableSyncTimeout bounds a background sync.
const jobsTableSyncTimeout = 2 * time.Minute
