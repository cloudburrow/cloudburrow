package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
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

// jobsTableName is the table of JOBS rows in resultsDataset. It does not
// begin with scratchPrefix, so queryResults takes it for no scratch table
// or result of its own to expire, and is unlike a client's table ID, as
// the emulator may read a table by its bare ID (#1015).
const jobsTableName = "cloudburrow_information_schema_jobs"

// jobsTableBatch is how many rows one INSERT writes.
const jobsTableBatch = 1000

// jobsTable is what the front wrote to each project's table of JOBS rows.
type jobsTable struct {
	mu sync.Mutex
	// rows are the rows written, by project and job ID; a project with
	// none is one whose table the front has not made since it started
	// or the emulator restarted.
	rows map[string]map[string]string
}

// reset forgets the tables: the emulator restarted, and has none.
func (j *jobsTable) reset() {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.rows = nil
	j.mu.Unlock()
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
// row's SQL), new ones in the order of ids, and reports whether it does.
// No order of a query's rows is promised without ORDER BY.
func (j *jobsTable) sync(f front, r *http.Request, project string, ids []string, rows map[string]string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	run := func(sql string) bool {
		body, _ := json.Marshal(map[string]any{"query": sql, "useLegacySql": false})
		st, _ := f.send(r, http.MethodPost, "/queries", body)
		return st == http.StatusOK
	}
	have, made := j.rows[project]
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
		delete(j.rows, project) // made again next time
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
// (above), written from jobs, or "" when it could not be written.
func (f front) tableJobsRows(r *http.Request, project string, jobs []map[string]any) string {
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
	if !f.jobsTable.sync(f, r, project, ids, rows) {
		return ""
	}
	return "(SELECT * FROM " + jobsTablePath() + ")"
}
