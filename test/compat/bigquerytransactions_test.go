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
		"a ROLLBACK with none open": "ROLLBACK TRANSACTION",
		"a COMMIT with none open":   "COMMIT TRANSACTION",
		"DDL in a transaction":      "BEGIN TRANSACTION; CREATE TABLE " + ds.DatasetID + ".txn_ddl (n INT64); COMMIT TRANSACTION",
		"a TEMP function in one":    "BEGIN TRANSACTION; CREATE TEMP FUNCTION f(x INT64) AS (x); INSERT INTO " + tbl + " (n) VALUES (f(8)); COMMIT TRANSACTION",
		"a read of its writes":      "DECLARE k INT64; BEGIN TRANSACTION; INSERT INTO " + tbl + " (n) VALUES (9); SET k = (SELECT COUNT(*) FROM " + tbl + "); COMMIT TRANSACTION",
		"its TEMP table after it":   "BEGIN TRANSACTION; CREATE TEMP TABLE kept AS SELECT 1 AS a; COMMIT TRANSACTION; SELECT * FROM kept",
	} {
		_, err := scriptRows(ctx, c, sql, false)
		wantReason(t, name, err, 501, "notImplemented")
	}
	count("after the 501s", 2)
}

// TestBigQueryScriptTransactionsMore (#1201): variables, control flow,
// EXECUTE IMMEDIATE, CALL and TEMP tables inside a transaction, through
// jobs.query and query jobs, and BigQuery's own example of a transaction
// (https://cloud.google.com/bigquery/docs/transactions, "Example"): a TEMP
// table of new arrivals, a DELETE, a MERGE from the TEMP table, DROP TABLE,
// COMMIT, and a handler that rolls back. Expected values follow that page:
// a statement sees the transaction's earlier writes, COMMIT keeps them, an
// error goes to the handler with the transaction open, where COMMIT keeps
// what the statements before the error did and ROLLBACK discards it.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults
func TestBigQueryScriptTransactionsMore(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	tbl := "`" + d + ".txn_more`"
	for _, q := range []string{
		"CREATE TABLE " + tbl + " (n INT64)",
		"CREATE TABLE `" + d + ".Inventory` (product STRING, quantity INT64, supply_constrained BOOL)",
		"CREATE TABLE `" + d + ".NewArrivals` (product STRING, quantity INT64, warehouse STRING)",
		"CREATE PROCEDURE `" + d + ".add_n`(v INT64) BEGIN INSERT INTO " + tbl + " (n) VALUES (v); END",
	} {
		if _, err := scriptRows(ctx, c, q, false); err != nil {
			t.Fatal(err)
		}
	}
	rows := func(q string) [][]bigquery.Value {
		t.Helper()
		got, err := scriptRows(ctx, c, q, false)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return got
	}
	ns := func() [][]bigquery.Value { return rows("SELECT n FROM " + tbl + " ORDER BY n") }
	v := func(xs ...int64) [][]bigquery.Value {
		out := [][]bigquery.Value{}
		for _, x := range xs {
			out = append(out, []bigquery.Value{x})
		}
		return out
	}
	for _, insert := range []bool{false, true} {
		rows("DELETE FROM " + tbl + " WHERE TRUE")
		// DECLARE, SET, IF, WHILE, FOR, EXECUTE IMMEDIATE and CALL inside a
		// transaction, which COMMIT keeps.
		if _, err := scriptRows(ctx, c, "BEGIN TRANSACTION;\nDECLARE i INT64 DEFAULT 0;\nWHILE i < 3 DO\n  SET i = i + 1;\n"+
			"  IF MOD(i, 2) = 1 THEN INSERT INTO "+tbl+" (n) VALUES (i); END IF;\nEND WHILE;\n"+
			"FOR r IN (SELECT 10 AS x UNION ALL SELECT 20) DO INSERT INTO "+tbl+" (n) VALUES (r.x + i); END FOR;\n"+
			"EXECUTE IMMEDIATE 'INSERT INTO "+tbl+" (n) VALUES (?)' USING i * 100;\n"+
			"CALL `"+d+".add_n`(1000);\n"+
			"UPDATE "+tbl+" SET n = n + 1 WHERE n = 23;\nCOMMIT TRANSACTION", insert); err != nil {
			t.Fatalf("control flow (insert %v): %v", insert, err)
		}
		if got := ns(); !reflect.DeepEqual(got, v(1, 3, 13, 24, 300, 1000)) {
			t.Errorf("after control flow (insert %v): %v", insert, got)
		}
		// ROLLBACK inside an IF discards it all.
		if _, err := scriptRows(ctx, c, "DECLARE k INT64 DEFAULT 5;\nBEGIN TRANSACTION;\nDELETE FROM "+tbl+" WHERE TRUE;\n"+
			"IF k > 1 THEN ROLLBACK TRANSACTION; ELSE COMMIT TRANSACTION; END IF", insert); err != nil {
			t.Fatalf("ROLLBACK in an IF (insert %v): %v", insert, err)
		}
		if got := ns(); len(got) != 6 {
			t.Errorf("after ROLLBACK in an IF (insert %v): %v", insert, got)
		}
		// An error goes to the handler with the transaction open: COMMIT
		// there keeps what came before the error, and a read sees it.
		got, err := scriptRows(ctx, c, "BEGIN\n  BEGIN TRANSACTION;\n  DELETE FROM "+tbl+" WHERE n > 100;\n  SELECT 1/0;\n  INSERT INTO "+tbl+" (n) VALUES (7);\n"+
			"EXCEPTION WHEN ERROR THEN\n  INSERT INTO "+tbl+" (n) VALUES (2);\n  SELECT COUNT(*) FROM "+tbl+";\n  COMMIT TRANSACTION;\nEND;\n"+
			"SELECT COUNT(*) AS c FROM "+tbl, insert)
		if err != nil || !reflect.DeepEqual(got, v(5)) {
			t.Errorf("COMMIT in the handler (insert %v): %v %v", insert, got, err)
		}
		if got := ns(); !reflect.DeepEqual(got, v(1, 2, 3, 13, 24)) {
			t.Errorf("after COMMIT in the handler (insert %v): %v", insert, got)
		}
		// An error inside a block inside the transaction: its handler
		// runs, and the transaction goes on with the writes before it.
		if _, err := scriptRows(ctx, c, "BEGIN TRANSACTION;\nINSERT INTO "+tbl+" (n) VALUES (50);\n"+
			"BEGIN SELECT ERROR('x'); EXCEPTION WHEN ERROR THEN INSERT INTO "+tbl+" (n) VALUES (51); END;\nCOMMIT TRANSACTION", insert); err != nil {
			t.Fatalf("a handled error inside (insert %v): %v", insert, err)
		}
		if got := ns(); !reflect.DeepEqual(got, v(1, 2, 3, 13, 24, 50, 51)) {
			t.Errorf("after a handled error inside (insert %v): %v", insert, got)
		}

		// BigQuery's example.
		rows("DELETE FROM `" + d + ".Inventory` WHERE TRUE")
		rows("DELETE FROM `" + d + ".NewArrivals` WHERE TRUE")
		rows("INSERT INTO `" + d + ".Inventory` VALUES ('dryer', 30, false), ('washer', 10, false)")
		rows("INSERT INTO `" + d + ".NewArrivals` VALUES ('dryer', 200, 'warehouse #2'), ('oven', 300, 'warehouse #2'), ('washer', 100, 'warehouse #1')")
		example := "BEGIN\n\nBEGIN TRANSACTION;\n\n" +
			"CREATE TEMP TABLE tmp\n  AS SELECT * FROM " + d + ".NewArrivals WHERE warehouse = 'warehouse #2';\n\n" +
			"DELETE " + d + ".NewArrivals WHERE warehouse = 'warehouse #2';\n\n" +
			"MERGE " + d + ".Inventory AS I\nUSING tmp AS T\nON I.product = T.product\nWHEN NOT MATCHED THEN\n" +
			" INSERT(product, quantity, supply_constrained)\n VALUES(product, quantity, false)\nWHEN MATCHED THEN\n" +
			" UPDATE SET quantity = I.quantity + T.quantity;\n\nDROP TABLE tmp;\n\nCOMMIT TRANSACTION;\n\n" +
			"EXCEPTION WHEN ERROR THEN\n  SELECT @@error.message;\n  ROLLBACK TRANSACTION;\nEND;"
		if _, err := scriptRows(ctx, c, example, insert); err != nil {
			t.Fatalf("BigQuery's example (insert %v): %v", insert, err)
		}
		inv := rows("SELECT product, quantity, supply_constrained FROM `" + d + ".Inventory` ORDER BY product")
		want := [][]bigquery.Value{{"dryer", int64(230), false}, {"oven", int64(300), false}, {"washer", int64(10), false}}
		if !reflect.DeepEqual(inv, want) {
			t.Errorf("Inventory after the example (insert %v): %v, want %v", insert, inv, want)
		}
		if got := rows("SELECT product FROM `" + d + ".NewArrivals` ORDER BY product"); !reflect.DeepEqual(got, [][]bigquery.Value{{"washer"}}) {
			t.Errorf("NewArrivals after the example (insert %v): %v", insert, got)
		}
		// The example's handler: a failing MERGE rolls it all back.
		rows("INSERT INTO `" + d + ".NewArrivals` VALUES ('oven', 1, 'warehouse #2')")
		failing := strings.Replace(example, "I.quantity + T.quantity", "I.quantity + T.quantity + ERROR('boom')", 1)
		if _, err := scriptRows(ctx, c, failing, insert); err != nil {
			t.Fatalf("the example's handler (insert %v): %v", insert, err)
		}
		if got := rows("SELECT COUNT(*) FROM `" + d + ".NewArrivals`"); !reflect.DeepEqual(got, v(2)) {
			t.Errorf("NewArrivals after the handler's ROLLBACK (insert %v): %v", insert, got)
		}
		rows("DELETE FROM `" + d + ".NewArrivals` WHERE warehouse = 'warehouse #2'")
	}
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
