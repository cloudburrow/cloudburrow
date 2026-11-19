package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Jobs that outlive their request (#1819).
//
// On BigQuery a job runs whatever its client does, and the methods that
// start one do not wait for its end:
//
//   - jobs.insert answers with the Job, which is usually PENDING or
//     RUNNING; the client polls jobs.get, or jobs.getQueryResults.
//   - jobs.query waits at most timeoutMs: "By default, this limit is 10
//     seconds (10,000 milliseconds). If the query is complete, the
//     jobComplete field in the response is true. If the query has not yet
//     completed, jobComplete is false", and "the call is not guaranteed to
//     wait for the specified timeout; it typically returns after around
//     200 seconds". Its jobReference "will be present even if the original
//     request timed out, in which case jobs.getQueryResults can be used to
//     read the results once the query has completed"
//     (https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/query).
//   - jobs.getQueryResults waits the same way, with the same default, and
//     answers jobComplete false while the job runs
//     (https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/getQueryResults).
//   - jobs.get reports status.state PENDING, RUNNING or DONE
//     (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatus).
//   - jobs.cancel "Requests that a job be cancelled. This call will return
//     immediately, and the client will need to poll for the job status"
//     (https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/cancel);
//     a cancelled job ends with reason "stopped": "This status code
//     returns when a job is canceled"
//     (https://cloud.google.com/bigquery/docs/error-messages).
//
// The front's layers carry out a job within the request that starts it
// (each answers once the job is done), so this layer stands outside them:
// it runs each jobs.insert and jobs.query on a request of its own, whose
// context no client ends, and answers the client when the job is done or
// when its wait is over, whichever comes first. A job still running is
// kept here (pending) until it ends: jobs.get answers it RUNNING,
// jobs.getQueryResults waits for it up to its timeoutMs and then answers
// jobComplete false, jobs.list and INFORMATION_SCHEMA.JOBS list it RUNNING
// (listed), and jobs.cancel ends its request's context, which stops the
// engine's statement (a DML statement stopped writes nothing, as
// jobtimeout.go's timeouts do) and the front's own work, and the job is
// then DONE, failed with reason "stopped". A job that finished before it
// could be stopped keeps its result, as BigQuery's "might not always
// succeed in canceling it".
//
// Every kind of job goes through here: queries, DML, scripts, DDL and
// CREATE MODEL, and loads, copies and extracts. A jobs.insert waits
// insertWait for its job, so a job that fails at once (an invalid query)
// is still refused by the jobs.insert, as BigQuery refuses it, and one
// that ends at once answers DONE. A resumable upload's requests, a body
// the front cannot read (compressed) and a dry run are carried out within
// their request, as before.
//
// A jobs.query names no job ID, and the job it makes is named deep in the
// front or by the engine. So each one is given an ID first (queryJobID),
// which its job takes: the engine's (bigquery-emulator patch 0097, the
// header JobIDPrefix sends with the request's first jobs.query to the
// engine), or where the front names it (a failed query, recorded by
// failedjobs.go). A slow jobs.query answers with that ID. When the job it
// made has another all the same (a script whose job the front names), the
// ID stands for it (alias) in jobs.get, jobs.getQueryResults and
// jobs.cancel, the answers naming the client's ID; jobs.list and
// INFORMATION_SCHEMA.JOBS then list the job by its own ID once it is done
// (gaps.md).
//
// The jobs running are kept in memory alone: a restart of the front's
// container ends their requests, and what the jobs store keeps of them
// (the engine's jobs, the front's state directory) is all that is left.

const (
	// queryWaitDefault is jobs.query's and getQueryResults' default
	// timeoutMs; queryWaitMax the most either waits.
	queryWaitDefault = 10 * time.Second
	queryWaitMax     = 200 * time.Second
	// insertWaitDefault is how long a jobs.insert waits for its job.
	insertWaitDefault = 2 * time.Second
	// maxAsyncDone is how many ended jobs are kept for their alias or
	// their error.
	maxAsyncDone = 2000
)

// asyncJobs are the jobs started through this layer (above).
type asyncJobs struct {
	mu    sync.Mutex
	jobs  map[string]*asyncJob // project + "/" + client's job ID
	ended []string             // keys of ended jobs, oldest first
	// insertWait is how long jobs.insert waits (insertWaitDefault when 0).
	insertWait time.Duration
	// location is a project's default location.
	location func(project string) string
	// clock is the instance's clock.
	clock func() time.Time
	// made is told of each job started (jobstable.go's noteMade).
	made func(project, id string)
	// stopped records a cancelled job that ended in error as failed with
	// e: f is a front of its project.
	stopped func(r *http.Request, j *asyncJob, e rowError)
}

// asyncJob is a job started through this layer.
type asyncJob struct {
	project, id, location string
	// conf is the job's configuration, as the client sent it.
	conf map[string]any
	// query marks a jobs.query's job.
	query   bool
	created int64
	host    string
	base    string
	done    chan struct{}
	cancel  context.CancelFunc
	// Set under asyncJobs.mu.
	cancelled bool
	ended     bool
	// told marks a job its client was answered RUNNING (or jobComplete
	// false): only those are listed (listed).
	told bool
	// rec is the answer of the job's request once it ended; engineID the
	// job ID that answer names.
	rec      *recorder
	engineID string
}

// stoppedError is a cancelled job's error.
func stoppedError() rowError {
	return rowError{Reason: "stopped", Message: "Job execution was cancelled: User requested cancellation"}
}

// queryJobIDKey carries a jobs.query's job ID (queryJobID).
type queryJobIDKey struct{}

// queryJobRef is the job ID this layer gave a jobs.query, which one job
// takes: the engine's (bigquery-emulator patch 0097, JobIDPrefix sends it
// with the request's first jobs.query to the engine) or the one the front
// names (a failed query, failedjobs.go), whichever comes first.
type queryJobRef struct {
	id   string
	used atomic.Bool
}

// takeQueryJobID returns the job ID ctx's jobs.query was given, once; ""
// when it has none or it was taken.
func takeQueryJobID(ctx context.Context) string {
	if ref, _ := ctx.Value(queryJobIDKey{}).(*queryJobRef); ref != nil && ref.used.CompareAndSwap(false, true) {
		return ref.id
	}
	return ""
}

// queryJobID is the job ID this layer gave the jobs.query of ctx, when no
// job has taken it, or a new one (newJobID).
func queryJobID(ctx context.Context) string {
	if id := takeQueryJobID(ctx); id != "" {
		return id
	}
	return newJobID()
}

func (a *asyncJobs) now() int64 {
	if a.clock != nil {
		return a.clock().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// handler returns next with this layer in front of it.
func (a *asyncJobs) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		if isInternalRequest(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		if m := jobsRoute.FindStringSubmatch(path); m != nil && r.Method == http.MethodPost && r.URL.Query().Get("uploadType") != "resumable" {
			if a.start(w, r, next, m[1]+"/projects/"+m[2], projectOf("/"+m[2]), m[3] == "queries") {
				return
			}
		}
		if m := jobActionRoute.FindStringSubmatch(path); m != nil && m[4] == "cancel" && r.Method == http.MethodPost {
			if a.serveCancel(w, r, next, projectOf("/"+m[2]), m[3]) {
				return
			}
		}
		if m := jobActionRoute.FindStringSubmatch(path); m != nil && m[4] == "delete" {
			if id, err := url.PathUnescape(m[3]); err == nil {
				a.forgetEnded(projectOf("/"+m[2]), id)
			}
		}
		if m := jobRoute.FindStringSubmatch(path); m != nil && r.Method == http.MethodGet {
			if a.serveGet(w, r, next, projectOf("/"+m[2]), m[4], m[3] == "queries") {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// start carries out a jobs.insert or jobs.query (query) on a request of
// its own (above), reporting false when it is to be carried out within
// r, as before.
func (a *asyncJobs) start(w http.ResponseWriter, r *http.Request, next http.Handler, base, project string, query bool) bool {
	if r.Header.Get("Content-Encoding") != "" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	_ = r.Body.Close()
	if err != nil {
		setBody(r, body)
		return false
	}
	if len(body) > maxBody {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		return false
	}
	setBody(r, body)
	j := &asyncJob{project: project, query: query, created: a.now(), host: r.Host, base: base, done: make(chan struct{})}
	wait := a.insertWait
	if wait <= 0 {
		wait = insertWaitDefault
	}
	if query {
		var q struct {
			DryRun    bool            `json:"dryRun"`
			Location  string          `json:"location"`
			TimeoutMs json.RawMessage `json:"timeoutMs"`
			Labels    any             `json:"labels"`
		}
		var raw map[string]any
		if json.Unmarshal(body, &q) != nil || json.Unmarshal(body, &raw) != nil || q.DryRun {
			return false
		}
		wait = queryWaitDefault
		if len(q.TimeoutMs) > 0 {
			wait = time.Duration(jobMs(q.TimeoutMs)) * time.Millisecond
		}
		j.id, j.location = newJobID(), q.Location
		qc := map[string]any{}
		for _, k := range queryRequestConfig {
			if v, ok := raw[k]; ok {
				qc[k] = v
			}
		}
		j.conf = map[string]any{"query": qc}
		if q.Labels != nil {
			j.conf["labels"] = q.Labels
		}
	} else {
		var job jobBody
		if !decodeJob(r, &job) {
			return false
		}
		var full struct {
			JobReference struct {
				Location string `json:"location"`
			} `json:"jobReference"`
			Configuration map[string]any `json:"configuration"`
		}
		if !decodeJob(r, &full) || full.Configuration == nil {
			return false
		}
		if dry, _ := full.Configuration["dryRun"].(bool); dry {
			return false
		}
		if job.JobReference.ProjectID != "" && job.JobReference.ProjectID != project {
			return false
		}
		if !withReference(r, &job, project) {
			return false
		}
		j.id, j.location, j.conf = job.JobReference.JobID, full.JobReference.Location, full.Configuration
	}
	if j.location == "" && a.location != nil {
		j.location = a.location(project)
	}
	if j.location == "" {
		j.location = "US"
	}
	key := project + "/" + j.id
	a.mu.Lock()
	if a.jobs == nil {
		a.jobs = map[string]*asyncJob{}
	}
	if old := a.jobs[key]; old != nil && !old.ended {
		a.mu.Unlock()
		writeError(w, http.StatusConflict, "duplicate", "Already Exists: Job "+project+":"+j.location+"."+j.id)
		return true
	}
	a.jobs[key] = j
	a.mu.Unlock()

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	if query {
		ctx = context.WithValue(ctx, queryJobIDKey{}, &queryJobRef{id: j.id})
	}
	j.cancel = cancel
	run := r.Clone(ctx)
	b, err := readBody(r)
	if err != nil {
		cancel()
		a.forget(key)
		return false
	}
	setBody(run, b)
	if a.made != nil {
		a.made(project, j.id)
	}
	go func() {
		defer cancel()
		rec := newRecorder()
		next.ServeHTTP(rec, run)
		a.end(run, j, rec)
	}()
	if wait > queryWaitMax {
		wait = queryWaitMax
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-j.done:
		a.mu.Lock()
		rec := j.rec
		a.mu.Unlock()
		rec.copyTo(w)
	case <-t.C:
		a.mu.Lock()
		j.told = true
		a.mu.Unlock()
		if query {
			writeJSON(w, http.StatusOK, map[string]any{"kind": "bigquery#queryResponse", "jobReference": j.ref(), "jobComplete": false})
		} else {
			writeJSON(w, http.StatusOK, a.running(j))
		}
	case <-r.Context().Done():
		// The client is gone; its job is not.
	}
	return true
}

// forgetEnded drops project's job id when it has ended: jobs.delete.
func (a *asyncJobs) forgetEnded(project, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if j := a.jobs[project+"/"+id]; j != nil && j.ended {
		delete(a.jobs, project+"/"+id)
	}
}

// reset drops the jobs that ended: the engine restarted, empty. Those
// running end on their own.
func (a *asyncJobs) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, j := range a.jobs {
		if j.ended {
			delete(a.jobs, k)
		}
	}
	a.ended = nil
}

// forget drops key's job.
func (a *asyncJobs) forget(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.jobs, key)
}

// end records j's end, rec its request's answer.
func (a *asyncJobs) end(run *http.Request, j *asyncJob, rec *recorder) {
	engineID := j.id
	if j.query && (rec.status == 0 || rec.status == http.StatusOK) {
		var resp struct {
			JobReference struct {
				JobID string `json:"jobId"`
			} `json:"jobReference"`
		}
		if json.Unmarshal(rec.body.Bytes(), &resp) == nil && resp.JobReference.JobID != "" {
			engineID = resp.JobReference.JobID
		}
	}
	a.mu.Lock()
	cancelled := j.cancelled
	a.mu.Unlock()
	// An answer that is no 200 with a body (a layer that wrote nothing
	// when its context ended) is a failure.
	failed := rec.status != 0 && rec.status != http.StatusOK || rec.body.Len() == 0
	if !failed && !j.query {
		// A jobs.insert answered 200 with its job failed is failed too.
		var job map[string]any
		if json.Unmarshal(rec.body.Bytes(), &job) == nil {
			st, _ := job["status"].(map[string]any)
			failed = st["errorResult"] != nil
		}
	}
	if cancelled && failed && a.stopped != nil {
		a.stopped(run, j, stoppedError())
		if j.query {
			rec = newRecorder()
			writeError(rec, http.StatusBadRequest, "stopped", stoppedError().Message)
		}
	}
	a.mu.Lock()
	j.rec, j.engineID, j.ended = rec, engineID, true
	key := j.project + "/" + j.id
	if j.query && (engineID != j.id || failed) {
		a.ended = append(a.ended, key)
		for len(a.ended) > maxAsyncDone {
			if old := a.jobs[a.ended[0]]; old != nil && old.ended {
				delete(a.jobs, a.ended[0])
			}
			a.ended = a.ended[1:]
		}
	} else {
		delete(a.jobs, key)
	}
	a.mu.Unlock()
	close(j.done)
}

// lookup returns project's job of the client's ID id, or nil.
func (a *asyncJobs) lookup(project, id string) *asyncJob {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.jobs[project+"/"+id]
}

// ref is j's jobReference.
func (j *asyncJob) ref() map[string]any {
	return map[string]any{"projectId": j.project, "jobId": j.id, "location": j.location}
}

// running is j's Job resource while it runs.
func (a *asyncJobs) running(j *asyncJob) map[string]any {
	conf := map[string]any{}
	for k, v := range j.conf {
		conf[k] = v
	}
	conf["jobType"] = jobTypeOf(conf)
	ms := strconv.FormatInt(j.created, 10)
	return map[string]any{
		"kind":          "bigquery#job",
		"etag":          strconv.Quote(j.id + "-" + ms),
		"id":            j.project + ":" + j.location + "." + j.id,
		"selfLink":      "http://" + j.host + j.base + "/jobs/" + url.PathEscape(j.id) + "?location=" + url.QueryEscape(j.location),
		"jobReference":  j.ref(),
		"configuration": conf,
		"status":        map[string]any{"state": "RUNNING"},
		"statistics":    map[string]any{"creationTime": ms, "startTime": ms},
	}
}

// serveGet answers jobs.get and jobs.getQueryResults (results) of a job
// started here, reporting false for any other.
func (a *asyncJobs) serveGet(w http.ResponseWriter, r *http.Request, next http.Handler, project, rawID string, results bool) bool {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		return false
	}
	j := a.lookup(project, id)
	if j == nil {
		return false
	}
	if results {
		wait := queryWaitDefault
		if v := r.URL.Query().Get("timeoutMs"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "invalid", "Invalid value for timeoutMs: "+v)
				return true
			}
			wait = time.Duration(n) * time.Millisecond
		}
		if wait > queryWaitMax {
			wait = queryWaitMax
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-j.done:
		case <-t.C:
		case <-r.Context().Done():
			return true
		}
	}
	a.mu.Lock()
	ended, rec, engineID := j.ended, j.rec, j.engineID
	a.mu.Unlock()
	if !ended {
		if results {
			writeJSON(w, http.StatusOK, map[string]any{"kind": "bigquery#getQueryResultsResponse", "etag": strconv.Quote(j.id),
				"jobReference": j.ref(), "jobComplete": false})
		} else {
			writeJSON(w, http.StatusOK, a.running(j))
		}
		return true
	}
	out := newRecorder()
	req := r
	if engineID != id {
		req = r.Clone(r.Context())
		u := *r.URL
		esc := url.PathEscape(engineID)
		u.RawPath = strings.TrimSuffix(r.URL.EscapedPath(), rawID) + esc
		u.Path, _ = url.PathUnescape(u.RawPath)
		req.URL = &u
	}
	next.ServeHTTP(out, req)
	if out.status == http.StatusNotFound && rec != nil && rec.status != 0 && rec.status != http.StatusOK {
		// No job was kept of a jobs.query that failed: its error is the
		// job's.
		if results {
			rec.copyTo(w)
			return true
		}
		e, ok := recordedFailure(rec.status, rec.body.Bytes())
		if !ok {
			e = rowError{Reason: "internalError", Message: strings.TrimSpace(rec.body.String())}
		}
		job := a.running(j)
		failJob(job, e)
		stats := job["statistics"].(map[string]any)
		stats["endTime"] = strconv.FormatInt(a.now(), 10)
		writeJSON(w, http.StatusOK, job)
		return true
	}
	if engineID != id {
		b := bytes.ReplaceAll(out.body.Bytes(), []byte(`"`+engineID+`"`), []byte(`"`+id+`"`))
		b = bytes.ReplaceAll(b, []byte("."+engineID+`"`), []byte("."+id+`"`))
		b = bytes.ReplaceAll(b, []byte(":"+engineID+`"`), []byte(":"+id+`"`))
		out.body.Reset()
		out.body.Write(b)
		out.header.Del("Content-Length")
	}
	out.copyTo(w)
	return true
}

// serveCancel answers jobs.cancel of a job started here that has not
// ended, reporting false for any other.
func (a *asyncJobs) serveCancel(w http.ResponseWriter, r *http.Request, next http.Handler, project, rawID string) bool {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		return false
	}
	j := a.lookup(project, id)
	if j == nil {
		return false
	}
	a.mu.Lock()
	ended, engineID := j.ended, j.engineID
	if !ended {
		j.cancelled = true
	}
	a.mu.Unlock()
	if ended {
		if engineID == id {
			return false
		}
		req := r.Clone(r.Context())
		u := *r.URL
		u.RawPath = strings.Replace(r.URL.EscapedPath(), "/jobs/"+rawID+"/", "/jobs/"+url.PathEscape(engineID)+"/", 1)
		u.Path, _ = url.PathUnescape(u.RawPath)
		req.URL = &u
		out := newRecorder()
		next.ServeHTTP(out, req)
		b := bytes.ReplaceAll(out.body.Bytes(), []byte(`"`+engineID+`"`), []byte(`"`+id+`"`))
		out.body.Reset()
		out.body.Write(b)
		out.header.Del("Content-Length")
		out.copyTo(w)
		return true
	}
	j.cancel()
	// "This call will return immediately": the job is answered as it is
	// now, or DONE if it ended in the meantime.
	select {
	case <-j.done:
	case <-time.After(500 * time.Millisecond):
	}
	a.mu.Lock()
	ended = j.ended
	a.mu.Unlock()
	if ended {
		out := newRecorder()
		a.serveGet(out, getOf(r, j), next, project, url.PathEscape(id), false)
		var job map[string]any
		if json.Unmarshal(out.body.Bytes(), &job) == nil && out.status == http.StatusOK {
			writeJSON(w, http.StatusOK, map[string]any{"kind": "bigquery#jobCancelResponse", "job": job})
			return true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": "bigquery#jobCancelResponse", "job": a.running(j)})
	return true
}

// getOf is a jobs.get of j on r's connection.
func getOf(r *http.Request, j *asyncJob) *http.Request {
	req := r.Clone(r.Context())
	req.Method = http.MethodGet
	req.Body = http.NoBody
	req.ContentLength = 0
	u := *r.URL
	u.RawPath = j.base + "/jobs/" + url.PathEscape(j.id)
	u.Path, _ = url.PathUnescape(u.RawPath)
	u.RawQuery = ""
	req.URL = &u
	return req
}

// listed adds to l, a jobs.list of project's jobs, the jobs started here
// that are still running, and that their client was told so, and the
// engine did not list. (A job that ends within its jobs.insert's or
// jobs.query's wait is listed once, DONE, as its client first saw it.) Every list the
// front asks the engine for (engineList) is of jobs that may be running:
// those made after a cursor, those not DONE, or those named, and a job
// started here was named to the table of JOBS rows when it started
// (jobstable.go's noteMade), so each list has them.
func (a *asyncJobs) listed(r *http.Request, project string, l jobListing) jobListing {
	if l.list == nil {
		return l
	}
	a.mu.Lock()
	var running []*asyncJob
	for _, j := range a.jobs {
		if j.project == project && !j.ended && j.told {
			running = append(running, j)
		}
	}
	a.mu.Unlock()
	if len(running) == 0 {
		return l
	}
	items := l.jobs()
	have := map[string]bool{}
	for _, item := range items {
		job, _ := item.(map[string]any)
		_, id := jobRef(job)
		have[id] = true
	}
	for _, j := range running {
		if have[j.id] {
			continue
		}
		job := a.running(j)
		job["state"] = "RUNNING"
		delete(job, "etag")
		delete(job, "selfLink")
		items = append(items, job)
	}
	l.list["jobs"] = items
	return l
}
