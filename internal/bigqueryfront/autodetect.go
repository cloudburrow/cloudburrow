package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// A load with autodetect and no schema takes its columns from its data
// (#901). The emulator names them from a CSV's first row whatever they
// hold: it loaded a header "a b!,c" into a new table with a column "a b!"
// (measured against the pinned image). BigQuery's rule is its load's
// columnNameCharacterMap: STRICT, the default, "Support flexible column
// name and reject invalid column names"; V1 and V2 normalize an invalid
// name instead, to the classic rule and to the flexible one.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad
//
// The front does not read the data (it may be in Cloud Storage, or
// megabytes of an upload), so the schema is checked after the load, in the
// table the load made. Only a load that makes the table is checked: into
// an existing table the emulator keeps that table's schema (measured),
// whose names were held to the rules when it was made. A detected name the
// map refuses is then:
//
//   - STRICT or no map: the job fails, as BigQuery fails it. The table the
//     load made is deleted, so nothing is left of it, as BigQuery's load
//     creates its table only on success; the job is answered with
//     status.errorResult reason "invalid", and jobs.get reports it so
//     (jobFailures).
//   - V1 or V2: 501. BigQuery would rename the column; the emulator
//     ignores the map (measured: "a!" was kept under V2), and the front
//     does not rename. The table is deleted.
//
// A NEWLINE_DELIMITED_JSON load with autodetect into a new table is 501
// before it is sent: the emulator answers it with 500 "runtime error:
// invalid memory address or nil pointer dereference" (measured), which the
// Go client retries until its deadline.

// jobRoute matches jobs.get.
var jobRoute = regexp.MustCompile(`^(/bigquery/v2)?/projects/([^/]+)/jobs/([^/]+)$`)

func (f front) autodetectLoad(w http.ResponseWriter, r *http.Request, job jobBody) {
	l := job.Configuration.Load
	dest := l.DestinationTable
	if dest.DatasetID == "" || dest.TableID == "" {
		f.next.ServeHTTP(w, r)
		return
	}
	table := "/datasets/" + url.PathEscape(dest.DatasetID) + "/tables/" + url.PathEscape(dest.TableID)
	if status, _ := f.get(r, table); status != http.StatusNotFound {
		// An existing table keeps its schema; or the emulator is failing,
		// and its own answer to the load stands.
		f.next.ServeHTTP(w, r)
		return
	}
	if strings.EqualFold(l.SourceFormat, "NEWLINE_DELIMITED_JSON") {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a NEWLINE_DELIMITED_JSON load "+
			"with autodetect into a new table. BigQuery detects the schema from the data, but the emulator behind "+
			"CloudBurrow fails such a load (500, \"nil pointer dereference\", measured). Nothing was loaded. Give the "+
			"load a schema, or create the table first.")
		return
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	if rec.status != http.StatusOK {
		rec.copyTo(w)
		return
	}
	status, got := f.get(r, table)
	var meta struct {
		Schema tableSchema `json:"schema"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &meta) != nil {
		rec.copyTo(w)
		return
	}
	charMap := strings.ToUpper(l.ColumnNameCharacterMap)
	check := checkColumnName
	if charMap == "V1" {
		check = checkClassicColumnName
	}
	msg := checkNames(meta.Schema.Fields, "", nil, check)
	if msg == "" {
		rec.copyTo(w)
		return
	}
	f.send(r, http.MethodDelete, table, nil)

	var resp map[string]any
	_ = json.Unmarshal(rec.body.Bytes(), &resp)
	project, id := job.JobReference.ProjectID, job.JobReference.JobID
	if ref, ok := resp["jobReference"].(map[string]any); ok {
		if s, ok := ref["jobId"].(string); ok && s != "" {
			id = s
		}
		if s, ok := ref["projectId"].(string); ok && s != "" {
			project = s
		}
	}
	if project == "" {
		project = projectOf(f.base)
	}

	if charMap == "V1" || charMap == "V2" {
		e := rowError{Reason: "notImplemented", Message: "Not implemented here: columnNameCharacterMap " + charMap +
			" with autodetect. BigQuery would rename the detected column to its rules, but the emulator behind CloudBurrow " +
			"keeps the name as it is in the data, and CloudBurrow does not rename it. " + msg + " Nothing was loaded. " +
			"Give the load a schema, or name the columns in the data within the rules."}
		f.failed.add(project, id, e)
		writeError(w, http.StatusNotImplemented, e.Reason, e.Message)
		return
	}
	e := rowError{Reason: "invalid", Message: msg + " The name was detected from the load's data (autodetect); " +
		"columnNameCharacterMap STRICT, the default, refuses it rather than renaming it. Nothing was loaded."}
	f.failed.add(project, id, e)
	if resp == nil {
		writeError(w, http.StatusBadRequest, e.Reason, e.Message)
		return
	}
	failJob(resp, e)
	writeJSON(w, http.StatusOK, resp)
}

// failJob sets a Job resource's status to done and failed with e.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatus
func failJob(job map[string]any, e rowError) {
	job["status"] = map[string]any{"state": "DONE", "errorResult": e, "errors": []rowError{e}}
}

// classicColumnName is the rule columnNameCharacterMap V1 holds names to:
// "Support alphanumeric + underscore characters and names must start with
// a letter or underscore."
var classicColumnName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func checkClassicColumnName(name string) string {
	if utf8.RuneCountInString(name) <= maxColumnLength && !classicColumnName.MatchString(name) {
		return fmt.Sprintf("Invalid field name %q. Under columnNameCharacterMap V1, fields must contain only letters, numbers, and underscores, and start with a letter or underscore.", name)
	}
	return checkColumnName(name)
}

// jobFailures are the jobs the front failed after the emulator ran them.
// The emulator reports each done and succeeded, so jobs.get's answer is
// given the failure (getJob). The most recent maxJobFailures are kept.
type jobFailures struct {
	mu    sync.Mutex
	errs  map[string]rowError
	order []string
}

const maxJobFailures = 1000

func (j *jobFailures) add(project, id string, e rowError) {
	if id == "" {
		return
	}
	key := project + "/" + id
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.errs == nil {
		j.errs = map[string]rowError{}
	}
	if _, ok := j.errs[key]; !ok {
		j.order = append(j.order, key)
	}
	j.errs[key] = e
	for len(j.order) > maxJobFailures {
		delete(j.errs, j.order[0])
		j.order = j.order[1:]
	}
}

func (j *jobFailures) get(project, id string) (rowError, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.errs[project+"/"+id]
	return e, ok
}

// getJob answers jobs.get, with the failure the front gave the job, if it
// gave it one.
func (j *jobFailures) getJob(next http.Handler, w http.ResponseWriter, r *http.Request, project, rawID string) {
	id, err := url.PathUnescape(rawID)
	e, failed := j.get(project, id)
	if err != nil || !failed {
		next.ServeHTTP(w, r)
		return
	}
	rec := newRecorder()
	next.ServeHTTP(rec, r)
	var job map[string]any
	if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &job) != nil {
		rec.copyTo(w)
		return
	}
	failJob(job, e)
	writeJSON(w, http.StatusOK, job)
}
