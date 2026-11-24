package bigqueryfront

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
)

// Optional job creation mode (#2094).
//
// "Queries using optional job creation mode don't create a job when
// executed unless BigQuery determines that a job creation is necessary to
// complete the query": BigQuery "executes the query and returns all
// results in the rows field of the response. Since a job isn't created for
// this query, BigQuery doesn't return a jobReference in the response body.
// Instead, it returns a queryId field"; when a job is created, the
// response carries the jobReference and a jobCreationReason
// (https://docs.cloud.google.com/bigquery/docs/running-queries#optional-job-creation,
// read 2026-11-23; QueryResponse's queryId and jobCreationReason,
// https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/jobs/query).
//
// The engine makes a job of every jobs.query. So for a jobs.query whose
// jobCreationMode is JOB_CREATION_OPTIONAL and whose answer is a query's
// (a schema and no DML count) that completed with every row in the answer
// (no pageToken), the front deletes the engine's job, as it deletes its
// own (forgetInternalJob), and answers with a queryId in place of the
// jobReference: the job is in no jobs.list, jobs.get or JOBS view. A query
// that did not complete within the request, or whose rows need a page
// token, keeps its job, with jobCreationReason LONG_RUNNING or
// LARGE_RESULTS. A query that fails is a job (failedjobs.go). Which other
// conditions make BigQuery create one "are internal and subject to change"
// (the Go client's options.go); a DML or DDL statement is given its job,
// with jobCreationReason OTHER.

// optionalJobCreation rewrites answer, jobs.query's answer to the request
// whose body is raw, as above.
func (f front) optionalJobCreation(r *http.Request, raw []byte, answer *recorder) {
	var req struct {
		JobCreationMode string `json:"jobCreationMode"`
		DryRun          bool   `json:"dryRun"`
	}
	if json.Unmarshal(raw, &req) != nil || req.JobCreationMode != "JOB_CREATION_OPTIONAL" || req.DryRun {
		return
	}
	if answer.status != 0 && answer.status != http.StatusOK {
		return
	}
	var resp map[string]any
	if json.Unmarshal(answer.body.Bytes(), &resp) != nil {
		return
	}
	ref, _ := resp["jobReference"].(map[string]any)
	id, _ := ref["jobId"].(string)
	if id == "" {
		return
	}
	complete, _ := resp["jobComplete"].(bool)
	token, _ := resp["pageToken"].(string)
	_, hasSchema := resp["schema"]
	_, dml := resp["numDmlAffectedRows"]
	reason := ""
	switch {
	case !complete:
		reason = "LONG_RUNNING"
	case !hasSchema || dml:
		reason = "OTHER"
	case token != "":
		reason = "LARGE_RESULTS"
	}
	if reason != "" {
		resp["jobCreationReason"] = map[string]any{"code": reason}
	} else {
		path := "/jobs/" + url.PathEscape(id) + "/delete"
		if loc, _ := ref["location"].(string); loc != "" {
			path += "?location=" + url.QueryEscape(loc)
		}
		if st, _ := f.send(r, http.MethodDelete, path, nil); st != http.StatusOK && st != http.StatusNoContent {
			// The job could not be forgotten: it stays, and says why.
			resp["jobCreationReason"] = map[string]any{"code": "OTHER"}
		} else {
			delete(resp, "jobReference")
			resp["queryId"] = newQueryID()
		}
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return
	}
	answer.body.Reset()
	answer.body.Write(out)
	answer.header.Del("Content-Length")
}

// newQueryID is a query's ID, as BigQuery's are: an opaque URL-safe string.
func newQueryID() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
