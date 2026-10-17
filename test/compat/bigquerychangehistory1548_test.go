//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryChangeHistoryForUpdateColumn (#1548): BigQuery reserves the
// column name prefixes _CHANGE_TYPE, _CHANGE_TIMESTAMP and
// _CHANGE_SEQUENCE_NUMBER, not _CHANGE_IS_FOR_UPDATE, the name of
// CHANGES' flag of an update's DELETE, so a table may have a column of
// that name; its history is kept like any table's (googlesqlite patch
// 0065): APPENDS reads the column's values as they were inserted, and
// CHANGES each change, through an ALTER TABLE that makes the table
// again too. A column under a reserved prefix is still refused.
//
// covers: bigquery.tables.insert, bigquery.jobs.query
func TestBigQueryChangeHistoryForUpdateColumn(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	read := func(sql string) string {
		t.Helper()
		return strings.Join(changeRows(t, rows(t, ctx, c, sql)), "; ")
	}

	// Made through the API, with the column in its schema.
	tbl := ds.Table("flagged")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "_CHANGE_IS_FOR_UPDATE", Type: bigquery.BooleanFieldType},
	}}); err != nil {
		t.Fatalf("tables.insert with a column _CHANGE_IS_FOR_UPDATE: %v", err)
	}
	ref := "`" + ds.DatasetID + ".flagged`"
	run("ALTER TABLE " + ref + " SET OPTIONS (enable_change_history = TRUE)")
	run("INSERT INTO " + ref + " VALUES (1, TRUE), (2, FALSE)")
	run("UPDATE " + ref + " SET id = 3 WHERE id = 2")
	run("DELETE " + ref + " WHERE id = 1")

	if got := read("SELECT id, _CHANGE_IS_FOR_UPDATE, _CHANGE_TYPE FROM APPENDS(TABLE " + ref +
		", NULL, NULL) ORDER BY id"); got != "1 true INSERT; 2 false INSERT" {
		t.Errorf("APPENDS of a table with a column _CHANGE_IS_FOR_UPDATE: %q", got)
	}
	if got := read("SELECT id, _CHANGE_TYPE FROM CHANGES(TABLE " + ref + ", NULL, NULL) ORDER BY _CHANGE_TIMESTAMP, _CHANGE_TYPE DESC, id"); got != "1 INSERT; 2 INSERT; 3 UPDATE; 2 DELETE; 1 DELETE" {
		t.Errorf("CHANGES of a table with a column _CHANGE_IS_FOR_UPDATE: %q", got)
	}

	// Made again by an ALTER TABLE, it keeps its history.
	run("ALTER TABLE " + ref + " ADD COLUMN note STRING")
	if got := read("SELECT id, _CHANGE_IS_FOR_UPDATE, note FROM APPENDS(TABLE " + ref +
		", NULL, NULL) ORDER BY id"); got != "1 true <nil>; 2 false <nil>" {
		t.Errorf("APPENDS after ADD COLUMN: %q", got)
	}
	if got := read("SELECT COUNT(*) FROM CHANGES(TABLE " + ref + ", NULL, NULL)"); got != "5" {
		t.Errorf("CHANGES after ADD COLUMN: %q changes, want 5", got)
	}

	// A table made by DDL with the column keeps its history too.
	ddl := "`" + ds.DatasetID + ".flagged_ddl`"
	run("CREATE TABLE " + ddl + " (id INT64, _change_is_for_update STRING) OPTIONS (enable_change_history = TRUE)")
	run("INSERT INTO " + ddl + " VALUES (7, 'mine')")
	if got := read("SELECT id, _change_is_for_update, _CHANGE_TYPE FROM APPENDS(TABLE " + ddl + ", NULL, NULL)"); got != "7 mine INSERT" {
		t.Errorf("APPENDS of a DDL table with a column _change_is_for_update: %q", got)
	}

	// The change table's own flag is under a reserved prefix.
	if err := ds.Table("reserved").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "_CHANGE_TYPE_FOR_UPDATE", Type: bigquery.BooleanFieldType},
	}}); err == nil {
		t.Errorf("tables.insert with a column _CHANGE_TYPE_FOR_UPDATE: no error, want the reserved prefix refused")
	}
}
