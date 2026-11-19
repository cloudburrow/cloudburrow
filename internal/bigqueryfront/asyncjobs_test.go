package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowEngine answers jobs.insert and jobs.query once release is closed,
// or with an error once the request's context ends; jobs.get and
// getQueryResults name the job as it was answered.
type slowEngine struct {
	release chan struct{}
	mu      sync.Mutex
	done    map[string]bool
}

func (e *slowEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/jobs"):
		var job map[string]any
		_ = json.NewDecoder(r.Body).Decode(&job)
		ref := job["jobReference"].(map[string]any)
		select {
		case <-e.release:
		case <-r.Context().Done():
			writeError(w, http.StatusInternalServerError, "internalError", "context canceled")
			return
		}
		e.mu.Lock()
		e.done[ref["jobId"].(string)] = true
		e.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"jobReference": ref, "status": map[string]any{"state": "DONE"}})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/queries"):
		select {
		case <-e.release:
		case <-r.Context().Done():
			writeError(w, http.StatusInternalServerError, "internalError", "context canceled")
			return
		}
		e.mu.Lock()
		e.done["engine_job"] = true
		e.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"kind": "bigquery#queryResponse", "jobComplete": true,
			"jobReference": map[string]any{"projectId": "p", "jobId": "engine_job"}, "totalRows": "1"})
	case r.Method == http.MethodGet:
		id := p[strings.LastIndex(p, "/")+1:]
		e.mu.Lock()
		ok := e.done[id]
		e.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "notFound", "Not found: Job p:"+id)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": id},
			"status": map[string]any{"state": "DONE"}, "jobComplete": true})
	default:
		http.NotFound(w, r)
	}
}

func asyncDo(t *testing.T, h http.Handler, method, path, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	return got
}

func stateOf(job map[string]any) string {
	st, _ := job["status"].(map[string]any)
	s, _ := st["state"].(string)
	return s
}

func TestAsyncInsertRunsPastItsRequest(t *testing.T) {
	e := &slowEngine{release: make(chan struct{}), done: map[string]bool{}}
	a := &asyncJobs{insertWait: 20 * time.Millisecond}
	h := a.handler(e)
	job := asyncDo(t, h, http.MethodPost, "/bigquery/v2/projects/p/jobs",
		`{"jobReference":{"jobId":"slow"},"configuration":{"query":{"query":"SELECT 1"}}}`)
	if stateOf(job) != "RUNNING" {
		t.Fatalf("jobs.insert: %v, want RUNNING", job)
	}
	if conf := job["configuration"].(map[string]any); conf["jobType"] != "QUERY" {
		t.Errorf("jobType %v", conf["jobType"])
	}
	if got := asyncDo(t, h, http.MethodGet, "/bigquery/v2/projects/p/jobs/slow", ""); stateOf(got) != "RUNNING" {
		t.Fatalf("jobs.get: %v", got)
	}
	res := asyncDo(t, h, http.MethodGet, "/bigquery/v2/projects/p/queries/slow?timeoutMs=10", "")
	if res["jobComplete"] != false || res["kind"] != "bigquery#getQueryResultsResponse" {
		t.Fatalf("getQueryResults: %v", res)
	}
	l := a.listed(httptest.NewRequest(http.MethodGet, "/", nil), "p", jobListing{list: map[string]any{}})
	if n := len(l.jobs()); n != 1 {
		t.Fatalf("listed %d jobs, want the running one", n)
	}
	close(e.release)
	res = asyncDo(t, h, http.MethodGet, "/bigquery/v2/projects/p/queries/slow?timeoutMs=5000", "")
	if res["jobComplete"] != true {
		t.Fatalf("getQueryResults after the end: %v", res)
	}
	if got := asyncDo(t, h, http.MethodGet, "/bigquery/v2/projects/p/jobs/slow", ""); stateOf(got) != "DONE" {
		t.Fatalf("jobs.get after the end: %v", got)
	}
}

func TestAsyncQueryAnswersJobCompleteFalse(t *testing.T) {
	e := &slowEngine{release: make(chan struct{}), done: map[string]bool{}}
	a := &asyncJobs{}
	h := a.handler(e)
	got := asyncDo(t, h, http.MethodPost, "/bigquery/v2/projects/p/queries", `{"query":"SELECT 1","timeoutMs":20}`)
	if got["jobComplete"] != false {
		t.Fatalf("jobs.query: %v", got)
	}
	ref := got["jobReference"].(map[string]any)
	id := ref["jobId"].(string)
	if id == "" || ref["location"] != "US" {
		t.Fatalf("jobReference %v", ref)
	}
	close(e.release)
	res := asyncDo(t, h, http.MethodGet, "/bigquery/v2/projects/p/queries/"+id+"?timeoutMs=5000", "")
	if res["jobComplete"] != true || res["jobReference"].(map[string]any)["jobId"] != id {
		t.Fatalf("getQueryResults: %v, want the job named %s", res, id)
	}
}

func TestAsyncCancelStopsTheJob(t *testing.T) {
	e := &slowEngine{release: make(chan struct{}), done: map[string]bool{}}
	stopped := make(chan rowError, 1)
	a := &asyncJobs{insertWait: 10 * time.Millisecond, stopped: func(_ *http.Request, _ *asyncJob, e rowError) { stopped <- e }}
	h := a.handler(e)
	asyncDo(t, h, http.MethodPost, "/bigquery/v2/projects/p/jobs",
		`{"jobReference":{"jobId":"c1"},"configuration":{"query":{"query":"SELECT 1"}}}`)
	got := asyncDo(t, h, http.MethodPost, "/bigquery/v2/projects/p/jobs/c1/cancel", "")
	if got["kind"] != "bigquery#jobCancelResponse" || got["job"] == nil {
		t.Fatalf("jobs.cancel: %v", got)
	}
	select {
	case e := <-stopped:
		if e.Reason != "stopped" {
			t.Fatalf("stopped with %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled job was never recorded stopped")
	}
}

func TestAsyncDuplicateRunningJob(t *testing.T) {
	e := &slowEngine{release: make(chan struct{}), done: map[string]bool{}}
	defer close(e.release)
	h := (&asyncJobs{insertWait: 10 * time.Millisecond}).handler(e)
	body := `{"jobReference":{"jobId":"d1"},"configuration":{"query":{"query":"SELECT 1"}}}`
	asyncDo(t, h, http.MethodPost, "/bigquery/v2/projects/p/jobs", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/bigquery/v2/projects/p/jobs", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second jobs.insert: %d %s", rec.Code, rec.Body.String())
	}
}
