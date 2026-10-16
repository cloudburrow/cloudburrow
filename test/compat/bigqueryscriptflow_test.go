//go:build compat

package compat

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// scriptRows runs a script through jobs.query (Query.Read) or as a query
// job (Query.Run, then Job.Read) and returns its answer's rows.
func scriptRows(ctx context.Context, c *bigquery.Client, sql string, insert bool) ([][]bigquery.Value, error) {
	qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var it *bigquery.RowIterator
	var err error
	if insert {
		var job *bigquery.Job
		if job, err = c.Query(sql).Run(qctx); err != nil {
			return nil, err
		}
		it, err = job.Read(qctx)
	} else {
		it, err = c.Query(sql).Read(qctx)
	}
	if err != nil {
		return nil, err
	}
	var out [][]bigquery.Value
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
}

// TestBigQueryScriptControlFlow (#1201): the emulator does not run the
// statements inside IF, LOOP, WHILE, REPEAT, FOR or a script CASE, nor a
// BEGIN ... EXCEPTION block's handler; the front carries such a script
// out itself. The answer is the last statement's result, through
// jobs.query and as a query job. Expected values follow BigQuery's
// procedural-language reference.
// Each construct is a subtest of its own.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.getQueryResults
func TestBigQueryScriptControlFlow(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := "`" + ds.DatasetID + ".flow`"
	if _, err := scriptRows(ctx, c, "CREATE TABLE "+tbl+" (n INT64)", false); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, sql string
		want      [][]bigquery.Value
	}{
		{"IF", "DECLARE x INT64 DEFAULT 3; IF x > 2 THEN SELECT 'big' AS s; ELSEIF x > 1 THEN SELECT 'mid' AS s; ELSE SELECT 'small' AS s; END IF",
			[][]bigquery.Value{{"big"}}},
		{"ELSEIF", "DECLARE x INT64 DEFAULT 2; IF x > 2 THEN SELECT 'big' AS s; ELSEIF x > 1 THEN SELECT 'mid' AS s; ELSE SELECT 'small' AS s; END IF",
			[][]bigquery.Value{{"mid"}}},
		{"ELSE", "DECLARE x INT64; IF x > 2 THEN SELECT 'big' AS s; ELSE SELECT 'null is not true' AS s; END IF",
			[][]bigquery.Value{{"null is not true"}}},
		{"WHILE", "DECLARE i INT64 DEFAULT 0; DECLARE total INT64 DEFAULT 0; WHILE i < 5 DO SET i = i + 1; SET total = total + i; END WHILE; SELECT i, total",
			[][]bigquery.Value{{int64(5), int64(15)}}},
		{"LOOP and BREAK", "DECLARE i INT64 DEFAULT 0; LOOP SET i = i + 1; IF i >= 4 THEN BREAK; END IF; END LOOP; SELECT i",
			[][]bigquery.Value{{int64(4)}}},
		{"CONTINUE", "DECLARE i INT64 DEFAULT 0; DECLARE odd INT64 DEFAULT 0; WHILE i < 6 DO SET i = i + 1; IF MOD(i, 2) = 0 THEN CONTINUE; END IF; SET odd = odd + 1; END WHILE; SELECT odd",
			[][]bigquery.Value{{int64(3)}}},
		{"labels, LEAVE and ITERATE", "DECLARE i INT64 DEFAULT 0; DECLARE j INT64 DEFAULT 0; outer_loop: LOOP SET i = i + 1; IF i > 3 THEN LEAVE outer_loop; END IF; inner_loop: LOOP SET j = j + 1; ITERATE outer_loop; END LOOP inner_loop; END LOOP outer_loop; SELECT i, j",
			[][]bigquery.Value{{int64(4), int64(3)}}},
		{"REPEAT", "DECLARE i INT64 DEFAULT 10; REPEAT SET i = i + 1; UNTIL i >= 3 END REPEAT; SELECT i",
			[][]bigquery.Value{{int64(11)}}},
		{"FOR ... IN", "DECLARE total INT64 DEFAULT 0; DECLARE names STRING DEFAULT ''; FOR r IN (SELECT n, CONCAT('n', CAST(n AS STRING)) AS name FROM UNNEST([1, 2, 3]) AS n ORDER BY n) DO SET total = total + r.n; SET names = CONCAT(names, r.name); END FOR; SELECT total, names",
			[][]bigquery.Value{{int64(6), "n1n2n3"}}},
		{"CASE", "DECLARE x STRING DEFAULT 'b'; CASE x WHEN 'a' THEN SELECT 1 AS v; WHEN 'b' THEN SELECT 2 AS v; ELSE SELECT 3 AS v; END CASE",
			[][]bigquery.Value{{int64(2)}}},
		{"searched CASE", "DECLARE x INT64 DEFAULT 7; CASE WHEN x < 5 THEN SELECT 'low' AS v; WHEN x < 10 THEN SELECT 'high' AS v; END CASE",
			[][]bigquery.Value{{"high"}}},
		{"block scope", "DECLARE x INT64 DEFAULT 1; BEGIN DECLARE y INT64 DEFAULT 10; SET x = x + y; END; IF TRUE THEN SELECT x; END IF",
			[][]bigquery.Value{{int64(11)}}},
		{"DML in a loop", "DECLARE i INT64 DEFAULT 0; DELETE FROM " + tbl + " WHERE TRUE; WHILE i < 3 DO SET i = i + 1; INSERT INTO " + tbl + " (n) VALUES (i); END WHILE; SELECT SUM(n) AS s, COUNT(*) AS c FROM " + tbl,
			[][]bigquery.Value{{int64(6), int64(3)}}},
		{"EXCEPTION", "BEGIN SELECT * FROM `" + ds.DatasetID + ".missing_flow`; SELECT 'not reached'; EXCEPTION WHEN ERROR THEN SELECT @@error.message LIKE '%missing_flow%' AS caught, @@error.statement_text LIKE 'SELECT * FROM%' AS stmt; END",
			[][]bigquery.Value{{true, true}}},
		{"RETURN", "SELECT 1 AS n; IF TRUE THEN RETURN; END IF; SELECT 2 AS n",
			[][]bigquery.Value{{int64(1)}}},
		{"RAISE caught", "BEGIN RAISE USING MESSAGE = CONCAT('bo', 'om'); EXCEPTION WHEN ERROR THEN SELECT @@error.message AS m; END",
			[][]bigquery.Value{{"boom"}}},
		{"EXCEPTION not raised", "BEGIN SELECT 1 AS n; EXCEPTION WHEN ERROR THEN SELECT 2 AS n; END",
			[][]bigquery.Value{{int64(1)}}},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
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
		})
	}

	// A failure no handler catches ends the script with its error; what
	// the statements before it did is kept.
	for _, insert := range []bool{false, true} {
		sql := "DELETE FROM " + tbl + " WHERE TRUE; IF TRUE THEN INSERT INTO " + tbl + " (n) VALUES (42); SELECT * FROM `" + ds.DatasetID + ".missing_flow`; END IF; SELECT 'not reached'"
		if _, err := scriptRows(ctx, c, sql, insert); err == nil {
			t.Errorf("an uncaught failure (insert %v): no error", insert)
		}
		if got, err := scriptRows(ctx, c, "SELECT n FROM "+tbl, false); err != nil || !reflect.DeepEqual(got, [][]bigquery.Value{{int64(42)}}) {
			t.Errorf("after an uncaught failure (insert %v), the table holds %v %v, want [[42]]", insert, got, err)
		}
	}
	// RAISE no handler catches ends the script with its message, also a
	// bare RAISE in a handler.
	for _, insert := range []bool{false, true} {
		for _, sql := range []string{
			"IF TRUE THEN RAISE USING MESSAGE = 'boom'; END IF",
			"BEGIN RAISE USING MESSAGE = 'boom'; EXCEPTION WHEN ERROR THEN RAISE; END",
		} {
			if err := bqRun(ctx, c, sql, insert); err == nil || !strings.Contains(err.Error(), "boom") {
				t.Errorf("%s (insert %v): %v, want the error boom", sql, insert, err)
			}
		}
	}
	// A CALL in a block runs the procedure (#1217, TestBigQueryStoredProcedures):
	// one that does not exist is notFound.
	wantReason(t, "CALL in a block", bqRun(ctx, c, "IF TRUE THEN CALL "+ds.DatasetID+".p(); END IF", false), 404, "notFound")
}
