package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
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

// jobRoute matches jobs.get, and jobs.getQueryResults (queries).
var jobRoute = regexp.MustCompile(`^(/bigquery/v2)?/projects/([^/]+)/(jobs|queries)/([^/]+)$`)

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
	csv := l.SourceFormat == "" || strings.EqualFold(l.SourceFormat, "CSV")
	skip, skipSet := skipLeadingRows(l.SkipLeadingRows)
	if csv && skipSet && skip != 1 {
		writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf("Not implemented here: a CSV load with "+
			"autodetect and skipLeadingRows %d into a new table. BigQuery skips the rows it is told to and looks for a header "+
			"in the last of them (none, for 0), but the emulator behind CloudBurrow ignores skipLeadingRows and takes the "+
			"first row as the header (measured). Nothing was loaded. Give the load a schema, or leave skipLeadingRows unset "+
			"or 1.", skip))
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
	var header string
	if csv {
		header = headerDiffers(meta.Schema.Fields)
	}
	msg := ""
	if header == "" {
		msg = checkNames(meta.Schema.Fields, "", nil, check)
	}
	if msg == "" && header == "" {
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

	if header != "" {
		e := rowError{Reason: "notImplemented", Message: "Not implemented here: a CSV load with autodetect whose first row " +
			"BigQuery would not take as its header: " + header + " BigQuery takes the first row as the header only when it " +
			"holds only strings and another row does not; otherwise it loads it as data and names the columns itself. The " +
			"emulator behind CloudBurrow always takes the first row as the header (measured). Nothing was loaded. Give the " +
			"load a schema, or a header row BigQuery detects."}
		f.failed.add(project, id, e)
		writeError(w, http.StatusNotImplemented, e.Reason, e.Message)
		return
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
// loads are what the front counted of the CSV loads it read, which
// getJob and listJobs add to the emulator's answer (#960, loadstats.go).
type jobFailures struct {
	mu        sync.Mutex
	errs      map[string]rowError
	order     []string
	loads     map[string]loadCounts
	loadOrder []string
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

func (j *jobFailures) remove(project, id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.errs, project+"/"+id)
}

// watch returns next, noting the failure of each job the emulator fails
// when it answers the jobs.insert sent through it (#934): a query job
// answered with status.errorResult, or a load answered with an error
// status. The emulator keeps neither: measured against the pinned image, a
// query job it answered with errorResult "Table not found" read back from
// jobs.get as done with no error, and a load it answered 400 read back
// from jobs.get the same way and was listed by jobs.list with no status.
// BigQuery keeps a job's errorResult, so getJob and listJobs report it.
//
// project and id are the job's, from the request: an error answer names
// no job. Only 400 and 404 answers are noted, as the emulator has recorded
// the job by then (a load's data is read after its job is made); a 409 is
// about a job ID that is already some other job's. A job the emulator
// answers without an error is no longer noted failed.
func (j *jobFailures) watch(next http.Handler, project, id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := jobsRoute.FindStringSubmatch(r.URL.EscapedPath()); m == nil || m[3] != "jobs" || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		j.note(project, id, rec)
		rec.copyTo(w)
	})
}

// note records what a jobs.insert answer says of the job.
func (j *jobFailures) note(project, id string, rec *recorder) {
	switch rec.status {
	case http.StatusOK:
		var job struct {
			JobReference struct {
				ProjectID string `json:"projectId"`
				JobID     string `json:"jobId"`
			} `json:"jobReference"`
			Status struct {
				ErrorResult *rowError `json:"errorResult"`
			} `json:"status"`
		}
		if json.Unmarshal(rec.body.Bytes(), &job) != nil {
			return
		}
		if job.JobReference.JobID != "" {
			id = job.JobReference.JobID
		}
		if job.JobReference.ProjectID != "" {
			project = job.JobReference.ProjectID
		}
		if job.Status.ErrorResult == nil {
			j.remove(project, id)
			return
		}
		j.add(project, id, *job.Status.ErrorResult)
	case http.StatusBadRequest, http.StatusNotFound:
		var e struct {
			Error struct {
				Message string     `json:"message"`
				Errors  []rowError `json:"errors"`
			} `json:"error"`
		}
		if json.Unmarshal(rec.body.Bytes(), &e) != nil || e.Error.Message == "" {
			return
		}
		re := rowError{Reason: "invalid", Message: e.Error.Message}
		if len(e.Error.Errors) > 0 && e.Error.Errors[0].Reason != "" {
			re.Reason = e.Error.Errors[0].Reason
		}
		j.add(project, id, re)
	}
}

func (j *jobFailures) get(project, id string) (rowError, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.errs[project+"/"+id]
	return e, ok
}

// getJob answers jobs.get, with the failure the front gave the job, if it
// gave it one. With results, it answers jobs.getQueryResults, which the Go
// client's Job.Wait reads for a query job, with that failure as the error,
// as BigQuery answers it for a failed job: the emulator answers it with
// the rows of the job it ran, or with its own error (measured).
func (j *jobFailures) getJob(next http.Handler, w http.ResponseWriter, r *http.Request, project, rawID string, results bool) {
	id, err := url.PathUnescape(rawID)
	e, failed := j.get(project, id)
	if err == nil && !failed && !results {
		if c, ok := j.load(project, id); ok {
			rec := newRecorder()
			next.ServeHTTP(rec, r)
			var job map[string]any
			if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &job) != nil {
				rec.copyTo(w)
				return
			}
			c.apply(job)
			writeJSON(w, http.StatusOK, job)
			return
		}
	}
	if err != nil || !failed {
		next.ServeHTTP(w, r)
		return
	}
	if results {
		code := http.StatusBadRequest
		if e.Reason == "notImplemented" {
			code = http.StatusNotImplemented
		}
		writeError(w, code, e.Reason, e.Message)
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

// skipLeadingRows reads a load's skipLeadingRows, and whether it was given.
func skipLeadingRows(raw json.RawMessage) (int64, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// headerDiffers returns why BigQuery would not take the first row of the
// CSV an autodetect load made a table from as its header, or "" (#919).
//
// "If the first line contains only strings, and the other lines contain
// other data types, BigQuery assumes that the first row is a header row"
// (https://cloud.google.com/bigquery/docs/schema-detect#csv_header). The
// emulator takes it as the header whatever it holds (measured: a,b / c,d
// gave columns a and b, and 1,2 / 3,4 columns "1" and "2"), so the table it
// made tells both halves of the rule: the first line is the column names,
// and the other lines' types are the columns' types. A name that reads as
// a value of another type (a number, a boolean, a date or a timestamp, as
// the emulator's own detection reads them) means the first line was not
// all strings; columns that are all STRING mean no other line held another
// type.
func headerDiffers(fields []field) string {
	allStrings := true
	for _, fl := range fields {
		if looksTyped(fl.Name) {
			return fmt.Sprintf("its first row holds %q, which is not a string.", fl.Name)
		}
		if !strings.EqualFold(fl.Type, "STRING") {
			allStrings = false
		}
	}
	if allStrings && len(fields) > 0 {
		return "every row holds only strings."
	}
	return ""
}

// looksTyped reports whether a CSV value reads as a number, a boolean, a
// date or a timestamp: the types the emulator detects a column as.
func looksTyped(s string) bool {
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	switch strings.ToLower(s) {
	case "true", "false":
		return true
	}
	for _, layout := range []string{"2006-01-02", "2006-01-02 15:04:05", "2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999", time.RFC3339, time.RFC3339Nano} {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

// listJobs answers jobs.list, with the failure the front gave each job it
// failed (#919): the emulator lists such a job as it recorded it, done and
// succeeded (measured).
// https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/list
func (j *jobFailures) listJobs(next http.Handler, w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	none := len(j.errs) == 0 && len(j.loads) == 0
	j.mu.Unlock()
	if none {
		next.ServeHTTP(w, r)
		return
	}
	rec := newRecorder()
	next.ServeHTTP(rec, r)
	var list map[string]any
	if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil {
		rec.copyTo(w)
		return
	}
	jobs, _ := list["jobs"].([]any)
	for _, item := range jobs {
		job, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ref, _ := job["jobReference"].(map[string]any)
		project, _ := ref["projectId"].(string)
		id, _ := ref["jobId"].(string)
		if e, failed := j.get(project, id); failed {
			failJob(job, e)
			job["errorResult"] = e
			job["state"] = "DONE"
		} else if c, ok := j.load(project, id); ok {
			c.apply(job)
		}
	}
	writeJSON(w, http.StatusOK, list)
}
