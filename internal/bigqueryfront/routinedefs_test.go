package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// TestRoutineFromDDL (#1177): a CREATE FUNCTION statement's text gives the
// routine's type, language, arguments, return type and body as written.
func TestRoutineFromDDL(t *testing.T) {
	for _, c := range []struct {
		text  string
		table bool
		want  string
	}{
		{"(x INT64) AS (x + 1)", false,
			`{"arguments":[{"argumentKind":"FIXED_TYPE","dataType":{"typeKind":"INT64"},"name":"x"}],"definitionBody":"x + 1","language":"SQL","routineType":"SCALAR_FUNCTION"}`},
		{"(a ARRAY<STRUCT<n INTEGER, s STRING(10)>>, b ANY TYPE) RETURNS FLOAT64 OPTIONS(description='d') AS ((SELECT 1.0))", false,
			`{"arguments":[{"argumentKind":"FIXED_TYPE","dataType":{"arrayElementType":{"structType":{"fields":[{"name":"n","type":{"typeKind":"INT64"}},{"name":"s","type":{"typeKind":"STRING"}}]},"typeKind":"STRUCT"},"typeKind":"ARRAY"},"name":"a"},{"argumentKind":"ANY_TYPE","name":"b"}],"definitionBody":"(SELECT 1.0)","description":"d","language":"SQL","returnType":{"typeKind":"FLOAT64"},"routineType":"SCALAR_FUNCTION"}`},
		{"(s STRING) RETURNS STRING LANGUAGE js AS r\"\"\"return s + '!';\"\"\"", false,
			`{"arguments":[{"argumentKind":"FIXED_TYPE","dataType":{"typeKind":"STRING"},"name":"s"}],"definitionBody":"return s + '!';","language":"JAVASCRIPT","returnType":{"typeKind":"STRING"},"routineType":"SCALAR_FUNCTION"}`},
		{"() RETURNS TABLE<y INT64> AS SELECT 1 AS y", true,
			`{"definitionBody":"SELECT 1 AS y","language":"SQL","returnTableType":{"columns":[{"name":"y","type":{"typeKind":"INT64"}}]},"routineType":"TABLE_VALUED_FUNCTION"}`},
	} {
		b, _ := json.Marshal(routineFromDDL(c.text, c.table))
		var got, want any
		_ = json.Unmarshal(b, &got)
		_ = json.Unmarshal([]byte(c.want), &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("routineFromDDL(%q) = %s\nwant %s", c.text, b, c.want)
		}
	}
}

// TestRoutinesListGetDelete (#1177): routines.list and routines.get give
// the functions CREATE FUNCTION and routines.insert made, routines.delete
// drops one in the engine, and a routine the front does not know, or that
// is gone, is 404.
func TestRoutinesListGetDelete(t *testing.T) {
	e := newStateEmulator()
	e.datasets["ds"] = true
	h := Wrap(e)
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "CREATE FUNCTION ds.f(x INT64) AS (x + 1)")); code != 200 {
		t.Fatalf("CREATE FUNCTION: %d %v", code, got)
	}
	if code, got := do(t, h, "POST", base+"/datasets/ds/routines", `{"routineReference":{"projectId":"p","datasetId":"ds","routineId":"g"},`+
		`"routineType":"SCALAR_FUNCTION","language":"SQL","arguments":[{"name":"x","dataType":{"typeKind":"INT64"}}],"definitionBody":"x * 10"}`); code != 200 {
		t.Fatalf("routines.insert: %d %v", code, got)
	}
	code, got := do(t, h, "GET", base+"/datasets/ds/routines", "")
	list, _ := got["routines"].([]any)
	if code != 200 || len(list) != 2 {
		t.Fatalf("routines.list: %d %v", code, got)
	}
	if r := list[0].(map[string]any); r["definitionBody"] != nil || r["routineType"] != "SCALAR_FUNCTION" ||
		r["routineReference"].(map[string]any)["routineId"] != "f" || r["creationTime"] == nil {
		t.Errorf("routines.list's first routine = %v", r)
	}
	code, got = do(t, h, "GET", base+"/datasets/ds/routines?maxResults=1", "")
	if list, _ := got["routines"].([]any); code != 200 || len(list) != 1 || got["nextPageToken"] != "1" {
		t.Errorf("routines.list, one a page: %d %v", code, got)
	}
	code, got = do(t, h, "GET", base+"/datasets/ds/routines/f", "")
	if code != 200 || got["definitionBody"] != "x + 1" || got["language"] != "SQL" {
		t.Errorf("routines.get of f: %d %v", code, got)
	}
	code, got = do(t, h, "GET", base+"/datasets/ds/routines/g", "")
	if code != 200 || got["definitionBody"] != "x * 10" {
		t.Errorf("routines.get of g: %d %v", code, got)
	}
	if code, got := do(t, h, "GET", base+"/datasets/ds/routines/nope", ""); code != 404 || errorReason(got) != "notFound" {
		t.Errorf("routines.get of one that does not exist: %d %v", code, got)
	}
	if code, got := do(t, h, "DELETE", base+"/datasets/ds/routines/f", ""); code != 204 {
		t.Errorf("routines.delete: %d %v", code, got)
	}
	if _, ok := e.funcs["ds.f"]; ok {
		t.Errorf("routines.delete left the function in the engine: %v", e.funcs)
	}
	if code, got := do(t, h, "GET", base+"/datasets/ds/routines/f", ""); code != 404 {
		t.Errorf("routines.get after routines.delete: %d %v", code, got)
	}
	if code, got := do(t, h, "DELETE", base+"/datasets/ds/routines/f", ""); code != 404 {
		t.Errorf("routines.delete of one that is gone: %d %v", code, got)
	}
	// DROP FUNCTION and routines.delete agree.
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "DROP FUNCTION ds.g")); code != 200 {
		t.Fatalf("DROP FUNCTION: %d %v", code, got)
	}
	code, got = do(t, h, "GET", base+"/datasets/ds/routines", "")
	if list, _ := got["routines"].([]any); code != 200 || len(list) != 0 {
		t.Errorf("routines.list after both are gone: %d %v", code, got)
	}
	if code, got := do(t, h, "GET", base+"/datasets/nope/routines", ""); code != 404 {
		t.Errorf("routines.list of a dataset that does not exist: %d %v", code, got)
	}
	if code, got := do(t, h, "PUT", base+"/datasets/ds/routines/g", "{}"); code != 404 {
		t.Errorf("routines.update of one that is gone: %d %v", code, got)
	}
}

// routines.update (#1238): a drop and an insert, keeping the creation time;
// a stale If-Match, a routineReference naming another routine and a PATCH
// are refused.
func TestRoutinesUpdate(t *testing.T) {
	e := newStateEmulator()
	e.datasets["ds"] = true
	h := Wrap(e)
	const def = `"routineType":"SCALAR_FUNCTION","language":"SQL","arguments":[{"name":"x","dataType":{"typeKind":"INT64"}}]`
	if code, got := do(t, h, "POST", base+"/datasets/ds/routines", `{"routineReference":{"projectId":"p","datasetId":"ds","routineId":"g"},`+
		def+`,"definitionBody":"x * 10"}`); code != 200 {
		t.Fatalf("routines.insert: %d %v", code, got)
	}
	_, before := do(t, h, "GET", base+"/datasets/ds/routines/g", "")
	code, got := do(t, h, "PUT", base+"/datasets/ds/routines/g", `{"routineReference":{"projectId":"p","datasetId":"ds","routineId":"g"},`+
		def+`,"definitionBody":"x * 20","description":"twenty"}`)
	if code != 200 || got["definitionBody"] != "x * 20" || got["creationTime"] != before["creationTime"] || got["etag"] == before["etag"] {
		t.Fatalf("routines.update: %d %v (before %v)", code, got, before)
	}
	if code, read := do(t, h, "GET", base+"/datasets/ds/routines/g", ""); code != 200 || read["definitionBody"] != "x * 20" || read["description"] != "twenty" {
		t.Errorf("routines.get after routines.update: %d %v", code, read)
	}
	if _, ok := e.funcs["ds.g"]; !ok {
		t.Errorf("routines.update left no function in the engine: %v", e.funcs)
	}
	// With no routineReference, the path names it.
	if code, got := do(t, h, "PUT", base+"/datasets/ds/routines/g", `{`+def+`,"definitionBody":"x * 30"}`); code != 200 || got["definitionBody"] != "x * 30" {
		t.Errorf("routines.update with no routineReference: %d %v", code, got)
	}
	if code, got := do(t, h, "PUT", base+"/datasets/ds/routines/g", `{"routineReference":{"routineId":"other"},`+def+`,"definitionBody":"x"}`); code != 400 {
		t.Errorf("routines.update naming another routine: %d %v", code, got)
	}
	req := httptest.NewRequest("PUT", base+"/datasets/ds/routines/g", strings.NewReader(`{`+def+`,"definitionBody":"x"}`))
	req.Header.Set("If-Match", `"stale"`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("routines.update with a stale If-Match: %d %s", rec.Code, rec.Body)
	}
	if code, got := do(t, h, "PUT", base+"/datasets/ds/routines/g", `{"definitionBody":"x"}`); code != 400 {
		t.Errorf("routines.update with only a body: %d %v", code, got)
	}
	if code, got := do(t, h, "PATCH", base+"/datasets/ds/routines/g", "{}"); code != 501 {
		t.Errorf("a PATCH of a routine: %d %v", code, got)
	}
	if code, got := do(t, h, "PUT", base+"/datasets/ds/routines/nope", "{}"); code != 404 {
		t.Errorf("routines.update of one that does not exist: %d %v", code, got)
	}
}
