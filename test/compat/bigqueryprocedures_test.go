//go:build compat

package compat

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryStoredProcedures (#1217): CREATE [OR REPLACE] PROCEDURE [IF
// NOT EXISTS] with IN, OUT and INOUT arguments and OPTIONS, CALL, DROP
// PROCEDURE, through jobs.query and query jobs; an error in a procedure
// reaching the caller's EXCEPTION handler with its frame in @@error; the
// maximum call depth of 50; and routines.list, routines.get and
// routines.delete showing and dropping a procedure. Expected values follow
// BigQuery's procedural-language and DDL references.
func TestBigQueryStoredProcedures(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	tbl := "`" + d + ".proc_rows`"
	if _, err := scriptRows(ctx, c, "CREATE TABLE "+tbl+" (n INT64)", false); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		// A procedure that writes a row, made by one kind of request and
		// called by the other.
		if _, err := scriptRows(ctx, c, "CREATE OR REPLACE PROCEDURE "+d+".add_row(v INT64) OPTIONS(description = 'adds a row') "+
			"BEGIN INSERT INTO "+tbl+" (n) VALUES (v); END", insert); err != nil {
			t.Fatalf("CREATE PROCEDURE (insert %v): %v", insert, err)
		}
		if _, err := scriptRows(ctx, c, "CALL "+d+".add_row(40 + 2)", !insert); err != nil {
			t.Fatalf("CALL (insert %v): %v", !insert, err)
		}
	}
	if got, err := scriptRows(ctx, c, "SELECT COUNT(*), SUM(n) FROM "+tbl, false); err != nil || !reflect.DeepEqual(got, [][]bigquery.Value{{int64(2), int64(84)}}) {
		t.Errorf("after two CALLs: %v %v", got, err)
	}

	if _, err := scriptRows(ctx, c, "CREATE PROCEDURE "+d+".math(IN x INT64, OUT doubled INT64, INOUT acc STRING) "+
		"BEGIN DECLARE k INT64 DEFAULT 2; SET doubled = x * k; SET acc = CONCAT(acc, '+', CAST(x AS STRING)); "+
		"IF x > 100 THEN RETURN; END IF; SET acc = CONCAT(acc, '!'); END", false); err != nil {
		t.Fatal(err)
	}
	if _, err := scriptRows(ctx, c, "CREATE PROCEDURE "+d+".fails(msg STRING) BEGIN SELECT 1;\nRAISE USING MESSAGE = msg; END", false); err != nil {
		t.Fatal(err)
	}
	if _, err := scriptRows(ctx, c, "CREATE PROCEDURE "+d+".last() BEGIN SELECT 'from the procedure' AS s; END", false); err != nil {
		t.Fatal(err)
	}
	if _, err := scriptRows(ctx, c, "CREATE PROCEDURE "+d+".deep(n INT64) BEGIN CALL "+d+".deep(n + 1); END", false); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, sql string
		want      [][]bigquery.Value
	}{
		{"IN, OUT and INOUT", "DECLARE d INT64; DECLARE a STRING DEFAULT 'a'; CALL " + d + ".math(3 + 4, d, a); CALL " + d + ".math(200, d, a); SELECT d, a",
			[][]bigquery.Value{{int64(400), "a+7!+200"}}},
		{"no access to the caller's variables", "DECLARE k INT64 DEFAULT 99; DECLARE d INT64; DECLARE a STRING DEFAULT ''; CALL " + d + ".math(1, d, a); SELECT k, d",
			[][]bigquery.Value{{int64(99), int64(2)}}},
		{"the last statement is the procedure's", "CALL " + d + ".last()",
			[][]bigquery.Value{{"from the procedure"}}},
		{"an error reaches the caller's handler", "BEGIN\n  CALL " + d + ".fails('boom');\nEXCEPTION WHEN ERROR THEN\n  SELECT @@error.message AS m, ARRAY_LENGTH(@@error.stack_trace) AS n, " +
			"@@error.stack_trace[OFFSET(0)].location AS loc, @@error.stack_trace[OFFSET(0)].filename IS NULL AS nofile, @@error.stack_trace[OFFSET(1)].line AS l, " +
			"@@error.formatted_stack_trace AS f;\nEND",
			[][]bigquery.Value{{"boom", int64(2), project + "." + d + ".fails:2:1", true, int64(2),
				"At " + project + "." + d + ".fails[2:1]\nAt [2:3]"}}},
		{"the maximum call depth", "BEGIN CALL " + d + ".deep(1); EXCEPTION WHEN ERROR THEN SELECT STRPOS(@@error.message, '50') > 0 AS depth; END",
			[][]bigquery.Value{{true}}},
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

	// Errors.
	wantReason(t, "CALL of a procedure that does not exist", bqRun(ctx, c, "CALL "+d+".nope()", false), 404, "notFound")
	wantReason(t, "CREATE of a procedure that exists", bqRun(ctx, c, "CREATE PROCEDURE "+d+".last() BEGIN SELECT 2; END", false), 409, "duplicate")
	if _, err := scriptRows(ctx, c, "CREATE PROCEDURE IF NOT EXISTS "+d+".last() BEGIN SELECT 2; END", true); err != nil {
		t.Errorf("CREATE PROCEDURE IF NOT EXISTS of one that exists: %v", err)
	}
	if got, err := scriptRows(ctx, c, "CALL "+d+".last()", false); err != nil || !reflect.DeepEqual(got, [][]bigquery.Value{{"from the procedure"}}) {
		t.Errorf("IF NOT EXISTS replaced the procedure: %v %v", got, err)
	}
	wantReason(t, "CREATE PROCEDURE in a dataset that does not exist", bqRun(ctx, c, "CREATE PROCEDURE nodataset_1217.p() BEGIN SELECT 1; END", false), 404, "notFound")
	if err := bqRun(ctx, c, "CALL "+d+".fails('uncaught')", false); err == nil || !strings.Contains(err.Error(), "uncaught") {
		t.Errorf("an uncaught error in a procedure: %v", err)
	}
	wantReason(t, "OUT argument that is not a variable", bqRun(ctx, c, "DECLARE a STRING; CALL "+d+".math(1, 2, a)", false), 400, "invalidQuery")

	// routines.list, get and delete.
	r, err := c.Dataset(d).Routine("math").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get: %v", err)
	}
	if r.Type != "PROCEDURE" || r.Language != "SQL" || len(r.Arguments) != 3 || r.Arguments[1].Mode != "OUT" || r.Arguments[2].Mode != "INOUT" ||
		r.Arguments[0].DataType == nil || r.Arguments[0].DataType.TypeKind != "INT64" || !strings.HasPrefix(r.Body, "BEGIN DECLARE k INT64") {
		t.Errorf("routines.get: %+v", r)
	}
	if r, err := c.Dataset(d).Routine("add_row").Metadata(ctx); err != nil || r.Description != "adds a row" {
		t.Errorf("routines.get of add_row: %+v %v", r, err)
	}
	listed := map[string]string{}
	it := c.Dataset(d).Routines(ctx)
	for {
		rt, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("routines.list: %v", err)
		}
		m, err := rt.Metadata(ctx)
		if err != nil {
			t.Fatalf("routines.get of %s: %v", rt.RoutineID, err)
		}
		listed[rt.RoutineID] = m.Type
	}
	for _, id := range []string{"add_row", "math", "fails", "last", "deep"} {
		if listed[id] != "PROCEDURE" {
			t.Errorf("routines.list: %s is %q; listed %v", id, listed[id], listed)
		}
	}
	if err := c.Dataset(d).Routine("last").Delete(ctx); err != nil {
		t.Errorf("routines.delete: %v", err)
	}
	wantReason(t, "CALL after routines.delete", bqRun(ctx, c, "CALL "+d+".last()", false), 404, "notFound")
	if _, err := scriptRows(ctx, c, "DROP PROCEDURE "+d+".math", true); err != nil {
		t.Errorf("DROP PROCEDURE: %v", err)
	}
	if _, err := c.Dataset(d).Routine("math").Metadata(ctx); err == nil {
		t.Error("routines.get after DROP PROCEDURE found it")
	}
	wantReason(t, "DROP PROCEDURE of none", bqRun(ctx, c, "DROP PROCEDURE "+d+".math", false), 404, "notFound")
	if _, err := scriptRows(ctx, c, "DROP PROCEDURE IF EXISTS "+d+".math", false); err != nil {
		t.Errorf("DROP PROCEDURE IF EXISTS of none: %v", err)
	}

	// A transaction beside a CALL is carried out (#1201,
	// TestBigQueryScriptTransactions); a CALL inside one is 501.
	if err := bqRun(ctx, c, "CALL "+d+".add_row(1); BEGIN TRANSACTION; INSERT INTO "+tbl+" (n) VALUES (5); COMMIT TRANSACTION", false); err != nil {
		t.Errorf("a transaction beside a CALL: %v", err)
	}
	err = bqRun(ctx, c, "BEGIN TRANSACTION; CALL "+d+".add_row(1); COMMIT TRANSACTION", false)
	wantReason(t, "a CALL inside a transaction", err, 501, "notImplemented")
}
