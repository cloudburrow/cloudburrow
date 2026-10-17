package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReasonStatus(t *testing.T) {
	for _, c := range []struct {
		reason   string
		code     int
		want     int
		describe string
	}{
		{"notFound", 400, 404, "notFound documents 404"},
		{"duplicate", 400, 409, "duplicate documents 409"},
		{"invalidQuery", 404, 400, "invalidQuery documents 400"},
		{"notImplemented", 400, 501, "notImplemented documents 501"},
		{"rateLimitExceeded", 429, 429, "one of the codes it documents"},
		{"somethingElse", 418, 418, "a reason the table does not name"},
	} {
		if got := reasonStatus(c.reason, c.code); got != c.want {
			t.Errorf("%s: reasonStatus(%q, %d) = %d, want %d", c.describe, c.reason, c.code, got, c.want)
		}
	}
}

// TestJobErrors: an error answer of jobs.query, jobs.insert and
// jobs.getQueryResults is given the code its reason documents, and a dry
// run's job that failed is answered as its error and kept by no one.
func TestJobErrors(t *testing.T) {
	failed := &jobFailures{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/queries/j1"):
			writeError(w, http.StatusBadRequest, "notFound", "Not found: Dataset p:nods")
		case strings.HasSuffix(r.URL.Path, "/queries"):
			writeError(w, http.StatusBadRequest, "duplicate", "Already Exists: Dataset p:ds")
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			failed.add("p", "dry1", rowError{Reason: "notFound", Message: "Not found: Dataset p:nods"})
			writeJSON(w, http.StatusOK, map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": "dry1"},
				"status": map[string]any{"state": "DONE", "errorResult": rowError{Reason: "notFound", Message: "Not found: Dataset p:nods"}}})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
		}
	})
	h := jobErrors(next, next, nil, failed)
	do := func(method, path, body string) (int, map[string]any) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w.Code, got
	}
	code, got := do(http.MethodPost, "/bigquery/v2/projects/p/queries", `{"query":"CREATE SCHEMA ds"}`)
	if e, _ := got["error"].(map[string]any); code != 409 || e["code"] != float64(409) || e["status"] != "ALREADY_EXISTS" {
		t.Errorf("jobs.query: %d %v, want 409 ALREADY_EXISTS", code, got)
	}
	code, got = do(http.MethodGet, "/bigquery/v2/projects/p/queries/j1", "")
	if e, _ := got["error"].(map[string]any); code != 404 || e["code"] != float64(404) {
		t.Errorf("jobs.getQueryResults: %d %v, want 404", code, got)
	}
	code, got = do(http.MethodPost, "/bigquery/v2/projects/p/jobs",
		`{"jobReference":{"jobId":"dry1"},"configuration":{"dryRun":true,"query":{"query":"DROP SCHEMA nods"}}}`)
	if e, _ := got["error"].(map[string]any); code != 404 || e == nil || !strings.Contains(e["message"].(string), "nods") {
		t.Errorf("dry-run jobs.insert: %d %v, want the 404 error", code, got)
	}
	if _, ok := failed.get("p", "dry1"); ok {
		t.Error("the dry run's job is still kept as failed")
	}
	code, got = do(http.MethodGet, "/bigquery/v2/projects/p/queries/ok", "")
	if code != 200 || got["rows"] == nil {
		t.Errorf("a successful answer was changed: %d %v", code, got)
	}
}
