package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// A query's error reaches the client with the same reason, and the HTTP
// status its reason documents, whichever way the query was run (#1581).
//
// BigQuery's "Troubleshoot errors" page gives each error reason the HTTP
// code it is answered with (its "Error table",
// https://cloud.google.com/bigquery/docs/error-messages: notFound 404,
// invalidQuery 400, duplicate 409, notImplemented 501, ...). jobs.query
// answers a query that fails with its error, so with that code; jobs.insert
// makes the job, DONE with status.errorResult and status.errors
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatus),
// and jobs.getQueryResults of it, which the Go client's Job.Wait and
// Job.Read call (job.go waitForQuery), answers that error with the same
// code. A dry run makes no job: "an invalid query will return the same
// error it would if it wasn't a dry run" (JobConfiguration.dryRun,
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfiguration).
//
// Measured through the front first (2026-10-17): ALTER SCHEMA and DROP
// SCHEMA of a dataset that does not exist answered jobs.query 404
// notFound, and jobs.getQueryResults of their job 400 notFound; CREATE
// SCHEMA of one that exists 409 and 400 duplicate; and a dry-run
// jobs.insert of a query that fails answered 200, a job DONE with the
// errorResult, whose jobs.getQueryResults then answered the error, where
// BigQuery has no such job. So jobErrors gives each error answer of
// jobs.query, jobs.insert and jobs.getQueryResults the code of its reason,
// and a failed dry-run jobs.insert is answered with its error.

// reasonCodes are the HTTP codes of BigQuery's error reasons, from the
// error table (above). A reason with several is answered with any of them.
var reasonCodes = map[string][]int{
	"accessDenied":                {http.StatusForbidden},
	"attributeError":              {http.StatusBadRequest},
	"backendError":                {http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout},
	"badRequest":                  {http.StatusBadRequest},
	"billingNotEnabled":           {http.StatusForbidden},
	"billingTierLimitExceeded":    {http.StatusBadRequest},
	"blocked":                     {http.StatusForbidden},
	"duplicate":                   {http.StatusConflict},
	"internalError":               {http.StatusInternalServerError},
	"invalid":                     {http.StatusBadRequest},
	"invalidQuery":                {http.StatusBadRequest},
	"invalidUser":                 {http.StatusBadRequest},
	"jobBackendError":             {http.StatusBadRequest},
	"jobInternalError":            {http.StatusBadRequest},
	"jobRateLimitExceeded":        {http.StatusBadRequest},
	"notFound":                    {http.StatusNotFound},
	"notImplemented":              {http.StatusNotImplemented},
	"proxyAuthenticationRequired": {http.StatusProxyAuthRequired},
	"quotaExceeded":               {http.StatusForbidden},
	"rateLimitExceeded":           {http.StatusForbidden, http.StatusTooManyRequests},
	"resourceInUse":               {http.StatusBadRequest},
	"resourcesExceeded":           {http.StatusBadRequest},
	"responseTooLarge":            {http.StatusForbidden},
	"tableUnavailable":            {http.StatusBadRequest},
	"timeout":                     {http.StatusBadRequest},
}

// reasonStatus is the HTTP code an error of reason is answered with: code
// when the reason documents it, or documents none (a reason not in the
// table); else the first the reason documents.
func reasonStatus(reason string, code int) int {
	codes, ok := reasonCodes[reason]
	if !ok || slices.Contains(codes, code) {
		return code
	}
	return codes[0]
}

// grpcStatus is the status name a Google API error body gives code.
var grpcStatus = map[int]string{
	http.StatusBadRequest:          "INVALID_ARGUMENT",
	http.StatusUnauthorized:        "UNAUTHENTICATED",
	http.StatusForbidden:           "PERMISSION_DENIED",
	http.StatusNotFound:            "NOT_FOUND",
	http.StatusConflict:            "ALREADY_EXISTS",
	http.StatusPreconditionFailed:  "FAILED_PRECONDITION",
	http.StatusTooManyRequests:     "RESOURCE_EXHAUSTED",
	http.StatusInternalServerError: "INTERNAL",
	http.StatusNotImplemented:      "UNIMPLEMENTED",
	http.StatusServiceUnavailable:  "UNAVAILABLE",
	http.StatusGatewayTimeout:      "DEADLINE_EXCEEDED",
}

// jobErrors returns next, answering jobs.query, jobs.insert and
// jobs.getQueryResults as above; failed are the jobs the front failed,
// from which a dry run's is dropped.
func jobErrors(next http.Handler, failed *jobFailures) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		project, dry := "", false
		switch m := jobsRoute.FindStringSubmatch(path); {
		case m != nil && r.Method == http.MethodPost:
			project = projectOf("/" + m[2])
			dry = m[3] == "jobs" && !strings.HasPrefix(path, "/upload/") && dryRun(r, true)
		case r.Method == http.MethodGet && jobRoute.MatchString(path) && jobRoute.FindStringSubmatch(path)[3] == "queries":
		default:
			next.ServeHTTP(w, r)
			return
		}
		dryID := ""
		if dry {
			if b, err := readBody(r); err == nil {
				var body struct {
					JobReference struct {
						JobID string `json:"jobId"`
					} `json:"jobReference"`
				}
				if json.Unmarshal(b, &body) == nil {
					dryID = body.JobReference.JobID
				}
			}
		}
		ew := &errorWriter{w: w, all: dry}
		next.ServeHTTP(ew, r)
		if ew.rec == nil {
			return
		}
		rec := ew.rec
		if dry && rec.status == http.StatusOK {
			var job struct {
				JobReference struct {
					ProjectID string `json:"projectId"`
					JobID     string `json:"jobId"`
				} `json:"jobReference"`
				Status struct {
					ErrorResult *rowError `json:"errorResult"`
				} `json:"status"`
			}
			if json.Unmarshal(rec.body.Bytes(), &job) == nil && job.Status.ErrorResult != nil && job.Status.ErrorResult.Message != "" {
				e := *job.Status.ErrorResult
				if job.JobReference.ProjectID != "" {
					project = job.JobReference.ProjectID
				}
				if failed != nil && job.JobReference.JobID != "" {
					failed.remove(project, job.JobReference.JobID)
				}
				if e.Reason == "" {
					e.Reason = "invalidQuery"
				}
				writeError(w, reasonStatus(e.Reason, http.StatusBadRequest), e.Reason, e.Message)
				return
			}
		}
		if dry && rec.status >= 400 && failed != nil {
			// The engine may have answered the dry run with a job DONE
			// and failed, which the front noted and then answered as an
			// error (scriptdryrun.go): no job is left.
			if dryID != "" {
				failed.remove(project, dryID)
			}
		}
		recodeError(rec)
		rec.copyTo(w)
	})
}

// recodeError gives rec, an error answer, the code its reason documents.
func recodeError(rec *recorder) {
	if rec.status < 400 {
		return
	}
	var body map[string]any
	if json.Unmarshal(rec.body.Bytes(), &body) != nil {
		return
	}
	e, _ := body["error"].(map[string]any)
	errs, _ := e["errors"].([]any)
	if len(errs) == 0 {
		return
	}
	first, _ := errs[0].(map[string]any)
	reason, _ := first["reason"].(string)
	code := reasonStatus(reason, rec.status)
	if code == rec.status {
		return
	}
	e["code"] = code
	if s, ok := grpcStatus[code]; ok {
		e["status"] = s
	} else {
		delete(e, "status")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return
	}
	rec.status = code
	rec.body.Reset()
	rec.body.Write(b)
}

// errorWriter passes an answer on to w as it is written, unless it is an
// error, or all is set: those it keeps in rec, for jobErrors to read.
type errorWriter struct {
	w   http.ResponseWriter
	all bool
	rec *recorder
	// passing: the answer is being written to w.
	passing bool
}

func (e *errorWriter) Header() http.Header {
	if e.rec != nil {
		return e.rec.Header()
	}
	return e.w.Header()
}

func (e *errorWriter) WriteHeader(code int) {
	if e.rec != nil || e.passing {
		return
	}
	if e.all || code >= 400 {
		e.rec = newRecorder()
		for k, v := range e.w.Header() {
			e.rec.header[k] = v
		}
		e.rec.WriteHeader(code)
		return
	}
	e.passing = true
	e.w.WriteHeader(code)
}

func (e *errorWriter) Write(b []byte) (int, error) {
	if e.rec == nil && !e.passing {
		e.WriteHeader(http.StatusOK)
	}
	if e.rec != nil {
		return e.rec.Write(b)
	}
	return e.w.Write(b)
}

// Flush flushes a passed-on answer.
func (e *errorWriter) Flush() {
	if f, ok := e.w.(http.Flusher); ok && e.passing {
		f.Flush()
	}
}
