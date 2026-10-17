package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestStatementTypeOf (#1459): a child job's statementType, from its text
// (JobStatistics2.statementType).
func TestStatementTypeOf(t *testing.T) {
	for sql, want := range map[string]string{
		"SELECT 1":                             "SELECT",
		"WITH a AS (SELECT 1) SELECT * FROM a": "SELECT",
		"INSERT INTO ds.t VALUES (1)":          "INSERT",

		"merge ds.t USING ds.s ON TRUE WHEN MATCHED THEN DELETE": "MERGE",

		"CREATE TEMP TABLE t (a INT64)":                   "CREATE_TABLE",
		"CREATE OR REPLACE TABLE ds.t AS SELECT 1 AS a":   "CREATE_TABLE_AS_SELECT",
		"CREATE TABLE ds.t (a INT64) AS SELECT 1":         "CREATE_TABLE_AS_SELECT",
		"CREATE TABLE ds.t LIKE ds.s":                     "CREATE_TABLE",
		"CREATE TEMP FUNCTION f(x INT64) AS (x)":          "CREATE_FUNCTION",
		"CREATE TABLE FUNCTION ds.f() AS SELECT 1 AS a":   "CREATE_TABLE_FUNCTION",
		"CREATE MATERIALIZED VIEW ds.mv AS SELECT 1 AS a": "CREATE_MATERIALIZED_VIEW",
		"CREATE SNAPSHOT TABLE ds.s CLONE ds.t":           "CREATE_SNAPSHOT_TABLE",
		"DROP TABLE t":                                    "DROP_TABLE",
		"DROP SCHEMA ds CASCADE":                          "DROP_SCHEMA",
		"ALTER TABLE ds.t ADD COLUMN b INT64":             "ALTER_TABLE",
		"TRUNCATE TABLE ds.t":                             "TRUNCATE_TABLE",
		"ASSERT TRUE":                                     "ASSERT",
		"DECLARE x INT64":                                 "",
	} {
		if got := statementTypeOf(sql); got != want {
			t.Errorf("%s: %q, want %q", sql, got, want)
		}
	}
}

// TestTempScriptChildren (#1459): a script with a TEMP table is carried
// out a statement at a time: each statement a child job with its own
// statementType, the last DML statement too, run once, its counts the
// parent's; a SELECT child a query job, which names its destination
// table.
func TestTempScriptChildren(t *testing.T) {
	jobs := &jobsEmulator{}
	h := Wrap(jobs)
	code, got := do(t, h, "POST", base+"/jobs", queryBody("/jobs",
		"CREATE TEMP TABLE staged (v INT64); SELECT v FROM staged; INSERT INTO ds.t SELECT v FROM staged"))
	if code != 200 {
		t.Fatalf("%d %v", code, got)
	}
	stats, _ := got["statistics"].(map[string]any)
	if stats["numChildJobs"] != "3" {
		t.Errorf("the parent: %v, want 3 children", stats)
	}
	inserts, selectJob := 0, false
	for _, l := range jobs.log {
		if strings.Contains(l, "INSERT INTO ds.t") {
			inserts++
		}
		selectJob = selectJob || strings.HasPrefix(l, "jobs ") && strings.Contains(l, "SELECT v FROM")
	}
	if inserts != 1 || !selectJob {
		t.Errorf("sent %v: want the INSERT once, the SELECT as a query job", jobs.log)
	}
	ref := got["jobReference"].(map[string]any)
	_, list := do(t, h, "GET", base+"/jobs?parentJobId="+ref["jobId"].(string), "")
	var types []string
	for _, j := range list["jobs"].([]any) {
		qs, _ := j.(map[string]any)["statistics"].(map[string]any)["query"].(map[string]any)
		types = append(types, qs["statementType"].(string))
	}
	if s := strings.Join(types, ","); !strings.Contains(s, "CREATE_TABLE") || !strings.Contains(s, "SELECT") || !strings.Contains(s, "INSERT") {
		t.Errorf("the children's statementTypes: %s", s)
	}
}

// TestScriptDryRun (#1459): a dry run of a script the emulator cannot
// analyze is 400 invalidQuery; a valid one's job is of statementType
// SCRIPT.
func TestScriptDryRun(t *testing.T) {
	emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "nope") {
			_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"d"},"configuration":{"dryRun":true,"query":{"query":"x"}},"status":{"state":"DONE","errorResult":{"reason":"jobInternalError","message":"failed to analyze: Unrecognized name: nope [at 1:47]"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"d"},"configuration":{"dryRun":true,"query":{"query":"x"}},"statistics":{"query":{"statementType":"SELECT"}},"status":{"state":"DONE"}}`)
	})
	h := Wrap(emu)
	dry := func(sql string) (int, map[string]any) {
		q, _ := json.Marshal(sql)
		return do(t, h, "POST", base+"/jobs", `{"configuration":{"dryRun":true,"query":{"query":`+string(q)+`,"useLegacySql":false}}}`)
	}
	if code, got := dry("CREATE TEMP TABLE d AS SELECT 1 AS a; SELECT nope FROM d"); code != 400 || errorReason(got) != "invalidQuery" ||
		!strings.Contains(got["error"].(map[string]any)["message"].(string), "Unrecognized name: nope") {
		t.Errorf("an invalid script: %d %v", code, got)
	}
	code, got := dry("CREATE TEMP TABLE d AS SELECT 1 AS a; SELECT a FROM d")
	qs, _ := got["statistics"].(map[string]any)["query"].(map[string]any)
	if code != 200 || qs["statementType"] != "SCRIPT" {
		t.Errorf("a valid script: %d %v", code, got)
	}
}
