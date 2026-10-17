package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

// A query that fails is a job (#1397).
//
// jobs.query's jobCreationMode defaults to JOB_CREATION_REQUIRED, "Job
// creation is always required"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/query):
// a query that fails is a job, DONE with its errorResult and errors, which
// jobs.get, jobs.list and INFORMATION_SCHEMA.JOBS show; jobs.query's
// answer is still the error, which names no job (the official Go client
// reads no jobReference from an error, query.go Query.Read).
// JOB_CREATION_OPTIONAL lets BigQuery skip the job under conditions that
// "are internal and subject to change" (the Go client's options.go);
// nothing documents a failed query run so, and it is given a job too.
//
// The pinned emulator makes none: measured through the front
// (2026-10-16), the eight jobs.query of twenty queries that failed (Table
// not found, a syntax error) were missing from jobs.list, and its source
// (server/handler.go, jobsQueryHandler.Handle) returns the query's error
// before AddJob. jobsInsertHandler.Handle records a query job that fails,
// but returns before AddJob on a missing destination dataset, a
// CREATE_NEVER destination that does not exist and a failed write of the
// destination; the front answered those jobs.insert with the emulator's
// error, and jobs.get of the job then answered 404.
//
// So the front records such a job itself, as one of its own (frontJobs):
// DONE, failed with the error, with the client's configuration and the
// times the front received the request and answered it. jobs.get,
// jobs.list (and so INFORMATION_SCHEMA.JOBS), jobs.cancel and jobs.delete
// read it there, and jobs.getQueryResults answers its error. A jobs.insert
// the emulator so refused is answered as BigQuery answers a query job that
// fails, 200 with the job DONE and its errorResult; the front's own
// refusals of a jobs.insert are left as they are (failBeforeRun records
// those BigQuery runs and fails). No job is made for a dry run (BigQuery
// makes none), a jobs.insert answered 409 (its job ID is another job's),
// an answer 401 or 403, or a query answered 501, which BigQuery would run.

// recordedFailure reads a query's error answer (status and body) as the
// job's errorResult, and reports false for an answer that is no failure
// of the query (above).
func recordedFailure(status int, body []byte) (rowError, bool) {
	if status == 0 || status == http.StatusOK || status == http.StatusConflict ||
		status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotImplemented {
		return rowError{}, false
	}
	var e struct {
		Error struct {
			Message string     `json:"message"`
			Errors  []rowError `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error.Message == "" {
		return rowError{}, false
	}
	re := rowError{Reason: "invalidQuery", Message: e.Error.Message}
	if len(e.Error.Errors) > 0 {
		if e.Error.Errors[0].Reason != "" {
			re.Reason = e.Error.Errors[0].Reason
		}
		re.Location = e.Error.Errors[0].Location
	}
	return re, true
}

// recordFailedJob records a query job that failed with e, of the
// project, ID, location (or "") and configuration given, received at
// created, among the front's own jobs, and returns it.
func (f front) recordFailedJob(r *http.Request, project, id, location string, conf map[string]any, e rowError, created int64) map[string]any {
	ref := map[string]any{"projectId": project, "jobId": id}
	if location != "" {
		ref["location"] = location
	}
	conf["jobType"] = "QUERY"
	ended := f.records.clock()
	ms := func(t int64) string { return strconv.FormatInt(t, 10) }
	job := map[string]any{
		"kind":          "bigquery#job",
		"id":            project + ":" + id,
		"jobReference":  ref,
		"configuration": conf,
		"selfLink":      "http://" + r.Host + f.base + "/jobs/" + url.PathEscape(id),
		"statistics":    map[string]any{"creationTime": ms(created), "startTime": ms(created), "endTime": ms(ended)},
	}
	failJob(job, e)
	if f.jobs != nil {
		f.jobs.add(project, id, job)
	}
	return job
}

// queryRequestConfig is the part of a QueryRequest that is a query job's
// configuration.query, and its labels.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/query#QueryRequest
var queryRequestConfig = []string{"query", "useLegacySql", "defaultDataset", "parameterMode", "queryParameters",
	"maximumBytesBilled", "useQueryCache", "connectionProperties", "createSession"}

// recordFailedQuery records a jobs.query (raw its request, answered
// status and got) that failed as a job (above).
func (f front) recordFailedQuery(r *http.Request, raw []byte, status int, got []byte, created int64) {
	e, ok := recordedFailure(status, got)
	if !ok {
		return
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return
	}
	if dry, _ := body["dryRun"].(bool); dry {
		return
	}
	if q, _ := body["query"].(string); q == "" {
		return
	}
	qc := map[string]any{}
	for _, k := range queryRequestConfig {
		if v, ok := body[k]; ok {
			qc[k] = v
		}
	}
	conf := map[string]any{"query": qc}
	if l, ok := body["labels"]; ok {
		conf["labels"] = l
	}
	location, _ := body["location"].(string)
	f.recordFailedJob(r, projectOf(f.base), newJobID(), location, conf, e, created)
}

// recordFailedInsert answers w with rec, the answer to a query job's
// jobs.insert (raw its body as the client sent it), as the job failed when the emulator refused the job
// without recording it (refused, above).
func (f front) recordFailedInsert(w http.ResponseWriter, r *http.Request, raw []byte, rec *recorder, job jobBody, project string, created int64, refused *emulatorRefusal) {
	e, ok := recordedFailure(rec.status, rec.body.Bytes())
	if !ok || refused.status != rec.status || job.Configuration.Query == nil || job.JobReference.JobID == "" {
		rec.copyTo(w)
		return
	}
	var body struct {
		JobReference struct {
			Location string `json:"location"`
		} `json:"jobReference"`
		Configuration map[string]any `json:"configuration"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Configuration == nil {
		rec.copyTo(w)
		return
	}
	if dry, _ := body.Configuration["dryRun"].(bool); dry {
		rec.copyTo(w)
		return
	}
	if st, _ := f.get(r, "/jobs/"+url.PathEscape(job.JobReference.JobID)); st != http.StatusNotFound {
		rec.copyTo(w) // the emulator has the job (jobFailures noted its failure)
		return
	}
	// The client's query, destination and disposition: the front may have
	// sent others (querywrite.go, results.go).
	if qc, ok := body.Configuration["query"].(map[string]any); ok {
		c := job.Configuration.Query
		qc["query"] = c.Query
		delete(qc, "destinationTable")
		delete(qc, "writeDisposition")
		if c.DestinationTable != nil {
			qc["destinationTable"] = map[string]any{"projectId": c.DestinationTable.ProjectID,
				"datasetId": c.DestinationTable.DatasetID, "tableId": c.DestinationTable.TableID}
		}
		if c.WriteDisposition != "" {
			qc["writeDisposition"] = c.WriteDisposition
		}
	}
	writeJSON(w, http.StatusOK, f.recordFailedJob(r, project, job.JobReference.JobID, body.JobReference.Location, body.Configuration, e, created))
}

// emulatorRefusal is the error status the emulator answered a jobs.insert
// with, or 0.
type emulatorRefusal struct{ status int }

// watch returns next, noting in e the status of each jobs.insert it
// answers 400 or 404.
func (e *emulatorRefusal) watch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := jobsRoute.FindStringSubmatch(r.URL.EscapedPath()); m == nil || m[3] != "jobs" || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == http.StatusBadRequest || sw.status == http.StatusNotFound {
			e.status = sw.status
		}
	})
}

// failedJobResults answers jobs.getQueryResults of one of the front's own
// jobs that failed with its error, as BigQuery answers it, and reports
// whether the job was one.
func (j *frontJobs) failedJobResults(w http.ResponseWriter, project, rawID string) bool {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		return false
	}
	job, ok := j.get(project, id)
	if !ok {
		return false
	}
	st, _ := job["status"].(map[string]any)
	e, ok := st["errorResult"].(rowError)
	if !ok {
		return false
	}
	writeError(w, reasonStatus(e.Reason, http.StatusBadRequest), e.Reason, e.Message) // #1581, errorcodes.go
	return true
}
