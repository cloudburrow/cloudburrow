//go:build compat

package compat

import (
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryScriptRuntimeErrorBeforeLast (#1249): a runtime error in a
// query that is not a script's last statement fails the
// script, as BigQuery fails it at that statement, keeping what the
// statements before it did. Measured first against the pinned emulator:
// `INSERT ...; SELECT 1/0; INSERT ...` answered done and kept both rows.
// A script with a TEMP table and such a query was 501, nothing run,
// until #1459.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryScriptRuntimeErrorBeforeLast(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	if err := bqRun(ctx, c, "CREATE TABLE "+d+".t (n INT64)", false); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	count := func() int64 {
		t.Helper()
		it, err := c.Query("SELECT COUNT(*) FROM " + d + ".t").Read(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil && err != iterator.Done {
			t.Fatalf("count: %v", err)
		}
		return row[0].(int64)
	}
	for _, insert := range []bool{false, true} {
		before := count()
		err := bqRun(ctx, c, "INSERT INTO "+d+".t VALUES (5); SELECT 1/0; INSERT INTO "+d+".t VALUES (6)", insert)
		if err == nil || !strings.Contains(err.Error(), "zero divided") {
			t.Errorf("insert=%v: a SELECT 1/0 before the last statement: %v, want the zero divided error", insert, err)
		}
		// The INSERT before it is kept, the one after it never ran.
		if got := count(); got != before+1 {
			t.Errorf("insert=%v: %d rows after the failed script, want %d", insert, got, before+1)
		}
	}
	if err := bqRun(ctx, c, "SELECT 1; SELECT 2", false); err != nil {
		t.Errorf("a script of two queries: %v", err)
	}
	before := count()
	// #1459: with a TEMP table too, the script run as its session's text
	// (tempsession.go): the query's error fails it, the INSERT before it
	// kept.
	err := bqRun(ctx, c, "CREATE TEMP TABLE tt AS SELECT 0 AS a; INSERT INTO "+d+".t VALUES (7); SELECT 1/a FROM tt; INSERT INTO "+d+".t VALUES (8)", false)
	if err == nil || !strings.Contains(err.Error(), "zero divided") {
		t.Errorf("a TEMP table and a failing query before the last statement: %v, want the zero divided error", err)
	}
	if got := count(); got != before+1 {
		t.Errorf("%d rows after the failed script, want %d", got, before+1)
	}
}

// TestBigQueryAssert (#1253): ASSERT expr [AS 'description'] fails the
// query when expr is FALSE or NULL, with "Assertion failed: " and the
// description, or the expression's text without one, alone or in a script
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/debugging-statements#assert).
// Measured first against the pinned emulator: ASSERT FALSE answered done
// and `ASSERT FALSE; SELECT 1` the row 1. A runtime error in its
// expression fails it with that error, a statement after a failed ASSERT
// does not run, what ran before it is kept, and a handler catches it.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryAssert(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	if err := bqRun(ctx, c, "CREATE TABLE "+d+".t (n INT64)", false); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	count := func() int64 { return rows(t, ctx, c, "SELECT COUNT(*) FROM "+d+".t")[0][0].(int64) }
	for _, insert := range []bool{false, true} {
		for sql, want := range map[string]string{
			"ASSERT FALSE":                                          "Assertion failed: FALSE",
			"ASSERT 1 > 2 AS 'one must be greater'":                 "Assertion failed: one must be greater",
			"ASSERT CAST(NULL AS BOOL) AS 'null fails'":             "Assertion failed: null fails",
			"ASSERT FALSE; SELECT 1":                                "Assertion failed: FALSE",
			"ASSERT (SELECT SUM(x) FROM (SELECT 1/0 AS x)) IS NULL": "zero divided",
			"IF TRUE THEN ASSERT (SELECT COUNT(*) > 10 FROM UNNEST([1, 2, 3])) AS 'Table must contain more than 10 rows.'; END IF": "Assertion failed: Table must contain more than 10 rows.",
		} {
			err := bqRun(ctx, c, sql, insert)
			var be *bigquery.Error
			if err == nil || !strings.Contains(err.Error(), want) || errors.As(err, &be) && be.Reason == "notImplemented" {
				t.Errorf("insert=%v: %s: %v, want it failed with %q", insert, sql, err, want)
			}
		}
		for _, sql := range []string{
			"ASSERT TRUE",
			"ASSERT EXISTS(SELECT x FROM UNNEST([7877, 7907]) AS x WHERE x = 7907) AS 'Column X must contain the value 7907.'",
			"DECLARE n INT64 DEFAULT 3; ASSERT n = 3",
		} {
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Errorf("insert=%v: %s: %v, want it to succeed", insert, sql, err)
			}
		}
		// What ran before the failed ASSERT is kept; what follows it never runs.
		before := count()
		err := bqRun(ctx, c, "INSERT INTO "+d+".t VALUES (1); ASSERT FALSE AS 'stop'; INSERT INTO "+d+".t VALUES (2)", insert)
		if err == nil || !strings.Contains(err.Error(), "Assertion failed: stop") {
			t.Errorf("insert=%v: an ASSERT between two INSERTs: %v", insert, err)
		}
		if got := count(); got != before+1 {
			t.Errorf("insert=%v: %d rows after the failed ASSERT, want %d", insert, got, before+1)
		}
	}
	if got := rows(t, ctx, c, "ASSERT TRUE; SELECT 2 AS a"); len(got) != 1 || got[0][0] != int64(2) {
		t.Errorf("ASSERT TRUE; SELECT 2: %v", got)
	}
	if got := rows(t, ctx, c, "BEGIN ASSERT FALSE AS 'caught'; EXCEPTION WHEN ERROR THEN SELECT @@error.message; END"); len(got) != 1 || got[0][0] != "Assertion failed: caught" {
		t.Errorf("a handler's @@error.message: %v", got)
	}
}
