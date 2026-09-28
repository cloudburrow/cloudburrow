package bigqueryfront

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// jobsEmulator records the jobs it is sent, as the emulator does, and
// answers jobs.get and jobs.list with them. A query is answered with
// run's answer; tables are kept by REST path.
type jobsEmulator struct {
	tables map[string]string
	run    func(query string) (int, string)
	jobs   map[string]map[string]any
	order  []string
	log    []string
	n      int
}

func (e *jobsEmulator) record(job map[string]any) map[string]any {
	e.n++
	ref, _ := job["jobReference"].(map[string]any)
	if ref == nil {
		ref = map[string]any{}
		job["jobReference"] = ref
	}
	ref["projectId"] = "p"
	if id, _ := ref["jobId"].(string); id == "" {
		ref["jobId"] = fmt.Sprintf("auto%d", e.n)
	}
	id := ref["jobId"].(string)
	job["status"] = map[string]any{"state": "DONE"}
	if e.jobs == nil {
		e.jobs = map[string]map[string]any{}
	}
	if _, ok := e.jobs[id]; !ok {
		e.order = append(e.order, id)
	}
	e.jobs[id] = job
	return job
}

func (e *jobsEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.Contains(path, "/tables/"):
		t, ok := e.tables[path]
		if !ok {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, t)
	case r.Method == http.MethodDelete:
		delete(e.tables, path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/jobs"):
		var list []any
		for _, id := range e.order {
			list = append(list, e.jobs[id])
		}
		writeJSON(w, 200, map[string]any{"jobs": list})
	case r.Method == http.MethodGet && strings.Contains(path, "/jobs/"):
		job, ok := e.jobs[path[strings.LastIndex(path, "/")+1:]]
		if !ok {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, 200, job)
	case r.Method == http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		insert := strings.HasSuffix(path, "/jobs")
		q, _ := body["query"].(string)
		if insert {
			conf, _ := body["configuration"].(map[string]any)
			if qc, ok := conf["query"].(map[string]any); ok {
				q, _ = qc["query"].(string)
			}
		}
		e.log = append(e.log, fmt.Sprintf("%s %s", path[strings.LastIndex(path, "/")+1:], b))
		status, answer := 200, `{"jobComplete":true}`
		if e.run != nil && q != "" {
			status, answer = e.run(q)
		}
		if status != 200 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, answer)
			return
		}
		if insert {
			writeJSON(w, 200, e.record(body))
			return
		}
		var resp map[string]any
		_ = json.Unmarshal([]byte(answer), &resp)
		job := e.record(map[string]any{"configuration": map[string]any{"query": map[string]any{"query": q}}})
		resp["jobReference"] = job["jobReference"]
		writeJSON(w, 200, resp)
	default:
		http.Error(w, "unexpected", http.StatusTeapot)
	}
}

// sentQuery returns the query text of the last query sent.
func (e *jobsEmulator) sentQuery(t *testing.T) string {
	t.Helper()
	if len(e.log) == 0 {
		t.Fatal("nothing was sent")
	}
	last := e.log[len(e.log)-1]
	var body struct {
		Query         string `json:"query"`
		Configuration struct {
			Query struct {
				Query string `json:"query"`
			} `json:"query"`
		} `json:"configuration"`
	}
	_ = json.Unmarshal([]byte(last[strings.Index(last, " ")+1:]), &body)
	if body.Query != "" {
		return body.Query
	}
	return body.Configuration.Query.Query
}

func TestRenameVariables(t *testing.T) {
	unique := regexp.MustCompile(`cbvar_[0-9a-f]{12}_`)
	// The tables the statements read: ds.t has columns a and n.
	lookup := func(parts []string, _ int) ([]string, bool) {
		if strings.EqualFold(strings.Join(parts, "."), "ds.t") {
			return []string{"a", "N"}, true
		}
		if strings.EqualFold(strings.Join(parts, "."), "ds.u") {
			return []string{"a"}, true
		}
		return nil, false
	}
	for _, c := range []struct{ in, want string }{
		{"DECLARE zz INT64 DEFAULT 5; SELECT zz", "DECLARE @zz INT64 DEFAULT 5; SELECT @zz"},
		{"DECLARE Zz INT64; SELECT ZZ, zz", "DECLARE @zz INT64; SELECT @zz, @zz"},
		// Only what the engine replaces with the value: not a call (zz
		// (1)), a string or a quoted name, nor a word before the DECLARE
		// or in its own DEFAULT.
		{"SELECT zz; DECLARE zz INT64 DEFAULT zz; SELECT zz (1), 'zz', \"zz\", `zz`, zz\n(2), zz",
			"SELECT zz; DECLARE @zz INT64 DEFAULT zz; SELECT zz (1), 'zz', \"zz\", `zz`, @zz\n(2), @zz"},
		{"DECLARE a INT64 DEFAULT 1; DECLARE b INT64 DEFAULT a + 1; SET b = b * 2; SELECT a, b",
			"DECLARE @a INT64 DEFAULT 1; DECLARE @b INT64 DEFAULT @a + 1; SET @b = @b * 2; SELECT @a, @b"},
		// The engine records only the first name of a list.
		{"DECLARE a, b INT64; SELECT a, b", "DECLARE @a, b INT64; SELECT @a, b"},
		{"SELECT 1 AS declared", "SELECT 1 AS declared"},
		{"SELECT 'DECLARE x'", "SELECT 'DECLARE x'"},
		// #956: an alias, a table, a column list's name, a STRUCT field
		// and a column after a dot keep the client's name.
		{"DECLARE n INT64 DEFAULT 1; SELECT 2 AS n", "DECLARE @n INT64 DEFAULT 1; SELECT 2 AS n"},
		{"DECLARE n INT64 DEFAULT 1; SELECT 2 n, 'x' n2", "DECLARE @n INT64 DEFAULT 1; SELECT 2 n, 'x' n2"},
		{"DECLARE n INT64 DEFAULT 1; SELECT n FROM (SELECT 7 AS m) AS t", "DECLARE @n INT64 DEFAULT 1; SELECT @n FROM (SELECT 7 AS m) AS t"},
		{"DECLARE n INT64 DEFAULT 1; SELECT a FROM ds.u WHERE a = n", "DECLARE @n INT64 DEFAULT 1; SELECT a FROM ds.u WHERE a = @n"},
		{"DECLARE n INT64 DEFAULT 1; INSERT INTO ds.t (a, n) VALUES (n, 2)", "DECLARE @n INT64 DEFAULT 1; INSERT INTO ds.t (a, n) VALUES (@n, 2)"},
		{"DECLARE n INT64 DEFAULT 1; CREATE TEMP TABLE x (n INT64, s STRUCT<n STRING>); SELECT n",
			"DECLARE @n INT64 DEFAULT 1; CREATE TEMP TABLE x (n INT64, s STRUCT<n STRING>); SELECT @n"},
		{"DECLARE n INT64 DEFAULT 1; SELECT * FROM ds.t", "DECLARE @n INT64 DEFAULT 1; SELECT * FROM ds.t"},
		{"DECLARE s STRUCT<a INT64>; SELECT s.a", "DECLARE @s STRUCT<a INT64>; SELECT @s.a"},
		{"DECLARE d DATE; SELECT EXTRACT(YEAR FROM d) AS y", "DECLARE @d DATE; SELECT EXTRACT(YEAR FROM @d) AS y"},
		{"DECLARE n INT64; MERGE ds.u T USING ds.u S ON FALSE WHEN NOT MATCHED THEN INSERT (a, n) VALUES (n, 1)",
			"DECLARE @n INT64; MERGE ds.u T USING ds.u S ON FALSE WHEN NOT MATCHED THEN INSERT (a, n) VALUES (@n, 1)"},
		{"DECLARE n INT64; SELECT ARRAY<INT64>[n] AS arr", "DECLARE @n INT64; SELECT ARRAY<INT64>[@n] AS arr"},
	} {
		got, names, msg := renameVariables(c.in, lookup)
		if norm := unique.ReplaceAllString(got, "@"); norm != c.want || msg != "" {
			t.Errorf("%q: %q %q, want %q", c.in, norm, msg, c.want)
		}
		for u, orig := range names {
			if !unique.MatchString(u) || !strings.HasSuffix(u, "_"+strings.ToLower(orig)) {
				t.Errorf("%q: name %q for %q", c.in, u, orig)
			}
		}
		if again, _, _ := renameVariables(c.in, lookup); again == got && got != c.in {
			t.Errorf("%q: two renames gave the same names %q", c.in, got)
		}
	}
	// #956: a statement naming a variable where BigQuery may read a
	// column, an alias or a table of the name.
	for _, sql := range []string{
		"DECLARE n INT64 DEFAULT 1; SELECT n FROM (SELECT 7 AS n)",
		"DECLARE n INT64 DEFAULT 1; SELECT n FROM ds.t",
		"DECLARE n INT64 DEFAULT 1; SELECT a FROM ds.u, ds.t WHERE a = n",
		"DECLARE n INT64 DEFAULT 1; SELECT a FROM ds.nope WHERE a = n",
		"DECLARE n INT64 DEFAULT 1; SELECT t.n, n FROM ds.u AS t",
		"DECLARE n INT64 DEFAULT 1; SELECT n.a FROM ds.u n",
		"DECLARE n INT64 DEFAULT 1; SELECT a + n AS n FROM ds.u ORDER BY n",
		"DECLARE n INT64 DEFAULT 1; UPDATE ds.t SET a = n WHERE TRUE",
		"DECLARE n INT64 DEFAULT 1; SELECT * EXCEPT (n), n FROM ds.u",
	} {
		got, names, msg := renameVariables(sql, lookup)
		if got != sql || names != nil || !strings.Contains(msg, "Not implemented here: the script's variable n") {
			t.Errorf("%q: %q %v %q, want it refused", sql, got, names, msg)
		}
	}
}

func TestSelectColumns(t *testing.T) {
	for _, c := range []struct {
		in    string
		want  string
		known bool
	}{
		{"SELECT 1 AS a, b, t.c, d e, `f g`, COUNT(*) h, x + 1", "[a b c e f g h]", true},
		{"WITH w AS (SELECT 1 AS z) SELECT z AS y FROM w", "[y]", true},
		{"SELECT DISTINCT a FROM t UNION ALL SELECT b FROM u", "[a]", true},
		{"SELECT * FROM t", "[]", false},
		{"SELECT t.* FROM t", "[]", false},
		{"SELECT AS STRUCT 1 AS a", "[]", false},
	} {
		got, known := selectColumns(c.in)
		if fmt.Sprint(got) != c.want && !(len(got) == 0 && c.want == "[]") || known != c.known {
			t.Errorf("%q: %v %v, want %s %v", c.in, got, known, c.want, c.known)
		}
	}
}

// TestScriptVariablesDoNotOutliveTheScript (#933): a script's variables
// are sent to the emulator under names of their own, so no later query
// sees them; an error naming one names it as the client wrote it, and the
// job shows the client's text.
func TestScriptVariablesDoNotOutliveTheScript(t *testing.T) {
	sql := "DECLARE zz INT64 DEFAULT 5; SELECT zz"
	for _, path := range []string{"/queries", "/jobs"} {
		emu := &jobsEmulator{}
		h := Wrap(emu)
		code, got := do(t, h, "POST", base+path, queryBody(path, sql))
		sent := emu.sentQuery(t)
		if code != 200 || sent == sql || strings.Contains(sent, " zz") {
			t.Errorf("%s: %d %v, sent %q", path, code, got, sent)
		}
		id := "j1"
		if path == "/queries" {
			id, _ = got["jobReference"].(map[string]any)["jobId"].(string)
		} else if q := got["configuration"].(map[string]any)["query"].(map[string]any)["query"]; q != sql {
			t.Errorf("jobs.insert answered with the query %q", q)
		}
		if _, job := do(t, h, "GET", base+"/jobs/"+id, ""); job["configuration"].(map[string]any)["query"].(map[string]any)["query"] != sql {
			t.Errorf("%s: jobs.get: %v", path, job)
		}
		_, list := do(t, h, "GET", base+"/jobs", "")
		if q := list["jobs"].([]any)[0].(map[string]any)["configuration"].(map[string]any)["query"].(map[string]any)["query"]; q != sql {
			t.Errorf("%s: jobs.list: %v", path, q)
		}
	}

	emu := &jobsEmulator{run: func(q string) (int, string) {
		name := regexp.MustCompile(`cbvar_[0-9a-f]+_zz`).FindString(q)
		return 400, `{"error":{"code":400,"message":"Unrecognized name: ` + name + `","errors":[{"reason":"invalidQuery"}]}}`
	}}
	_, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "DECLARE zz INT64; SELECT zz + nope"))
	if msg, _ := got["error"].(map[string]any)["message"].(string); msg != "Unrecognized name: zz" {
		t.Errorf("the error: %v", got)
	}
}

// TestFailedScriptThatWroteIs501 (#935): the emulator rolls a failed
// script back whole, where BigQuery keeps what the statements before the
// failing one did, so a failed script with a statement that changes data
// and another after it is 501, naming the emulator's error; through
// jobs.insert the job is failed so. A syntax error, and a failed script
// that changes nothing kept, are answered as the emulator answered them.
func TestFailedScriptThatWroteIs501(t *testing.T) {
	fail := func(q string) (int, string) {
		if !strings.Contains(q, "nope") {
			return 200, `{"jobComplete":true}`
		}
		return 400, `{"error":{"code":400,"message":"failed to analyze: Table not found: nope.nope","errors":[{"reason":"jobInternalError"}]}}`
	}
	for _, c := range []struct {
		sql  string
		want int
	}{
		{"CREATE TABLE ds.e1 AS SELECT 1 AS a; SELECT * FROM nope.nope; CREATE TABLE ds.e2 AS SELECT 1 AS a", 501},
		{"INSERT INTO ds.t VALUES (1); SELECT * FROM nope.nope", 501},
		{"DROP TABLE ds.t; SELECT * FROM nope.nope", 501},
		{"BEGIN TRANSACTION; INSERT INTO ds.t VALUES (1); COMMIT TRANSACTION; SELECT * FROM nope.nope", 501},
		// Nothing kept either way.
		{"SELECT 1; SELECT * FROM nope.nope", 400},
		{"DECLARE x INT64; SET x = 2; SELECT * FROM nope.nope", 400},
		{"CREATE TEMP TABLE tt AS SELECT 1 AS a; INSERT INTO tt VALUES (2); SELECT * FROM nope.nope", 400},
		{"SELECT 1; CREATE TABLE ds.e AS SELECT * FROM nope.nope", 400},
		{"CREATE TABLE ds.e AS SELECT * FROM nope.nope", 400},
	} {
		emu := &jobsEmulator{run: fail}
		code, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", c.sql))
		msg, _ := got["error"].(map[string]any)["message"].(string)
		if code != c.want || !strings.Contains(msg, "Table not found: nope.nope") {
			t.Errorf("%s: %d %v, want %d", c.sql, code, got, c.want)
		}
	}
	emu := &jobsEmulator{run: func(string) (int, string) {
		return 400, `{"error":{"code":400,"message":"failed to parse statements: Syntax error","errors":[{"reason":"jobInternalError"}]}}`
	}}
	if code, _ := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "CREATE TABLE ds.e AS SELECT 1 AS a; SELEC x")); code != 400 {
		t.Errorf("a syntax error: %d", code)
	}

	emu = &jobsEmulator{run: func(string) (int, string) {
		return 200, `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE","errorResult":{"reason":"jobInternalError","message":"Table not found: nope.nope"}}}`
	}}
	// (#955) A query job the emulator failed as a job kept what the
	// statements before the failing one did, as BigQuery does: it fails
	// with the emulator's error; with a transaction in the script, 501.
	for sql, want := range map[string]string{
		"CREATE TABLE ds.e1 AS SELECT 1 AS a; SELECT * FROM nope.nope":                           "jobInternalError",
		"BEGIN TRANSACTION; INSERT INTO ds.t VALUES (1); COMMIT TRANSACTION; SELECT * FROM nope": "notImplemented",
	} {
		h := Wrap(&listingEmulator{scriptEmulator: &scriptEmulator{run: emu.run}})
		code, got := do(t, h, "POST", base+"/jobs", queryBody("/jobs", sql))
		if code != 200 || errorReason(got) != want {
			t.Errorf("jobs.insert %s: %d %v, want %s", sql, code, got, want)
		}
		if _, job := do(t, h, "GET", base+"/jobs/j1", ""); errorReason(job) != want {
			t.Errorf("jobs.get %s: %v, want %s", sql, job, want)
		}
	}
}

// TestReplacingATempTableIs501 (#936): the emulator fails a script that
// replaces or drops a TEMP table it created, so such a script is 501 and
// nothing is run; CREATE OR REPLACE TEMP TABLE of a new name, and a DROP
// of another table, are sent on.
func TestReplacingATempTableIs501(t *testing.T) {
	for _, c := range []struct {
		sql  string
		want int
	}{
		{"CREATE TEMP TABLE t AS SELECT 1 AS a; CREATE OR REPLACE TEMP TABLE t AS SELECT 2 AS a; SELECT * FROM t", 501},
		{"CREATE TEMP TABLE t AS SELECT 1 AS a; DROP TABLE t; CREATE TEMP TABLE t AS SELECT 3 AS a", 501},
		{"CREATE TEMPORARY TABLE t (a INT64); DROP TABLE IF EXISTS T", 501},
		{"CREATE TEMP TABLE t (a INT64); CREATE OR REPLACE TEMPORARY TABLE _SESSION.t (b INT64)", 501},
		{"CREATE OR REPLACE TEMP TABLE t2 AS SELECT 1 AS a; SELECT * FROM t2", 200},
		{"CREATE TEMP TABLE t AS SELECT 1 AS a; DROP TABLE ds.t", 200},
		{"DROP TABLE t; CREATE TEMP TABLE t AS SELECT 1 AS a", 200},
	} {
		emu := &jobsEmulator{}
		code, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", c.sql))
		if code != c.want || (code == 501) != (len(emu.log) == 0) {
			t.Errorf("%s: %d %v, want %d; sent %v", c.sql, code, got, c.want, emu.log)
		}
	}
}

// TestTempTableColumnsAreChecked (#938): the columns of a CREATE TEMP
// TABLE ... AS SELECT whose query cannot run alone are read by running the
// statements before it and then its query, when none of them changes
// data: a name BigQuery refuses is 400 and the script is not run. After a
// statement that changes data, or when the columns cannot be read, it is
// 501.
func TestTempTableColumnsAreChecked(t *testing.T) {
	answer := func(names ...string) string {
		var fields []string
		for _, n := range names {
			fields = append(fields, `{"name":"`+n+`","type":"INTEGER"}`)
		}
		return `{"jobComplete":true,"schema":{"fields":[` + strings.Join(fields, ",") + `]}}`
	}
	for _, c := range []struct {
		sql, check string // check: the columns the checking script returns, "" for a failure
		want       int
		ran        bool
	}{
		{"DECLARE x INT64 DEFAULT 1; CREATE TEMP TABLE tt AS SELECT x AS `b!`; SELECT * FROM tt", "b!", 400, false},
		{"DECLARE x INT64 DEFAULT 1; CREATE TEMP TABLE tt AS SELECT x AS b; SELECT * FROM tt", "b", 200, true},
		{"CREATE TEMP TABLE s AS SELECT 1 AS a; BEGIN CREATE TEMP TABLE tt AS SELECT a AS `c?` FROM s; END", "c?", 400, false},
		{"CREATE TABLE ds.s AS SELECT 1 AS a; CREATE TEMP TABLE tt AS SELECT a FROM ds.s", "a", 501, false},
		// The columns could not be read: the script's own answer, if it
		// fails; 501 if it does not.
		{"DECLARE x INT64; CREATE TEMP TABLE tt AS SELECT x AS b", "", 501, true},
	} {
		var checks []string
		emu := &jobsEmulator{}
		emu.run = func(q string) (int, string) {
			switch {
			case strings.HasPrefix(q, "SELECT * FROM (\n") && !strings.Contains(q, ";") && regexp.MustCompile(`\bx\b|FROM s|ds\.s`).MatchString(q):
				return 400, `{"error":{"code":400,"message":"Unrecognized name"}}` // the query alone
			case strings.Contains(q, ") LIMIT 0"):
				checks = append(checks, q)
				if c.check == "" {
					return 400, `{"error":{"code":400,"message":"no such table"}}`
				}
				return 200, answer(c.check)
			}
			return 200, `{"jobComplete":true}`
		}
		code, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", c.sql))
		ran := len(emu.log) > 0 && !strings.Contains(emu.sentQuery(t), "LIMIT 0")
		if code != c.want || ran != c.ran {
			t.Errorf("%s: %d %v, want %d; ran %v; sent %v", c.sql, code, got, c.want, ran, emu.log)
		}
		for _, q := range checks {
			if strings.Contains(q, "DECLARE x") || strings.Count(q, "BEGIN") != strings.Count(q, "END") {
				t.Errorf("%s: the check %q keeps its variables' names or leaves a block open", c.sql, q)
			}
		}
	}
}

// TestReplacedTableJobShowsTheClientsQuery (#939): the front carries out a
// lone CREATE OR REPLACE TABLE by sending another query; jobs.insert's
// answer, jobs.get and jobs.list show the client's.
func TestReplacedTableJobShowsTheClientsQuery(t *testing.T) {
	sql := "CREATE OR REPLACE TABLE ds.t AS SELECT 2 AS a"
	emu := &jobsEmulator{tables: map[string]string{tablesBase + "t": `{"type":"TABLE","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`},
		run: func(q string) (int, string) {
			return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`
		}}
	h := Wrap(emu)
	code, got := do(t, h, "POST", base+"/jobs", queryBody("/jobs", sql))
	query := func(job map[string]any) any {
		conf, _ := job["configuration"].(map[string]any)
		q, _ := conf["query"].(map[string]any)
		return q["query"]
	}
	if code != 200 || query(got) != sql || !strings.Contains(emu.sentQuery(t), "_cloudburrow_replace_") {
		t.Errorf("jobs.insert: %d %v; sent %q", code, got, emu.sentQuery(t))
	}
	if _, job := do(t, h, "GET", base+"/jobs/j1", ""); query(job) != sql {
		t.Errorf("jobs.get: %v", job)
	}
	_, list := do(t, h, "GET", base+"/jobs", "")
	for _, j := range list["jobs"].([]any) {
		job := j.(map[string]any)
		if job["jobReference"].(map[string]any)["jobId"] == "j1" && query(job) != sql {
			t.Errorf("jobs.list: %v", job)
		}
	}
}

// TestExtractJobs (#939): an extract to Cloud Storage is sent on only as
// the emulator carries it out as BigQuery does.
func TestExtractJobs(t *testing.T) {
	uploads := map[string]string{} // object name → content type and body
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/storage/v1/b/b":
			_, _ = io.WriteString(w, `{"name":"b"}`)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/b/o" && r.URL.Query().Get("uploadType") == "media":
			b, _ := io.ReadAll(r.Body)
			uploads[r.URL.Query().Get("name")] = r.Header.Get("Content-Type") + "\n" + string(b)
			_, _ = io.WriteString(w, `{"name":"x"}`)
			return
		}
		http.Error(w, "{}", http.StatusNotFound)
	}))
	defer storage.Close()
	tables := map[string]string{
		tablesBase + "t":      `{"type":"TABLE","numRows":"2","schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"s","type":"STRING"},{"name":"d","type":"DATE"}]}}`,
		tablesBase + "empty":  `{"type":"TABLE","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`,
		tablesBase + "v":      `{"type":"VIEW","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`,
		tablesBase + "nested": `{"type":"TABLE","numRows":"1","schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"r","type":"RECORD","fields":[{"name":"b","type":"INTEGER"}]}]}}`,
		tablesBase + "ts":     `{"type":"TABLE","numRows":"1","schema":{"fields":[{"name":"at","type":"TIMESTAMP"}]}}`,
	}
	job := func(extract string) string {
		return `{"jobReference":{"jobId":"x1"},"configuration":{"extract":{"sourceTable":{"projectId":"p","datasetId":"ds","tableId":` + extract + `}}}`
	}
	for _, c := range []struct {
		name, extract string
		want          int
		sent          []string // what the extract the emulator got holds
	}{
		{"CSV by default", `"t"},"destinationUris":["gs://b/out.csv"]`, 200, []string{`"destinationFormat":"CSV"`, `"destinationUris":["gs://b/out.csv"]`}},
		{"a wildcard", `"t"},"destinationUris":["gs://b/out-*.csv"],"destinationFormat":"CSV"`, 200, []string{`"destinationUris":["gs://b/out-000000000000.csv"]`}},
		{"destinationUri", `"t"},"destinationUri":"gs://b/out.csv"`, 200, []string{`"destinationUris":["gs://b/out.csv"]`}},
		{"printHeader true", `"t"},"destinationUris":["gs://b/out.csv"],"printHeader":true`, 200, []string{`"destinationUris"`}},
		{"no header, empty", `"empty"},"destinationUris":["gs://b/out.csv"],"printHeader":false`, 200, []string{`"printHeader":false`}},
		{"AVRO", `"t"},"destinationUris":["gs://b/out.avro"],"destinationFormat":"AVRO"`, 501, nil},
		{"PARQUET", `"t"},"destinationUris":["gs://b/out"],"destinationFormat":"PARQUET"`, 501, nil},
		{"JSON of a DATE", `"t"},"destinationUris":["gs://b/out.json"],"destinationFormat":"NEWLINE_DELIMITED_JSON"`, 501, nil},
		{"DEFLATE", `"t"},"destinationUris":["gs://b/out.csv"],"compression":"DEFLATE"`, 501, nil},
		{"a delimiter of two", `"t"},"destinationUris":["gs://b/out.csv"],"fieldDelimiter":"||"`, 501, nil},
		{"two URIs", `"t"},"destinationUris":["gs://b/a-*.csv","gs://b/b-*.csv"]`, 501, nil},
		{"two wildcards", `"t"},"destinationUris":["gs://b/a-*-*.csv"]`, 400, nil},
		{"a view", `"v"},"destinationUris":["gs://b/out.csv"]`, 400, nil},
		{"nested", `"nested"},"destinationUris":["gs://b/out.csv"]`, 400, nil},
		{"a TIMESTAMP", `"ts"},"destinationUris":["gs://b/out.csv"]`, 501, nil},
		{"no such bucket", `"t"},"destinationUris":["gs://nope/out.csv"]`, 404, nil},
		{"no such table", `"nope"},"destinationUris":["gs://b/out.csv"]`, 200, []string{`"tableId":"nope"`}},
	} {
		emu := &jobsEmulator{tables: tables}
		h := WrapStorage(emu, strings.TrimPrefix(storage.URL, "http://"))
		code, got := do(t, h, "POST", base+"/jobs", job(c.extract))
		if c.want == 501 && len(uploads) > 0 {
			t.Errorf("%s: 501, and %v was written", c.name, uploads)
		}
		sent := ""
		for _, l := range emu.log {
			if strings.HasPrefix(l, "jobs ") {
				sent = l
			}
		}
		if code != c.want || (sent != "") != (c.want == 200) {
			t.Errorf("%s: %d %v, want %d; sent %v", c.name, code, got, c.want, emu.log)
			continue
		}
		for _, s := range c.sent {
			if !strings.Contains(sent, s) {
				t.Errorf("%s: the emulator got %s, want %s", c.name, sent, s)
			}
		}
		if c.name == "printHeader true" && strings.Contains(sent, "printHeader") {
			t.Errorf("%s: the emulator got %s", c.name, sent)
		}
		if c.name == "a wildcard" {
			uris := func(job map[string]any) any {
				return job["configuration"].(map[string]any)["extract"].(map[string]any)["destinationUris"]
			}
			if u := uris(got); fmt.Sprint(u) != "[gs://b/out-*.csv]" {
				t.Errorf("jobs.insert answered with %v", u)
			}
			if _, j := do(t, h, "GET", base+"/jobs/x1", ""); fmt.Sprint(uris(j)) != "[gs://b/out-*.csv]" {
				t.Errorf("jobs.get: %v", j)
			}
		}
	}
	// A model.
	emu := &jobsEmulator{tables: tables}
	if code, _ := do(t, Wrap(emu), "POST", base+"/jobs", `{"configuration":{"extract":{"sourceModel":{"modelId":"m"},"destinationUris":["gs://b/m"]}}}`); code != 501 {
		t.Errorf("a model: %d", code)
	}
}

// TestExtractsTheFrontWrites (#957): a JSON extract, a GZIP one, one with
// another delimiter and an empty table's with a header are written by the
// front to the instance's Cloud Storage, as BigQuery documents them, and
// the job is the front's: jobs.get, jobs.list, jobs.cancel and
// jobs.delete answer it.
func TestExtractsTheFrontWrites(t *testing.T) {
	uploads := map[string]string{}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/storage/v1/b/b":
			_, _ = io.WriteString(w, `{"name":"b"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/b/o":
			b, _ := io.ReadAll(r.Body)
			uploads[r.URL.Query().Get("name")] = r.Header.Get("Content-Type") + "\n" + string(b)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "{}", http.StatusNotFound)
		}
	}))
	defer storage.Close()
	tables := map[string]string{
		tablesBase + "t":          `{"type":"TABLE","numRows":"2","schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"s","type":"STRING"}]}}`,
		tablesBase + "t/data":     `{"rows":[{"f":[{"v":"1"},{"v":"x,y<&>"}]},{"f":[{"v":"2"},{"v":"q\"r"}]}],"totalRows":"2"}`,
		tablesBase + "n":          `{"type":"TABLE","numRows":"1","schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"s","type":"STRING"}]}}`,
		tablesBase + "n/data":     `{"rows":[{"f":[{"v":"1"},{"v":null}]}]}`,
		tablesBase + "empty":      `{"type":"TABLE","numRows":"0","schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}}`,
		tablesBase + "empty/data": `{"totalRows":"0"}`,
	}
	job := func(id, table, extract string) string {
		return `{"jobReference":{"jobId":"` + id + `"},"configuration":{"extract":{"sourceTable":{"projectId":"p","datasetId":"ds","tableId":"` + table + `"},` + extract + `}}}`
	}
	gunzip := func(s string) string {
		zr, err := gzip.NewReader(strings.NewReader(s))
		if err != nil {
			return "not gzip: " + err.Error()
		}
		b, _ := io.ReadAll(zr)
		return string(b)
	}
	emu := &jobsEmulator{tables: tables}
	h := WrapStorage(emu, strings.TrimPrefix(storage.URL, "http://"))
	for _, c := range []struct {
		id, table, extract, object, want string
		gz                               bool
	}{
		{"j1", "t", `"destinationUris":["gs://b/t.json"],"destinationFormat":"NEWLINE_DELIMITED_JSON"`, "t.json",
			"application/json\n{\"a\":\"1\",\"s\":\"x,y\\u003c\\u0026\\u003e\"}\n{\"a\":\"2\",\"s\":\"q\\\"r\"}\n", false},
		{"j2", "t", `"destinationUris":["gs://b/t-*.csv.gz"],"compression":"GZIP"`, "t-000000000000.csv.gz",
			"a,s\n1,\"x,y<&>\"\n2,\"q\"\"r\"\n", true},
		{"j3", "t", `"destinationUris":["gs://b/t.tsv"],"fieldDelimiter":"\t","printHeader":false`, "t.tsv",
			"text/plain; charset=utf-8\n1\tx,y<&>\n2\t\"q\"\"r\"\n", false},
		{"j4", "empty", `"destinationUris":["gs://b/empty.csv"]`, "empty.csv", "text/plain; charset=utf-8\na,b\n", false},
		{"j5", "t", `"destinationUris":["gs://b/t.json.gz"],"destinationFormat":"NEWLINE_DELIMITED_JSON","compression":"GZIP"`, "t.json.gz",
			"{\"a\":\"1\",\"s\":\"x,y\\u003c\\u0026\\u003e\"}\n{\"a\":\"2\",\"s\":\"q\\\"r\"}\n", true},
	} {
		code, got := do(t, h, "POST", base+"/jobs", job(c.id, c.table, c.extract))
		if code != 200 || got["status"].(map[string]any)["state"] != "DONE" || got["status"].(map[string]any)["errorResult"] != nil {
			t.Errorf("%s: %d %v", c.id, code, got)
			continue
		}
		body := uploads[c.object]
		if c.gz {
			ct, data, _ := strings.Cut(body, "\n")
			body = gunzip(data)
			if ct != "application/x-gzip" {
				t.Errorf("%s: content type %q", c.id, ct)
			}
		}
		if body != c.want {
			t.Errorf("%s: wrote %q, want %q", c.id, body, c.want)
		}
		if _, j := do(t, h, "GET", base+"/jobs/"+c.id, ""); j["configuration"].(map[string]any)["jobType"] != "EXTRACT" {
			t.Errorf("%s: jobs.get: %v", c.id, j)
		}
	}
	if len(emu.log) != 0 {
		t.Errorf("the emulator was sent %v", emu.log)
	}
	// A NULL in JSON is not written; the job's ID is still free.
	if code, _ := do(t, h, "POST", base+"/jobs", job("j6", "n", `"destinationUris":["gs://b/n.json"],"destinationFormat":"NEWLINE_DELIMITED_JSON"`)); code != 501 || uploads["n.json"] != "" {
		t.Errorf("a NULL in JSON: %d", code)
	}
	if code, _ := do(t, h, "POST", base+"/jobs", job("j1", "t", `"destinationUris":["gs://b/again.json"],"destinationFormat":"NEWLINE_DELIMITED_JSON"`)); code != 409 {
		t.Errorf("a job ID in use: %d", code)
	}
	_, list := do(t, h, "GET", base+"/jobs?projection=full", "")
	ids := []string{}
	for _, j := range list["jobs"].([]any) {
		job := j.(map[string]any)
		if job["configuration"] == nil {
			t.Errorf("jobs.list with projection=full: %v", job)
		}
		ids = append(ids, job["jobReference"].(map[string]any)["jobId"].(string))
	}
	if fmt.Sprint(ids) != "[j1 j2 j3 j4 j5]" {
		t.Errorf("jobs.list: %v", ids)
	}
	if code, got := do(t, h, "POST", base+"/jobs/j1/cancel", ""); code != 200 || got["job"] == nil {
		t.Errorf("jobs.cancel: %d %v", code, got)
	}
	if code, _ := do(t, h, "DELETE", base+"/jobs/j1/delete", ""); code != 204 {
		t.Errorf("jobs.delete: %d", code)
	}
	if code, _ := do(t, h, "GET", base+"/jobs/j1", ""); code != 404 {
		t.Errorf("jobs.get after jobs.delete: %d", code)
	}
	// Without the instance's Cloud Storage, nothing is written.
	if code, _ := do(t, Wrap(&jobsEmulator{tables: tables}), "POST", base+"/jobs", job("j7", "t", `"destinationUris":["gs://b/x.json"],"destinationFormat":"NEWLINE_DELIMITED_JSON"`)); code != 501 {
		t.Errorf("no Cloud Storage: %d", code)
	}
}

// TestJobListGivesConfigurations (#958): the emulator's jobs.list gives no
// configuration; with projection=full the front gives each job's, from
// jobs.insert's answer or jobs.get, and none with the minimal projection.
func TestJobListGivesConfigurations(t *testing.T) {
	emu := &bareListEmulator{jobsEmulator: &jobsEmulator{}}
	h := Wrap(emu)
	if code, _ := do(t, h, "POST", base+"/jobs", queryBody("/jobs", "SELECT 1")); code != 200 {
		t.Fatal(code)
	}
	if code, _ := do(t, h, "POST", base+"/queries", queryBody("/queries", "SELECT 2")); code != 200 {
		t.Fatal(code)
	}
	query := func(job map[string]any) any {
		conf, _ := job["configuration"].(map[string]any)
		q, _ := conf["query"].(map[string]any)
		return q["query"]
	}
	for i := 0; i < 2; i++ {
		_, list := do(t, h, "GET", base+"/jobs?projection=full&allUsers=true", "")
		var got []any
		for _, j := range list["jobs"].([]any) {
			got = append(got, query(j.(map[string]any)))
		}
		if fmt.Sprint(got) != "[SELECT 1 SELECT 2]" {
			t.Errorf("jobs.list with projection=full: %v", got)
		}
	}
	if emu.gets != 1 {
		t.Errorf("jobs.get was sent %d times, want once, for the jobs.query job", emu.gets)
	}
	_, list := do(t, h, "GET", base+"/jobs", "")
	for _, j := range list["jobs"].([]any) {
		if j.(map[string]any)["configuration"] != nil {
			t.Errorf("jobs.list with the minimal projection: %v", j)
		}
	}
}

// bareListEmulator lists jobs as the emulator does: without their
// configuration.
type bareListEmulator struct {
	*jobsEmulator
	gets int
}

func (e *bareListEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs"):
		var list []any
		for _, id := range e.order {
			j := e.jobs[id]
			list = append(list, map[string]any{"jobReference": j["jobReference"], "kind": "bigquery#job", "status": j["status"]})
		}
		writeJSON(w, 200, map[string]any{"jobs": list})
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/jobs/"):
		e.gets++
		e.jobsEmulator.ServeHTTP(w, r)
	default:
		e.jobsEmulator.ServeHTTP(w, r)
	}
}

// TestFailedScriptResyncsTheCatalog (#955): after a failed script, the
// front puts the emulator's catalog back in step with its tables: through
// jobs.query, which the emulator rolls back, to the tables as it lists
// them; through a query job it commits, its list to the tables as the
// engine reads them.
func TestFailedScriptResyncsTheCatalog(t *testing.T) {
	script := "DROP TABLE ds.keep; CREATE TABLE ds.c1 AS SELECT 1 AS a; CREATE TEMP TABLE g AS SELECT 1 AS a; SELECT * FROM nope.nope"
	scratch := regexp.MustCompile("`_cloudburrow_replace_[0-9a-f]+\\.t`")
	for _, c := range []struct {
		path string
		want []string
	}{
		{"/queries", []string{
			"DROP TABLE IF EXISTS `ds.keep`; SELECT * FROM @",
			"CREATE TABLE IF NOT EXISTS `ds.keep` (`a` INT64 NOT NULL, `s` STRUCT<`b` STRING>, `r` ARRAY<NUMERIC(10, 2)>)",
			"DROP TABLE IF EXISTS `ds.c1`",
			"DROP TABLE IF EXISTS `g`",
		}},
		{"/jobs", []string{
			"SELECT * FROM `ds.keep` LIMIT 0",
			"CREATE TABLE IF NOT EXISTS `ds.keep` (`a` INT64 NOT NULL, `s` STRUCT<`b` STRING>, `r` ARRAY<NUMERIC(10, 2)>)",
			"DROP TABLE IF EXISTS `ds.keep`",
			"SELECT * FROM `ds.c1` LIMIT 0",
			"DROP TABLE IF EXISTS `ds.c1`; SELECT * FROM @",
			"CREATE TABLE IF NOT EXISTS `ds.c1` (`a` INT64)",
			"DROP TABLE IF EXISTS `g`",
		}},
	} {
		var sent []string
		emu := &jobsEmulator{tables: map[string]string{
			tablesBase + "keep": `{"type":"TABLE","schema":{"fields":[{"name":"a","type":"INTEGER","mode":"REQUIRED"},` +
				`{"name":"s","type":"RECORD","fields":[{"name":"b","type":"STRING"}]},{"name":"r","type":"NUMERIC","mode":"REPEATED","precision":"10","scale":"2"}]}}`,
		}}
		emu.run = func(q string) (int, string) {
			switch {
			case q == script && c.path == "/jobs":
				return 200, `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE","errorResult":{"reason":"jobInternalError","message":"Table not found: nope.nope"}}}`
			case q == script:
				return 400, `{"error":{"code":400,"message":"failed to analyze: Table not found: nope.nope","errors":[{"reason":"jobInternalError"}]}}`
			}
			if strings.HasPrefix(q, "SELECT * FROM (\n") {
				// A CREATE's query run alone (ctasColumns).
				return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`
			}
			sent = append(sent, scratch.ReplaceAllString(q, "@"))
			if q == "SELECT * FROM `ds.c1` LIMIT 0" {
				return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`
			}
			if strings.HasSuffix(q, "LIMIT 0") {
				return 400, `{"error":{"code":400,"message":"failed to analyze: Table not found: ds.keep"}}`
			}
			return 200, `{"jobComplete":true}`
		}
		emu2 := &listingEmulator{scriptEmulator: &scriptEmulator{run: emu.run}}
		var h http.Handler = Wrap(emu)
		if c.path == "/jobs" {
			emu2.scriptEmulator.tables = emu.tables
			h = Wrap(emu2)
		}
		do(t, h, "POST", base+c.path, queryBody(c.path, script))
		if strings.Join(sent, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s: sent\n%s\nwant\n%s", c.path, strings.Join(sent, "\n"), strings.Join(c.want, "\n"))
		}
	}
}
