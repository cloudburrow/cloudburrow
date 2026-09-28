package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestTableNamesAreQualified (#1015): each table name a query gives
// without a dataset is sent in its default dataset; CTEs, TEMP tables,
// table-valued functions, qualified names and what is not a table are
// left as they are. With no default dataset, one in the first statement
// is refused as BigQuery refuses it, and one in a later statement is sent
// in a dataset that does not exist.
func TestTableNamesAreQualified(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"SELECT * FROM t", "SELECT * FROM `ds.t`"},
		{"SELECT * FROM `t` WHERE a = 1", "SELECT * FROM `ds.t` WHERE a = 1"},
		{"SELECT * FROM ds2.t", ""},
		{"SELECT * FROM `p.ds2.t`", ""},
		{"SELECT 'FROM t' AS s", ""},
		{"WITH c AS (SELECT 1 AS a) SELECT * FROM c", ""},
		{"WITH c AS (SELECT * FROM t) SELECT * FROM c, u", "WITH c AS (SELECT * FROM `ds.t`) SELECT * FROM c, `ds.u`"},
		{"CREATE TEMP TABLE tt AS SELECT 1 AS a; SELECT * FROM tt", ""},
		{"SELECT EXTRACT(YEAR FROM d), a IS DISTINCT FROM b FROM t", "SELECT EXTRACT(YEAR FROM d), a IS DISTINCT FROM b FROM `ds.t`"},
		{"SELECT * FROM t1, t2 x JOIN t3 ON TRUE LEFT JOIN t4 USING (a)", "SELECT * FROM `ds.t1`, `ds.t2` x JOIN `ds.t3` ON TRUE LEFT JOIN `ds.t4` USING (a)"},
		{"SELECT * FROM t, UNNEST(t.arr) AS e", "SELECT * FROM `ds.t`, UNNEST(t.arr) AS e"},
		{"SELECT * FROM tvf(1)", ""},
		{"SELECT (SELECT COUNT(*) FROM t) AS n, b FROM u", "SELECT (SELECT COUNT(*) FROM `ds.t`) AS n, b FROM `ds.u`"},
		{"INSERT INTO t (a) VALUES (1)", "INSERT INTO `ds.t` (a) VALUES (1)"},
		{"INSERT t (a) SELECT a FROM u", "INSERT `ds.t` (a) SELECT a FROM `ds.u`"},
		{"UPDATE t SET a = 1 FROM u WHERE TRUE", "UPDATE `ds.t` SET a = 1 FROM `ds.u` WHERE TRUE"},
		{"DELETE t WHERE TRUE", "DELETE `ds.t` WHERE TRUE"},
		{"DELETE FROM t WHERE TRUE", "DELETE FROM `ds.t` WHERE TRUE"},
		{"MERGE t USING u ON t.a = u.a WHEN NOT MATCHED THEN INSERT (a) VALUES (u.a)",
			"MERGE `ds.t` USING `ds.u` ON t.a = u.a WHEN NOT MATCHED THEN INSERT (a) VALUES (u.a)"},
		{"TRUNCATE TABLE t", "TRUNCATE TABLE `ds.t`"},
		{"CREATE TABLE IF NOT EXISTS t (a INT64)", "CREATE TABLE IF NOT EXISTS `ds.t` (a INT64)"},
		// A view's query is not read in the default dataset (#1049).
		{"CREATE OR REPLACE VIEW v AS (SELECT a FROM ds2.t)", "CREATE OR REPLACE VIEW `ds.v` AS (SELECT a FROM ds2.t)"},
		{"CREATE VIEW IF NOT EXISTS v AS WITH c AS (SELECT 1 AS a) SELECT * FROM c", "CREATE VIEW IF NOT EXISTS `ds.v` AS WITH c AS (SELECT 1 AS a) SELECT * FROM c"},
		{"SELECT 1; CREATE VIEW v AS SELECT * FROM t", "SELECT 1; CREATE VIEW `ds.v` AS SELECT * FROM `" + noDefaultDataset + ".t`"},
		{"CREATE VIEW ds2.v AS SELECT 1 AS a; SELECT * FROM t", "CREATE VIEW ds2.v AS SELECT 1 AS a; SELECT * FROM `ds.t`"},
		{"DROP TABLE IF EXISTS t", "DROP TABLE IF EXISTS `ds.t`"},
		{"CREATE TABLE FUNCTION f(x INT64) AS (SELECT x AS y)", ""},
		{"CREATE TEMP FUNCTION f(x INT64) AS (x)", ""},
		{"BEGIN SELECT * FROM t; END", "BEGIN SELECT * FROM `ds.t`; END"},
		{"DECLARE n INT64 DEFAULT (SELECT COUNT(*) FROM t); SELECT n", "DECLARE n INT64 DEFAULT (SELECT COUNT(*) FROM `ds.t`); SELECT n"},
	} {
		want := c.want
		if want == "" {
			want = c.sql
		}
		got, changed, msg := qualifyTables(c.sql, "ds")
		if got != want || changed != (c.want != "") || msg != "" {
			t.Errorf("%q: %q %v %q, want %q", c.sql, got, changed, msg, want)
		}
	}

	// A CREATE VIEW's query that names a table without a dataset, in the
	// first statement, is refused whatever the default dataset (#1049).
	if _, _, msg := qualifyTables("CREATE VIEW v AS SELECT * FROM t", "ds"); !strings.Contains(msg, `Table "t"`) {
		t.Errorf("CREATE VIEW v in ds: %q", msg)
	}
	for _, sql := range []string{"CREATE VIEW ds.v AS SELECT * FROM t", "CREATE OR REPLACE VIEW ds.v AS (SELECT a FROM ds.u JOIN t USING (a))",
		"CREATE MATERIALIZED VIEW ds.v AS SELECT * FROM t"} {
		for _, dataset := range []string{"ds", ""} {
			if got, changed, msg := qualifyTables(sql, dataset); changed || got != sql ||
				msg != `Table "t" must be qualified with a dataset (e.g. dataset.table).`+viewBodyNote {
				t.Errorf("%q in %q: %q %v %q", sql, dataset, got, changed, msg)
			}
		}
	}

	// No default dataset.
	if _, _, msg := qualifyTables("SELECT * FROM t", ""); msg != `Table "t" must be qualified with a dataset (e.g. dataset.table).` {
		t.Errorf("no default dataset: %q", msg)
	}
	if got, _, msg := qualifyTables("SELECT 1; SELECT * FROM t", ""); msg != "" || got != "SELECT 1; SELECT * FROM `"+noDefaultDataset+".t`" {
		t.Errorf("no default dataset, a later statement: %q %q", got, msg)
	}
	if got, changed, msg := qualifyTables("SELECT * FROM ds.t", ""); changed || msg != "" || got != "SELECT * FROM ds.t" {
		t.Errorf("no default dataset, qualified: %q %q", got, msg)
	}
}

// TestQueriesAreSentQualified (#1015): through jobs.query and a query job,
// the emulator is sent the qualified text, and the job shows the client's;
// with no default dataset a query naming a table without one is refused,
// 400 for jobs.query and a failed job for jobs.insert.
func TestQueriesAreSentQualified(t *testing.T) {
	emu := &jobsEmulator{}
	h := Wrap(emu)
	code, got := do(t, h, "POST", base+"/queries", `{"query":"SELECT * FROM t","defaultDataset":{"datasetId":"ds"}}`)
	if code != 200 || emu.sentQuery(t) != "SELECT * FROM `ds.t`" {
		t.Errorf("jobs.query: %d %v, sent %q", code, got, emu.sentQuery(t))
	}
	id := got["jobReference"].(map[string]any)["jobId"].(string)
	if _, job := do(t, h, "GET", base+"/jobs/"+id, ""); job["configuration"].(map[string]any)["query"].(map[string]any)["query"] != "SELECT * FROM t" {
		t.Errorf("jobs.get of the jobs.query: %v", job)
	}
	code, got = do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"q1"},"configuration":{"query":{"query":"SELECT * FROM t","defaultDataset":{"datasetId":"ds"}}}}`)
	if code != 200 || emu.sentQuery(t) != "SELECT * FROM `ds.t`" || got["configuration"].(map[string]any)["query"].(map[string]any)["query"] != "SELECT * FROM t" {
		t.Errorf("jobs.insert: %d %v, sent %q", code, got, emu.sentQuery(t))
	}
	if _, job := do(t, h, "GET", base+"/jobs/q1", ""); job["configuration"].(map[string]any)["query"].(map[string]any)["query"] != "SELECT * FROM t" {
		t.Errorf("jobs.get of the query job: %v", job)
	}

	emu = &jobsEmulator{}
	h = Wrap(emu)
	if code, got := do(t, h, "POST", base+"/queries", `{"query":"SELECT * FROM t"}`); code != 400 || !strings.Contains(errMsg(got), "must be qualified") {
		t.Errorf("jobs.query with no default dataset: %d %v", code, got)
	}
	code, got = do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"q2"},"configuration":{"query":{"query":"SELECT * FROM t"}}}`)
	if reason, done := jobState(got); code != 200 || !done || reason != "invalid" {
		t.Errorf("jobs.insert with no default dataset: %d %v", code, got)
	}
	for _, l := range emu.log {
		if strings.Contains(l, "FROM t") {
			t.Errorf("the refused query was sent: %v", emu.log)
		}
	}
}

func errMsg(got map[string]any) string {
	e, _ := got["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

// TestTableDataListReadsTheWholeName (#1015): tabledata.list is answered
// from a query of dataset.table, with BigQuery's paging; selectedFields
// is 501; a table that does not exist is the emulator's to answer.
func TestTableDataListReadsTheWholeName(t *testing.T) {
	var sent []string
	emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base+"/datasets/ds/tables/t":
			_, _ = io.WriteString(w, `{"type":"TABLE"}`)
		case r.Method == http.MethodGet:
			http.Error(w, `{"error":{"code":404,"message":"table nope is not found"}}`, http.StatusNotFound)
		case r.URL.Path == base+"/queries":
			var q map[string]any
			_ = json.Unmarshal(b, &q)
			sent = append(sent, q["query"].(string)+" "+string(must(json.Marshal(q["formatOptions"]))))
			_, _ = io.WriteString(w, `{"jobReference":{"jobId":"x"},"rows":[{"f":[{"v":"1"}]},{"f":[{"v":"2"}]},{"f":[{"v":"3"}]}],"totalRows":"3"}`)
		}
	})
	h := Wrap(emu)
	rows := func(got map[string]any) []string {
		var out []string
		list, _ := got["rows"].([]any)
		for _, r := range list {
			out = append(out, r.(map[string]any)["f"].([]any)[0].(map[string]any)["v"].(string))
		}
		return out
	}
	for _, c := range []struct {
		query, rows, token string
	}{
		{"", "1,2,3", ""},
		{"?maxResults=2", "1,2", "2"},
		{"?maxResults=2&pageToken=2", "3", ""},
		{"?startIndex=1", "2,3", ""},
		{"?maxResults=5", "1,2,3", ""},
		{"?startIndex=9", "", ""},
	} {
		code, got := do(t, h, "GET", base+"/datasets/ds/tables/t/data"+c.query, "")
		token, _ := got["pageToken"].(string)
		if code != 200 || strings.Join(rows(got), ",") != c.rows || token != c.token || got["totalRows"] != "3" {
			t.Errorf("%s: %d %v", c.query, code, got)
		}
	}
	if len(sent) == 0 || sent[0] != "SELECT * FROM `ds.t` null" {
		t.Errorf("the emulator was sent %q", sent)
	}
	do(t, h, "GET", base+"/datasets/ds/tables/t/data?formatOptions.useInt64Timestamp=true", "")
	if last := sent[len(sent)-1]; last != "SELECT * FROM `ds.t` {\"useInt64Timestamp\":true}" {
		t.Errorf("formatOptions: sent %q", last)
	}
	if code, _ := do(t, h, "GET", base+"/datasets/ds/tables/t/data?selectedFields=a", ""); code != 501 {
		t.Errorf("selectedFields: %d", code)
	}
	if code, _ := do(t, h, "GET", base+"/datasets/ds/tables/t/data?maxResults=x", ""); code != 400 {
		t.Errorf("maxResults=x: %d", code)
	}
	if code, got := do(t, h, "GET", base+"/datasets/ds/tables/nope/data", ""); code != 404 {
		t.Errorf("no such table: %d %v", code, got)
	}
}

func must(b []byte, _ error) []byte { return b }

// TestTableDataListOfAnUnsharedID (#1015): when no other dataset has a
// table of the ID, tabledata.list is the emulator's own read, paged by the
// front; when one has, it is a query of dataset.table.
func TestTableDataListOfAnUnsharedID(t *testing.T) {
	for _, other := range []bool{false, true} {
		var log []string
		emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log = append(log, r.Method+" "+r.URL.RequestURI())
			switch p := strings.TrimPrefix(r.URL.Path, base); {
			case p == "/datasets":
				_, _ = io.WriteString(w, `{"datasets":[{"datasetReference":{"datasetId":"ds"}},{"datasetReference":{"datasetId":"ds2"}}]}`)
			case p == "/datasets/ds/tables/t" || p == "/datasets/ds2/tables/t" && other:
				_, _ = io.WriteString(w, `{"type":"TABLE"}`)
			case p == "/datasets/ds/tables/t/data":
				_, _ = io.WriteString(w, `{"rows":[{"f":[{"v":"1"}]},{"f":[{"v":"2"}]}],"totalRows":"2"}`)
			case p == "/queries":
				_, _ = io.WriteString(w, `{"jobReference":{"jobId":"x"},"rows":[{"f":[{"v":"q"}]}],"totalRows":"1"}`)
			default:
				http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			}
		})
		code, got := do(t, Wrap(emu), "GET", base+"/datasets/ds/tables/t/data?maxResults=1", "")
		rows, _ := got["rows"].([]any)
		first := ""
		if len(rows) > 0 {
			first = rows[0].(map[string]any)["f"].([]any)[0].(map[string]any)["v"].(string)
		}
		want, token := "1", "1"
		if other {
			want, token = "q", ""
		}
		if tok, _ := got["pageToken"].(string); code != 200 || len(rows) != 1 || first != want || tok != token {
			t.Errorf("another dataset's table %v: %d %v; sent %v", other, code, got, log)
		}
	}
}

// TestExecuteImmediateIsExpanded (#1011): EXECUTE IMMEDIATE of a string
// literal or a variable set to one is replaced by its statement, its
// parameters by the USING values; the rest is 501, and a parameter with
// no value 400.
func TestExecuteImmediateIsExpanded(t *testing.T) {
	for _, c := range []struct {
		sql, want string
		code      int
	}{
		{"EXECUTE IMMEDIATE 'SELECT 1 AS a'", "SELECT 1 AS a", 0},
		{"execute immediate \"SELECT 'x' AS s\";", "SELECT 'x' AS s;", 0},
		{"EXECUTE IMMEDIATE '''SELECT\n1'''", "SELECT\n1", 0},
		{`EXECUTE IMMEDIATE 'SELECT \'q\' AS s, "\x41" AS t'`, `SELECT 'q' AS s, "A" AS t`, 0},
		{`EXECUTE IMMEDIATE r'SELECT "\n" AS s'`, `SELECT "\n" AS s`, 0},
		{"EXECUTE IMMEDIATE 'SELECT @a + 1, @A' USING 1 AS a", "SELECT (1) + 1, (1)", 0},
		{"EXECUTE IMMEDIATE 'SELECT ?, ?' USING -2.5, 'x'", "SELECT (-2.5), ('x')", 0},
		{"EXECUTE IMMEDIATE 'SELECT @a, @@time_zone' USING @p AS a", "SELECT (@p), @@time_zone", 0},
		{"EXECUTE IMMEDIATE 'SELECT \"@a ?\"' USING 1 AS a", "SELECT \"@a ?\"", 0},
		{"DECLARE s STRING DEFAULT 'SELECT 2'; EXECUTE IMMEDIATE s", "DECLARE s STRING DEFAULT 'SELECT 2'; SELECT 2", 0},
		{"DECLARE s STRING; SET s = 'SELECT 3'; EXECUTE IMMEDIATE s", "DECLARE s STRING; SET s = 'SELECT 3'; SELECT 3", 0},
		{"BEGIN EXECUTE IMMEDIATE 'CREATE TABLE ds.t (x INT64)'; END", "BEGIN CREATE TABLE ds.t (x INT64); END", 0},
		{"DECLARE n INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT @v' USING n AS v", "DECLARE n INT64 DEFAULT 1; SELECT (n)", 0},
		{"SELECT 'EXECUTE IMMEDIATE x'", "", 0},
		{"EXECUTE IMMEDIATE CONCAT('SELECT ', '1')", "", 501},
		{"EXECUTE IMMEDIATE 'SELECT 1' INTO x", "", 501},
		{"DECLARE s STRING DEFAULT 'SELECT 2'; SET s = CONCAT(s, '1'); EXECUTE IMMEDIATE s", "", 501},
		{"DECLARE s STRING DEFAULT (SELECT 'x'); EXECUTE IMMEDIATE s", "", 501},
		{"EXECUTE IMMEDIATE 'SELECT @a' USING 1 + 1 AS a", "", 501},
		{"EXECUTE IMMEDIATE 'SELECT 1; SELECT 2'", "", 501},
		{"EXECUTE IMMEDIATE \"EXECUTE IMMEDIATE 'SELECT 1'\"", "", 501},
		{"EXECUTE IMMEDIATE b'SELECT 1'", "", 501},
		{"EXECUTE IMMEDIATE 'SELECT @b' USING 1 AS a", "", 400},
		{"EXECUTE IMMEDIATE 'SELECT ?, ?' USING 1", "", 400},
		{"EXECUTE IMMEDIATE 'SELECT ?' USING 1, 2", "", 400},
		{"EXECUTE IMMEDIATE 'SELECT ?, @a' USING 1 AS a", "", 400},
		{"EXECUTE IMMEDIATE ''", "", 400},
	} {
		got, changed, code, msg := expandExecuteImmediate(c.sql)
		want := c.want
		if want == "" {
			want = c.sql
		}
		if code != c.code || got != want || changed != (c.want != "") || (code != 0) != (msg != "") {
			t.Errorf("%q: %q %v %d %q, want %q %d", c.sql, got, changed, code, msg, want, c.code)
		}
	}

	// Through the front: the statement is run, 501s run nothing, and the
	// job shows the client's text.
	emu := &jobsEmulator{}
	h := Wrap(emu)
	code, got := do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"e1"},"configuration":{"query":{"query":"EXECUTE IMMEDIATE 'CREATE TABLE ds.t (x INT64)'"}}}`)
	if code != 200 || emu.sentQuery(t) != "CREATE TABLE ds.t (x INT64)" ||
		got["configuration"].(map[string]any)["query"].(map[string]any)["query"] != "EXECUTE IMMEDIATE 'CREATE TABLE ds.t (x INT64)'" {
		t.Errorf("jobs.insert: %d %v, sent %q", code, got, emu.sentQuery(t))
	}
	n := len(emu.log)
	for _, path := range []string{"/queries", "/jobs"} {
		body := `{"query":"CREATE TABLE ds.u (x INT64); EXECUTE IMMEDIATE CONCAT('SELECT ', '1')"}`
		if path == "/jobs" {
			body = `{"configuration":{"query":` + body + `}}`
		}
		if code, got := do(t, h, "POST", base+path, body); code != 501 || got["error"].(map[string]any)["status"] != "UNIMPLEMENTED" {
			t.Errorf("%s: %d %v", path, code, got)
		}
	}
	if len(emu.log) != n {
		t.Errorf("a 501 sent %v", emu.log[n:])
	}
}

// TestSchemaUpdates (#1010, #1013): a tables.patch or tables.update whose
// schema adds columns makes the table again with them and its rows before
// it is sent on; what BigQuery refuses is 400 and what CloudBurrow does
// not carry out 501, both sending nothing; the rest is sent as it is.
func TestSchemaUpdates(t *testing.T) {
	const old = `{"fields":[{"name":"id","type":"INTEGER","mode":"REQUIRED"},{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"}]}]}`
	setup := func() *stateEmulator {
		e := newStateEmulator()
		e.datasets["ds"] = true
		e.tables["ds.t"] = `{"type":"TABLE","description":"d","labels":{"k":"v"},"creationTime":"123","schema":` + old + `}`
		e.rows["ds.t"] = 2
		return e
	}
	schema := func(extra string) string {
		return `{"schema":{"fields":[{"name":"id","type":"INT64"},{"name":"r","type":"STRUCT","fields":[{"name":"x","type":"STRING"}]}` + extra + `]}}`
	}
	for _, method := range []string{"PATCH", "PUT"} {
		// Columns added: remade with its rows and settings, then sent,
		// by the legacy type names (#1034).
		e := setup()
		body := schema(`,{"name":"g","type":"STRING"},{"name":"h","type":"STRING","mode":"REPEATED"}`)
		if code, got := do(t, Wrap(e), method, base+"/datasets/ds/tables/t", body); code != 200 {
			t.Fatalf("%s adding columns: %d %v", method, code, got)
		}
		if e.rows["ds.t"] != 2 || !strings.Contains(e.tables["ds.t"], `"name":"g"`) {
			t.Errorf("%s: the table is %s with %d rows", method, e.tables["ds.t"], e.rows["ds.t"])
		}
		if !e.sent(`^POST \S+/datasets/ds/tables .*"description":"d".*"labels":\{"k":"v"\}.*"tableId":"t"`) ||
			!e.sent(`^PATCH \S+/tables/t \{"creationTime":"123"\}`) ||
			!strings.HasPrefix(e.log[len(e.log)-1], method+" ") ||
			!strings.HasSuffix(e.log[len(e.log)-1], strings.NewReplacer(`"INT64"`, `"INTEGER"`, `"STRUCT"`, `"RECORD"`).Replace(sortedJSON(body))) {
			t.Errorf("%s: sent %v", method, e.log)
		}
		for k := range e.tables {
			if strings.Contains(k, "_cloudburrow_") {
				t.Errorf("%s: a scratch table was left: %s", method, k)
			}
		}

		// Sent as it is: a mode relaxed, a description, no schema; and
		// the same schema by its GoogleSQL type names, sent by their
		// legacy names (#1034).
		e = setup()
		if code, got := do(t, Wrap(e), method, base+"/datasets/ds/tables/t", schema("")); code != 200 || e.sent(`^POST`) ||
			!strings.HasSuffix(e.log[len(e.log)-1], `{"schema":{"fields":[{"name":"id","type":"INTEGER"},{"fields":[{"name":"x","type":"STRING"}],"name":"r","type":"RECORD"}]}}`) {
			t.Errorf("%s by GoogleSQL type names: %d %v, sent %v", method, code, got, e.log)
		}
		for _, body := range []string{`{"description":"new"}`,
			`{"schema":{"fields":[{"name":"id","type":"INTEGER","mode":"REQUIRED","description":"x"},{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"}]}]}}`} {
			e := setup()
			code, got := do(t, Wrap(e), method, base+"/datasets/ds/tables/t", body)
			last := e.log[len(e.log)-1]
			// A tables.patch's description is sent as the tables.update
			// of the whole table that carries it out (#1009, #1054).
			sentAs := strings.HasSuffix(last, body)
			if method == "PATCH" && strings.Contains(body, "description") && !strings.Contains(body, "schema") {
				sentAs = strings.HasPrefix(last, "PUT ") && strings.Contains(last, `"description":"new","labels":{"k":"v"}`)
			}
			if code != 200 || e.sent(`^POST`) || !sentAs || len(e.log) > 2 {
				t.Errorf("%s %s: %d %v, sent %v", method, body, code, got, e.log)
			}
		}

		// Refused, nothing changed.
		for _, c := range []struct {
			body, reason, msg string
			code              int
		}{
			{`{"schema":{"fields":[{"name":"id","type":"INTEGER"}]}}`, "invalid", "Field r is missing in new schema", 400},
			{`{"schema":{"fields":[{"name":"id","type":"STRING"},{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"}]}]}}`,
				"invalid", "Field id has changed type from INTEGER to STRING", 400},
			{`{"schema":{"fields":[{"name":"id","type":"INTEGER","mode":"REPEATED"},{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"}]}]}}`,
				"invalid", "Field id has changed mode from REQUIRED to REPEATED", 400},
			{`{"schema":{"fields":[{"name":"id","type":"INTEGER"},{"name":"r","type":"RECORD","fields":[]}]}}`, "invalid", "", 400},
			{`{"schema":{"fields":[{"name":"id","type":"INTEGER"},{"name":"r","type":"RECORD","fields":[{"name":"y","type":"STRING"}]}]}}`,
				"invalid", "Field r.x is missing in new schema", 400},
			{schema(`,{"name":"g","type":"STRING","mode":"REQUIRED"}`), "invalid", "Cannot add required fields", 400},
			{`{"schema":{"fields":[{"name":"id","type":"INTEGER"},{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"},{"name":"y","type":"STRING"}]}]}}`,
				"notImplemented", "adds a field to the RECORD r", 501},
			{`{"schema":{"fields":[{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"}]},{"name":"id","type":"INTEGER"}]}}`,
				"notImplemented", "another order", 501},
			{`{"schema":{"fields":[{"name":"g","type":"STRING"},{"name":"id","type":"INTEGER"},{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"}]}]}}`,
				"notImplemented", "another order", 501},
			{`{"schema":{"fields":[{"name":"ID","type":"INTEGER"},{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"}]}]}}`,
				"notImplemented", "renames the column id to ID", 501},
		} {
			e := setup()
			code, got := do(t, Wrap(e), method, base+"/datasets/ds/tables/t", c.body)
			reason := ""
			if errs, _ := got["error"].(map[string]any)["errors"].([]any); len(errs) > 0 {
				reason, _ = errs[0].(map[string]any)["reason"].(string)
			}
			if code != c.code || reason != c.reason || !strings.Contains(errMsg(got), c.msg) || len(e.log) > 1 {
				t.Errorf("%s %s: %d %v, sent %v; want %d %s %q", method, c.body, code, got, e.log, c.code, c.reason, c.msg)
			}
		}
	}

	// One tables.patch that adds a column and changes the labels: the
	// table is read once, made again with the column, and the patch sent
	// as the update of the whole table, its labels merged (#1054).
	e := setup()
	body := `{"labels":{"k":null,"n":"1"},` + schema(`,{"name":"g","type":"STRING"}`)[1:]
	if code, got := do(t, Wrap(e), "PATCH", base+"/datasets/ds/tables/t", body); code != 200 {
		t.Fatalf("adding a column and labels: %d %v", code, got)
	}
	gets := 0
	for _, l := range e.log {
		if strings.HasPrefix(l, "GET ") && strings.HasSuffix(strings.TrimSpace(l), "/tables/t") {
			gets++
		}
	}
	if last := e.log[len(e.log)-1]; gets != 1 || !strings.HasPrefix(last, "PUT ") || !strings.Contains(last, `"labels":{"n":"1"}`) ||
		!strings.Contains(last, `"name":"g"`) || !strings.Contains(e.tables["ds.t"], `"name":"g"`) || e.rows["ds.t"] != 2 {
		t.Errorf("adding a column and labels: %d reads, sent %v; the table is %s", gets, e.log, e.tables["ds.t"])
	}

	// A copy that fails before the table is deleted leaves it as it was.
	e = setup()
	e.fail = regexp.MustCompile(`^INSERT`)
	code, got := do(t, Wrap(e), "PATCH", base+"/datasets/ds/tables/t", schema(`,{"name":"g","type":"STRING"}`))
	if code != 501 || e.rows["ds.t"] != 2 || strings.Contains(e.tables["ds.t"], `"name":"g"`) {
		t.Errorf("a failed copy: %d %v; the table is %s with %d rows", code, got, e.tables["ds.t"], e.rows["ds.t"])
	}

	// A view's schema other than its descriptions: 501.
	e = setup()
	e.tables["ds.v"] = `{"type":"VIEW","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`
	if code, _ := do(t, Wrap(e), "PATCH", base+"/datasets/ds/tables/v", `{"schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}}`); code != 501 {
		t.Errorf("a view: %d", code)
	}
}

// TestAlterTableAdviceIsTrue (#1013): ALTER TABLE's 501 points to
// tables.patch and tables.update, which now add columns.
func TestAlterTableAdviceIsTrue(t *testing.T) {
	v := checkDDL("ALTER TABLE ds.t ADD COLUMN c STRING")
	if v.code != 501 || !strings.Contains(v.msg, "Add columns to a table") || strings.Contains(v.msg, "which the emulator applies") {
		t.Errorf("ALTER TABLE: %d %q", v.code, v.msg)
	}
}

// sortedJSON is b re-encoded, its keys sorted, as the front sends a body
// it rewrote.
func sortedJSON(b string) string {
	var v any
	if json.Unmarshal([]byte(b), &v) != nil {
		return b
	}
	out, _ := json.Marshal(v)
	return string(out)
}
