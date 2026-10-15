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
		v.head != "CREATE OR REPLACE VIEW IF NOT EXISTS `p.ds.v` " || v.columnOptions {
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
	if v, ok := parseViewColumnList("CREATE VIEW ds.v (c OPTIONS(description='x'), d) AS SELECT 1, 2"); !ok || !v.columnOptions ||
		strings.Join(v.columns, "|") != "c|d" {
		t.Errorf("a column's OPTIONS: %+v %v", v, ok)
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
