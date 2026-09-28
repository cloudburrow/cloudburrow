package bigqueryfront

import (
	"strings"
	"testing"
)

// TestDropSchemaFindsEveryFunction (#1001): DROP SCHEMA finds the
// functions routines.insert made through the front, and those the
// emulator's jobs from before the front started made, so RESTRICT counts
// them and CASCADE drops them; a job made after the front started is not
// read again.
func TestDropSchemaFindsEveryFunction(t *testing.T) {
	e := newStateEmulator()
	e.datasets["ds"], e.datasets["rt"] = true, true
	h := Wrap(e)
	if code, got := do(t, h, "POST", base+"/datasets/rt/routines", `{"routineReference":{"projectId":"p","datasetId":"rt","routineId":"viarest"},`+
		`"routineType":"SCALAR_FUNCTION","language":"SQL","definitionBody":"x * 10"}`); code != 200 {
		t.Fatalf("routines.insert: %d %v", code, got)
	}
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "DROP SCHEMA rt")); code != 400 || errorReason(got) != "resourceInUse" {
		t.Errorf("DROP SCHEMA of a dataset with a function routines.insert made: %d %v", code, got)
	}
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "DROP SCHEMA rt CASCADE")); code != 200 || e.datasets["rt"] {
		t.Errorf("DROP SCHEMA CASCADE: %d %v", code, got)
	}
	if _, ok := e.funcs["rt.viarest"]; ok {
		t.Errorf("the function routines.insert made outlived its dataset: %v", e.funcs)
	}

	// Functions made before the front started, one qualified and one by
	// the job's default dataset; a job after the start is not read.
	e = newStateEmulator()
	e.datasets["ds"] = true
	e.funcs["ds.early"], e.funcs["ds.dflt"] = "x", "x"
	e.jobs["j1"] = map[string]any{"configuration": map[string]any{"query": map[string]any{
		"query": "SELECT 1; CREATE FUNCTION ds.early(x INT64) AS (x)"}}}
	e.jobs["j2"] = map[string]any{"configuration": map[string]any{"query": map[string]any{
		"query": "CREATE FUNCTION dflt(x INT64) AS (x)", "defaultDataset": map[string]any{"projectId": "p", "datasetId": "ds"}}}}
	e.jobs["j3"] = map[string]any{"configuration": map[string]any{"query": map[string]any{"query": "SELECT 1"}}}
	e.jobs["late"] = map[string]any{"configuration": map[string]any{"query": map[string]any{"query": "SELECT 2"}}}
	e.created = map[string]string{"j1": "1", "j2": "2", "j3": "3", "late": "99999999999999"}
	h = Wrap(e)
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "DROP SCHEMA ds")); code != 400 || errorReason(got) != "resourceInUse" {
		t.Errorf("DROP SCHEMA of a dataset with functions from before the front started: %d %v", code, got)
	}
	if code, got := do(t, h, "POST", base+"/jobs", queryBody("/jobs", "DROP SCHEMA ds CASCADE")); code != 200 || e.datasets["ds"] {
		t.Errorf("DROP SCHEMA CASCADE: %d %v", code, got)
	}
	if len(e.funcs) != 0 {
		t.Errorf("functions outlived their dataset: %v", e.funcs)
	}
	if e.sent(`^GET \S+/jobs/late `) {
		t.Errorf("a job made after the front started was read: %v", e.log)
	}
	lists := 0
	for _, l := range e.log {
		if strings.HasPrefix(l, "GET "+base+"/jobs?") {
			lists++
		}
	}
	if lists != 1 {
		t.Errorf("the jobs were listed %d times, want once: %v", lists, e.log)
	}
}
