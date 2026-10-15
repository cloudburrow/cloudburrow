//go:build compat

package compat

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
)

// TestBigQueryScriptTransactions (#1201): BEGIN TRANSACTION, COMMIT
// TRANSACTION and ROLLBACK TRANSACTION in a script, through jobs.query and
// query jobs. Expected values follow BigQuery's multi-statement
// transactions page (https://cloud.google.com/bigquery/docs/transactions):
// a statement sees the transaction's earlier writes, COMMIT keeps them,
// ROLLBACK, an error and the end of the query discard them, an error goes to
// the exception handler, and transactions cannot be nested.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults
func TestBigQueryScriptTransactions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := "`" + ds.DatasetID + ".txn_rows`"
	if _, err := scriptRows(ctx, c, "CREATE TABLE "+tbl+" (n INT64)", false); err != nil {
		t.Fatal(err)
	}
	count := func(when string, want int64) {
		t.Helper()
		got, err := scriptRows(ctx, c, "SELECT COUNT(*) FROM "+tbl, false)
		if err != nil || !reflect.DeepEqual(got, [][]bigquery.Value{{want}}) {
			t.Errorf("%s: %v %v, want %d rows", when, got, err, want)
		}
	}
	for _, insert := range []bool{false, true} {
		if _, err := scriptRows(ctx, c, "DELETE FROM "+tbl+" WHERE TRUE", false); err != nil {
			t.Fatal(err)
		}
		// COMMIT keeps the writes; a statement sees the earlier ones; a
		// variable is read in the transaction.
		if _, err := scriptRows(ctx, c, "DECLARE x INT64 DEFAULT 1;\nBEGIN TRANSACTION;\nINSERT INTO "+tbl+" (n) VALUES (x), (x + 1);\n"+
			"UPDATE "+tbl+" SET n = n * 10 WHERE n = 2;\nSELECT ERROR('not seen') FROM "+tbl+" WHERE n = 2;\nCOMMIT TRANSACTION", insert); err != nil {
			t.Fatalf("COMMIT (insert %v): %v", insert, err)
		}
		if got, err := scriptRows(ctx, c, "SELECT n FROM "+tbl+" ORDER BY n", false); err != nil ||
			!reflect.DeepEqual(got, [][]bigquery.Value{{int64(1)}, {int64(20)}}) {
			t.Errorf("after COMMIT (insert %v): %v %v", insert, got, err)
		}
		// ROLLBACK discards them, and the script goes on.
		got, err := scriptRows(ctx, c, "BEGIN TRANSACTION; INSERT INTO "+tbl+" (n) VALUES (3); DELETE FROM "+tbl+" WHERE n = 1; "+
			"ROLLBACK TRANSACTION; SELECT COUNT(*) AS c FROM "+tbl, insert)
		if err != nil || !reflect.DeepEqual(got, [][]bigquery.Value{{int64(2)}}) {
			t.Errorf("ROLLBACK (insert %v): %v %v", insert, got, err)
		}
		// An error inside fails the query and rolls the transaction back,
		// a query's error too (SELECT 1/0, as in BigQuery's example).
		_, err = scriptRows(ctx, c, "BEGIN TRANSACTION; INSERT INTO "+tbl+" (n) VALUES (4); SELECT 1/0; COMMIT TRANSACTION", insert)
		if err == nil {
			t.Errorf("a failing transaction (insert %v) succeeded", insert)
		}
		count("after a failed transaction", 2)
		// The query ends before COMMIT: rolled back.
		if _, err := scriptRows(ctx, c, "BEGIN TRANSACTION; INSERT INTO "+tbl+" (n) VALUES (5)", insert); err != nil {
			t.Errorf("an unfinished transaction (insert %v): %v", insert, err)
		}
		count("after an unfinished transaction", 2)
		// BigQuery's example: the handler reads @@error.message and rolls
		// back.
		if _, err := scriptRows(ctx, c, "BEGIN\n  BEGIN TRANSACTION;\n  INSERT INTO "+tbl+" (n) VALUES (6);\n  SELECT 1/0;\n  COMMIT TRANSACTION;\n"+
			"EXCEPTION WHEN ERROR THEN\n  SELECT @@error.message;\n  ROLLBACK TRANSACTION;\nEND;", insert); err != nil {
			t.Errorf("the handler's ROLLBACK (insert %v): %v", insert, err)
		}
		count("after the handler's ROLLBACK", 2)
		got, err = scriptRows(ctx, c, "BEGIN BEGIN TRANSACTION; INSERT INTO "+tbl+" (n) VALUES (6); SELECT 1/0; COMMIT TRANSACTION; "+
			"EXCEPTION WHEN ERROR THEN ROLLBACK TRANSACTION; SELECT @@error.statement_text AS s; END", insert)
		if err != nil || !reflect.DeepEqual(got, [][]bigquery.Value{{"SELECT 1/0"}}) {
			t.Errorf("@@error.statement_text (insert %v): %v %v", insert, got, err)
		}
		// Nested: an error.
		if _, err := scriptRows(ctx, c, "BEGIN TRANSACTION; INSERT INTO "+tbl+" (n) VALUES (7); BEGIN TRANSACTION; COMMIT TRANSACTION", insert); err == nil ||
			!strings.Contains(err.Error(), "nested") {
			t.Errorf("a nested transaction (insert %v): %v", insert, err)
		}
		count("after a nested transaction", 2)
	}

	// What CloudBurrow cannot carry out exactly is 501, nothing run.
	for name, sql := range map[string]string{
		"a SET in a transaction":       "DECLARE x INT64; BEGIN TRANSACTION; SET x = 1; INSERT INTO " + tbl + " (n) VALUES (x); ROLLBACK TRANSACTION",
		"a block in a transaction":     "BEGIN TRANSACTION; IF TRUE THEN INSERT INTO " + tbl + " (n) VALUES (8); END IF; COMMIT TRANSACTION; IF FALSE THEN SELECT 1; END IF",
		"COMMIT in a handler":          "BEGIN BEGIN TRANSACTION; SELECT 1/0; COMMIT TRANSACTION; EXCEPTION WHEN ERROR THEN COMMIT TRANSACTION; END",
		"a ROLLBACK with none open":    "ROLLBACK TRANSACTION",
		"a read after a failure in it": "BEGIN BEGIN TRANSACTION; INSERT INTO " + tbl + " (n) VALUES (9); SELECT 1/0; COMMIT TRANSACTION; EXCEPTION WHEN ERROR THEN SELECT COUNT(*) FROM " + tbl + "; ROLLBACK TRANSACTION; END",
	} {
		_, err := scriptRows(ctx, c, sql, false)
		wantReason(t, name, err, 501, "notImplemented")
	}
	count("after the 501s", 2)
}

// TestBigQueryScriptRangeVariables (#1201): a script variable, and a FOR
// row, holding a RANGE<DATE>, RANGE<DATETIME> or RANGE<TIMESTAMP>, in a
// script the front carries out.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryScriptRangeVariables(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		got, err := scriptRows(ctx, c, "DECLARE r RANGE<DATE> DEFAULT RANGE(DATE '2020-01-01', NULL);\n"+
			"DECLARE s RANGE<TIMESTAMP> DEFAULT RANGE(TIMESTAMP '2020-01-01 10:00:00.5', TIMESTAMP '2020-01-02');\n"+
			"DECLARE d RANGE<DATETIME>;\nDECLARE n INT64 DEFAULT 0;\n"+
			"IF RANGE_END(r) IS NULL THEN SET r = RANGE(RANGE_START(r), DATE '2021-01-01'); END IF;\n"+
			"SET d = RANGE(DATETIME '2020-05-01 08:00:00', DATETIME '2020-05-02 00:00:00');\n"+
			"FOR row IN (SELECT RANGE(DATE '2020-01-01', DATE '2020-03-01') AS w UNION ALL SELECT RANGE(DATE '2020-02-01', NULL)) DO\n"+
			"  IF RANGE_CONTAINS(row.w, DATE '2020-02-15') THEN SET n = n + 1; END IF;\nEND FOR;\n"+
			"SELECT RANGE_START(r), RANGE_END(r), RANGE_START(s), RANGE_END(d), n", insert)
		if err != nil {
			t.Fatalf("insert %v: %v", insert, err)
		}
		want := []bigquery.Value{civil.Date{Year: 2020, Month: 1, Day: 1}, civil.Date{Year: 2021, Month: 1, Day: 1},
			time.Date(2020, 1, 1, 10, 0, 0, 500000000, time.UTC),
			civil.DateTime{Date: civil.Date{Year: 2020, Month: 5, Day: 2}}, int64(2)}
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Errorf("insert %v: %v, want %v", insert, got, want)
		}
	}
}
