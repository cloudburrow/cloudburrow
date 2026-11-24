package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOptionalJobCreation (#2094): a jobs.query with jobCreationMode
// JOB_CREATION_OPTIONAL whose rows all came back answers a queryId and no
// jobReference, and the engine's job is deleted; one with a pageToken, one
// not complete and a DML statement keep their job with its
// jobCreationReason; without the mode, and in a dry run, nothing changes.
func TestOptionalJobCreation(t *testing.T) {
	var deleted []string
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/delete") {
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})
	f := front{next: engine, base: base}
	run := func(req, resp string) map[string]any {
		t.Helper()
		answer := newRecorder()
		answer.status = http.StatusOK
		answer.body.WriteString(resp)
		f.optionalJobCreation(httptest.NewRequest(http.MethodPost, base+"/queries", nil), []byte(req), answer)
		var got map[string]any
		if err := json.Unmarshal(answer.body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	const opt = `{"query":"SELECT 1","jobCreationMode":"JOB_CREATION_OPTIONAL"}`
	const done = `{"jobComplete":true,"jobReference":{"projectId":"p","jobId":"j1","location":"US"},"schema":{"fields":[{"name":"f0_","type":"INTEGER"}]},"rows":[{"f":[{"v":"1"}]}],"totalRows":"1"}`

	got := run(opt, done)
	if _, ok := got["jobReference"]; ok || got["queryId"] == "" || got["queryId"] == nil {
		t.Fatalf("optional, complete: %v, want a queryId and no jobReference", got)
	}
	if len(deleted) != 1 || deleted[0] != base+"/jobs/j1/delete" {
		t.Fatalf("deleted %v, want the engine's job j1", deleted)
	}
	if rows, _ := got["rows"].([]any); len(rows) != 1 {
		t.Fatalf("rows %v", got["rows"])
	}

	for _, c := range []struct{ name, resp, reason string }{
		{"pageToken", strings.Replace(done, `"totalRows"`, `"pageToken":"x","totalRows"`, 1), "LARGE_RESULTS"},
		{"running", strings.Replace(done, `"jobComplete":true`, `"jobComplete":false`, 1), "LONG_RUNNING"},
		{"dml", `{"jobComplete":true,"jobReference":{"projectId":"p","jobId":"j2"},"numDmlAffectedRows":"3"}`, "OTHER"},
	} {
		got := run(opt, c.resp)
		ref, _ := got["jobReference"].(map[string]any)
		reason, _ := got["jobCreationReason"].(map[string]any)
		if ref == nil || reason["code"] != c.reason || got["queryId"] != nil {
			t.Errorf("%s: %v, want the job and jobCreationReason %s", c.name, got, c.reason)
		}
	}
	for _, req := range []string{`{"query":"SELECT 1"}`, `{"query":"SELECT 1","jobCreationMode":"JOB_CREATION_REQUIRED"}`,
		`{"query":"SELECT 1","jobCreationMode":"JOB_CREATION_OPTIONAL","dryRun":true}`} {
		if got := run(req, done); got["jobReference"] == nil || got["jobCreationReason"] != nil {
			t.Errorf("%s: %v, want the answer as it was", req, got)
		}
	}
	if len(deleted) != 1 {
		t.Errorf("deleted %v, want only j1", deleted)
	}
}
