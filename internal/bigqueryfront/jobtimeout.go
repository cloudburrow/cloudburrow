package bigqueryfront

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// Query job timeouts (#1456).
//
// A job's configuration.jobTimeoutMs, and jobs.query's jobTimeoutMs: "Job
// timeout in milliseconds relative to the job creation time. If this time
// limit is exceeded, BigQuery attempts to stop the job, but might not
// always succeed in canceling it before the job completes"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfiguration).
// A query job given none takes its region's default_query_job_timeout_ms
// (projectoptions.go, "the default time after which a query job times
// out"; https://cloud.google.com/bigquery/docs/default-configuration).
//
// The front runs the query with a deadline that far from when it received
// the request: the emulator's requests carry it, and the engine stops the
// statement when the request is cancelled, rolling back its transaction,
// so a DML statement stopped writes nothing. The job is then DONE, failed
// with reason "timeout" (https://cloud.google.com/bigquery/docs/error-messages:
// "This error returns when the job timed out"), and recorded among the
// front's own jobs (failedjobs.go). A query that finished before the
// front could stop it keeps its result, as BigQuery's may.
//
// The queue timeouts (default_interactive_query_queue_timeout_ms,
// default_batch_query_queue_timeout_ms) bound how long a job waits in the
// queue for slots; an instance queues no job, each runs as it arrives, so
// none waits for them to bound.

// jobTimeout returns the timeout of a query job: given (jobTimeoutMs, a
// number or its string), else the default_query_job_timeout_ms of the
// job's region (location, or the project's default location); 0 for none.
func (f front) jobTimeout(given json.RawMessage, location string) time.Duration {
	if ms := jobMs(given); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if f.settings == nil || !f.settings.hasAny("default_query_job_timeout_ms") {
		return 0
	}
	project := projectOf(f.base)
	if location == "" {
		location = f.settings.defaultLocation(project)
	}
	if ms := jobMs(json.RawMessage(f.settings.effective(project, location, "default_query_job_timeout_ms"))); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 0
}

// jobMs reads an int64 of the REST API, a number or its string; 0 for
// none or one it cannot read.
func jobMs(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// withJobTimeout returns r with a deadline of d from now, and a function
// that reports whether the run, answered status, was stopped by it. With d
// 0, r is returned unchanged.
func withJobTimeout(r *http.Request, d time.Duration) (*http.Request, func(status int) bool, context.CancelFunc) {
	if d <= 0 {
		return r, func(int) bool { return false }, func() {}
	}
	ctx, cancel := context.WithTimeout(r.Context(), d)
	return r.WithContext(ctx), func(status int) bool {
		return status != http.StatusOK && errors.Is(ctx.Err(), context.DeadlineExceeded)
	}, cancel
}

// timeoutError is a job's error when its timeout, d, stopped it.
func timeoutError(d time.Duration) rowError {
	return rowError{Reason: "timeout", Message: "Job execution was cancelled: Job timed out after " + d.String()}
}

// timedOutInsert answers a query job's jobs.insert (raw its body as the
// client sent it) that its timeout stopped, with the job DONE and failed
// with e, recorded among the front's own jobs.
func (f front) timedOutInsert(w http.ResponseWriter, r *http.Request, raw []byte, job jobBody, project string, created int64, e rowError) {
	var body struct {
		JobReference struct {
			Location string `json:"location"`
		} `json:"jobReference"`
		Configuration map[string]any `json:"configuration"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Configuration == nil {
		writeError(w, http.StatusBadRequest, e.Reason, e.Message)
		return
	}
	writeJSON(w, http.StatusOK, f.recordFailedJob(r, project, job.JobReference.JobID, body.JobReference.Location, body.Configuration, e, created))
}
