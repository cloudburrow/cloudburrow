//go:build compat

package compat

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryScriptsUndropAndAlterSchema (#1548): UNDROP SCHEMA and
// ALTER SCHEMA … SET OPTIONS in a multi-statement query, each carried out
// when BigQuery runs it, in its turn
// (https://cloud.google.com/bigquery/docs/multi-statement-queries: a
// script's statements run in order, each a child job;
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#undrop_schema_statement,
// #alter_schema_set_options_statement):
//
//   - UNDROP SCHEMA inside IF, and in a script whose later statements read
//     the dataset it undeletes, through jobs.query and jobs.insert;
//   - IF NOT EXISTS of a dataset that exists does nothing, in a script;
//   - ALTER SCHEMA … SET OPTIONS in a script whose next statement reads
//     the option it set (INFORMATION_SCHEMA.SCHEMATA_OPTIONS), and inside
//     a loop;
//   - in a script with a TEMP table, run so too since #1562
//     (TestBigQueryTempScriptsUndropAndAlterSchema).
//
// Measured first: each script was 501 ("in a script with control flow",
// "whose other statements name the dataset", "ALTER SCHEMA … SET OPTIONS
// in a multi-statement query").
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.datasets.get, bigquery.datasets.delete
func TestBigQueryScriptsUndropAndAlterSchema(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	base := strings.ReplaceAll(h.Project(), "-", "_")
	read := func(sql string) []string {
		t.Helper()
		it, err := c.Query(sql).Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return readRows(t, it, sql)
	}
	run := func(sql string, insert bool) {
		t.Helper()
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("%s (insert %v): %v", sql, insert, err)
		}
	}
	for _, insert := range []bool{false, true} {
		ds := c.Dataset(fmt.Sprintf("%s_undrop_%v", base, insert))
		if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
			t.Fatalf("Dataset.Create: %v", err)
		}
		t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
		tbl := "`" + ds.DatasetID + ".t`"
		run("CREATE TABLE "+tbl+" (n INT64)", false)
		run("INSERT INTO "+tbl+" VALUES (1), (2)", false)

		// Inside IF, and read after.
		run("DROP SCHEMA "+ds.DatasetID+" CASCADE", false)
		run("DECLARE go BOOL DEFAULT TRUE; IF go THEN UNDROP SCHEMA "+ds.DatasetID+"; END IF; "+
			"CREATE TABLE `"+ds.DatasetID+".counted` AS SELECT COUNT(*) AS c FROM "+tbl, insert)
		if got := read("SELECT c FROM `" + ds.DatasetID + ".counted`"); !reflect.DeepEqual(got, []string{"2"}) {
			t.Errorf("insert %v: the count a script made after UNDROP SCHEMA in IF: %v, want [2]", insert, got)
		}
		// A branch not taken undeletes nothing.
		run("DROP SCHEMA "+ds.DatasetID+" CASCADE", false)
		run("IF FALSE THEN UNDROP SCHEMA "+ds.DatasetID+"; END IF; SELECT 1", insert)
		if _, err := ds.Metadata(ctx); err == nil {
			t.Errorf("insert %v: UNDROP SCHEMA in a branch not taken undeleted the dataset", insert)
		}
		// IF NOT EXISTS of one that exists, after it is undeleted.
		run("UNDROP SCHEMA "+ds.DatasetID+"; UNDROP SCHEMA IF NOT EXISTS "+ds.DatasetID+"; SELECT 1", insert)
		if got := read("SELECT n FROM " + tbl + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1", "2"}) {
			t.Errorf("insert %v: rows after UNDROP SCHEMA in a script: %v, want [1 2]", insert, got)
		}

		// ALTER SCHEMA … SET OPTIONS, read by the next statement.
		got := read("ALTER SCHEMA `" + ds.DatasetID + "` SET OPTIONS (description = 'set in a script'); " +
			"SELECT option_value FROM `" + ds.DatasetID + "`.INFORMATION_SCHEMA.SCHEMATA_OPTIONS WHERE option_name = 'description'")
		if !reflect.DeepEqual(got, []string{`"set in a script"`}) {
			t.Errorf("insert %v: the description the next statement read: %v", insert, got)
		}
		run("DECLARE i INT64 DEFAULT 0; WHILE i < 2 DO SET i = i + 1; "+
			"EXECUTE IMMEDIATE FORMAT(\"ALTER SCHEMA `%s` SET OPTIONS (labels = [('round', '%d')])\", '"+ds.DatasetID+"', i); END WHILE", insert)
		md, err := ds.Metadata(ctx)
		if err != nil {
			t.Fatalf("datasets.get: %v", err)
		}
		if md.Description != "set in a script" || md.Labels["round"] != "2" {
			t.Errorf("insert %v: after the scripts, description %q and labels %v, want 'set in a script' and round=2", insert, md.Description, md.Labels)
		}
		run("ALTER SCHEMA `"+ds.DatasetID+"` SET OPTIONS (description = 'plain'); SELECT 1", insert)
		if md, err := ds.Metadata(ctx); err != nil || md.Description != "plain" {
			t.Errorf("insert %v: description after ALTER SCHEMA; SELECT 1: %+v, %v", insert, md, err)
		}
	}
}
