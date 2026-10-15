//go:build compat

package compat

import (
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryScriptControlFlowMore (#1201): the rest of the scripts the
// front carries out itself: SET (a, b) and EXECUTE IMMEDIATE inside control
// flow, a FOR over many rows, @@error's stack trace in BigQuery's forms, a
// script ending in SET (no rows: the last statement executed has none), and
// a query job whose last statement changes data keeping its DML row count.
// Expected values follow BigQuery's procedural-language reference and
// multi-statement-queries page.
func TestBigQueryScriptControlFlowMore(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := "`" + ds.DatasetID + ".flow2`"
	if _, err := scriptRows(ctx, c, "CREATE TABLE "+tbl+" (n INT64)", false); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, sql string
		want      [][]bigquery.Value
	}{
		{"SET (a, b) in IF", "DECLARE a INT64; DECLARE b STRING; IF TRUE THEN SET (a, b) = (1 + 1, CONCAT('x', 'y')); END IF; SELECT a, b",
			[][]bigquery.Value{{int64(2), "xy"}}},
		{"SET (a, b) from a query", "DECLARE a INT64; DECLARE b INT64; IF TRUE THEN SET (a, b) = (SELECT AS STRUCT MIN(n), MAX(n) FROM UNNEST([3, 9, 5]) AS n); END IF; SELECT a, b",
			[][]bigquery.Value{{int64(3), int64(9)}}},
		{"EXECUTE IMMEDIATE in IF", "IF TRUE THEN EXECUTE IMMEDIATE CONCAT('SELECT ', '7', ' AS v'); END IF",
			[][]bigquery.Value{{int64(7)}}},
		{"EXECUTE IMMEDIATE INTO USING in a loop", "DECLARE i INT64 DEFAULT 0; DECLARE total INT64 DEFAULT 0; DECLARE x INT64; WHILE i < 3 DO SET i = i + 1; EXECUTE IMMEDIATE 'SELECT @v * 10' INTO x USING i AS v; SET total = total + x; END WHILE; SELECT total",
			[][]bigquery.Value{{int64(60)}}},
		{"FOR over many rows", "DECLARE total INT64 DEFAULT 0; FOR r IN (SELECT n FROM UNNEST(GENERATE_ARRAY(1, 300)) AS n) DO SET total = total + r.n; END FOR; SELECT total",
			[][]bigquery.Value{{int64(45150)}}},
		{"stack trace", "DECLARE x INT64;\nBEGIN\n  SELECT ERROR('boom');\nEXCEPTION WHEN ERROR THEN\n  SELECT @@error.formatted_stack_trace AS f, ARRAY_LENGTH(@@error.stack_trace) AS n, @@error.stack_trace[OFFSET(0)].line AS l, @@error.stack_trace[OFFSET(0)].column AS c, @@error.stack_trace[OFFSET(0)].location IS NULL AS loc;\nEND",
			[][]bigquery.Value{{"At [3:3]", int64(1), int64(3), int64(3), true}}},
		{"ending in SET", "DECLARE x INT64; IF TRUE THEN SELECT 1 AS a; SET x = 2; END IF",
			nil},
	}
	for _, cs := range cases {
		for _, insert := range []bool{false, true} {
			got, err := scriptRows(ctx, c, cs.sql, insert)
			if err != nil {
				t.Errorf("%s (insert %v): %v", cs.name, insert, err)
				continue
			}
			if !reflect.DeepEqual(got, cs.want) {
				t.Errorf("%s (insert %v): %v, want %v", cs.name, insert, got, cs.want)
			}
		}
	}

	// A query job whose last statement changes data keeps its DML count.
	job, err := c.Query("DECLARE i INT64 DEFAULT 0; DELETE FROM " + tbl + " WHERE TRUE; WHILE i < 2 DO SET i = i + 1; END WHILE; IF i = 2 THEN INSERT INTO " + tbl + " (n) VALUES (1), (2), (3); END IF").Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := job.Wait(ctx)
	if err != nil || st.Err() != nil {
		t.Fatalf("the DML script: %v %v", err, st)
	}
	qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics)
	if !ok || qs.NumDMLAffectedRows != 3 {
		t.Errorf("the DML script's statistics: %+v", st.Statistics.Details)
	}
	if got, err := scriptRows(ctx, c, "SELECT COUNT(*) FROM "+tbl, false); err != nil || !reflect.DeepEqual(got, [][]bigquery.Value{{int64(3)}}) {
		t.Errorf("after the DML script: %v %v", got, err)
	}
	// A DML statement held back runs before what reads it.
	if got, err := scriptRows(ctx, c, "IF TRUE THEN INSERT INTO "+tbl+" (n) VALUES (4); END IF; IF (SELECT COUNT(*) FROM "+tbl+") = 4 THEN SELECT 'seen' AS s; END IF", false); err != nil || !reflect.DeepEqual(got, [][]bigquery.Value{{"seen"}}) {
		t.Errorf("a read after DML: %v %v", got, err)
	}

	// Still 501, with the reason: a transaction in such a script. Stored
	// procedures are TestBigQueryStoredProcedures (#1217).
	err = bqRun(ctx, c, "IF TRUE THEN BEGIN TRANSACTION; INSERT INTO "+tbl+" (n) VALUES (5); COMMIT TRANSACTION; END IF", false)
	wantReason(t, "a transaction in a block", err, 501, "notImplemented")
	if err != nil && !strings.Contains(err.Error(), "transaction") {
		t.Errorf("the transaction's 501 gives no reason: %v", err)
	}
}
