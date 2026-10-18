package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEngine answers datasets.get of ds (and of other's ds2) and 404 for
// any other dataset.
func fakeEngine() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bigquery/v2/projects/p/datasets/ds", "/bigquery/v2/projects/other/datasets/ds2":
			writeJSON(w, http.StatusOK, map[string]any{"datasetReference": map[string]any{"datasetId": "ds"}})
		default:
			writeError(w, http.StatusNotFound, "notFound", "dataset is not found")
		}
	})
}

func TestEngineErrorMapping(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range []struct {
		msg, reason, want string
		defaultDataset    string
	}{
		{"failed to analyze: Table not found: ds.nope [at 1:15]", "notFound",
			"Not found: Table p:ds.nope was not found in location EU", ""},
		{"failed to analyze: Table not found: nods.nope [at 1:15]", "notFound",
			"Not found: Dataset p:nods was not found in location EU", ""},
		{"failed to analyze: Table not found: `other.ds2.t`; Did you mean other_ds2_u? [at 1:15]", "notFound",
			"Not found: Table other:ds2.t was not found in location EU", ""},
		{"failed to analyze: Table not found: `other`.ds2.t [at 1:15]", "notFound",
			"Not found: Table other:ds2.t was not found in location EU", ""},
		{"failed to analyze: Table not found: nope [at 1:15]", "notFound",
			"Not found: Table p:ds.nope was not found in location EU", "ds"},
		{"failed to analyze: Table not found: nope [at 1:15]", "invalidQuery",
			"Table not found: nope at [1:15]", ""},
		{"failed to analyze: Unrecognized name: nope [at 1:8]", "invalidQuery", "Unrecognized name: nope at [1:8]", ""},
		{"failed to analyze: INVALID_ARGUMENT: Could not cast literal \"x\" to type INT64 [at 1:8]", "invalidQuery",
			"Could not cast literal \"x\" to type INT64 at [1:8]", ""},
		{"failed to analyze: No matching signature for function LENGTH\n  Argument types: INT64 [at 1:8]", "invalidQuery",
			"No matching signature for function LENGTH\n  Argument types: INT64 at [1:8]", ""},
		{`failed to parse statements: failed to parse statement: Syntax error: Unexpected identifier "blah" [at 1:1]`,
			"invalidQuery", `Syntax error: Unexpected identifier "blah" at [1:1]`, ""},
		{`failed to parse statements: failed to parse statement: Syntax error: Unexpected end of statement`,
			"invalidQuery", `Syntax error: Unexpected end of statement`, ""},
		{"dataset nods is not found", "notFound", "Not found: Dataset p:nods was not found in location EU", ""},
	} {
		ec := &engineContext{r: r, engine: fakeEngine(), prefix: "/bigquery/v2", project: "p", location: "EU", defaultDataset: c.defaultDataset}
		got, ok := ec.mapError(rowError{Reason: "jobInternalError", Message: c.msg})
		if !ok || got.Reason != c.reason || got.Message != c.want {
			t.Errorf("%q: got %v %s %q, want %s %q", c.msg, ok, got.Reason, got.Message, c.reason, c.want)
		}
	}
	// The front's invalidQuery with the engine's text (a script's
	// statement) is mapped too.
	ec0 := &engineContext{r: r, engine: fakeEngine(), prefix: "/bigquery/v2", project: "p", location: "EU"}
	if got, _ := ec0.mapError(rowError{Reason: "invalidQuery", Message: "failed to analyze: Table not found: nods.t [at 1:15]"}); got.Reason != "notFound" ||
		got.Message != "Not found: Dataset p:nods was not found in location EU" {
		t.Errorf("invalidQuery with the engine's text: %v", got)
	}
	// The engine's errors of a query it ran, and every other reason, are
	// left as they are.
	ec := &engineContext{r: r, engine: fakeEngine(), project: "p"}
	for _, e := range []rowError{
		{Reason: "jobInternalError", Message: "zero divided error ( 1.000000 / 0 )"},
		{Reason: "invalidQuery", Message: "Unrecognized name: nope at [1:8]"},
		{Reason: "invalidQuery", Message: "dataset nods is not found"},
		{Reason: "notFound", Message: "Not found: Dataset p:nods"},
	} {
		if got, ok := ec.mapError(e); ok || got != e {
			t.Errorf("%v was mapped to %v", e, got)
		}
	}
}

// TestJobErrorsEngineReasons: the engine's errors are given BigQuery's
// reason, message and code through jobs.query, jobs.insert, jobs.get,
// jobs.list and jobs.getQueryResults.
func TestJobErrorsEngineReasons(t *testing.T) {
	const notFound = "failed to analyze: Table not found: ds.nope [at 1:15]"
	job := map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": "j1", "location": "asia-east1"},
		"configuration": map[string]any{"query": map[string]any{"query": "SELECT * FROM ds.nope"}},
		"status": map[string]any{"state": "DONE", "errorResult": rowError{Reason: "jobInternalError", Message: notFound},
			"errors": []rowError{{Reason: "jobInternalError", Message: notFound}}}}
	engine := fakeEngine()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/queries/j1"):
			writeError(w, http.StatusBadRequest, "jobInternalError", notFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs/j1"):
			writeJSON(w, http.StatusOK, job)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs"):
			writeJSON(w, http.StatusOK, map[string]any{"jobs": []any{job}})
		case r.URL.Path == "/bigquery/v2/projects/s/queries":
			writeJSON(w, http.StatusBadRequest, job) // a script's statement that failed
		case strings.HasSuffix(r.URL.Path, "/queries"):
			writeError(w, http.StatusBadRequest, "jobInternalError",
				`failed to parse statements: failed to parse statement: Syntax error: Unexpected identifier "blah" [at 1:1]`)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			writeJSON(w, http.StatusOK, job)
		default:
			engine.ServeHTTP(w, r)
		}
	})
	h := jobErrors(next, engine, nil, &jobFailures{})
	do := func(method, path, body string) (int, map[string]any) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w.Code, got
	}
	const want = "Not found: Table p:ds.nope was not found in location asia-east1"
	code, got := do(http.MethodPost, "/bigquery/v2/projects/p/queries", `{"query":"blah"}`)
	if e, _ := got["error"].(map[string]any); code != 400 || e["message"] != `Syntax error: Unexpected identifier "blah" at [1:1]` ||
		e["errors"].([]any)[0].(map[string]any)["reason"] != "invalidQuery" {
		t.Errorf("jobs.query: %d %v, want 400 invalidQuery", code, got)
	}
	code, got = do(http.MethodPost, "/bigquery/v2/projects/s/queries", `{"query":"SELECT 1; SELECT * FROM ds.nope"}`)
	if e, _ := got["error"].(map[string]any); code != 404 || e["message"] != "Not found: Dataset s:ds was not found in location asia-east1" ||
		e["status"] != "NOT_FOUND" {
		t.Errorf("jobs.query answered with a failed job: %d %v, want its errorResult as a 404 error", code, got)
	}
	code, got = do(http.MethodGet, "/bigquery/v2/projects/p/queries/j1", "")
	if e, _ := got["error"].(map[string]any); code != 404 || e["message"] != want || e["status"] != "NOT_FOUND" {
		t.Errorf("jobs.getQueryResults: %d %v, want 404 %q", code, got, want)
	}
	checkJob := func(name string, j any) {
		t.Helper()
		status, _ := j.(map[string]any)["status"].(map[string]any)
		er, _ := status["errorResult"].(map[string]any)
		errs, _ := status["errors"].([]any)
		if er["reason"] != "notFound" || er["message"] != want || len(errs) != 1 || errs[0].(map[string]any)["message"] != want {
			t.Errorf("%s: status %v, want notFound %q", name, status, want)
		}
	}
	code, got = do(http.MethodPost, "/bigquery/v2/projects/p/jobs", `{"configuration":{"query":{"query":"SELECT * FROM ds.nope"}}}`)
	if code != 200 {
		t.Errorf("jobs.insert: %d", code)
	}
	checkJob("jobs.insert", got)
	_, got = do(http.MethodGet, "/bigquery/v2/projects/p/jobs/j1", "")
	checkJob("jobs.get", got)
	_, got = do(http.MethodGet, "/bigquery/v2/projects/p/jobs", "")
	if jobs, _ := got["jobs"].([]any); len(jobs) == 1 {
		checkJob("jobs.list", jobs[0])
	} else {
		t.Errorf("jobs.list: %v", got)
	}
}

// TestEngineRuntimeErrors (#1585): the engine's errors of a query it ran
// get reason invalidQuery and BigQuery's message; a table's storage name
// is written as the client names it.
func TestEngineRuntimeErrors(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range []struct {
		msg, query string
		params     []string
		want       string
	}{
		{"division by zero: 1 / 0", "SELECT 1/0", nil, "division by zero: 1 / 0"},
		{"failed to exec query: division by zero: DIV(1, 0)", "SELECT DIV(1, 0)", nil, "division by zero: DIV(1, 0)"},
		{"failed to exec query: int64 overflow: ABS(-9223372036854775808)", "SELECT ABS(x)", nil, "int64 overflow: ABS(-9223372036854775808)"},
		{"ERROR function: boom", `SELECT ERROR("boom")`, nil, "boom"},
		{"not enough query arguments", "SELECT @p", nil, "Query parameter 'p' not found at [1:8]"},
		{"not enough query arguments", "SELECT @@error.message, @q, -- @x\n @Rr", []string{"Q"}, "Query parameter 'Rr' not found at [2:2]"},
		{"not enough query arguments", "SELECT ?", nil, "not enough query arguments"},
		{"failed to analyze: Column nosuch is not present in table p_ds_t [at 1:23]", "", nil,
			"Column nosuch is not present in table ds.t at [1:23]"},
		{"failed to analyze: Column nosuch is not present in table p_ds_my_t [at 1:23]", "", nil,
			"Column nosuch is not present in table ds.my_t at [1:23]"},
		{"failed to analyze: Column nosuch is not present in table p_nods_t [at 1:23]", "", nil,
			"Column nosuch is not present in table p_nods_t at [1:23]"},
		{"failed to analyze: Column nosuch is not present in table xp_ds_t [at 1:23]", "", nil,
			"Column nosuch is not present in table xp_ds_t at [1:23]"},
	} {
		ec := &engineContext{r: r, engine: fakeEngine(), prefix: "/bigquery/v2", project: "p", location: "EU"}
		params := []any{}
		for _, p := range c.params {
			params = append(params, map[string]any{"name": p})
		}
		ec.fromQueryConf(map[string]any{"query": c.query, "queryParameters": params})
		got, ok := ec.mapError(rowError{Reason: "jobInternalError", Message: c.msg})
		if !ok || got.Reason != "invalidQuery" || got.Message != c.want {
			t.Errorf("%q: got %v %s %q, want invalidQuery %q", c.msg, ok, got.Reason, got.Message, c.want)
		}
	}
	ec := &engineContext{r: r, engine: fakeEngine(), project: "p"}
	if got, ok := ec.mapError(rowError{Reason: "jobInternalError", Message: "boom"}); ok {
		t.Errorf("an error of the engine's own: mapped to %v", got)
	}
}
