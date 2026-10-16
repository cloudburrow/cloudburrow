//go:build compat

package compat

import (
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryScriptFailedChildJobs (#1201): a statement of a script that
// fails is a child job of its own, as BigQuery runs one ("each statement
// in the script creates a child job",
// https://cloud.google.com/bigquery/docs/multi-statement-queries): failed
// with the statement's error, statistics.parentJobId the script's job and
// scriptStatistics evaluationKind STATEMENT with a stack frame at the
// statement (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#ScriptStatistics),
// also when an EXCEPTION handler catches the error. The script's job fails
// with the error no handler caught. Measured first: the failed statements
// had no child job (the emulator records none for a jobs.query that fails),
// so the script listed only the statements that succeeded.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.list
func TestBigQueryScriptFailedChildJobs(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	script := "DECLARE x INT64 DEFAULT 1;\n" +
		"BEGIN\n" +
		"  SELECT * FROM `" + ds.DatasetID + ".missing_1201`;\n" +
		"EXCEPTION WHEN ERROR THEN\n" +
		"  SELECT 'caught' AS s;\n" +
		"END;\n" +
		"IF x = 1 THEN\n" +
		"  RAISE USING MESSAGE = 'boom 1201';\n" +
		"END IF"
	job, err := c.Query(script).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Wait answers the job's own error when it failed.
	if _, err := job.Wait(ctx); err == nil || !strings.Contains(err.Error(), "boom 1201") {
		t.Fatalf("the script's job ended %v, want the error boom 1201", err)
	}

	var failed []*bigquery.Job
	n := 0
	it := job.Children(ctx)
	for {
		k, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
		if k.LastStatus().Err() != nil {
			failed = append(failed, k)
		}
	}
	// DECLARE's DEFAULT, the failed SELECT, the handler's SELECT, IF's
	// condition and RAISE's message, an expression.
	if n != 5 {
		t.Errorf("children = %d, want 5", n)
	}
	if len(failed) != 1 {
		t.Fatalf("failed children = %d, want 1 (the SELECT the handler caught)", len(failed))
	}
	k, err := c.JobFromID(ctx, failed[0].ID()) // jobs.get
	if err != nil {
		t.Fatal(err)
	}
	st := k.LastStatus()
	if st.Err() == nil || !strings.Contains(st.Err().Error(), "missing_1201") {
		t.Errorf("the failed child's error = %v, want the table not found", st.Err())
	}
	if st.Statistics.ParentJobID != job.ID() {
		t.Errorf("the failed child's parentJobId = %q, want %q", st.Statistics.ParentJobID, job.ID())
	}
	ss := st.Statistics.ScriptStatistics
	if ss == nil || ss.EvaluationKind != "STATEMENT" || len(ss.StackFrames) != 1 {
		t.Fatalf("the failed child's scriptStatistics = %+v", ss)
	}
	if f := ss.StackFrames[0]; f.StartLine != 3 || f.StartColumn != 3 || !strings.HasPrefix(f.Text, "SELECT * FROM") {
		t.Errorf("the failed child's frame = %+v, want 3:3 and the statement", f)
	}
	if cfg, err := k.Config(); err != nil {
		t.Error(err)
	} else if q, ok := cfg.(*bigquery.QueryConfig); !ok || !strings.HasPrefix(q.Q, "SELECT * FROM") {
		t.Errorf("the failed child's query = %+v, want the statement", cfg)
	}
}
