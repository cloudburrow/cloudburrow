package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// The jobs.list path decodes the engine's list once (#1639).
//
// Each layer of jobs.list (the failures the front gave, its own jobs, the
// times, paging and filters, the configurations, the client's texts:
// front.go) decoded the whole JSON list its inner layer wrote and wrote
// it again, and INFORMATION_SCHEMA.JOBS (listedJobs) decoded the top
// layer's once more. Measured natively at 10,000 jobs (arm64, 2026-10-18,
// #1632), one INFORMATION_SCHEMA.JOBS query allocated about 800 MB with
// a live heap of 57 MB, and in the pod the front's memory reached about
// 500 MiB under 20 JOBS queries, past its 256 MiB GOMEMLIMIT
// (components.FrontGoMemLimitMiB). So the engine's answer is decoded
// once, here, and each layer changes the decoded list in place and hands
// it up (jobListing); a client's jobs.list is written once, at the top,
// and INFORMATION_SCHEMA.JOBS reads the decoded jobs, never written.

// jobListing is a layer's answer to jobs.list: the JobList decoded once,
// or, when the engine (or a layer) answered with an error or with
// something not a JobList, that answer as it was.
type jobListing struct {
	// list is the decoded JobList; nil when raw is the answer.
	list map[string]any
	raw  *recorder
}

// jobs are the list's jobs, each a map[string]any as the JSON decodes.
func (l jobListing) jobs() []any {
	items, _ := l.list["jobs"].([]any)
	return items
}

// write answers w with the listing, the list encoded once.
func (l jobListing) write(w http.ResponseWriter) {
	if l.list == nil {
		l.raw.copyTo(w)
		return
	}
	writeJSON(w, http.StatusOK, l.list)
}

// errorListing is an error answer of jobs.list, as writeError writes it.
func errorListing(status int, reason, msg string) jobListing {
	rec := newRecorder()
	writeError(rec, status, reason, msg)
	return jobListing{raw: rec}
}

// decodeListing decodes rec, a jobs.list answer, once.
func decodeListing(rec *recorder) jobListing {
	if rec.status != 0 && rec.status != http.StatusOK {
		return jobListing{raw: rec}
	}
	var list map[string]any
	if json.Unmarshal(rec.body.Bytes(), &list) != nil || list == nil {
		return jobListing{raw: rec}
	}
	return jobListing{list: list}
}

// engineListing is the engine's (next's) answer to r, a jobs.list of
// project's jobs, decoded once. With an engineList in r's context
// (jobstable.go), it is sent with the parameters of bigquery-emulator
// patch 0064 when some is set, and the engine's last job is recorded in
// it. The newest job the engine listed is recorded in newest (below).
func engineListing(next http.Handler, r *http.Request, project string, newest *newestJobs) jobListing {
	l, _ := r.Context().Value(engineListKey{}).(*engineList)
	if l != nil && l.some {
		q := r.URL.Query()
		q.Set("cloudburrowAfterJobId", l.after)
		q.Set("cloudburrowJobIds", strings.Join(l.ids, ","))
		u := *r.URL
		u.RawQuery = q.Encode()
		r = r.Clone(r.Context())
		r.URL = &u
	}
	rec := newRecorder()
	next.ServeHTTP(rec, r)
	out := decodeListing(rec)
	items := out.jobs()
	last := ""
	if len(items) > 0 {
		job, _ := items[len(items)-1].(map[string]any)
		_, last = jobRef(job)
	}
	if l != nil {
		l.last = l.after
		if len(items) > 0 {
			l.last = last
		}
	}
	// The engine lists its jobs in the order it made them, and patch
	// 0064 the jobs named and made before l.after first: the last of a
	// list of every job is the newest, and so is the last of a list of
	// some when no job named it (one made after l.after, so after the
	// newest recorded before). A list cut by creation time (patch 0060)
	// may end before the newest.
	q := r.URL.Query()
	cut := q.Has("minCreationTime") || q.Has("maxCreationTime")
	if last != "" && !cut && (l == nil || !l.some || !slices.Contains(l.ids, last)) {
		newest.set(project, last)
	}
	return out
}

// newestJobs are the newest job the engine listed of each project, which
// a query of INFORMATION_SCHEMA.JOBS that names its jobs (job_id = or IN)
// lists them after (#1633): with bigquery-emulator patch 0064, the jobs it
// names and those the engine made since, not every job. A job of the ID
// no longer there (deleted, or the engine restarted) makes the engine
// list every job, which the filter then cuts as before.
type newestJobs struct {
	mu  sync.Mutex
	ids map[string]string
}

func (n *newestJobs) set(project, id string) {
	if n == nil || id == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ids == nil {
		n.ids = map[string]string{}
	}
	n.ids[project] = id
}

// get returns the project's newest job, or "" when none was listed.
func (n *newestJobs) get(project string) string {
	if n == nil {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ids[project]
}

// reset forgets them: the emulator restarted.
func (n *newestJobs) reset() {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.ids = nil
	n.mu.Unlock()
}

// jsonCopy is a deep copy of v as its JSON decodes, or v when it does not
// encode.
func jsonCopy(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

// jobHead is what the front reads of an answer that names a job (a Job, a
// QueryResponse, a GetQueryResultsResponse) to decide whether it changes
// it, without decoding the rest, such as the rows of a query of every
// job's INFORMATION_SCHEMA.JOBS row (#1639).
type jobHead struct {
	JobReference map[string]any `json:"jobReference"`
	// Configuration is the answer's configuration as it was written, or
	// empty when it has none: a Job's, which only a Job has.
	Configuration json.RawMessage `json:"configuration"`
}

// readJobHead reads body's jobHead, reporting whether body is a JSON
// object.
func readJobHead(body []byte) (jobHead, bool) {
	var h jobHead
	if json.Unmarshal(body, &h) != nil {
		return jobHead{}, false
	}
	return h, true
}

// ref is the job the answer names: its project and ID.
func (h jobHead) ref() (project, id string) {
	project, _ = h.JobReference["projectId"].(string)
	id, _ = h.JobReference["jobId"].(string)
	return project, id
}
