//go:build compat

package compat

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryScriptChildFrames (#1569): every child job of a script, as
// jobs.list with parentJobId lists it, carries scriptStatistics with its
// evaluationKind and stackFrames, as BigQuery documents them
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#ScriptStatistics):
// the 1-based start and end lines and columns of its statement or
// expression in the script, and its text as written. So the first, a
// middle and the last statement, statements that read a variable, inside
// BEGIN, IF, an EXCEPTION handler, a loop and a transaction; a statement of
// a CALLed procedure has a frame in the procedure, with its procedureId,
// then the CALL's. Measured first: a statement that read a variable, the
// last one too, had no position, and the procedure's statement one frame.
//
// covers: bigquery.jobs.list
func TestBigQueryScriptChildFrames(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	tbl := "`" + ds.DatasetID + ".f1569`"
	proc := "`" + ds.DatasetID + ".p1569`"
	lines := []string{
		"DECLARE x INT64 DEFAULT 2;",                // 1
		"CREATE TABLE " + tbl + " (v INT64);",       // 2
		"CREATE PROCEDURE " + proc + "(y INT64)",    // 3
		"BEGIN",                                     // 4
		"  INSERT INTO " + tbl + " (v) VALUES (y);", // 5
		"END;",  // 6
		"BEGIN", // 7
		"  INSERT INTO " + tbl + " (v) VALUES (x);", // 8
		"  IF x > 1 THEN", // 9
		"    SELECT * FROM `" + ds.DatasetID + ".nope1569`;", // 10
		"  END IF;",                 // 11
		"EXCEPTION WHEN ERROR THEN", // 12
		"  INSERT INTO " + tbl + " (v) VALUES (x + 1);", // 13
		"END;", // 14
		"LOOP", // 15
		"  INSERT INTO " + tbl + " (v) VALUES (4);", // 16
		"  BREAK;",              // 17
		"END LOOP;",             // 18
		"CALL " + proc + "(x);", // 19
		"BEGIN TRANSACTION;",    // 20
		"INSERT INTO " + tbl + " (v) VALUES (5);", // 21
		"COMMIT TRANSACTION;",                     // 22
		"DELETE FROM " + tbl + " WHERE v = x",     // 23, the last
	}
	job, err := c.Query(strings.Join(lines, "\n")).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := job.Wait(ctx); err != nil || status.Err() != nil {
		t.Fatal(err, status.Err())
	}

	// at is a frame as the test writes it: line:column-line:column, then
	// its text, then " in " its procedure.
	at := func(f *bigquery.ScriptStackFrame) string {
		s := fmt.Sprintf("%d:%d-%d:%d %s", f.StartLine, f.StartColumn, f.EndLine, f.EndColumn, f.Text)
		if f.ProcedureID != "" {
			s += " in " + f.ProcedureID
		}
		return s
	}
	// stmt is the frame of the statement on line n, as written: from its
	// first character to the one before its semicolon.
	stmt := func(n int) string {
		l := lines[n-1]
		text := strings.TrimSuffix(strings.TrimSpace(l), ";")
		col := len(l) - len(strings.TrimLeft(l, " ")) + 1
		return fmt.Sprintf("%d:%d-%d:%d %s", n, col, n, col+len(text)-1, text)
	}
	var got []string
	it := job.Children(ctx)
	for {
		k, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ss := k.LastStatus().Statistics.ScriptStatistics
		if ss == nil || len(ss.StackFrames) == 0 {
			t.Errorf("child %s has no stack frames: %+v", k.ID(), ss)
			continue
		}
		var frames []string
		for _, f := range ss.StackFrames {
			if f.StartLine < 1 || f.StartColumn < 1 || f.EndLine < f.StartLine || f.Text == "" {
				t.Errorf("child %s frame %+v has no position", k.ID(), f)
			}
			frames = append(frames, at(f))
		}
		got = append(got, string(ss.EvaluationKind)+" "+strings.Join(frames, " < "))
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"EXPRESSION 1:25-1:25 2",
		"STATEMENT " + stmt(2),
		"STATEMENT " + stmt(8),
		"EXPRESSION 9:6-9:10 x > 1",
		"STATEMENT " + stmt(10),
		"STATEMENT " + stmt(13),
		"STATEMENT " + stmt(16),
		fmt.Sprintf("STATEMENT 2:3-2:%d %s in %s.%s.p1569 < %s", len(lines[4])-1, strings.TrimSuffix(strings.TrimSpace(lines[4]), ";"),
			project, ds.DatasetID, stmt(19)),
		"STATEMENT " + stmt(23),
	} {
		if !strings.Contains("\n"+all+"\n", "\n"+want+"\n") {
			t.Errorf("no child at %q; the children:\n%s", want, all)
		}
	}
	if !strings.Contains(all, " 21:1-") && !strings.Contains(all, " 22:1-") {
		t.Errorf("no child of the transaction's statements; the children:\n%s", all)
	}
}
