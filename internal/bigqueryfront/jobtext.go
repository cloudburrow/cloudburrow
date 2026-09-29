package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
)

// jobText is what the client sent in a job the front changed before the
// emulator ran it (#939): the emulator records the job it was sent, so
// jobs.insert's answer, jobs.get and jobs.list would show the front's
// text, not the client's. BigQuery shows the client's.
//
// The front changes a query's text when it carries out a lone CREATE OR
// REPLACE (replace), makes a CREATE ... IF NOT EXISTS of an existing table
// a no-op (skipIfExists) or renames a script's variables
// (renameVariables); an extract's destination URIs when it names the
// file of a wildcard URI (extractJob); and a load's writeDisposition
// WRITE_TRUNCATE_DATA (truncateData).
type jobText struct {
	// query is the client's query text, or "".
	query string
	// uris are the client's extract destinationUris, or nil.
	uris []string
	// names are the variable names renameVariables gave, each mapped to
	// the client's; an error the emulator wrote names them.
	names map[string]string
	// dml is what a DML statement did, which the emulator does not report
	// (#1008, dml.go), or nil.
	dml *dmlCounts
	// writeDisposition is the client's load writeDisposition, when the
	// front sent another (#1067, writedisposition.go), or "".
	writeDisposition string
	// params and paramMode are the client's queryParameters and
	// parameterMode, when paramsSet: the front sent others (#1078,
	// bytesparams.go).
	params    json.RawMessage
	paramMode string
	paramsSet bool
	// dest and queryWrite are a query job's destinationTable and
	// writeDisposition, when the front sent others (#1080,
	// querywrite.go): dest is nil otherwise.
	dest       *tableRef
	queryWrite string
	// load is the client's configuration.load of a load from Cloud
	// Storage, which the front sends the emulator as an upload without
	// its sourceUris (#944, #998, loadconfig.go), or nil.
	load json.RawMessage
}

func (t jobText) empty() bool {
	return t.query == "" && t.uris == nil && len(t.names) == 0 && t.dml == nil && t.writeDisposition == "" &&
		!t.paramsSet && t.dest == nil && t.load == nil
}

// merge adds what o changed to t: o's query text, parameters and
// destination, when o has them.
func (t *jobText) merge(o jobText) {
	if o.query != "" {
		t.query = o.query
	}
	if o.paramsSet {
		t.params, t.paramMode, t.paramsSet = o.params, o.paramMode, true
	}
	if o.dest != nil {
		t.dest, t.queryWrite = o.dest, o.queryWrite
	}
}

// patch puts the client's text back in a Job resource.
func (t jobText) patch(job map[string]any) {
	conf, _ := job["configuration"].(map[string]any)
	if q, ok := conf["query"].(map[string]any); ok {
		if t.query != "" {
			q["query"] = t.query
		}
		if t.paramsSet {
			var params any
			if len(t.params) > 0 && json.Unmarshal(t.params, &params) == nil {
				q["queryParameters"] = params
			} else {
				delete(q, "queryParameters")
			}
			if t.paramMode != "" {
				q["parameterMode"] = t.paramMode
			} else {
				delete(q, "parameterMode")
			}
		}
		if t.dest != nil {
			q["destinationTable"] = map[string]any{"projectId": t.dest.ProjectID, "datasetId": t.dest.DatasetID,
				"tableId": t.dest.TableID}
			if t.queryWrite != "" {
				q["writeDisposition"] = t.queryWrite
			} else {
				delete(q, "writeDisposition")
			}
		}
	}
	if e, ok := conf["extract"].(map[string]any); ok && t.uris != nil {
		uris := make([]any, len(t.uris))
		for i, u := range t.uris {
			uris[i] = u
		}
		e["destinationUris"] = uris
	}
	if l, ok := conf["load"].(map[string]any); ok && t.writeDisposition != "" {
		l["writeDisposition"] = t.writeDisposition
	}
	if _, ok := conf["load"]; ok && t.load != nil {
		var l any
		if json.Unmarshal(t.load, &l) == nil {
			conf["load"] = l
			if _, set := conf["jobType"]; !set {
				conf["jobType"] = "LOAD"
			}
		}
	}
	if t.dml != nil {
		t.dml.patch(job)
	}
}

// jobTexts are the jobs whose text the front changed, by project and job
// ID. The most recent maxJobTexts are kept.
type jobTexts struct {
	mu    sync.Mutex
	texts map[string]jobText
	order []string
	// path is the file they are kept in (keep, jobtextstate.go), or "".
	path string
	logf func(string, ...any)
}

const maxJobTexts = 1000

func (j *jobTexts) add(project, id string, t jobText) {
	if id == "" || t.empty() {
		return
	}
	key := project + "/" + id
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.texts == nil {
		j.texts = map[string]jobText{}
	}
	if _, ok := j.texts[key]; !ok {
		j.order = append(j.order, key)
	}
	j.texts[key] = t
	for len(j.order) > maxJobTexts {
		delete(j.texts, j.order[0])
		j.order = j.order[1:]
	}
	j.saveLocked()
}

func (j *jobTexts) get(project, id string) (jobText, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	t, ok := j.texts[project+"/"+id]
	return t, ok
}

func (j *jobTexts) none() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.texts) == 0
}

// forward sends r, whose job the front changed from what the client sent
// (t), to the emulator, and answers w with the emulator's answer, the job
// in it showing t: jobs.insert's Job, or the job jobs.query names, which
// jobs.get and jobs.list then show so too (serveJob, serveJobList). With
// no change, r is sent as it is.
func (f front) forward(w http.ResponseWriter, r *http.Request, t jobText) {
	if t.empty() || f.texts == nil {
		f.next.ServeHTTP(w, r)
		return
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	f.answer(w, rec, t)
}

// answer answers w with rec, the emulator's answer to a job the front
// changed (t), as forward does.
func (f front) answer(w http.ResponseWriter, rec *recorder, t jobText) {
	if t.empty() || f.texts == nil {
		rec.copyTo(w)
		return
	}
	body := rec.body.Bytes()
	var resp map[string]any
	if json.Unmarshal(body, &resp) == nil {
		if ref, ok := resp["jobReference"].(map[string]any); ok {
			project, _ := ref["projectId"].(string)
			id, _ := ref["jobId"].(string)
			if project == "" {
				project = projectOf(f.base)
			}
			f.texts.add(project, id, t)
		}
		if _, ok := resp["configuration"]; ok || t.dml != nil && rec.status == http.StatusOK {
			t.patch(resp)
			if b, err := json.Marshal(resp); err == nil {
				body = b
			}
		}
	}
	body = unname(body, t.names)
	rec.body.Reset()
	rec.body.Write(body)
	rec.copyTo(w)
}

// serveJob answers jobs.get or jobs.getQueryResults (serve, which writes
// to the writer it is given) with the client's text in the job, if the
// front changed it.
func (j *jobTexts) serveJob(w http.ResponseWriter, project, rawID string, serve func(http.ResponseWriter)) {
	id, err := url.PathUnescape(rawID)
	t, ok := j.get(project, id)
	if err != nil || !ok {
		serve(w)
		return
	}
	rec := newRecorder()
	serve(rec)
	body := rec.body.Bytes()
	var job map[string]any
	if rec.status == http.StatusOK && json.Unmarshal(body, &job) == nil {
		t.patch(job)
		if b, err := json.Marshal(job); err == nil {
			body = b
		}
	}
	body = unname(body, t.names)
	rec.body.Reset()
	rec.body.Write(body)
	rec.copyTo(w)
}

// serveJobList answers jobs.list (serve) with the client's text in each
// job whose text the front changed.
func (j *jobTexts) serveJobList(w http.ResponseWriter, serve func(http.ResponseWriter)) {
	if j.none() {
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
	changed := false
	for i, item := range jobs {
		job, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ref, _ := job["jobReference"].(map[string]any)
		project, _ := ref["projectId"].(string)
		id, _ := ref["jobId"].(string)
		t, ok := j.get(project, id)
		if !ok {
			continue
		}
		t.patch(job)
		if len(t.names) > 0 {
			if b, err := json.Marshal(job); err == nil {
				var back map[string]any
				if json.Unmarshal(unname(b, t.names), &back) == nil {
					jobs[i] = back
				}
			}
		}
		changed = true
	}
	if !changed {
		rec.copyTo(w)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
