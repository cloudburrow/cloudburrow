package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// CREATE FUNCTION of a function that exists (#986).
//
// BigQuery: "CREATE FUNCTION" of a function that exists fails "Already
// Exists", unless OR REPLACE is given, which replaces it, or IF NOT EXISTS,
// which does nothing
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_function_statement).
//
// Measured against the pinned image through the official Go client,
// jobs.query and a query job alike: `CREATE FUNCTION ds.h(x INT64) AS (x +
// 1)` then `CREATE FUNCTION ds.h(x INT64) AS (x + 2)` succeeded, and
// `SELECT ds.h(1)` still read 2; `CREATE OR REPLACE FUNCTION ds.h ... (x +
// 3)` then succeeded and ds.h(1) still read 2; so for CREATE [OR REPLACE]
// TABLE FUNCTION. IF NOT EXISTS did nothing, as in BigQuery. `DROP FUNCTION
// ds.h` then took it out ("Function not found"), and a CREATE FUNCTION after
// it made it with its new body; a CREATE FUNCTION whose body does not
// analyse failed and made nothing.
//
// So, for each CREATE [OR REPLACE] [TABLE] FUNCTION that is not TEMP and
// not IF NOT EXISTS, the front tells whether the function exists before the
// statement runs: it exists before the query (functionExists), or an
// earlier statement of the script makes it, and no earlier DROP FUNCTION
// takes it out.
//
//   - CREATE FUNCTION of one that exists fails as BigQuery fails it, 409
//     duplicate "Already Exists", when it is the query's first statement
//     (nothing has run before it in BigQuery either); after other
//     statements of a script it is 501, as BigQuery runs those and fails
//     there, which the emulator cannot be stopped to do. Nothing is run.
//   - A lone CREATE OR REPLACE FUNCTION of one that exists is carried out:
//     the statement is first run under a scratch name in the same dataset,
//     so that one that fails fails as it is, with the function kept; then
//     the scratch function and the old one are dropped (DROP FUNCTION) and
//     the statement is sent as it is, which then makes the function. Inside
//     a script of several statements, and for a table function (the
//     engine cannot drop one: "Statement not supported:
//     DropTableFunctionStatement", measured in #976), it is 501, and nothing
//     is run.

// functionDDL checks the CREATE FUNCTION statements of a query (above). It
// reports whether it answered w; when it did not, it returns the
// function to replace (a lone CREATE OR REPLACE of one that exists), or nil.
func (f front) functionDDL(w http.ResponseWriter, r *http.Request, q queryOptions, v ddlVerdict, insert bool) (*funcStmt, bool) {
	made, dropped := map[string]bool{}, map[string]bool{}
	var replace *funcStmt
	for i := range v.funcs {
		s := v.funcs[i]
		if s.temp {
			continue
		}
		full, ok := functionPath(q, s.path)
		if !ok {
			continue
		}
		key := strings.ToLower(strings.Join(full, "."))
		if s.drop {
			dropped[key], made[key] = true, false
			continue
		}
		if s.ifNotExists {
			made[key] = true
			continue
		}
		exists := made[key]
		kind := ""
		if !exists && !dropped[key] {
			exists, kind = f.functionKind(r, full)
		}
		made[key] = true
		if !exists {
			continue
		}
		name := strings.Join(s.path, ".")
		what := "FUNCTION"
		if s.table {
			what = "TABLE FUNCTION"
		}
		if !s.replace {
			if s.index == 0 {
				ds := full[len(full)-2]
				f.failBeforeRun(w, r, q, insert, ds, http.StatusConflict, rowError{Reason: "duplicate",
					Message: "Already Exists: Routine " + projectOf(f.base) + ":" + ds + "." + full[len(full)-1]})
				return nil, true
			}
			writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: CREATE %s %s, "+
				"which exists, after other statements of a script. BigQuery runs the statements before it and fails there "+
				"(\"Already Exists\"), but the emulator behind CloudBurrow runs CREATE FUNCTION of an existing function as "+
				"done and keeps its old body (measured), and CloudBurrow cannot stop the script in between. Nothing was run. "+
				"Use CREATE OR REPLACE %s run as a query of its own, or IF NOT EXISTS.", what, name, what))
			return nil, true
		}
		switch {
		case s.table || kind == "table":
			writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: CREATE OR "+
				"REPLACE %s %s, which exists as a %s. BigQuery replaces it, but the emulator behind CloudBurrow keeps the "+
				"old one (measured), and its engine cannot drop a table function (\"Statement not supported: "+
				"DropTableFunctionStatement\") for CloudBurrow to replace it. Nothing was run.", what, name,
				map[bool]string{true: "table function", false: "function"}[kind == "table"]))
			return nil, true
		case v.statements > 1:
			writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: CREATE OR "+
				"REPLACE FUNCTION %s, which exists, inside a script of several statements. BigQuery replaces it, but the "+
				"emulator behind CloudBurrow keeps the old body (measured), and CloudBurrow replaces it only for a "+
				"statement run on its own. Nothing was run. Run the CREATE OR REPLACE FUNCTION as a query of its own, or "+
				"DROP FUNCTION it first.", name))
			return nil, true
		}
		s.path = full
		replace = &s
	}
	return replace, false
}

// functionKind reports whether the engine has a function, by calling it
// with no arguments (functionExists), and, when it does, whether it is a
// table function ("table"): the engine answers "Table-valued function is
// not expected here" for one called in a SELECT list (measured).
func (f front) functionKind(r *http.Request, p []string) (exists bool, kind string) {
	legacy := false
	body, err := json.Marshal(queryOptions{Query: "SELECT " + functionName(p) + "()", UseLegacySQL: &legacy})
	if err != nil {
		return true, ""
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	switch {
	case status == http.StatusOK:
		return true, ""
	case strings.Contains(string(got), "Function not found"):
		return false, ""
	case strings.Contains(string(got), "Table-valued function is not expected here"):
		return true, "table"
	}
	return true, ""
}

// replaceFunction carries out a lone CREATE OR REPLACE FUNCTION of a
// function that exists (s, whose path is full), which the emulator runs as
// done and keeps the old one (above).
func (f front) replaceFunction(w http.ResponseWriter, r *http.Request, q queryOptions, s funcStmt) {
	scratch := append(append([]string{}, s.path[:len(s.path)-1]...), scratchTable())
	text := q.Query[s.pos:s.end]
	trial := text[:s.pathPos-s.pos] + functionName(scratch) + text[s.pathEnd-s.pos:]
	body, err := json.Marshal(queryOptions{Query: trial, UseLegacySQL: q.UseLegacySQL, DefaultDataset: q.DefaultDataset,
		ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", err.Error())
		return
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	f.sendDDL(r, "DROP FUNCTION IF EXISTS "+functionName(scratch))
	if status != http.StatusOK {
		writeRaw(w, status, bytes.ReplaceAll(got, []byte(scratch[len(scratch)-1]), []byte(s.path[len(s.path)-1])))
		return
	}
	f.sendDDL(r, "DROP FUNCTION IF EXISTS "+functionName(s.path))
	f.next.ServeHTTP(w, r)
}

// failBeforeRun answers a query whose first statement fails in BigQuery
// before anything runs (e), as BigQuery answers it: jobs.query with code;
// a query job (insert) as a job with e as its errorResult, which jobs.get
// and jobs.list then report too. For the job, the emulator is sent a
// statement that does nothing in the query's place, DROP TABLE IF EXISTS
// of a table that is not there, in dataset (measured: it runs, also in a
// dataset that does not exist), so that it records the job; jobs.get shows
// the client's text (jobTexts).
func (f front) failBeforeRun(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, dataset string, code int, e rowError) {
	if !insert {
		writeError(w, code, e.Reason, e.Message)
		return
	}
	if !setQueryText(r, true, "DROP TABLE IF EXISTS "+quotePath([]string{dataset, scratchTable()})) {
		writeError(w, code, e.Reason, e.Message)
		return
	}
	rec := newRecorder()
	f.forward(rec, r, jobText{query: q.Query})
	var job map[string]any
	if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &job) != nil {
		writeError(w, code, e.Reason, e.Message)
		return
	}
	f.fail(w, rec, job, e)
}

// knownFunctions are the functions CREATE FUNCTION statements the front
// sent on may have made, by project and dataset (#990): the emulator has
// no routines.list (measured: 500 "unsupported bigquery.routines.list";
// routines.get answers 404 for every function, even one routines.insert
// made) nor INFORMATION_SCHEMA.ROUTINES, and datasets.delete leaves a
// dataset's functions callable in its engine (measured), so DROP SCHEMA
// looks these up (functionKind) to find the functions in a dataset.
//
// Since #1001 they also hold the functions routines.insert made through
// the front (noteRoutine), and, the first time a project's are asked for,
// those the CREATE FUNCTION statements of the emulator's jobs from before
// the front started may have made (scan): the emulator keeps every job,
// and its query, in memory for as long as it runs, so a front that
// restarted without it (its container, not the pod) reads them there. A
// function made by EXECUTE IMMEDIATE's string need not be known: the
// emulator runs no EXECUTE IMMEDIATE (measured: `EXECUTE IMMEDIATE 'SELECT
// 1'` answered no rows, and one of a CREATE FUNCTION or CREATE TABLE made
// nothing, #1011). Not known: a function made through routines.insert
// before the front started, which is in no job.
type knownFunctions struct {
	mu    sync.Mutex
	funcs map[string]map[string][]string // project/dataset, lower-case name -> path
	// started is when the front started, in Unix milliseconds; scanned,
	// the projects whose jobs from before it were read (scan).
	started int64
	scanned map[string]bool
}

// add records a function, path its dataset and name.
func (k *knownFunctions) add(project string, path []string) {
	key := project + "/" + path[0]
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.funcs == nil {
		k.funcs = map[string]map[string][]string{}
	}
	if k.funcs[key] == nil {
		k.funcs[key] = map[string][]string{}
	}
	k.funcs[key][strings.ToLower(path[1])] = path
}

// note records the functions a query's CREATE FUNCTION statements make.
func (k *knownFunctions) note(project string, q queryOptions, v ddlVerdict) {
	if k == nil {
		return
	}
	for _, s := range v.funcs {
		if s.temp || s.drop {
			continue
		}
		full, ok := functionPath(q, s.path)
		if !ok {
			continue
		}
		k.add(project, full[len(full)-2:])
	}
}

// in returns the functions known in a dataset.
func (k *knownFunctions) in(project, dataset string) [][]string {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	var out [][]string
	for _, p := range k.funcs[project+"/"+dataset] {
		out = append(out, p)
	}
	return out
}

// forget drops what is known of a dataset's functions.
func (k *knownFunctions) forget(project, dataset string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.funcs, project+"/"+dataset)
}

// scanSlack is how long before the front started a job may have been made
// and still be read by scan: the emulator's clock and the front's are the
// same node's, and a job the front sent on at its start is read again at
// no cost but a jobs.get.
const scanSlack = 1000 // milliseconds

// functionsIn returns the functions known in a dataset (knownFunctions),
// after reading, the first time a project's are asked for, the queries of
// the emulator's jobs from before the front started (scan).
func (f front) functionsIn(r *http.Request, project, dataset string) [][]string {
	k := f.functions
	if k == nil {
		return nil
	}
	k.mu.Lock()
	done := k.scanned[project]
	k.mu.Unlock()
	if !done && project == projectOf(f.base) && f.scanJobs(r, project) {
		k.mu.Lock()
		if k.scanned == nil {
			k.scanned = map[string]bool{}
		}
		k.scanned[project] = true
		k.mu.Unlock()
	}
	return k.in(project, dataset)
}

// scanJobs notes the functions the CREATE FUNCTION statements of the
// emulator's jobs made before the front started may have made (above):
// jobs.list gives every job with its creationTime but no configuration
// (#958), and jobs.get of each job from before then its query. It reports
// whether it could read the list.
func (f front) scanJobs(r *http.Request, project string) bool {
	status, got := f.get(r, "/jobs?allUsers=true&projection=full")
	var list struct {
		Jobs []struct {
			JobReference struct {
				JobID string `json:"jobId"`
			} `json:"jobReference"`
			Statistics struct {
				CreationTime json.Number `json:"creationTime"`
			} `json:"statistics"`
		} `json:"jobs"`
	}
	dec := json.NewDecoder(bytes.NewReader(got))
	dec.UseNumber()
	if status != http.StatusOK || dec.Decode(&list) != nil {
		return false
	}
	for _, j := range list.Jobs {
		if j.JobReference.JobID == "" {
			continue
		}
		if t, err := j.Statistics.CreationTime.Int64(); err == nil && f.functions.started > 0 && t > f.functions.started+scanSlack {
			continue
		}
		status, got := f.get(r, "/jobs/"+url.PathEscape(j.JobReference.JobID))
		var job struct {
			Configuration struct {
				Query *queryOptions `json:"query"`
			} `json:"configuration"`
		}
		if status != http.StatusOK || json.Unmarshal(got, &job) != nil || job.Configuration.Query == nil {
			continue
		}
		q := *job.Configuration.Query
		if q.UseLegacySQL != nil && *q.UseLegacySQL || !strings.Contains(strings.ToUpper(q.Query), "FUNCTION") {
			continue
		}
		f.functions.note(project, q, checkDDL(q.Query))
	}
	return true
}

// routinesRoute matches routines.insert.
var routinesRoute = regexp.MustCompile(`^(/bigquery/v2)?/projects/([^/]+)/datasets/([^/]+)/routines$`)

// insertRoutine sends routines.insert on and, when it succeeds, notes the
// function it made (knownFunctions): the emulator makes it in its engine
// (measured: a SQL function routines.insert made was called), and DROP
// SCHEMA drops it with its dataset.
func (f front) insertRoutine(w http.ResponseWriter, r *http.Request, project string) {
	if refuseTableFunctionRoutine(w, r) { // #1043, tablefunctions.go
		return
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	var made struct {
		RoutineReference struct {
			DatasetID string `json:"datasetId"`
			RoutineID string `json:"routineId"`
		} `json:"routineReference"`
	}
	if rec.status == http.StatusOK && json.Unmarshal(rec.body.Bytes(), &made) == nil &&
		made.RoutineReference.DatasetID != "" && made.RoutineReference.RoutineID != "" {
		f.functions.add(project, []string{made.RoutineReference.DatasetID, made.RoutineReference.RoutineID})
	}
	rec.copyTo(w)
}
