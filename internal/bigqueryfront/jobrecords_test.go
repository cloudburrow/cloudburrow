package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// secondsEmulator is jobsEmulator with the emulator's times: each job it
// records is given statistics in seconds, as the pinned image gives a
// query job, unless its ID starts "upload", which it gives none, as it
// gives an upload's job none.
type secondsEmulator struct {
	*jobsEmulator
	at int64 // seconds since the epoch
}

func (e *secondsEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	e.jobsEmulator.ServeHTTP(rec, r)
	for id, job := range e.jobs {
		if _, ok := job["statistics"]; !ok && !strings.HasPrefix(id, "upload") {
			s := strconv.FormatInt(e.at, 10)
			job["statistics"] = map[string]any{"creationTime": s, "startTime": s, "endTime": s}
		}
	}
	body := rec.Body.Bytes()
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") && rec.Code == 200 {
		// The answer is the job as recorded.
		var job map[string]any
		if json.Unmarshal(body, &job) == nil {
			_, id := jobRef(job)
			body, _ = json.Marshal(e.jobs[id])
		}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(body)
}

func statTimes(t *testing.T, job map[string]any) [3]int64 {
	t.Helper()
	stats, _ := job["statistics"].(map[string]any)
	var out [3]int64
	for i, k := range timeFields {
		n, ok := msValue(stats[k])
		if !ok {
			t.Fatalf("no %s in %v", k, job)
		}
		out[i] = n
	}
	return out
}

// TestJobWithoutReferenceIsGivenOne (#973): a jobs.insert with no
// jobReference, of each kind, reaches the emulator with one, in the
// request's project, and keeps a location it gave.
func TestJobWithoutReferenceIsGivenOne(t *testing.T) {
	for _, body := range []string{
		`{"configuration":{"query":{"query":"SELECT 1","useLegacySql":false}}}`,
		`{"jobReference":{"location":"EU"},"configuration":{"query":{"query":"SELECT 1","useLegacySql":false}}}`,
		`{"configuration":{"load":{"sourceUris":["gs://b/x.json"],"sourceFormat":"NEWLINE_DELIMITED_JSON",` +
			`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}}}`,
	} {
		emu := &jobsEmulator{}
		code, got := do(t, Wrap(emu), "POST", base+"/jobs", body)
		if code != 200 || len(emu.log) == 0 {
			t.Fatalf("%s: %d %v", body, code, got)
		}
		sent := emu.log[len(emu.log)-1]
		var job jobBody
		var loc struct {
			JobReference struct {
				Location string `json:"location"`
			} `json:"jobReference"`
		}
		raw := sent[strings.Index(sent, " ")+1:]
		if json.Unmarshal([]byte(raw), &job) != nil || json.Unmarshal([]byte(raw), &loc) != nil ||
			!strings.HasPrefix(job.JobReference.JobID, "job_") || job.JobReference.ProjectID != "p" {
			t.Errorf("%s: the emulator was sent %s", body, sent)
		}
		if strings.Contains(body, "EU") != (loc.JobReference.Location == "EU") {
			t.Errorf("%s: location sent %q", body, loc.JobReference.Location)
		}
		if _, id := jobRef(got); id != job.JobReference.JobID {
			t.Errorf("%s: answered job %q, sent %q", body, id, job.JobReference.JobID)
		}
	}
	// An upload's job is given one in its first part; the data follows.
	emu := &jobsEmulator{}
	w := upload(t, Wrap(emu), `{"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON",`+
		`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}}}`, "{\"a\":1}\n")
	if w.Code != 200 || len(emu.log) != 1 || !strings.Contains(emu.log[0], `"jobId":"job_`) || !strings.Contains(emu.log[0], `{"a":1}`) {
		t.Errorf("upload: %d %s, sent %v", w.Code, w.Body, emu.log)
	}
}

// TestJobTimesInMilliseconds (#971): a job the emulator gives times in
// seconds, or none, has the front's times, in milliseconds, in the
// jobs.insert answer, jobs.get, jobs.cancel and jobs.list; one the front
// did not time has its seconds as milliseconds.
func TestJobTimesInMilliseconds(t *testing.T) {
	emu := &secondsEmulator{jobsEmulator: &jobsEmulator{}, at: 1790593527}
	h := Wrap(emu)
	before := time.Now().UnixMilli()
	_, inserted := do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"q1"},"configuration":{"query":{"query":"SELECT 1","useLegacySql":false}}}`)
	w := upload(t, h, `{"jobReference":{"projectId":"p","jobId":"upload1"},"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON",`+
		`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}}}`, "{\"a\":1}\n")
	after := time.Now().UnixMilli()
	var uploaded map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &uploaded)
	// A job made before the front (not timed by it).
	emu.record(map[string]any{"jobReference": map[string]any{"jobId": "old"}, "configuration": map[string]any{}})
	emu.jobs["old"]["statistics"] = map[string]any{"creationTime": "1790000000", "startTime": "1790000001", "endTime": "1790000002"}

	inRange := func(what string, got [3]int64) {
		t.Helper()
		if got[0] < before || got[2] > after || got[0] != got[1] || got[2] < got[0] {
			t.Errorf("%s: times %v, want within [%d, %d]", what, got, before, after)
		}
	}
	inRange("jobs.insert of a query", statTimes(t, inserted))
	inRange("jobs.insert of an upload", statTimes(t, uploaded))
	for _, id := range []string{"q1", "upload1"} {
		_, got := do(t, h, "GET", base+"/jobs/"+id, "")
		inRange("jobs.get of "+id, statTimes(t, got))
	}
	_, old := do(t, h, "GET", base+"/jobs/old", "")
	if got := statTimes(t, old); got != [3]int64{1790000000000, 1790000001000, 1790000002000} {
		t.Errorf("jobs.get of a job the front did not time: %v", got)
	}
	_, list := do(t, h, "GET", base+"/jobs", "")
	for _, j := range list["jobs"].([]any) {
		job := j.(map[string]any)
		if _, id := jobRef(job); id != "old" {
			inRange("jobs.list of "+id, statTimes(t, job))
		}
	}
}

// TestJobListPagesAndFilters (#972): jobs.list is newest first, pages by
// maxResults and pageToken, filters by stateFilter, minCreationTime,
// maxCreationTime and parentJobId, and leaves out the front's own queries.
func TestJobListPagesAndFilters(t *testing.T) {
	emu := &jobsEmulator{}
	h := Wrap(emu)
	add := func(id string, created int64, state, parent string) {
		job := emu.record(map[string]any{"jobReference": map[string]any{"jobId": id}, "configuration": map[string]any{}})
		stats := map[string]any{"creationTime": strconv.FormatInt(created, 10)}
		if parent != "" {
			stats["parentJobId"] = parent
		}
		job["statistics"] = stats
		job["status"] = map[string]any{"state": state}
	}
	add("a", 1790000001000, "DONE", "")
	add("b", 1790000003000, "RUNNING", "")
	add("c", 1790000002000, "DONE", "")
	add("child", 1790000004000, "DONE", "b")
	add("d", 1790000005000, "PENDING", "")
	ids := func(path string) ([]string, string) {
		t.Helper()
		code, list := do(t, h, "GET", path, "")
		if code != 200 {
			t.Fatalf("%s: %d %v", path, code, list)
		}
		var out []string
		jobs, _ := list["jobs"].([]any)
		for _, j := range jobs {
			_, id := jobRef(j.(map[string]any))
			out = append(out, id)
		}
		tok, _ := list["nextPageToken"].(string)
		return out, tok
	}
	for _, c := range []struct{ query, want string }{
		{"", "[d b c a]"},
		{"?stateFilter=done", "[c a]"},
		{"?stateFilter=running&stateFilter=pending", "[d b]"},
		{"?minCreationTime=1790000002000", "[d b c]"},
		{"?maxCreationTime=1790000002000", "[c a]"},
		{"?minCreationTime=1790000002000&maxCreationTime=1790000003000", "[b c]"},
		{"?parentJobId=b", "[child]"},
		{"?parentJobId=nope", "[]"},
		{"?allUsers=true", "[d b c a]"},
	} {
		if got, _ := ids(base + "/jobs" + c.query); fmt.Sprint(got) != c.want {
			t.Errorf("jobs.list%s: %v, want %s", c.query, got, c.want)
		}
	}
	var all []string
	path := base + "/jobs?maxResults=2"
	for pages := 0; ; pages++ {
		got, tok := ids(path)
		if len(got) > 2 || pages > 5 {
			t.Fatalf("page %d: %v", pages, got)
		}
		all = append(all, got...)
		if tok == "" {
			break
		}
		path = base + "/jobs?maxResults=2&pageToken=" + tok
		if pages == 0 {
			// A job made between pages is newer: it does not move the
			// next page.
			add("e", 1790000009000, "DONE", "")
		}
	}
	if fmt.Sprint(all) != "[d b c a]" {
		t.Errorf("paged: %v", all)
	}
	if code, _ := do(t, h, "GET", base+"/jobs?pageToken=%21", ""); code != 400 {
		t.Errorf("a bad page token: %d", code)
	}
	if code, _ := do(t, h, "GET", base+"/jobs?stateFilter=nope", ""); code != 400 {
		t.Errorf("a bad stateFilter: %d", code)
	}

	// The front's own queries (here, a CREATE TABLE ... AS SELECT's
	// columns, read with a query of its own) are not listed.
	emu2 := &jobsEmulator{run: func(q string) (int, string) {
		return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`
	}}
	h2 := Wrap(emu2)
	if code, got := do(t, h2, "POST", base+"/jobs", `{"jobReference":{"jobId":"ctas"},"configuration":{"query":{"query":"CREATE TABLE ds.x AS SELECT 1 AS a","useLegacySql":false}}}`); code != 200 {
		t.Fatalf("CREATE TABLE AS SELECT: %d %v", code, got)
	}
	if len(emu2.order) < 2 {
		t.Fatalf("the front ran no query of its own: %v", emu2.log)
	}
	_, list := do(t, h2, "GET", base+"/jobs", "")
	if jobs, _ := list["jobs"].([]any); len(jobs) != 1 {
		t.Errorf("jobs.list lists the front's own queries: %v", list)
	}
}

// TestParquetLoadWithoutSchema (#970): a Parquet load with no schema into
// a table that exists is sent with the table's schema; one into a new
// table, or that replaces the table, is 501 and nothing is sent.
func TestParquetLoadWithoutSchema(t *testing.T) {
	tables := map[string]string{tablesBase + "t": `{"type":"TABLE","schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}}`}
	parquet := func(table, extra string) string {
		return `{"jobReference":{"projectId":"p","jobId":"pq"},"configuration":{"load":{"sourceFormat":"PARQUET",` +
			`"destinationTable":{"datasetId":"ds","tableId":"` + table + `"}` + extra + `}}}`
	}
	emu := &jobsEmulator{tables: tables}
	w := upload(t, Wrap(emu), parquet("t", ""), "PAR1")
	if w.Code != 200 || len(emu.log) == 0 || !strings.Contains(emu.log[len(emu.log)-1],
		`"schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}`) {
		t.Errorf("into a table that exists: %d %s, sent %v", w.Code, w.Body, emu.log)
	}
	emu = &jobsEmulator{tables: tables}
	if code, _ := do(t, Wrap(emu), "POST", base+"/jobs", parquet("t", `,"sourceUris":["gs://b/x.parquet"]`)); code != 200 ||
		!strings.Contains(emu.log[len(emu.log)-1], `"schema":{"fields":[{"name":"a"`) {
		t.Errorf("from Cloud Storage into a table that exists: %d, sent %v", code, emu.log)
	}
	for _, c := range []struct{ table, extra string }{
		{"new", ""},
		{"new", `,"sourceUris":["gs://b/x.parquet"]`},
		{"t", `,"writeDisposition":"WRITE_TRUNCATE"`},
		{"t", `,"schemaUpdateOptions":["ALLOW_FIELD_ADDITION"]`},
	} {
		emu := &jobsEmulator{tables: tables}
		w := upload(t, Wrap(emu), parquet(c.table, c.extra), "PAR1")
		if w.Code != 501 || !strings.Contains(w.Body.String(), "Parquet load with no schema") {
			t.Errorf("%s%s: %d %s", c.table, c.extra, w.Code, w.Body)
		}
		for _, l := range emu.log {
			if strings.HasPrefix(l, "jobs ") {
				t.Errorf("%s%s: the load was sent: %v", c.table, c.extra, emu.log)
			}
		}
	}
	// With a schema, the job is sent as it came.
	emu = &jobsEmulator{tables: tables}
	body := parquet("new", `,"schema":{"fields":[{"name":"z","type":"STRING"}]}`)
	if w := upload(t, Wrap(emu), body, "PAR1"); w.Code != 200 || !strings.Contains(emu.log[len(emu.log)-1], `"name":"z"`) {
		t.Errorf("with a schema: %d, sent %v", w.Code, emu.log)
	}
}

// TestCSVExtractOfAnEmptyString (#975): a CSV extract of a table with an
// empty STRING or BYTES value is 501, before anything is written; one
// without is sent on, and a JSON one is not looked at.
func TestCSVExtractOfAnEmptyString(t *testing.T) {
	tables := map[string]string{tablesBase + "t": `{"type":"TABLE","numRows":"2","schema":{"fields":[{"name":"n","type":"INTEGER"},` +
		`{"name":"s","type":"STRING"},{"name":"b","type":"BYTES"}]}}`}
	for _, c := range []struct {
		answer string
		want   int
	}{
		{`{"jobComplete":true,"rows":[{"f":[{"v":"true"},{"v":"false"}]}]}`, 501},
		{`{"jobComplete":true,"rows":[{"f":[{"v":"false"},{"v":"true"}]}]}`, 501},
		{`{"jobComplete":true,"rows":[{"f":[{"v":"false"},{"v":"false"}]}]}`, 200},
	} {
		emu := &jobsEmulator{tables: tables, run: func(q string) (int, string) {
			if strings.Contains(q, "COUNTIF(`s` = '') > 0, COUNTIF(`b` = b'') > 0") {
				return 200, c.answer
			}
			return 200, `{"jobComplete":true}`
		}}
		code, got := do(t, Wrap(emu), "POST", base+"/jobs", `{"jobReference":{"jobId":"x"},"configuration":{"extract":{`+
			`"sourceTable":{"projectId":"p","datasetId":"ds","tableId":"t"},"destinationUris":["gs://b/o.csv"]}}}`)
		if code != c.want {
			t.Errorf("%s: %d %v", c.answer, code, got)
		}
		sent := false
		for _, l := range emu.log {
			sent = sent || strings.HasPrefix(l, "jobs ")
		}
		if sent != (c.want == 200) {
			t.Errorf("%s: sent %v", c.answer, emu.log)
		}
	}
}

// TestDropSchemaIsNotImplemented (#976): DROP SCHEMA, which the emulator
// does not run, is 501 before anything runs, alone and in a script.
func TestDropSchemaIsNotImplemented(t *testing.T) {
	for _, sql := range []string{"DROP SCHEMA ds", "DROP SCHEMA ds CASCADE; SELECT 1", "DROP SCHEMA IF EXISTS ds CASCADE"} {
		for _, path := range []string{"/queries", "/jobs"} {
			emu := &fakeEmulator{datasets: map[string]bool{"ds": true}}
			code, got := do(t, Wrap(emu), "POST", base+path, queryBody(path, sql))
			if code != 501 || len(emu.writes) != 0 {
				t.Errorf("%s %q: %d %v, sent %v", path, sql, code, got, emu.writes)
			}
		}
	}
}

// TestFailedScriptFunctions (#976): a script given to jobs.query that
// makes a function and then fails has the function taken out of the
// engine's catalog again when it did not exist before; one that drops a
// function is 501 before it runs; a query job is left as it is.
func TestFailedScriptFunctions(t *testing.T) {
	const fail = "SELECT * FROM nope.nope"
	run := func(exists bool) func(string) (int, string) {
		return func(q string) (int, string) {
			switch {
			case strings.HasPrefix(q, "SELECT `ds`.`f`()") && !exists:
				return 400, `{"error":{"code":400,"message":"failed to analyze: Function not found: ds.f"}}`
			case strings.HasPrefix(q, "SELECT `ds`.`f`()"):
				return 400, `{"error":{"code":400,"message":"failed to analyze: No matching signature for function"}}`
			case strings.Contains(q, "nope.nope"):
				return 400, `{"error":{"code":400,"message":"Table not found: nope.nope"}}`
			}
			return 200, `{"jobComplete":true}`
		}
	}
	uncataloged := func(emu *jobsEmulator) bool {
		for _, l := range emu.log {
			if strings.Contains(l, "DROP FUNCTION IF EXISTS `ds`.`f`; SELECT * FROM") {
				return true
			}
		}
		return false
	}
	for _, exists := range []bool{false, true} {
		emu := &jobsEmulator{run: run(exists)}
		code, _ := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "CREATE FUNCTION ds.f(x INT64) AS (x + 1); "+fail))
		if code != 501 {
			t.Errorf("exists=%v: %d", exists, code)
		}
		if uncataloged(emu) == exists {
			t.Errorf("exists=%v: sent %v", exists, emu.log)
		}
	}
	// It succeeds: nothing is taken out.
	emu := &jobsEmulator{run: run(false)}
	if code, _ := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "CREATE FUNCTION ds.f(x INT64) AS (x + 1); SELECT 1")); code != 200 || uncataloged(emu) {
		t.Errorf("a script that succeeds: %d, sent %v", code, emu.log)
	}
	// As a query job, which the emulator commits, nothing is looked up.
	emu = &jobsEmulator{run: run(false)}
	do(t, Wrap(emu), "POST", base+"/jobs", queryBody("/jobs", "CREATE FUNCTION ds.f(x INT64) AS (x + 1); "+fail))
	if uncataloged(emu) || len(emu.log) != 1 {
		t.Errorf("a query job: sent %v", emu.log)
	}
	for _, sql := range []string{"DROP FUNCTION ds.g; SELECT 1", "DROP TABLE FUNCTION IF EXISTS ds.g; " + fail,
		"CREATE TABLE FUNCTION ds.tf(x INT64) AS (SELECT x AS y); SELECT 1", "DROP TABLE FUNCTION ds.tf"} {
		emu := &jobsEmulator{run: run(false)}
		if code, _ := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", sql)); code != 501 || len(emu.log) != 0 {
			t.Errorf("%q: %d, sent %v", sql, code, emu.log)
		}
	}
	// A TEMP function, and DROP FUNCTION on its own, are sent on.
	for _, sql := range []string{"CREATE TEMP FUNCTION f(x INT64) AS (x); " + fail, "DROP FUNCTION ds.g"} {
		emu := &jobsEmulator{run: run(false)}
		do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", sql))
		if len(emu.log) != 1 {
			t.Errorf("%q: sent %v", sql, emu.log)
		}
	}
}
