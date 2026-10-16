package bigqueryfront

import (
	"fmt"
	"strings"
	"testing"
)

// TestFailedQueriesAreJobs (#1397): a jobs.query that fails is answered
// with its error and recorded as a job, DONE with its errorResult, which
// jobs.list and jobs.get give with the client's text; so is a query job
// the emulator refused without recording it (a missing destination
// dataset). A dry run, and a jobs.insert answered 409, make no job.
func TestFailedQueriesAreJobs(t *testing.T) {
	emu := &jobsEmulator{tables: map[string]string{}}
	emu.run = func(q string) (int, string) {
		switch {
		case strings.Contains(q, "missing_table"):
			return 404, `{"error":{"code":404,"message":"Table not found: missing_table","errors":[{"reason":"notFound","message":"Table not found: missing_table"}]}}`
		case strings.Contains(q, "SELEC "):
			return 400, `{"error":{"code":400,"message":"Syntax error: Unexpected identifier","errors":[{"reason":"invalidQuery","message":"Syntax error: Unexpected identifier"}]}}`
		case strings.Contains(q, "into_missing_dataset"):
			return 404, `{"error":{"code":404,"message":"failed to find destination dataset: nope"}}`
		case strings.Contains(q, "duplicate"):
			return 409, `{"error":{"code":409,"message":"Already Exists: Job p:dup"}}`
		}
		return 200, `{"jobComplete":true}`
	}
	h := Wrap(emu)
	const n = 3
	for i := range n {
		if code, got := do(t, h, "POST", base+"/queries", fmt.Sprintf(`{"query":"SELECT %d"}`, i)); code != 200 {
			t.Fatalf("succeeding query: %d %v", code, got)
		}
		if code, got := do(t, h, "POST", base+"/queries", fmt.Sprintf(`{"query":"SELECT %d FROM ds.missing_table"}`, i)); code != 404 {
			t.Fatalf("failing query: %d %v, want its 404", code, got)
		}
		if code, got := do(t, h, "POST", base+"/queries", fmt.Sprintf(`{"query":"SELEC %d"}`, i)); code != 400 {
			t.Fatalf("syntax error: %d %v, want 400", code, got)
		}
	}
	if code, _ := do(t, h, "POST", base+"/queries", `{"query":"SELEC 9","dryRun":true}`); code != 400 {
		t.Fatalf("dry run: %d, want 400", code)
	}
	code, got := do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"ins"},"configuration":{"query":`+
		`{"query":"SELECT 1 AS into_missing_dataset","destinationTable":{"projectId":"p","datasetId":"nope","tableId":"t"}}}}`)
	if code != 200 || jobStateOf(got) != "DONE" || errorResultMessage(got) != "failed to find destination dataset: nope" {
		t.Fatalf("query job into a missing dataset: %d %v, want 200 and DONE with the error; sent %q", code, got, emu.log)
	}
	if code, _ := do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"dup"},"configuration":{"query":{"query":"SELECT 'duplicate'"}}}`); code != 409 {
		t.Fatalf("duplicate job: %d, want 409", code)
	}

	code, list := do(t, h, "GET", base+"/jobs?projection=full&allUsers=true", "")
	if code != 200 {
		t.Fatalf("jobs.list: %d %v", code, list)
	}
	jobs, _ := list["jobs"].([]any)
	var ok, failed, queries int
	for _, item := range jobs {
		job := item.(map[string]any)
		if jobStateOf(job) != "DONE" {
			t.Errorf("job %v not DONE", job)
		}
		conf, _ := job["configuration"].(map[string]any)
		qc, _ := conf["query"].(map[string]any)
		text, _ := qc["query"].(string)
		if strings.Contains(text, "DROP TABLE") {
			t.Errorf("job lists the front's statement %q, not the client's", text)
		}
		msg := errorResultMessage(job)
		switch {
		case strings.Contains(text, "missing_table"):
			queries++
			if msg != "Table not found: missing_table" {
				t.Errorf("job %q: errorResult %q", text, msg)
			}
		case strings.Contains(text, "SELEC "):
			queries++
			if !strings.HasPrefix(msg, "Syntax error") {
				t.Errorf("job %q: errorResult %q", text, msg)
			}
		}
		if msg != "" {
			failed++
		} else {
			ok++
		}
	}
	if ok != n || failed != 2*n+1 || queries != 2*n {
		t.Errorf("listed %d succeeded and %d failed jobs (%d failed jobs.query), want %d, %d and %d: %v", ok, failed, queries, n, 2*n+1, 2*n, jobs)
	}

	code, job := do(t, h, "GET", base+"/jobs/ins", "")
	if code != 200 || errorResultMessage(job) != "failed to find destination dataset: nope" {
		t.Fatalf("jobs.get of the refused query job: %d %v", code, job)
	}
	conf, _ := job["configuration"].(map[string]any)
	qc, _ := conf["query"].(map[string]any)
	if d, _ := qc["destinationTable"].(map[string]any); d["datasetId"] != "nope" || !strings.Contains(fmt.Sprint(qc["query"]), "into_missing_dataset") {
		t.Errorf("jobs.get of the refused query job: configuration %v, want the client's", qc)
	}
}

func errorResultMessage(job map[string]any) string {
	st, _ := job["status"].(map[string]any)
	e, _ := st["errorResult"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}
