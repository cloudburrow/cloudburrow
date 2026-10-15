//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryTableAndFunctionOfOneName (#1186): in BigQuery tables and
// routines are separate resources of a dataset (tables.* and routines.*),
// so a table may have the name of one of the dataset's functions, both
// answer, and deleting the one leaves the other (functions are dropped by
// DDL here: routines.get and routines.delete are 404 on the emulator,
// #1177). Before googlesqlite patch 0018 the table's CREATE panicked in the
// engine, and from then on every DROP on the instance panicked and the
// table functions were lost until a restart: the engine's catalog store
// kept the table and the function under one key.
func TestBigQueryTableAndFunctionOfOneName(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	run := func(sql string, insert bool) {
		t.Helper()
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	value := func(sql, want string) {
		t.Helper()
		if got := bqValue(ctx, c, sql); got != want {
			t.Errorf("%s = %s, want %s", sql, got, want)
		}
	}

	run("CREATE FUNCTION "+d+".twice(x INT64) RETURNS INT64 AS (x * 2)", false)
	run("CREATE TABLE FUNCTION "+d+".tf(n INT64) AS (SELECT n AS v)", false)
	// A table of the function's name through DDL, and one of another
	// function's name through tables.insert.
	run("CREATE TABLE "+d+".twice (n INT64)", false)
	run("INSERT INTO "+d+".twice (n) VALUES (7)", true)
	err := ds.Routine("thrice").Create(ctx, &bigquery.RoutineMetadata{
		Type:       "SCALAR_FUNCTION",
		Language:   "SQL",
		Body:       "x * 3",
		ReturnType: &bigquery.StandardSQLDataType{TypeKind: "INT64"},
		Arguments:  []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}},
	})
	if err != nil {
		t.Fatalf("routines.insert thrice: %v", err)
	}
	if err := ds.Table("thrice").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatalf("tables.insert thrice: %v", err)
	}
	value("SELECT "+d+".twice(3)", "[6]")
	value("SELECT n FROM "+d+".twice", "[7]")
	value("SELECT "+d+".thrice(3)", "[9]")
	value("SELECT COUNT(*) FROM "+d+".thrice", "[0]")
	if _, err := ds.Table("twice").Metadata(ctx); err != nil {
		t.Errorf("tables.get twice: %v", err)
	}

	// A DROP of another table rebuilds the engine's catalog.
	run("CREATE TABLE "+d+".other (a INT64)", false)
	run("DROP TABLE "+d+".other", false)
	value("SELECT * FROM "+d+".tf(4)", "[4]")
	value("SELECT "+d+".twice(5)", "[10]")
	value("SELECT n FROM "+d+".twice", "[7]")

	// Dropping the table leaves the function, and the function the table.
	run("DROP TABLE "+d+".twice", false)
	value("SELECT "+d+".twice(6)", "[12]")
	if got := bqValue(ctx, c, "SELECT n FROM "+d+".twice"); !strings.HasPrefix(got, "error:") {
		t.Errorf("a dropped table answered %s", got)
	}
	run("DROP FUNCTION "+d+".thrice", false)
	value("SELECT COUNT(*) FROM "+d+".thrice", "[0]")
	if got := bqValue(ctx, c, "SELECT "+d+".thrice(1)"); !strings.HasPrefix(got, "error:") {
		t.Errorf("a deleted function answered %s", got)
	}
	run("DROP FUNCTION "+d+".twice", false)
	value("SELECT * FROM "+d+".tf(8)", "[8]")
	if err := ds.Table("thrice").Delete(ctx); err != nil {
		t.Fatalf("tables.delete thrice: %v", err)
	}
	value("SELECT * FROM "+d+".tf(9)", "[9]")
}
