//go:build compat

package compat

import (
	"reflect"
	"sort"
	"testing"
)

// TestBigQueryNewColumn1827: a column a statement adds to a table is there
// for the next statement to select, as in BigQuery: "CREATE OR REPLACE
// TABLE" replaces the table with the new definition, and "ALTER TABLE ADD
// COLUMN" adds the column to the existing table
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language,
// read 2026-11-18). Each is checked by its own statement, by a script
// that selects the column in the same query, a TEMP table replaced in a
// script, and CREATE OR REPLACE TABLE ... AS SELECT.
//
// The engine on its own kept the old columns in its catalog after either
// (#1827): googlesqlite's catalog left a replaced table's analyzer handle
// as it was, and it answered ALTER TABLE as a no-op (googlesqlite patch
// 0104). Measured first with this test, it passed through the front before
// that patch too: the front names every table by project, dataset and
// name, so the replaced table's change table rebuilt the catalog, and it
// carries out ALTER TABLE itself. It holds the behaviour as the engine
// changes.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryNewColumn1827(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	exec := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rows := func(sql string, want ...string) {
		t.Helper()
		got := queryRows(t, ctx, c, "", "", sql)
		sort.Strings(got)
		if want == nil {
			want = []string{}
		}
		if len(got) == 0 {
			got = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %q, want %q", sql, got, want)
		}
	}
	columns := func(table string, want ...string) {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("tables.get %s: %v", table, err)
		}
		var got []string
		for _, f := range md.Schema {
			got = append(got, f.Name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s's columns: got %q, want %q", table, got, want)
		}
	}

	t.Run("create or replace", func(t *testing.T) {
		exec("CREATE TABLE " + p("replaced") + " (a INT64)")
		exec("INSERT INTO " + p("replaced") + " (a) VALUES (1)")
		rows("SELECT CAST(a AS STRING) FROM "+p("replaced"), "1")
		exec("CREATE OR REPLACE TABLE " + p("replaced") + " (a INT64, b STRING)")
		rows("SELECT b FROM " + p("replaced"))
		exec("INSERT INTO " + p("replaced") + " (a, b) VALUES (2, 'x')")
		rows("SELECT CONCAT(CAST(a AS STRING), b) FROM "+p("replaced"), "2x")
		columns("replaced", "a", "b")
		// A column the replacement leaves out is gone.
		exec("CREATE OR REPLACE TABLE " + p("replaced") + " (b STRING, c DATE)")
		rows("SELECT CONCAT(b, ':', CAST(c AS STRING)) FROM " + p("replaced"))
		if err := bqRun(ctx, c, "SELECT a FROM "+p("replaced"), false); err == nil {
			t.Errorf("SELECT a after it was replaced away: no error")
		}
	})

	t.Run("create or replace as select", func(t *testing.T) {
		exec("CREATE TABLE " + p("ctas") + " AS SELECT 1 AS a")
		rows("SELECT CAST(a AS STRING) FROM "+p("ctas"), "1")
		exec("CREATE OR REPLACE TABLE " + p("ctas") + " AS SELECT 2 AS a, 'y' AS b")
		rows("SELECT CONCAT(CAST(a AS STRING), b) FROM "+p("ctas"), "2y")
		columns("ctas", "a", "b")
	})

	t.Run("alter table add column", func(t *testing.T) {
		exec("CREATE TABLE " + p("altered") + " (a INT64)")
		exec("INSERT INTO " + p("altered") + " (a) VALUES (1)")
		exec("ALTER TABLE " + p("altered") + " ADD COLUMN b STRING")
		rows("SELECT CONCAT(CAST(a AS STRING), IFNULL(b, '-')) FROM "+p("altered"), "1-")
		exec("INSERT INTO " + p("altered") + " (a, b) VALUES (2, 'z')")
		rows("SELECT CONCAT(CAST(a AS STRING), IFNULL(b, '-')) FROM "+p("altered"), "1-", "2z")
		columns("altered", "a", "b")
	})

	t.Run("in one script", func(t *testing.T) {
		rows("CREATE TABLE "+p("scripted")+" (a INT64);\n"+
			"CREATE OR REPLACE TABLE "+p("scripted")+" (a INT64, b STRING);\n"+
			"INSERT INTO "+p("scripted")+" (a, b) VALUES (3, 's');\n"+
			"ALTER TABLE "+p("scripted")+" ADD COLUMN c INT64;\n"+
			"UPDATE "+p("scripted")+" SET c = 4 WHERE TRUE;\n"+
			"SELECT CONCAT(CAST(a AS STRING), b, CAST(c AS STRING)) FROM "+p("scripted"), "3s4")
	})

	t.Run("temp table replaced in a script", func(t *testing.T) {
		rows("CREATE TEMP TABLE t AS SELECT 1 AS a;\n"+
			"CREATE OR REPLACE TEMP TABLE t AS SELECT 2 AS a, 'w' AS b;\n"+
			"SELECT CONCAT(CAST(a AS STRING), b) FROM t", "2w")
	})
}
