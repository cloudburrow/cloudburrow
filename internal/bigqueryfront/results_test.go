package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// resultsEmulator answers datasets.insert, datasets.list and jobs.insert
// as the pinned emulator does, and logs what it was sent.
type resultsEmulator struct {
	datasets map[string]bool
	log      []string
}

func (e *resultsEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	e.log = append(e.log, r.Method+" "+r.URL.Path+" "+string(b))
	if e.datasets == nil {
		e.datasets = map[string]bool{}
	}
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/datasets"):
		var ds struct {
			DatasetReference struct {
				DatasetID string `json:"datasetId"`
			} `json:"datasetReference"`
		}
		_ = json.Unmarshal(b, &ds)
		if e.datasets[ds.DatasetReference.DatasetID] {
			writeError(w, http.StatusConflict, "duplicate", "Already Exists")
			return
		}
		e.datasets[ds.DatasetReference.DatasetID] = true
		_, _ = w.Write(b)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/datasets"):
		var list []any
		for _, id := range []string{"a", resultsDataset, "b"} {
			if e.datasets[id] {
				list = append(list, map[string]any{"id": "p:" + id, "datasetReference": map[string]any{"projectId": "p", "datasetId": id}})
			}
		}
		writeJSON(w, 200, map[string]any{"kind": "bigquery#datasetList", "etag": "e", "datasets": list})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
		var job jobBody
		_ = json.Unmarshal(b, &job)
		if q := job.Configuration.Query; q != nil && q.DestinationTable != nil && !e.datasets[q.DestinationTable.DatasetID] {
			writeError(w, http.StatusBadRequest, "jobInternalError", "failed to find destination dataset: "+q.DestinationTable.DatasetID)
			return
		}
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// sentJobs are the jobs.insert bodies in the log.
func (e *resultsEmulator) sentJobs() []string {
	var out []string
	for _, l := range e.log {
		if strings.HasPrefix(l, "POST "+base+"/jobs ") {
			out = append(out, strings.TrimPrefix(l, "POST "+base+"/jobs "))
		}
	}
	return out
}

func (e *resultsEmulator) datasetInserts() int {
	n := 0
	for _, l := range e.log {
		if strings.HasPrefix(l, "POST "+base+"/datasets ") {
			n++
		}
	}
	return n
}

func queryJob(id, sql, extra string) string {
	b, _ := json.Marshal(sql)
	return `{"jobReference":{"projectId":"p","jobId":"` + id + `"},"configuration":{` + extra + `"query":{"query":` + string(b) + `,"useLegacySql":false}}}`
}

// TestQueryResultsInOneDataset (#1017): a query job that names no
// destination and whose query is one SELECT is sent to the emulator with a
// destination table named after the job in resultsDataset, which the
// front makes once, and again when it is gone; the job answered names it.
// Other jobs are sent as they were.
func TestQueryResultsInOneDataset(t *testing.T) {
	emu := &resultsEmulator{}
	q := Results(emu)
	for i, id := range []string{"j1", "j2"} {
		code, got := do(t, q, "POST", base+"/jobs", queryJob(id, "SELECT 1 AS x", ""))
		conf, _ := got["configuration"].(map[string]any)
		qc, _ := conf["query"].(map[string]any)
		dest, _ := qc["destinationTable"].(map[string]any)
		if code != 200 || dest["datasetId"] != resultsDataset || dest["tableId"] != id || dest["projectId"] != "p" {
			t.Fatalf("%s: %d %v", id, code, got)
		}
		if n := emu.datasetInserts(); n != 1 {
			t.Errorf("after %d jobs, the dataset was made %d times", i+1, n)
		}
	}
	for _, sql := range []string{"WITH a AS (SELECT 1) SELECT * FROM a;", "(SELECT 1) UNION ALL (SELECT 2)", " -- c\nselect 1"} {
		emu.log = nil
		do(t, q, "POST", base+"/jobs", queryJob("k", sql, ""))
		if sent := emu.sentJobs(); len(sent) != 1 || !strings.Contains(sent[0], resultsDataset) {
			t.Errorf("%q: sent %v", sql, sent)
		}
	}
	for _, c := range []struct{ name, body string }{
		{"DDL", queryJob("d1", "CREATE TABLE ds.t (a INT64)", "")},
		{"DML", queryJob("d2", "INSERT INTO ds.t VALUES (1)", "")},
		{"script", queryJob("d3", "SELECT 1; SELECT 2", "")},
		{"script with DECLARE", queryJob("d4", "DECLARE x INT64; SELECT x", "")},
		{"dry run", queryJob("d5", "SELECT 1", `"dryRun":true,`)},
		{"no job ID", `{"configuration":{"query":{"query":"SELECT 1"}}}`},
		{"own destination", `{"jobReference":{"jobId":"d6"},"configuration":{"query":{"query":"SELECT 1",` +
			`"destinationTable":{"projectId":"p","datasetId":"a","tableId":"t"}}}}`},
		{"load", `{"jobReference":{"jobId":"d7"},"configuration":{"load":{"sourceUris":["gs://b/o"]}}}`},
	} {
		emu.log = nil
		do(t, q, "POST", base+"/jobs", c.body)
		if sent := emu.sentJobs(); len(sent) != 1 || sent[0] != c.body {
			t.Errorf("%s: sent %v, want it as it was", c.name, sent)
		}
	}

	// The dataset deleted (by a client): made again, and the job sent again.
	delete(emu.datasets, resultsDataset)
	emu.log = nil
	if code, got := do(t, q, "POST", base+"/jobs", queryJob("j3", "SELECT 1", "")); code != 200 || len(emu.sentJobs()) != 2 || emu.datasetInserts() != 1 {
		t.Errorf("after the dataset was deleted: %d %v, sent %v", code, got, emu.log)
	}
	// The emulator restarted: the front makes it before the next job.
	q.reset()
	emu.datasets = nil
	emu.log = nil
	if code, _ := do(t, q, "POST", base+"/jobs", queryJob("j4", "SELECT 1", "")); code != 200 || len(emu.sentJobs()) != 1 || emu.datasetInserts() != 1 {
		t.Errorf("after a restart: %d, sent %v", code, emu.log)
	}
}

// TestDatasetsListHidesResults (#1017): datasets.list leaves resultsDataset
// out, as BigQuery leaves a hidden dataset out, unless all=true.
func TestDatasetsListHidesResults(t *testing.T) {
	emu := &resultsEmulator{datasets: map[string]bool{"a": true, resultsDataset: true, "b": true}}
	q := Results(emu)
	ids := func(got map[string]any) []string {
		var out []string
		list, _ := got["datasets"].([]any)
		for _, d := range list {
			ref, _ := d.(map[string]any)["datasetReference"].(map[string]any)
			out = append(out, ref["datasetId"].(string))
		}
		return out
	}
	if code, got := do(t, q, "GET", base+"/datasets", ""); code != 200 || strings.Join(ids(got), ",") != "a,b" || got["etag"] != "e" {
		t.Errorf("datasets.list: %d %v", code, got)
	}
	if code, got := do(t, q, "GET", base+"/datasets?all=true", ""); code != 200 || strings.Join(ids(got), ",") != "a,"+resultsDataset+",b" {
		t.Errorf("datasets.list all=true: %d %v", code, got)
	}
	emu.datasets = map[string]bool{resultsDataset: true}
	if code, got := do(t, q, "GET", base+"/datasets", ""); code != 200 || got["datasets"] != nil || got["kind"] != "bigquery#datasetList" {
		t.Errorf("datasets.list of the results dataset alone: %d %v", code, got)
	}
}

func TestIsLoneQuery(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT 1": true, "select 1;": true, "WITH a AS (SELECT 1) SELECT * FROM a": true, "(SELECT 1)": true,
		"/* c */ SELECT 'a;b'": true, "SELECT 1;;": true,
		"": false, ";": false, "SELECT 1; SELECT 2": false, "INSERT INTO t SELECT 1": false, "CREATE TABLE t AS SELECT 1": false,
		"DECLARE x INT64": false, "BEGIN SELECT 1; END": false, "EXECUTE IMMEDIATE 'SELECT 1'": false, "SELECT 'unterminated": false,
	} {
		if got := isLoneQuery(sql); got != want {
			t.Errorf("isLoneQuery(%q) = %v, want %v", sql, got, want)
		}
	}
}
