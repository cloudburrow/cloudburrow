package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// loadEmulator stands in for the emulator's autodetect: a load job makes
// its table, if it is new, with the detected schema it is given, and
// reports the job done; jobs.get reports every job done and succeeded.
type loadEmulator struct {
	detected string
	tables   map[string]string
	loads    int
	deleted  []string
}

func (e *loadEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.Contains(path, "/tables/"):
		schema, ok := e.tables[path]
		if !ok {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"schema":`+schema+`}`)
	case r.Method == http.MethodDelete:
		e.deleted = append(e.deleted, path)
		delete(e.tables, path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.Contains(path, "/jobs/"):
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"`+path[strings.LastIndex(path, "/")+1:]+`"},"status":{"state":"DONE"}}`)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/jobs"):
		e.loads++
		var job jobBody
		_ = json.NewDecoder(r.Body).Decode(&job)
		d := job.Configuration.Load.DestinationTable
		table := base + "/datasets/" + d.DatasetID + "/tables/" + d.TableID
		if _, ok := e.tables[table]; !ok {
			e.tables[table] = e.detected
		}
		_, _ = io.WriteString(w, `{"kind":"bigquery#job","jobReference":{"projectId":"p","jobId":"`+job.JobReference.JobID+`"}}`)
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, http.StatusTeapot)
	}
}

// TestAutodetectedNamesAreHeldToTheCharacterMap (#901): a load with
// autodetect and no schema into a new table is checked after the emulator
// ran it, against the table it made. A name the load's
// columnNameCharacterMap refuses fails the job, as BigQuery fails it, and
// the table is deleted; jobs.get reports the failure too. V1 and V2 would
// rename the column, which the emulator does not do, so they are 501. A
// NEWLINE_DELIMITED_JSON autodetect into a new table, which the emulator
// fails with a 500, is 501 before it is sent. A load into an existing
// table keeps that table's schema and is not checked.
func TestAutodetectedNamesAreHeldToTheCharacterMap(t *testing.T) {
	flexibleOnly := `{"fields":[{"name":"first name","type":"STRING"},{"name":"n","type":"INTEGER"}]}`
	invalid := `{"fields":[{"name":"a b!","type":"INTEGER"},{"name":"c","type":"STRING"}]}`
	for _, c := range []struct {
		name, detected, extra string
		existing              bool
		code                  int
		failed                string // the job's errorResult reason, "" for none
	}{
		{"valid", flexibleOnly, "", false, 200, ""},
		{"strict", invalid, "", false, 200, "invalid"},
		{"explicit strict", invalid, `,"columnNameCharacterMap":"STRICT"`, false, 200, "invalid"},
		{"V2, invalid", invalid, `,"columnNameCharacterMap":"V2"`, false, 501, "notImplemented"},
		{"V2, flexible", flexibleOnly, `,"columnNameCharacterMap":"V2"`, false, 200, ""},
		{"V1, flexible but not classic", flexibleOnly, `,"columnNameCharacterMap":"V1"`, false, 501, "notImplemented"},
		// Into an existing table, BigQuery decides from the data whether
		// the first row is a header unless skipLeadingRows says (#945).
		{"existing table", invalid, "", true, 501, ""},
		{"existing table, skipLeadingRows 1", invalid, `,"skipLeadingRows":"1"`, true, 200, ""},
		{"JSON", invalid, `,"sourceFormat":"NEWLINE_DELIMITED_JSON"`, false, 501, ""},
		{"JSON into an existing table", invalid, `,"sourceFormat":"NEWLINE_DELIMITED_JSON"`, true, 200, ""},
	} {
		table := base + "/datasets/ds/tables/t"
		emu := &loadEmulator{detected: c.detected, tables: map[string]string{}}
		if c.existing {
			emu.tables[table] = `{"fields":[{"name":"a","type":"STRING"}]}`
		}
		h := Wrap(emu)
		body := `{"jobReference":{"projectId":"p","jobId":"job1"},"configuration":{"load":{"destinationTable":` +
			`{"projectId":"p","datasetId":"ds","tableId":"t"},"autodetect":true` + c.extra + `}}}`
		code, got := do(t, h, "POST", base+"/jobs", body)
		if code != c.code {
			t.Errorf("%s: %d %v, want %d", c.name, code, got, c.code)
			continue
		}
		reason := func(job map[string]any) string {
			st, _ := job["status"].(map[string]any)
			er, _ := st["errorResult"].(map[string]any)
			r, _ := er["reason"].(string)
			return r
		}
		if code == 200 && reason(got) != c.failed {
			t.Errorf("%s: the job's errorResult is %q, want %q: %v", c.name, reason(got), c.failed, got)
		}
		_, job := do(t, h, "GET", base+"/jobs/job1?location=US", "")
		if reason(job) != c.failed {
			t.Errorf("%s: jobs.get reports %q, want %q: %v", c.name, reason(job), c.failed, job)
		}
		_, left := emu.tables[table]
		if c.failed != "" && (left || len(emu.deleted) != 1) {
			t.Errorf("%s: the table the failed load made is still there (deleted %v)", c.name, emu.deleted)
		}
		if c.failed == "" && c.code == 200 && !left {
			t.Errorf("%s: the table is gone", c.name)
		}
		if c.name == "JSON" && emu.loads != 0 {
			t.Errorf("%s: the load reached the emulator", c.name)
		}
	}
	// Another job is not touched by the failure of one.
	emu := &loadEmulator{detected: invalid, tables: map[string]string{}}
	h := Wrap(emu)
	do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"bad"},"configuration":{"load":{"destinationTable":{"datasetId":"ds","tableId":"t"},"autodetect":true}}}`)
	if _, job := do(t, h, "GET", base+"/jobs/other", ""); job["status"].(map[string]any)["errorResult"] != nil {
		t.Errorf("another job was reported failed: %v", job)
	}
}

func TestJobFailuresAreBounded(t *testing.T) {
	var j jobFailures
	for i := 0; i < maxJobFailures+10; i++ {
		j.add("p", strings.Repeat("x", i+1), rowError{Reason: "invalid"})
	}
	if _, ok := j.get("p", "x"); ok {
		t.Error("the oldest failure was kept past the bound")
	}
	if _, ok := j.get("p", strings.Repeat("x", maxJobFailures+10)); !ok || len(j.errs) != maxJobFailures {
		t.Errorf("kept %d, the newest present = %v", len(j.errs), ok)
	}
}
