package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// stateEmulator stands in for the emulator with state of its own, as
// measured against the pinned image (#986, #987, #990): datasets and their
// tables, rows counted per table, and functions in the engine's catalog
// that CREATE FUNCTION of an existing one does not replace and that
// datasets.delete leaves behind. DROP SCHEMA is refused.
type stateEmulator struct {
	datasets map[string]bool
	tables   map[string]string // "ds.t" -> tables.get body
	rows     map[string]int    // "ds.t" -> rows
	funcs    map[string]string // "ds.f" -> body ("TABLE" for a table function)
	jobs     map[string]map[string]any
	log      []string
	// fail, when it matches a query, fails it with 400.
	fail *regexp.Regexp
	// created are the jobs' creationTimes, for jobs.list (#1001).
	created map[string]string
	// detected is the schema a load with none makes its table with, and
	// loaded the rows it loads (#1000).
	detected string
	loaded   int
}

func newStateEmulator() *stateEmulator {
	return &stateEmulator{datasets: map[string]bool{}, tables: map[string]string{}, rows: map[string]int{},
		funcs: map[string]string{}, jobs: map[string]map[string]any{}}
}

var (
	emuPath     = `((?:` + "`[^`]+`" + `|\w+)(?:\.(?:` + "`[^`]+`" + `|\w+))*)`
	reCall      = regexp.MustCompile(`^SELECT ` + emuPath + `\(\)$`)
	reCreateFn  = regexp.MustCompile(`^CREATE (OR REPLACE )?(TABLE )?FUNCTION (IF NOT EXISTS )?` + emuPath + `\(x INT64\) AS \((.*)\)$`)
	reDropFn    = regexp.MustCompile(`^DROP FUNCTION (IF EXISTS )?` + emuPath + `$`)
	reInsertSel = regexp.MustCompile(`^INSERT INTO ` + emuPath + ` \([^)]*\) (SELECT .*)$`)
	reFrom      = regexp.MustCompile(`FROM ` + emuPath)
)

func unquote(p string) string { return strings.ReplaceAll(p, "`", "") }

func (e *stateEmulator) errorf(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": fmt.Sprintf(format, args...)}})
}

// statement runs one statement, as the emulator's engine does.
func (e *stateEmulator) statement(q string) (int, string, string) {
	q = strings.TrimSpace(q)
	if e.fail != nil && e.fail.MatchString(q) {
		return 400, "failed on purpose", ""
	}
	switch m := []string(nil); {
	case strings.HasPrefix(q, "DROP SCHEMA"):
		return 400, "currently unsupported DROP SCHEMA statement", ""
	case strings.HasPrefix(q, "DROP TABLE IF EXISTS"), q == "SELECT 1":
		return 200, "", ""
	case func() bool { m = reCall.FindStringSubmatch(q); return m != nil }():
		switch body, ok := e.funcs[unquote(m[1])]; {
		case !ok:
			return 400, "failed to analyze: Function not found: " + unquote(m[1]), ""
		case body == "TABLE":
			return 400, "failed to analyze: Table-valued function is not expected here: " + unquote(m[1]), ""
		}
		return 400, "failed to analyze: No matching signature for function", ""
	case func() bool { m = reCreateFn.FindStringSubmatch(q); return m != nil }():
		if strings.Contains(m[5], "nope") {
			return 400, "failed to analyze: Unrecognized name: nope", ""
		}
		name := unquote(m[4])
		if _, ok := e.funcs[name]; !ok {
			if m[2] != "" {
				e.funcs[name] = "TABLE"
			} else {
				e.funcs[name] = m[5]
			}
		}
		return 200, "", ""
	case func() bool { m = reDropFn.FindStringSubmatch(q); return m != nil }():
		delete(e.funcs, unquote(m[2]))
		return 200, "", ""
	case func() bool { m = reInsertSel.FindStringSubmatch(q); return m != nil }():
		n := 0
		for _, f := range reFrom.FindAllStringSubmatch(m[2], -1) {
			n += e.rows[unquote(f[1])]
		}
		e.rows[unquote(m[1])] += n
		return 200, "", ""
	case strings.HasPrefix(q, "SELECT COUNT(*) FROM ("):
		n := 0
		for _, f := range reFrom.FindAllStringSubmatch(q, -1) {
			n += e.rows[unquote(f[1])]
		}
		return 200, "", fmt.Sprintf(`[{"f":[{"v":"%d"}]}]`, n)
	case strings.HasPrefix(q, "SELECT 1 FROM "):
		f := reFrom.FindStringSubmatch(q)
		if e.rows[unquote(f[1])] > 0 {
			return 200, "", `[{"f":[{"v":"1"}]}]`
		}
		return 200, "", `[]`
	}
	return 200, "", ""
}

func (e *stateEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, base)
	parts := strings.Split(strings.Trim(p, "/"), "/")
	b, _ := io.ReadAll(r.Body)
	e.log = append(e.log, r.Method+" "+r.URL.RequestURI()+" "+string(b))
	switch {
	case parts[0] == "datasets" && len(parts) == 2:
		ds := parts[1]
		if !e.datasets[ds] {
			e.errorf(w, 404, "dataset %s is not found", ds)
			return
		}
		if r.Method == http.MethodDelete {
			var in []string
			for k := range e.tables {
				if strings.HasPrefix(k, ds+".") {
					in = append(in, k)
				}
			}
			if len(in) > 0 && r.URL.Query().Get("deleteContents") != "true" {
				e.errorf(w, 400, "Dataset p:%s is still in use", ds)
				return
			}
			for _, k := range in {
				delete(e.tables, k)
				delete(e.rows, k)
			}
			delete(e.datasets, ds)
			w.WriteHeader(http.StatusOK)
			return
		}
		writeJSON(w, 200, map[string]any{"datasetReference": map[string]string{"datasetId": ds}})
	case parts[0] == "datasets" && len(parts) == 3 && r.Method == http.MethodGet:
		var list []any
		for k := range e.tables {
			if strings.HasPrefix(k, parts[1]+".") {
				list = append(list, map[string]any{"id": k})
			}
		}
		writeJSON(w, 200, map[string]any{"tables": list})
	case parts[0] == "datasets" && len(parts) == 3 && r.Method == http.MethodPost:
		var t struct {
			TableReference tableRef        `json:"tableReference"`
			Schema         json.RawMessage `json:"schema"`
		}
		_ = json.Unmarshal(b, &t)
		if !e.datasets[parts[1]] {
			e.errorf(w, 404, "dataset %s is not found", parts[1])
			return
		}
		key := parts[1] + "." + t.TableReference.TableID
		e.tables[key] = `{"type":"TABLE","schema":` + string(t.Schema) + `}`
		_, _ = io.WriteString(w, e.tables[key])
	case parts[0] == "datasets" && len(parts) == 4:
		key := parts[1] + "." + parts[3]
		meta, ok := e.tables[key]
		if !ok {
			e.errorf(w, 404, "table %s is not found", parts[3])
			return
		}
		switch r.Method {
		case http.MethodDelete:
			delete(e.tables, key)
			delete(e.rows, key)
			w.WriteHeader(http.StatusOK)
		case http.MethodPatch:
			var t struct {
				Schema json.RawMessage `json:"schema"`
			}
			_ = json.Unmarshal(b, &t)
			e.tables[key] = `{"type":"TABLE","schema":` + string(t.Schema) + `}`
		default:
			_, _ = io.WriteString(w, meta)
		}
	case parts[0] == "datasets" && len(parts) == 3 && parts[2] == "routines" && r.Method == http.MethodPost:
		var rt struct {
			RoutineReference struct {
				DatasetID string `json:"datasetId"`
				RoutineID string `json:"routineId"`
			} `json:"routineReference"`
			DefinitionBody string `json:"definitionBody"`
		}
		_ = json.Unmarshal(b, &rt)
		e.funcs[rt.RoutineReference.DatasetID+"."+rt.RoutineReference.RoutineID] = rt.DefinitionBody
		_, _ = w.Write(b)
	case parts[0] == "jobs" && len(parts) == 1 && r.Method == http.MethodGet:
		var list []any
		for id := range e.jobs {
			list = append(list, map[string]any{"jobReference": map[string]string{"projectId": "p", "jobId": id},
				"statistics": map[string]any{"creationTime": e.created[id]}, "status": map[string]any{"state": "DONE"}})
		}
		writeJSON(w, 200, map[string]any{"jobs": list})
	case parts[0] == "jobs" && len(parts) == 2 && r.Method == http.MethodGet:
		job, ok := e.jobs[parts[1]]
		if !ok {
			e.errorf(w, 404, "job %s is not found", parts[1])
			return
		}
		writeJSON(w, 200, job)
	case (parts[0] == "queries" || parts[0] == "jobs") && r.Method == http.MethodPost:
		var body struct {
			Query         string         `json:"query"`
			JobReference  map[string]any `json:"jobReference"`
			Configuration map[string]any `json:"configuration"`
		}
		_ = json.Unmarshal(b, &body)
		q := body.Query
		if parts[0] == "jobs" {
			qc, _ := body.Configuration["query"].(map[string]any)
			q, _ = qc["query"].(string)
			// A load makes its table with its schema, or detected's.
			if l, _ := body.Configuration["load"].(map[string]any); l != nil {
				d, _ := l["destinationTable"].(map[string]any)
				ds, _ := d["datasetId"].(string)
				tb, _ := d["tableId"].(string)
				schema, _ := json.Marshal(l["schema"])
				if l["schema"] == nil {
					schema = []byte(e.detected)
				}
				e.tables[ds+"."+tb] = `{"type":"TABLE","schema":` + string(schema) + `}`
				e.rows[ds+"."+tb] += e.loaded
			}
		}
		status, msg, rows := 200, "", ""
		for _, s := range strings.Split(q, ";") {
			if status, msg, rows = e.statement(s); status != 200 {
				break
			}
		}
		if parts[0] == "queries" {
			if status != 200 {
				e.errorf(w, status, "%s", msg)
				return
			}
			if rows == "" {
				rows = "[]"
			}
			_, _ = io.WriteString(w, `{"jobComplete":true,"jobReference":{"projectId":"p","jobId":"q`+fmt.Sprint(len(e.log))+`"},"rows":`+rows+`}`)
			return
		}
		job := map[string]any{"jobReference": body.JobReference, "configuration": body.Configuration, "status": map[string]any{"state": "DONE"}}
		if status != 200 {
			failJob(job, rowError{Reason: "invalidQuery", Message: msg})
		}
		id, _ := body.JobReference["jobId"].(string)
		e.jobs[id] = job
		writeJSON(w, 200, job)
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, http.StatusTeapot)
	}
}

// sent reports whether a request matching pattern was sent.
func (e *stateEmulator) sent(pattern string) bool {
	re := regexp.MustCompile(pattern)
	for _, l := range e.log {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

func jobState(got map[string]any) (reason string, done bool) {
	st, _ := got["status"].(map[string]any)
	done = st["state"] == "DONE"
	if e, ok := st["errorResult"].(map[string]any); ok {
		reason, _ = e["reason"].(string)
	}
	return reason, done
}

// TestCreateFunctionOfAnExistingFunction (#986): CREATE FUNCTION of a
// function that exists fails as BigQuery fails it; a lone CREATE OR
// REPLACE of one is carried out, keeping the old one when the new one
// fails; IF NOT EXISTS and a new function are sent as they are; in a
// script, after other statements, and for a table function, it is 501.
func TestCreateFunctionOfAnExistingFunction(t *testing.T) {
	setup := func() *stateEmulator {
		e := newStateEmulator()
		e.datasets["ds"] = true
		e.funcs["ds.f"] = "x + 1"
		e.funcs["ds.tf"] = "TABLE"
		return e
	}
	for _, path := range []string{"/queries", "/jobs"} {
		e := setup()
		code, got := do(t, Wrap(e), "POST", base+path, queryBody(path, "CREATE FUNCTION ds.f(x INT64) AS (x + 2)"))
		if path == "/queries" && (code != 409 || errorReason(got) != "duplicate") {
			t.Errorf("%s: CREATE FUNCTION of an existing one: %d %v", path, code, got)
		}
		if path == "/jobs" {
			if reason, done := jobState(got); code != 200 || !done || reason != "duplicate" {
				t.Errorf("%s: CREATE FUNCTION of an existing one: %d %v", path, code, got)
			}
			conf, _ := got["configuration"].(map[string]any)
			if qc, _ := conf["query"].(map[string]any); qc["query"] != "CREATE FUNCTION ds.f(x INT64) AS (x + 2)" {
				t.Errorf("%s: the job shows %v, want the client's text", path, qc)
			}
		}
		if e.funcs["ds.f"] != "x + 1" || e.sent(`CREATE FUNCTION ds\.f`) {
			t.Errorf("%s: the function was changed: %v, sent %v", path, e.funcs, e.log)
		}

		e = setup()
		code, _ = do(t, Wrap(e), "POST", base+path, queryBody(path, "CREATE OR REPLACE FUNCTION ds.f(x INT64) AS (x + 3)"))
		if code != 200 || e.funcs["ds.f"] != "x + 3" || len(e.funcs) != 2 {
			t.Errorf("%s: CREATE OR REPLACE: %d, functions %v, sent %v", path, code, e.funcs, e.log)
		}

		e = setup()
		code, _ = do(t, Wrap(e), "POST", base+path, queryBody(path, "CREATE OR REPLACE FUNCTION ds.f(x INT64) AS (nope + 3)"))
		if code == 200 || e.funcs["ds.f"] != "x + 1" || len(e.funcs) != 2 {
			t.Errorf("%s: CREATE OR REPLACE that fails: %d, functions %v, sent %v", path, code, e.funcs, e.log)
		}

		sqls := []string{"CREATE FUNCTION IF NOT EXISTS ds.f(x INT64) AS (x + 9)", "CREATE FUNCTION ds.g(x INT64) AS (x + 9)"}
		if path == "/jobs" {
			// (A DROP FUNCTION in a script given to jobs.query is 501, #976.)
			sqls = append(sqls, "DROP FUNCTION ds.f; CREATE FUNCTION ds.f(x INT64) AS (x + 9)")
		}
		for _, sql := range sqls {
			e = setup()
			if code, got := do(t, Wrap(e), "POST", base+path, queryBody(path, sql)); code != 200 || !e.sent(regexp.QuoteMeta(sql)) {
				t.Errorf("%s %q: %d %v, sent %v", path, sql, code, got, e.log)
			}
		}
		for _, sql := range []string{
			"SELECT 1; CREATE FUNCTION ds.f(x INT64) AS (x + 2)",
			"CREATE OR REPLACE FUNCTION ds.f(x INT64) AS (x + 2); SELECT 1",
			"CREATE FUNCTION ds.g(x INT64) AS (x + 2); CREATE FUNCTION ds.g(x INT64) AS (x + 3)",
			"CREATE OR REPLACE TABLE FUNCTION ds.tf(x INT64) AS (SELECT x AS y)",
			"CREATE OR REPLACE FUNCTION ds.tf(x INT64) AS (x)",
		} {
			e = setup()
			code, got := do(t, Wrap(e), "POST", base+path, queryBody(path, sql))
			if code != 501 || errorReason(got) != "notImplemented" || e.sent(`CREATE|DROP`) {
				t.Errorf("%s %q: %d %v, sent %v", path, sql, code, got, e.log)
			}
		}
	}
}

// TestDropSchema (#990): DROP SCHEMA is carried out through
// datasets.delete after the query, which the emulator is sent with a
// statement that does nothing in its place; failed as BigQuery fails it
// when it is the first statement; 501 in a script where the other
// statements could see the dataset.
func TestDropSchema(t *testing.T) {
	setup := func() *stateEmulator {
		e := newStateEmulator()
		e.datasets["empty"], e.datasets["full"] = true, true
		e.tables["full.t"] = `{"type":"TABLE","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`
		return e
	}
	for _, path := range []string{"/queries", "/jobs"} {
		for _, c := range []struct {
			sql    string
			reason string // "" for success
			gone   string // the dataset dropped
		}{
			{"DROP SCHEMA empty", "", "empty"},
			{"DROP SCHEMA IF EXISTS empty RESTRICT", "", "empty"},
			{"DROP SCHEMA full CASCADE", "", "full"},
			{"DROP SCHEMA full", "resourceInUse", ""},
			{"DROP SCHEMA nope", "notFound", ""},
			{"DROP SCHEMA IF EXISTS nope CASCADE", "", ""},
			{"DROP SCHEMA full CASCADE; SELECT 1", "", "full"},
			{"SELECT 1; DROP SCHEMA empty", "", "empty"},
			{"SELECT 1; DROP SCHEMA full", "notImplemented", ""},
			{"SELECT 1; DROP SCHEMA nope", "notImplemented", ""},
			{"CREATE TABLE full.u (a INT64); DROP SCHEMA full CASCADE", "notImplemented", ""},
			{"DROP SCHEMA full CASCADE; SELECT * FROM full.t", "notImplemented", ""},
			{"DROP SCHEMA full SOMETHING", "notImplemented", ""},
		} {
			e := setup()
			code, got := do(t, Wrap(e), "POST", base+path, queryBody(path, c.sql))
			reason := errorReason(got)
			if path == "/jobs" && code == 200 {
				reason, _ = jobState(got)
			}
			if reason != c.reason {
				t.Errorf("%s %q: %d %v, want %q", path, c.sql, code, got, c.reason)
			}
			for _, ds := range []string{"empty", "full"} {
				if e.datasets[ds] == (ds == c.gone) {
					t.Errorf("%s %q: dataset %s there: %v", path, c.sql, ds, e.datasets[ds])
				}
			}
			if e.sent("DROP SCHEMA") {
				t.Errorf("%s %q: the emulator was sent DROP SCHEMA: %v", path, c.sql, e.log)
			}
		}
	}

	// The functions the front saw made in a dataset: RESTRICT fails,
	// CASCADE drops them; a table function is 501 for CASCADE.
	e := setup()
	h := Wrap(e)
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "CREATE FUNCTION empty.f(x INT64) AS (x + 1)")); code != 200 {
		t.Fatalf("CREATE FUNCTION: %d %v", code, got)
	}
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "DROP SCHEMA empty")); code != 400 || errorReason(got) != "resourceInUse" {
		t.Errorf("DROP SCHEMA of a dataset with a function: %d %v", code, got)
	}
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "DROP SCHEMA empty CASCADE")); code != 200 || e.datasets["empty"] {
		t.Errorf("DROP SCHEMA CASCADE of a dataset with a function: %d %v", code, got)
	}
	if _, ok := e.funcs["empty.f"]; ok {
		t.Errorf("the function outlived its dataset: %v", e.funcs)
	}
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "CREATE TABLE FUNCTION full.tf(x INT64) AS (SELECT x AS y)")); code != 200 {
		t.Fatalf("CREATE TABLE FUNCTION: %d %v", code, got)
	}
	if code, got := do(t, h, "POST", base+"/queries", queryBody("/queries", "DROP SCHEMA full CASCADE")); code != 501 || !e.datasets["full"] {
		t.Errorf("DROP SCHEMA CASCADE of a dataset with a table function: %d %v", code, got)
	}

	// A failed script keeps the dataset; a failed query job says so.
	for _, path := range []string{"/queries", "/jobs"} {
		e := setup()
		e.fail = regexp.MustCompile(`SELECT 2`)
		code, got := do(t, Wrap(e), "POST", base+path, queryBody(path, "DROP SCHEMA full CASCADE; SELECT 2"))
		if !e.datasets["full"] {
			t.Errorf("%s: a failed script dropped the dataset", path)
		}
		reason := errorReason(got)
		if path == "/jobs" {
			reason, _ = jobState(got)
		}
		if reason != "notImplemented" {
			t.Errorf("%s: a failed script: %d %v", path, code, got)
		}
	}
}

// TestCopyJobs (#987): a copy job is carried out by the front, as a job
// of its own, through tables.insert and INSERT ... SELECT, honouring the
// write and create dispositions; what it cannot do is 501.
func TestCopyJobs(t *testing.T) {
	const schema = `{"fields":[{"name":"a","type":"INTEGER","mode":"REQUIRED"},{"name":"f","type":"FLOAT"}]}`
	setup := func() *stateEmulator {
		e := newStateEmulator()
		e.datasets["ds"] = true
		for _, n := range []string{"s", "s2", "full"} {
			e.tables["ds."+n] = `{"type":"TABLE","schema":` + schema + `}`
		}
		e.tables["ds.empty"] = `{"type":"TABLE","schema":` + schema + `}`
		e.tables["ds.other"] = `{"type":"TABLE","schema":{"fields":[{"name":"z","type":"STRING"}]}}`
		e.tables["ds.v"] = `{"type":"VIEW","schema":` + schema + `}`
		e.rows["ds.s"], e.rows["ds.s2"], e.rows["ds.full"], e.rows["ds.other"] = 2, 3, 1, 1
		return e
	}
	job := func(id string, srcs []string, dest, extra string) string {
		var refs []string
		for _, s := range srcs {
			ds, tb, _ := strings.Cut(s, ".")
			refs = append(refs, `{"projectId":"p","datasetId":"`+ds+`","tableId":"`+tb+`"}`)
		}
		ds, tb, _ := strings.Cut(dest, ".")
		return `{"jobReference":{"projectId":"p","jobId":"` + id + `"},"configuration":{"copy":{"sourceTables":[` + strings.Join(refs, ",") +
			`],"destinationTable":{"projectId":"p","datasetId":"` + ds + `","tableId":"` + tb + `"}` + extra + `}}}`
	}

	// A new table, from two sources.
	e := setup()
	h := Wrap(e)
	code, got := do(t, h, "POST", base+"/jobs", job("c1", []string{"ds.s", "ds.s2"}, "ds.new", ""))
	if reason, done := jobState(got); code != 200 || !done || reason != "" {
		t.Fatalf("copy to a new table: %d %v", code, got)
	}
	if e.rows["ds.new"] != 5 || !strings.Contains(e.tables["ds.new"], `"mode":"REQUIRED"`) || !strings.Contains(e.tables["ds.new"], `"type":"FLOAT"`) {
		t.Errorf("the new table: %s, %d rows", e.tables["ds.new"], e.rows["ds.new"])
	}
	if !e.sent(`POST /bigquery/v2/projects/p/datasets/ds/tables .*"type":"FLOAT64"`) || !e.sent(`PATCH .*/tables/new`) {
		t.Errorf("FLOAT was not sent as FLOAT64 and patched back: %v", e.log)
	}
	stats, _ := got["statistics"].(map[string]any)
	if cp, _ := stats["copy"].(map[string]any); cp["copiedRows"] != "5" {
		t.Errorf("statistics: %v", stats)
	}
	if conf, _ := got["configuration"].(map[string]any); conf["jobType"] != "COPY" {
		t.Errorf("configuration: %v", conf)
	}
	if code, got := do(t, h, "GET", base+"/jobs/c1", ""); code != 200 || got["id"] != "p:c1" {
		t.Errorf("jobs.get of the copy: %d %v", code, got)
	}
	if code, _ := do(t, h, "POST", base+"/jobs", job("c1", []string{"ds.s"}, "ds.new2", "")); code != 409 {
		t.Errorf("a job ID in use: %d", code)
	}

	for _, c := range []struct {
		name   string
		srcs   []string
		dest   string
		extra  string
		reason string
		rows   int // the destination's rows after
	}{
		{"WRITE_EMPTY into a table with rows", []string{"ds.s"}, "ds.full", "", "duplicate", 1},
		{"WRITE_EMPTY into an empty table", []string{"ds.s"}, "ds.empty", "", "", 2},
		{"WRITE_APPEND", []string{"ds.s"}, "ds.full", `,"writeDisposition":"WRITE_APPEND"`, "", 3},
		{"WRITE_TRUNCATE", []string{"ds.s", "ds.s2"}, "ds.full", `,"writeDisposition":"WRITE_TRUNCATE"`, "", 5},
		{"WRITE_TRUNCATE onto itself", []string{"ds.full"}, "ds.full", `,"writeDisposition":"WRITE_TRUNCATE"`, "", 1},
		{"WRITE_TRUNCATE of another schema", []string{"ds.other"}, "ds.full", `,"writeDisposition":"WRITE_TRUNCATE"`, "", 1},
		{"CREATE_NEVER", []string{"ds.s"}, "ds.none", `,"createDisposition":"CREATE_NEVER"`, "notFound", 0},
		{"a source not found", []string{"ds.nope"}, "ds.none", "", "notFound", 0},
		{"a view", []string{"ds.v"}, "ds.none", "", "invalid", 0},
		{"a dataset not found", []string{"ds.s"}, "nods.none", "", "notFound", 0},
	} {
		e := setup()
		code, got := do(t, Wrap(e), "POST", base+"/jobs", job("j", c.srcs, c.dest, c.extra))
		reason, done := jobState(got)
		if code != 200 || !done || reason != c.reason {
			t.Errorf("%s: %d %v", c.name, code, got)
		}
		if e.rows[strings.Replace(c.dest, "nods.", "ds.", 1)] != c.rows {
			t.Errorf("%s: %d rows, want %d; sent %v", c.name, e.rows[c.dest], c.rows, e.log)
		}
		if c.name == "WRITE_TRUNCATE of another schema" && !strings.Contains(e.tables["ds.full"], `"z"`) {
			t.Errorf("%s: the schema is %s", c.name, e.tables["ds.full"])
		}
		for k := range e.tables {
			if strings.Contains(k, "_cloudburrow_") {
				t.Errorf("%s: a scratch table was left: %s", c.name, k)
			}
		}
	}

	// A failed INSERT: the table made for it is deleted.
	e = setup()
	e.fail = regexp.MustCompile(`^INSERT`)
	code, got = do(t, Wrap(e), "POST", base+"/jobs", job("j", []string{"ds.s"}, "ds.new", ""))
	if reason, _ := jobState(got); code != 200 || reason != "backendError" {
		t.Errorf("a failed INSERT: %d %v", code, got)
	}
	if _, ok := e.tables["ds.new"]; ok {
		t.Errorf("a failed copy left its table")
	}
	// ... and WRITE_TRUNCATE keeps the destination.
	code, got = do(t, Wrap(e), "POST", base+"/jobs", job("j2", []string{"ds.s"}, "ds.full", `,"writeDisposition":"WRITE_TRUNCATE"`))
	if reason, _ := jobState(got); code != 200 || reason != "backendError" || e.rows["ds.full"] != 1 {
		t.Errorf("a failed WRITE_TRUNCATE: %d %v, %d rows", code, got, e.rows["ds.full"])
	}

	for _, c := range []struct {
		name  string
		srcs  []string
		dest  string
		extra string
		code  int
	}{
		{"SNAPSHOT", []string{"ds.s"}, "ds.new", `,"operationType":"SNAPSHOT"`, 501},
		{"CLONE", []string{"ds.s"}, "ds.new", `,"operationType":"CLONE"`, 501},
		{"an encryption key", []string{"ds.s"}, "ds.new", `,"destinationEncryptionConfiguration":{"kmsKeyName":"k"}`, 501},
		{"an expiration", []string{"ds.s"}, "ds.new", `,"destinationExpirationTime":"2030-01-01T00:00:00Z"`, 501},
		{"schemas that differ", []string{"ds.s", "ds.other"}, "ds.new", "", 501},
		{"WRITE_APPEND of another schema", []string{"ds.other"}, "ds.full", `,"writeDisposition":"WRITE_APPEND"`, 501},
		{"into a view", []string{"ds.s"}, "ds.v", `,"writeDisposition":"WRITE_TRUNCATE"`, 501},
		{"a bad writeDisposition", []string{"ds.s"}, "ds.new", `,"writeDisposition":"WRITE_SOMETIMES"`, 400},
		{"a bad destination ID", []string{"ds.s"}, "ds.t!", "", 400},
	} {
		e := setup()
		code, got := do(t, Wrap(e), "POST", base+"/jobs", job("j", c.srcs, c.dest, c.extra))
		if code != c.code {
			t.Errorf("%s: %d %v, want %d", c.name, code, got, c.code)
		}
		if e.sent(`^POST .*(tables|queries) `) {
			t.Errorf("%s: something was written: %v", c.name, e.log)
		}
	}
	e = setup()
	if code, got := do(t, Wrap(e), "POST", base+"/jobs", `{"jobReference":{"jobId":"x"},"configuration":{"copy":{"sourceTable":`+
		`{"projectId":"other","datasetId":"ds","tableId":"s"},"destinationTable":{"projectId":"p","datasetId":"ds","tableId":"n"}}}}`); code != 501 {
		t.Errorf("a table of another project: %d %v", code, got)
	}
}
