package bigqueryfront

import (
	"strings"
	"testing"
)

// A lone CREATE VIEW with a column list is read, and its query rewritten
// to alias the query's columns in order (#1240).
func TestViewColumnList(t *testing.T) {
	v, ok := parseViewColumnList("CREATE OR REPLACE VIEW IF NOT EXISTS `p.ds.v` (c, `d e`) OPTIONS(description = 'AS') AS SELECT a, b FROM ds.t;")
	if !ok || strings.Join(v.path, ".") != "p.ds.v" || strings.Join(v.columns, "|") != "c|d e" || !v.ifNotExists ||
		v.options != " OPTIONS(description = 'AS')" || v.query != "SELECT a, b FROM ds.t" ||
		v.head != "CREATE OR REPLACE VIEW IF NOT EXISTS `p.ds.v` " || v.badOption != "" || v.hasDescriptions() {
		t.Fatalf("parsed %+v, %v", v, ok)
	}
	got, msg := v.aliasedQuery([]field{{Name: "a", Type: "INTEGER"}, {Name: "b", Type: "STRING"}})
	if want := "SELECT `_cloudburrow_q`.`a` AS `c`, `_cloudburrow_q`.`b` AS `d e` FROM (\nSELECT a, b FROM ds.t\n) AS `_cloudburrow_q`"; msg != "" || got != want {
		t.Errorf("named columns: %q %q", got, msg)
	}
	got, msg = v.aliasedQuery([]field{{Name: "$col1", Type: "INTEGER"}, {Name: "s", Type: "RECORD", Mode: "REPEATED",
		Fields: []field{{Name: "x", Type: "STRING"}}}})
	if want := "SELECT * FROM (SELECT CAST(NULL AS INT64) AS `c`, CAST(NULL AS ARRAY<STRUCT<`x` STRING>>) AS `d e` FROM (SELECT 1) " +
		"WHERE FALSE UNION ALL (\nSELECT a, b FROM ds.t\n))"; msg != "" || got != want {
		t.Errorf("an unnamed column: %q %q", got, msg)
	}
	if _, msg := v.aliasedQuery([]field{{Name: "a"}}); !strings.Contains(msg, "column list (2)") {
		t.Errorf("a count mismatch: %q", msg)
	}
	for _, sql := range []string{"CREATE VIEW ds.v (c, C) AS SELECT 1, 2", "CREATE VIEW ds.v (`c!`, d) AS SELECT 1, 2"} {
		v, _ := parseViewColumnList(sql)
		if _, msg := v.aliasedQuery([]field{{Name: "a"}, {Name: "b"}}); msg == "" {
			t.Errorf("%s was not refused", sql)
		}
	}
	// A column's description is read (#1323); another option is not set.
	if v, ok := parseViewColumnList("CREATE VIEW ds.v (c OPTIONS(description='x'), d) AS SELECT 1, 2"); !ok || v.badOption != "" ||
		strings.Join(v.columns, "|") != "c|d" || len(v.descriptions) != 2 || v.descriptions[0] == nil ||
		*v.descriptions[0] != "x" || v.descriptions[1] != nil || v.withoutList() != "CREATE VIEW ds.v  AS SELECT 1, 2" {
		t.Errorf("a column's OPTIONS: %+v %v", v, ok)
	}
	for _, sql := range []string{
		"CREATE VIEW ds.v (c OPTIONS(rounding_mode='x')) AS SELECT 1",
		"CREATE VIEW ds.v (c OPTIONS(description=CONCAT('a', 'b'))) AS SELECT 1",
	} {
		if v, ok := parseViewColumnList(sql); !ok || v.badOption == "" {
			t.Errorf("%s: %+v %v, want an option not set", sql, v, ok)
		}
	}
	for _, sql := range []string{
		"CREATE VIEW ds.v AS SELECT 1 AS a",
		"CREATE MATERIALIZED VIEW ds.v (c) AS SELECT 1",
		"SELECT 1; CREATE VIEW ds.v (c) AS SELECT 1",
		"CREATE TABLE ds.t (c INT64)",
		"CREATE VIEW ds.v (c)",
	} {
		if _, ok := parseViewColumnList(sql); ok {
			t.Errorf("%q was read as a lone CREATE VIEW with a column list", sql)
		}
	}
}

// A script with a view's column list is carried out a statement at a time
// (#1323), unless a TEMP table keeps it in one request, and its listed
// names are checked before anything runs.
func TestViewColumnListInAScript(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT 1; CREATE VIEW ds.v (c, d) AS SELECT 1, 2":                       true,
		"CREATE VIEW ds.v (c) AS SELECT 1; SELECT * FROM ds.v":                   true,
		"CREATE TEMP TABLE t AS SELECT 1 AS a; CREATE VIEW ds.v (c) AS SELECT 1": false,
		"SELECT 1; CREATE VIEW ds.v AS SELECT 1 AS c":                            false,
		"SELECT 'VIEW'; SELECT 2":                                                false,
	} {
		if got := hasControlFlow(sql); got != want {
			t.Errorf("hasControlFlow(%q) = %v, want %v", sql, got, want)
		}
	}
	for sql, code := range map[string]int{
		"SELECT 1; CREATE VIEW ds.v (c, d) AS SELECT 1, 2": 0,
		"SELECT 1; CREATE VIEW ds.v (c, C) AS SELECT 1, 2": 400,
		"SELECT 1; CREATE VIEW ds.v (`c!`) AS SELECT 1":    400,
	} {
		toks, _ := lex(sql)
		p := &scriptParser{toks: toks}
		nodes := p.block()
		if v := checkStatements(sql, nodes); v.code != code {
			t.Errorf("checkStatements(%q) = %d %s, want %d", sql, v.code, v.msg, code)
		}
	}
}

// A view's column list in a script with a TEMP table is put in the
// script's text, and a dry run of a lone one answers as BigQuery's (#1335).
func TestViewColumnListInATempScript(t *testing.T) {
	for sql, want := range map[string]int{
		"CREATE TEMP TABLE t AS SELECT 1 AS a; CREATE VIEW ds.v (c) AS SELECT 1":                                1,
		"CREATE TEMP TABLE t AS SELECT 1 AS a; BEGIN CREATE VIEW ds.v (c) AS SELECT 1; END":                     1,
		"CREATE TEMP FUNCTION f() AS (1); CREATE VIEW ds.v (c) AS SELECT f(); CREATE VIEW ds.w (d) AS SELECT 2": 2,
		"SELECT 1; CREATE VIEW ds.v (c) AS SELECT 1":                                                            0,
		"CREATE TEMP TABLE t AS SELECT 1 AS a; CREATE VIEW ds.v AS SELECT 1 AS c":                               0,
		"CREATE VIEW ds.v (c) AS SELECT 'TEMP'":                                                                 0,
	} {
		views, ok := tempScriptViewLists(sql)
		if len(views) != want || ok != (want > 0) {
			t.Errorf("tempScriptViewLists(%q) = %d, %v; want %d", sql, len(views), ok, want)
		}
		for _, v := range views {
			if got := sql[v.pos:v.end]; !strings.HasPrefix(got, "CREATE VIEW") || strings.HasSuffix(got, ";") {
				t.Errorf("tempScriptViewLists(%q) found %q", sql, got)
			}
		}
	}
	v, _ := parseViewColumnList("CREATE VIEW ds.v (c, `d e`) AS SELECT a, b FROM ds.t")
	if got, want := v.untypedQuery(), "SELECT * FROM (SELECT NULL AS `c`, NULL AS `d e` FROM (SELECT 1) WHERE FALSE UNION ALL (\nSELECT a, b FROM ds.t\n))"; got != want {
		t.Errorf("untypedQuery = %q, want %q", got, want)
	}
	job := string(dryViewAnswer([]byte(`{"kind":"bigquery#job","statistics":{"query":{"statementType":"SELECT","schema":{"fields":[]}}}}`)))
	if job != `{"kind":"bigquery#job","statistics":{"query":{"statementType":"CREATE_VIEW","totalBytesProcessed":"0"},"totalBytesProcessed":"0"}}` {
		t.Errorf("dryViewAnswer of a job = %s", job)
	}
	resp := string(dryViewAnswer([]byte(`{"jobComplete":true,"rows":[],"schema":{},"totalRows":"0"}`)))
	if resp != `{"jobComplete":true,"totalBytesProcessed":"0"}` {
		t.Errorf("dryViewAnswer of a jobs.query response = %s", resp)
	}
}
