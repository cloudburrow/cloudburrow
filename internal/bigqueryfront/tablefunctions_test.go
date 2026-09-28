package bigqueryfront

import (
	"strings"
	"testing"
)

// TestTableFunctionsAreNotMade (#1043): every statement that makes a table
// function, alone or in a script, through jobs.query and a query job, and
// routines.insert of a TABLE_VALUED_FUNCTION routine, is 501 and nothing
// is run; a scalar function is sent on.
func TestTableFunctionsAreNotMade(t *testing.T) {
	for _, path := range []string{"/queries", "/jobs"} {
		for _, sql := range []string{
			"CREATE TABLE FUNCTION ds.tf(x INT64) AS (SELECT x AS y)",
			"CREATE OR REPLACE TABLE FUNCTION ds.tf(x INT64) AS (SELECT x AS y)",
			"CREATE TABLE FUNCTION IF NOT EXISTS ds.tf(x INT64) AS (SELECT x AS y)",
			"CREATE TEMP TABLE FUNCTION tf(x INT64) AS (SELECT x AS y); SELECT * FROM tf(1)",
			"SELECT 1; CREATE TABLE FUNCTION ds.tf(x INT64) AS (SELECT x AS y)",
		} {
			e := newStateEmulator()
			e.datasets["ds"] = true
			code, got := do(t, Wrap(e), "POST", base+path, queryBody(path, sql))
			reason := errorReason(got)
			if path == "/jobs" && code == 200 {
				reason, _ = jobState(got)
			}
			if (code != 501 && code != 200) || reason != "notImplemented" || !strings.Contains(errMsg(got)+jobMessage(got), "#1043") ||
				e.sent(`FUNCTION|SELECT 1`) {
				t.Errorf("%s %q: %d %v, sent %v", path, sql, code, got, e.log)
			}
		}
		for _, sql := range []string{"CREATE FUNCTION ds.f(x INT64) AS (x + 1)"} {
			e := newStateEmulator()
			e.datasets["ds"] = true
			if code, got := do(t, Wrap(e), "POST", base+path, queryBody(path, sql)); code != 200 || errorReason(got) != "" || !e.sent(`FUNCTION`) {
				t.Errorf("%s %q: %d %v, sent %v", path, sql, code, got, e.log)
			}
		}
	}

	e := newStateEmulator()
	e.datasets["ds"] = true
	code, got := do(t, Wrap(e), "POST", base+"/datasets/ds/routines",
		`{"routineReference":{"projectId":"p","datasetId":"ds","routineId":"tf"},"routineType":"TABLE_VALUED_FUNCTION",`+
			`"language":"SQL","definitionBody":"SELECT 1 AS y"}`)
	if code != 501 || errorReason(got) != "notImplemented" || len(e.log) != 0 {
		t.Errorf("routines.insert of a table function: %d %v, sent %v", code, got, e.log)
	}
	e = newStateEmulator()
	body := `{"routineReference":{"projectId":"p","datasetId":"ds","routineId":"f"},"routineType":"SCALAR_FUNCTION",` +
		`"language":"SQL","definitionBody":"1"}`
	if _, _ = do(t, Wrap(e), "POST", base+"/datasets/ds/routines", body); len(e.log) != 1 || !strings.HasSuffix(e.log[0], body) {
		t.Errorf("routines.insert of a scalar function: sent %v", e.log)
	}
}

// jobMessage is a failed job's error message, or "".
func jobMessage(got map[string]any) string {
	st, _ := got["status"].(map[string]any)
	er, _ := st["errorResult"].(map[string]any)
	m, _ := er["message"].(string)
	return m
}
