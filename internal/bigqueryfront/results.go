package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// Query results in one dataset (#1017).
//
// BigQuery writes the result of a query job that names no destination
// table to a temporary table in an anonymous (hidden) dataset, which the
// job's configuration.query.destinationTable then names and which clients
// read the rows from (the Java and Ruby clients read them through
// tabledata.list; the Go, Python and Node.js clients through
// jobs.getQueryResults):
// https://cloud.google.com/bigquery/docs/writing-results#temporary_and_permanent_tables
//
// The pinned emulator does the same, but makes a dataset of its own for
// each job, named after the job (server/handler.go,
// addQueryResultToDynamicDestinationTable), and its SQL engine
// (googlesqlite v0.3.1) builds a catalog with every builtin function for
// each dataset that holds a table (internal/catalog.go,
// getOrCreateSubCatalog), and rebuilds every one of them at each DROP TABLE
// (resetCatalog). That catalog work is what made the engine's memory grow
// with every query job until it failed (engine.go, #989), and every DROP
// TABLE slower than the one before.
//
// So the front gives such a job a destination table itself, before the
// emulator runs it: a table named after the job, in one dataset per
// project, resultsDataset, which the front makes when it first needs it.
// The emulator then writes the result there as it writes any query's
// destination table, and the job names it as before; jobs.getQueryResults
// is unchanged (the emulator answers it from the job, not the table). The
// engine builds one catalog for that dataset rather than one per job; a
// table in it costs the engine a table, not a catalog. Measured with
// scripts/bigquery-engine-soak.sh (docs/compatibility.md, BigQuery).
//
// Only a job whose query is one SELECT (or WITH, or a parenthesised
// query) is given one: that is a query whose result has columns, which
// the emulator would have written to a dataset of its own. A DDL or DML
// statement or a script is sent as it was, as is a dry run, a job that
// names its destination, and a job with no jobReference.jobId (Wrap gives
// every jobs.insert one before it gets here).
//
// Like BigQuery's anonymous datasets, resultsDataset begins with an
// underscore, which makes a dataset hidden: datasets.list leaves it out
// unless all=true (hideResults).
// https://cloud.google.com/bigquery/docs/datasets#dataset-naming
// https://cloud.google.com/bigquery/docs/reference/rest/v2/datasets/list
//
// The tables are not deleted: deleting one is a DROP TABLE, which rebuilds
// every catalog (above), and measured, deleting each job's result soon
// after the job grew the engine's memory faster than keeping them
// (docs/compatibility.md). The emulator keeps them, as it kept the
// per-job datasets, until it restarts.

// resultsDataset is the dataset the front has the emulator write query
// results to.
const resultsDataset = "_cloudburrow_query_results"

// missingDataset is how the emulator answers a query job whose destination
// dataset does not exist (server/handler.go, jobsInsertHandler.Handle).
var missingDataset = []byte("failed to find destination dataset")

// datasetsListRoute matches datasets.list.
var datasetsListRoute = regexp.MustCompile(`^(/bigquery/v2)?/projects/([^/]+)/datasets$`)

// queryResults stands between the front and the emulator (above).
type queryResults struct {
	next http.Handler
	mu   sync.Mutex
	// made are the projects whose resultsDataset the front has seen made.
	made map[string]bool
}

// Results returns next, the path to the emulator, with query results
// written to one dataset per project (above).
func Results(next http.Handler) *queryResults {
	return &queryResults{next: next}
}

// reset forgets which projects have resultsDataset: the emulator has
// restarted (restart.go) and has none.
func (q *queryResults) reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.made = nil
}

func (q *queryResults) isMade(project string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.made[project]
}

func (q *queryResults) setMade(project string, made bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.made == nil {
		q.made = map[string]bool{}
	}
	q.made[project] = made
}

func (q *queryResults) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if m := datasetsListRoute.FindStringSubmatch(path); m != nil && r.Method == http.MethodGet {
		q.hideResults(w, r)
		return
	}
	j := jobsRoute.FindStringSubmatch(path)
	if j == nil || j[3] != "jobs" || r.Method != http.MethodPost || strings.HasPrefix(path, "/upload/") {
		q.next.ServeHTTP(w, r)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && strings.HasPrefix(mt, "multipart/") {
		q.next.ServeHTTP(w, r)
		return
	}
	body, err := readBody(r)
	if err != nil {
		q.next.ServeHTTP(w, r)
		return
	}
	project := projectOf("/" + j[2])
	out, ok := withResultsTable(body, project)
	if !ok {
		setBody(r, body)
		q.next.ServeHTTP(w, r)
		return
	}
	base := j[1] + "/projects/" + j[2]
	if !q.isMade(project) {
		q.makeDataset(r, base, project)
	}
	rec := newRecorder()
	setBody(r, out)
	q.next.ServeHTTP(rec, r)
	if rec.status >= 400 && bytes.Contains(rec.body.Bytes(), missingDataset) {
		// The dataset is gone (a client deleted it): make it again, once.
		q.setMade(project, false)
		q.makeDataset(r, base, project)
		rec = newRecorder()
		setBody(r, out)
		q.next.ServeHTTP(rec, r)
	}
	rec.copyTo(w)
}

// makeDataset makes project's resultsDataset, and notes it made when the
// emulator made it or had it.
func (q *queryResults) makeDataset(r *http.Request, base, project string) {
	body, _ := json.Marshal(map[string]any{"datasetReference": map[string]string{"projectId": project, "datasetId": resultsDataset}})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+"/datasets", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = r.Host
	req.RemoteAddr = r.RemoteAddr
	rec := newRecorder()
	q.next.ServeHTTP(rec, req)
	if rec.status == 0 || rec.status == http.StatusOK || rec.status == http.StatusConflict {
		q.setMade(project, true)
	}
}

// withResultsTable returns the jobs.insert body job with a destination
// table in resultsDataset, and whether the job is one the front gives one
// (above).
func withResultsTable(body []byte, project string) ([]byte, bool) {
	var job map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&job) != nil {
		return nil, false
	}
	ref, _ := job["jobReference"].(map[string]any)
	id, _ := ref["jobId"].(string)
	conf, _ := job["configuration"].(map[string]any)
	q, _ := conf["query"].(map[string]any)
	if id == "" || q == nil || q["destinationTable"] != nil || conf["dryRun"] == true {
		return nil, false
	}
	text, _ := q["query"].(string)
	if !isLoneQuery(text) {
		return nil, false
	}
	q["destinationTable"] = map[string]any{"projectId": project, "datasetId": resultsDataset, "tableId": id}
	out, err := json.Marshal(job)
	return out, err == nil
}

// isLoneQuery reports whether sql is one query statement: SELECT, WITH or
// a parenthesised query, with nothing after it but a semicolon.
func isLoneQuery(sql string) bool {
	toks, ok := lex(sql)
	if !ok {
		return false
	}
	var stmts [][]token
	for _, s := range splitStatements(toks) {
		if len(s) > 0 {
			stmts = append(stmts, s)
		}
	}
	if len(stmts) != 1 {
		return false
	}
	first := stmts[0][0]
	return first.is("SELECT") || first.is("WITH") || first.punct("(")
}

// hideResults answers datasets.list without resultsDataset, as BigQuery
// leaves hidden datasets out unless all=true.
func (q *queryResults) hideResults(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") == "true" {
		q.next.ServeHTTP(w, r)
		return
	}
	rec := newRecorder()
	q.next.ServeHTTP(rec, r)
	if rec.status != 0 && rec.status != http.StatusOK ||
		!bytes.Contains(rec.body.Bytes(), []byte(resultsDataset)) && !bytes.Contains(rec.body.Bytes(), []byte(readAliasDataset)) {
		rec.copyTo(w)
		return
	}
	var list map[string]json.RawMessage
	var datasets []json.RawMessage
	if json.Unmarshal(rec.body.Bytes(), &list) != nil || json.Unmarshal(list["datasets"], &datasets) != nil {
		rec.copyTo(w)
		return
	}
	kept := datasets[:0]
	for _, d := range datasets {
		var ds struct {
			DatasetReference struct {
				DatasetID string `json:"datasetId"`
			} `json:"datasetReference"`
		}
		if json.Unmarshal(d, &ds) == nil && frontDataset(ds.DatasetReference.DatasetID) { // #1046's too
			continue
		}
		kept = append(kept, d)
	}
	if len(kept) == 0 {
		delete(list, "datasets")
	} else {
		list["datasets"], _ = json.Marshal(kept)
	}
	out, err := json.Marshal(list)
	if err != nil {
		rec.copyTo(w)
		return
	}
	for k, v := range rec.header {
		if k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	writeRaw(w, http.StatusOK, out)
}
