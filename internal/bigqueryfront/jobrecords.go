package bigqueryfront

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A job's reference, times, and place in jobs.list (#971, #972, #973).
//
// jobReference (#973). The emulator reads a jobs.insert's
// jobReference.jobId without looking for the reference: measured against
// the pinned image through the front with the generated Go client
// (google.golang.org/api/bigquery/v2), a query job, succeeding or failing,
// and a load from Cloud Storage sent with no jobReference were each
// answered 500 "runtime error: invalid memory address or nil pointer
// dereference", which the Go client retries until its deadline. BigQuery
// makes the reference itself: "jobReference: Optional. Reference
// describing the unique-per-user name of the job"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job). So the
// front gives such a job one before anything else reads it (withReference):
// the project of the request's path, a job ID of its own (newJobID), and
// the location the job's reference gave, if it gave one.
//
// Times (#971). BigQuery gives a job's statistics.creationTime, startTime
// and endTime "in milliseconds since the epoch"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatistics).
// The emulator gives every job it runs (query, load from Cloud Storage,
// extract, and a jobs.query's) its times in seconds (its source,
// server/handler.go: `startTime.Unix()`), and an upload's job none at all;
// measured through the official Go client, a load from Cloud Storage and
// a query job read back with creationTime "1790593527", which
// JobStatistics.CreationTime read as 1970-01-21, and an upload's as the
// zero time. So the front times each jobs.insert and jobs.query it sends
// on: the job's creationTime and startTime are when the front received
// it, its endTime when the emulator answered. A job answered with no
// times, or times in seconds, is given those, in the jobs.insert answer,
// jobs.get, jobs.cancel and jobs.list; a job the front did not time (one
// made before the front started, or one of the last maxJobRecords
// evicted) has its seconds read as milliseconds (times 1000, so to the
// second). A time already in milliseconds (the jobs the front carries
// out itself, frontJobs) is left as it is.
//
// jobs.list (#972). The emulator's jobs.list lists every job of the
// project, in the order it made them, and reads none of the request's
// parameters (measured: maxResults=2 listed nine jobs, with no
// nextPageToken; its source, jobsListHandler). BigQuery's is "sorted in
// reverse chronological order, by job creation time", pages by maxResults
// and pageToken, and filters by stateFilter, minCreationTime and
// maxCreationTime ("in milliseconds since the POSIX epoch ... after or at
// this timestamp", "before or at"), and parentJobId ("If set, show only
// child jobs of the specified parent. Otherwise, show all top-level
// jobs") (https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/list).
// The front does each over the whole list (serveJobList), after the
// front's own jobs and the times above are in it, and leaves out the
// queries the front ran itself for its checks (internal), which are no
// client's jobs. allUsers changes nothing: the emulator has one user, so
// every job is the caller's.

// maxJobRecords bounds what jobRecords keeps of each kind.
const maxJobRecords = 5000

// jobTime is when the front received a job and when the emulator answered
// it, in milliseconds since the epoch.
type jobTime struct {
	created, ended int64
}

// jobRecords are the times the front gave jobs, and the jobs the front
// ran itself.
type jobRecords struct {
	mu            sync.Mutex
	times         map[string]jobTime
	timeOrder     []string
	internal      map[string]bool
	internalOrder []string
	// now is the clock, for tests; nil is time.Now.
	now func() time.Time
}

func (j *jobRecords) clock() int64 {
	if j == nil || j.now == nil {
		return time.Now().UnixMilli()
	}
	return j.now().UnixMilli()
}

func (j *jobRecords) setTime(project, id string, t jobTime) {
	if j == nil || id == "" {
		return
	}
	key := project + "/" + id
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.times == nil {
		j.times = map[string]jobTime{}
	}
	if _, ok := j.times[key]; !ok {
		j.timeOrder = append(j.timeOrder, key)
	}
	j.times[key] = t
	for len(j.timeOrder) > maxJobRecords {
		delete(j.times, j.timeOrder[0])
		j.timeOrder = j.timeOrder[1:]
	}
}

func (j *jobRecords) time(project, id string) (jobTime, bool) {
	if j == nil {
		return jobTime{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	t, ok := j.times[project+"/"+id]
	return t, ok
}

// markInternal records a job the front ran itself (front.send).
func (j *jobRecords) markInternal(project, id string) {
	if j == nil || id == "" {
		return
	}
	key := project + "/" + id
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.internal == nil {
		j.internal = map[string]bool{}
	}
	if !j.internal[key] {
		j.internal[key] = true
		j.internalOrder = append(j.internalOrder, key)
	}
	for len(j.internalOrder) > maxJobRecords {
		delete(j.internal, j.internalOrder[0])
		j.internalOrder = j.internalOrder[1:]
	}
}

func (j *jobRecords) isInternal(project, id string) bool {
	if j == nil {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.internal[project+"/"+id]
}

// withReference gives a jobs.insert whose job has no jobReference.jobId
// one (above), in r's body and in job, and reports false when the body
// could not be changed.
func withReference(r *http.Request, job *jobBody, project string) bool {
	if job.JobReference.JobID != "" {
		return true
	}
	id := newJobID()
	ok := editJob(r, func(j map[string]any) bool {
		ref, _ := j["jobReference"].(map[string]any)
		if ref == nil {
			ref = map[string]any{}
			j["jobReference"] = ref
		}
		if p, _ := ref["projectId"].(string); p == "" {
			ref["projectId"] = project
		}
		ref["jobId"] = id
		return true
	})
	if ok {
		if job.JobReference.ProjectID == "" {
			job.JobReference.ProjectID = project
		}
		job.JobReference.JobID = id
	}
	return ok
}

// msValue reads a JSON int64 (a string, as the REST API writes it, or a
// number).
func msValue(v any) (int64, bool) {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case float64:
		return int64(x), true
	}
	return 0, false
}

// secondsBelow is where a time is taken to be in seconds: 1e11 seconds is
// the year 5138, 1e11 milliseconds 1973.
const secondsBelow = 1e11

var timeFields = []string{"creationTime", "startTime", "endTime"}

// inSeconds reports whether a Job's statistics has no creationTime or
// gives its times in seconds.
func inSeconds(stats map[string]any) bool {
	n, ok := msValue(stats["creationTime"])
	return !ok || n < secondsBelow
}

// patchTimes gives job (a Job resource, or an entry of jobs.list)
// BigQuery's times: t's, when it has none or has them in seconds and the
// front timed it; else its seconds as milliseconds. It reports whether it
// changed the job.
func patchTimes(job map[string]any, t *jobTime) bool {
	stats, _ := job["statistics"].(map[string]any)
	if stats != nil && !inSeconds(stats) {
		return false
	}
	if t != nil {
		if stats == nil {
			stats = map[string]any{}
			job["statistics"] = stats
		}
		stats["creationTime"] = strconv.FormatInt(t.created, 10)
		stats["startTime"] = strconv.FormatInt(t.created, 10)
		stats["endTime"] = strconv.FormatInt(t.ended, 10)
		return true
	}
	changed := false
	for _, k := range timeFields {
		if n, ok := msValue(stats[k]); ok && n > 0 && n < secondsBelow {
			stats[k] = strconv.FormatInt(n*1000, 10)
			changed = true
		}
	}
	return changed
}

// jobRef reads a Job's reference.
func jobRef(job map[string]any) (project, id string) {
	ref, _ := job["jobReference"].(map[string]any)
	project, _ = ref["projectId"].(string)
	id, _ = ref["jobId"].(string)
	return project, id
}

// timed answers w with rec, the answer to a jobs.insert (a Job) or a
// jobs.query (a QueryResponse) the front received at created: the job's
// times are kept, and a Job with no times or times in seconds is given
// them. project and id are the request's, for an answer that names no
// job.
func (j *jobRecords) timed(w http.ResponseWriter, rec *recorder, created int64, project, id string, isJob bool) {
	if j == nil {
		rec.copyTo(w)
		return
	}
	t := jobTime{created: created, ended: j.clock()}
	var resp map[string]any
	if rec.status != 0 && rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &resp) != nil {
		// A job the emulator refused may still be recorded (jobFailures).
		j.setTime(project, id, t)
		rec.copyTo(w)
		return
	}
	if p, i := jobRef(resp); i != "" {
		id = i
		if p != "" {
			project = p
		}
	}
	if !isJob {
		j.setTime(project, id, t)
		rec.copyTo(w)
		return
	}
	stats, _ := resp["statistics"].(map[string]any)
	if stats != nil && !inSeconds(stats) {
		// Timed already: a job the front carried out itself.
		rec.copyTo(w)
		return
	}
	j.setTime(project, id, t)
	patchTimes(resp, &t)
	writeRecorded(w, rec, resp)
}

// writeRecorded answers w with rec's status and headers and body as its
// JSON.
func writeRecorded(w http.ResponseWriter, rec *recorder, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		rec.copyTo(w)
		return
	}
	rec.body.Reset()
	rec.body.Write(b)
	rec.copyTo(w)
}

// serveJob answers jobs.get or jobs.cancel (serve) with the job's times
// (patchTimes); a jobs.cancel answer's job is under "job".
func (j *jobRecords) serveJob(w http.ResponseWriter, project string, cancel bool, serve func(http.ResponseWriter)) {
	rec := newRecorder()
	serve(rec)
	var resp map[string]any
	if rec.status != 0 && rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &resp) != nil {
		rec.copyTo(w)
		return
	}
	job := resp
	if cancel {
		job, _ = resp["job"].(map[string]any)
	}
	if job == nil || !j.patch(job, project) {
		rec.copyTo(w)
		return
	}
	writeRecorded(w, rec, resp)
}

// patch gives one job its times, and reports whether it changed it.
func (j *jobRecords) patch(job map[string]any, project string) bool {
	p, id := jobRef(job)
	if p == "" {
		p = project
	}
	if t, ok := j.time(p, id); ok {
		return patchTimes(job, &t)
	}
	return patchTimes(job, nil)
}

// listedJob is an entry of jobs.list with what serveJobList sorts and
// filters it by.
type listedJob struct {
	job     map[string]any
	id      string
	created int64
}

// serveJobList answers jobs.list (serve, which lists every job) as
// BigQuery pages and filters it (above).
func (j *jobRecords) serveJobList(w http.ResponseWriter, r *http.Request, project string, serve func(http.ResponseWriter)) {
	q := r.URL.Query()
	maxResults := -1
	if s := q.Get("maxResults"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid value for maxResults: "+s)
			return
		}
		if n > 0 {
			maxResults = n
		}
	}
	bound := func(name string) (int64, bool, bool) {
		s := q.Get(name)
		if s == "" {
			return 0, false, true
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid value for "+name+": "+s+" (milliseconds since the epoch)")
			return 0, false, false
		}
		return n, true, true
	}
	minTime, hasMin, ok := bound("minCreationTime")
	if !ok {
		return
	}
	maxTime, hasMax, ok := bound("maxCreationTime")
	if !ok {
		return
	}
	states := map[string]bool{}
	for _, s := range q["stateFilter"] {
		for _, one := range strings.Split(s, ",") {
			switch u := strings.ToUpper(strings.TrimSpace(one)); u {
			case "DONE", "PENDING", "RUNNING":
				states[u] = true
			case "":
			default:
				writeError(w, http.StatusBadRequest, "invalid", "Invalid value for stateFilter: "+one+" (DONE, PENDING or RUNNING)")
				return
			}
		}
	}
	parent := q.Get("parentJobId")
	var after *pageKey
	if tok := q.Get("pageToken"); tok != "" {
		k, ok := decodePageToken(tok)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid page token: "+tok)
			return
		}
		after = &k
	}

	rec := newRecorder()
	serve(rec)
	var list map[string]any
	if rec.status != 0 && rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil {
		rec.copyTo(w)
		return
	}
	items, _ := list["jobs"].([]any)
	var jobs []listedJob
	for _, item := range items {
		job, ok := item.(map[string]any)
		if !ok {
			continue
		}
		p, id := jobRef(job)
		if p == "" {
			p = project
		}
		if j.isInternal(p, id) {
			continue
		}
		stats, _ := job["statistics"].(map[string]any)
		if parentOf, _ := stats["parentJobId"].(string); parentOf != parent {
			continue
		}
		if len(states) > 0 {
			state, _ := job["state"].(string)
			if st, ok := job["status"].(map[string]any); ok && state == "" {
				state, _ = st["state"].(string)
			}
			if !states[strings.ToUpper(state)] {
				continue
			}
		}
		j.patch(job, p)
		stats, _ = job["statistics"].(map[string]any)
		created, timed := msValue(stats["creationTime"])
		if (hasMin || hasMax) && !timed {
			continue
		}
		if hasMin && created < minTime || hasMax && created > maxTime {
			continue
		}
		jobs = append(jobs, listedJob{job: job, id: id, created: created})
	}
	// Newest first; the emulator lists in the order it made them, so a
	// later one of the same millisecond comes first too.
	for a, b := 0, len(jobs)-1; a < b; a, b = a+1, b-1 {
		jobs[a], jobs[b] = jobs[b], jobs[a]
	}
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].created > jobs[b].created })
	start := 0
	if after != nil {
		// After the page's last job; when it is no longer listed, after
		// the jobs made before it.
		start = -1
		for i, lj := range jobs {
			if lj.id == after.id && lj.created == after.created {
				start = i + 1
				break
			}
		}
		if start < 0 {
			start = len(jobs)
			for i, lj := range jobs {
				if lj.created < after.created {
					start = i
					break
				}
			}
		}
	}
	page := jobs[start:]
	delete(list, "nextPageToken")
	if maxResults >= 0 && len(page) > maxResults {
		page = page[:maxResults]
		last := page[len(page)-1]
		list["nextPageToken"] = encodePageToken(pageKey{created: last.created, id: last.id})
	}
	out := make([]any, len(page))
	for i, lj := range page {
		out[i] = lj.job
	}
	list["jobs"] = out
	writeJSON(w, http.StatusOK, list)
}

// pageKey is the last job of a page: the next page starts after it.
type pageKey struct {
	created int64
	id      string
}

func encodePageToken(k pageKey) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(k.created, 10) + "/" + url.PathEscape(k.id)))
}

func decodePageToken(tok string) (pageKey, bool) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return pageKey{}, false
	}
	created, rawID, ok := strings.Cut(string(b), "/")
	if !ok {
		return pageKey{}, false
	}
	n, err := strconv.ParseInt(created, 10, 64)
	id, err2 := url.PathUnescape(rawID)
	if err != nil || err2 != nil {
		return pageKey{}, false
	}
	return pageKey{created: n, id: id}, true
}
