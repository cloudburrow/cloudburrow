//go:build compat

package compat

import (
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
// A script with a TEMP table and such a query is 501, nothing run.
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
	err := bqRun(ctx, c, "CREATE TEMP TABLE tt AS SELECT 1 AS a; SELECT * FROM tt; INSERT INTO "+d+".t VALUES (7)", false)
	if err == nil || !strings.Contains(err.Error(), "Not implemented here") {
		t.Errorf("a TEMP table and a query before the last statement: %v, want 501", err)
	}
	if got := count(); got != before {
		t.Errorf("%d rows after the refused script, want %d", got, before)
	}
}
