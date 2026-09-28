package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// jobConfigs are the configurations of the jobs the emulator ran, by
// project and job ID, which jobs.list with projection=full gives (#958).
//
// Measured against the pinned image over REST, the emulator's jobs.list
// lists each job with only jobReference, kind, statistics and status,
// whatever the projection (its jobsListHandler copies no other field of
// the job it keeps), where BigQuery's with projection=full "Includes all
// job data", and with minimal "Does not include the job configuration"
// (the enum's descriptions in the API's discovery document,
// https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/list): the
// Go client's Job.Config() of a listed job was nil. jobs.get answers each
// job with its configuration.
//
// So the front keeps the configuration of each job it sees: from the
// emulator's answer to jobs.insert, and, for a job it has not seen (a
// jobs.query's, which answers no configuration, or one made before the
// front started), from jobs.get, once: a job's configuration does not
// change. The most recent maxJobConfigs are kept; an older one is read
// again. The client's text of a job the front changed is then put in it
// (jobTexts).
type jobConfigs struct {
	mu    sync.Mutex
	confs map[string]json.RawMessage
	order []string
}

const maxJobConfigs = 5000

func (j *jobConfigs) add(project, id string, conf json.RawMessage) {
	if id == "" || len(conf) == 0 || string(conf) == "null" {
		return
	}
	key := project + "/" + id
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.confs == nil {
		j.confs = map[string]json.RawMessage{}
	}
	if _, ok := j.confs[key]; !ok {
		j.order = append(j.order, key)
	}
	j.confs[key] = conf
	for len(j.order) > maxJobConfigs {
		delete(j.confs, j.order[0])
		j.order = j.order[1:]
	}
}

func (j *jobConfigs) get(project, id string) (json.RawMessage, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	c, ok := j.confs[project+"/"+id]
	return c, ok
}

// note records the configuration in a Job resource the emulator answered.
func (j *jobConfigs) note(body []byte, project string) {
	var job struct {
		JobReference struct {
			ProjectID string `json:"projectId"`
			JobID     string `json:"jobId"`
		} `json:"jobReference"`
		Configuration json.RawMessage `json:"configuration"`
	}
	if json.Unmarshal(body, &job) != nil {
		return
	}
	if job.JobReference.ProjectID != "" {
		project = job.JobReference.ProjectID
	}
	j.add(project, job.JobReference.JobID, job.Configuration)
}

// recording returns next, noting the configuration of each job the
// emulator answers jobs.insert with.
func (j *jobConfigs) recording(next http.Handler, project string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := jobsRoute.FindStringSubmatch(r.URL.EscapedPath()); m == nil || m[3] != "jobs" || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusOK {
			j.note(rec.body.Bytes(), project)
		}
		rec.copyTo(w)
	})
}

// serveJobList answers jobs.list (serve) with each job's configuration
// when the request asks for projection=full and the emulator's answer
// gives none. A job whose configuration cannot be read is listed as the
// emulator listed it.
func (j *jobConfigs) serveJobList(w http.ResponseWriter, r *http.Request, next http.Handler, base string, serve func(http.ResponseWriter)) {
	if r.URL.Query().Get("projection") != "full" {
		serve(w)
		return
	}
	rec := newRecorder()
	serve(rec)
	var list map[string]any
	if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil {
		rec.copyTo(w)
		return
	}
	jobs, _ := list["jobs"].([]any)
	f := front{next: next, base: base}
	changed := false
	for _, item := range jobs {
		job, ok := item.(map[string]any)
		if !ok || job["configuration"] != nil {
			continue
		}
		ref, _ := job["jobReference"].(map[string]any)
		project, _ := ref["projectId"].(string)
		id, _ := ref["jobId"].(string)
		if project == "" {
			project = projectOf(base)
		}
		conf, ok := j.get(project, id)
		if !ok && id != "" {
			// base names the listed project: a job of another is read
			// under its own.
			jf := f
			jf.base = base[:strings.LastIndex(base, "/")+1] + url.PathEscape(project)
			if status, got := jf.get(r, "/jobs/"+url.PathEscape(id)); status == http.StatusOK {
				j.note(got, project)
				conf, ok = j.get(project, id)
			}
		}
		if !ok {
			continue
		}
		var c any
		if json.Unmarshal(conf, &c) == nil {
			job["configuration"] = c
			changed = true
		}
	}
	if !changed {
		rec.copyTo(w)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
