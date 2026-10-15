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

// TestBigQueryScriptChildJobs (#1353): a script the front carries out is a
// SCRIPT parent job with a child job per statement and per evaluated
// expression, as BigQuery runs one: Job.Children (jobs.list with
// parentJobId) lists them, each with statistics.parentJobId,
// scriptStatistics and its own query, statementType and DML counts;
// the parent counts them in numChildJobs, jobs.list without parentJobId
// leaves them out, getQueryResults on the parent gives the last
// statement's rows, and INFORMATION_SCHEMA.JOBS has them with
// parent_job_id. Measured first: the parent had no children
// (numChildJobs 0, statementType SELECT).
//
// covers: bigquery.jobs.list, bigquery.jobs.get, bigquery.jobs.getQueryResults
func TestBigQueryScriptChildJobs(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	tbl := "`" + ds.DatasetID + ".s1353`"
	script := "DECLARE x INT64 DEFAULT 2;\n" +
		"CREATE TABLE " + tbl + " (a INT64);\n" +
		"IF x > 1 THEN\n  INSERT INTO " + tbl + " (a) VALUES (1), (2);\nEND IF;\n" +
		"SELECT a FROM " + tbl + " ORDER BY a"
	job, err := c.Query(script).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err := job.Wait(ctx)
	if err != nil || status.Err() != nil {
		t.Fatal(err, status.Err())
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, row[0].(int64))
	}
	if fmt.Sprint(got) != "[1 2]" {
		t.Errorf("parent rows = %v, want [1 2] (the last statement's)", got)
	}
	parent, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatal(err)
	}
	pstats := parent.LastStatus().Statistics
	if qs, ok := pstats.Details.(*bigquery.QueryStatistics); !ok || qs.StatementType != "SCRIPT" {
		t.Errorf("parent statementType = %+v, want SCRIPT", pstats.Details)
	}

	var kids []*bigquery.Job
	cit := job.Children(ctx)
	for {
		k, err := cit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kids = append(kids, k)
	}
	// DECLARE's DEFAULT, CREATE TABLE, IF's condition, INSERT, SELECT.
	if len(kids) != 5 {
		t.Fatalf("children = %d, want 5", len(kids))
	}
	if pstats.NumChildJobs != int64(len(kids)) {
		t.Errorf("numChildJobs = %d, want %d", pstats.NumChildJobs, len(kids))
	}
	kinds := map[string]int{}
	types := map[string]*bigquery.Job{}
	byText := map[string]*bigquery.Job{} // the first word of a statement's text
	for _, k := range kids {
		st := k.LastStatus().Statistics
		if st.ParentJobID != job.ID() {
			t.Errorf("child %s parentJobId = %q, want %q", k.ID(), st.ParentJobID, job.ID())
		}
		if st.ScriptStatistics == nil || len(st.ScriptStatistics.StackFrames) == 0 {
			t.Errorf("child %s scriptStatistics = %+v", k.ID(), st.ScriptStatistics)
			continue
		}
		kinds[string(st.ScriptStatistics.EvaluationKind)]++
		if st.ScriptStatistics.EvaluationKind == "STATEMENT" {
			text := st.ScriptStatistics.StackFrames[0].Text
			if i := strings.IndexByte(text, ' '); i > 0 {
				byText[text[:i]] = k
			}
		}
		if qs, ok := st.Details.(*bigquery.QueryStatistics); ok {
			types[qs.StatementType] = k
		}
	}
	if kinds["STATEMENT"] != 3 || kinds["EXPRESSION"] != 2 {
		t.Errorf("evaluation kinds = %v, want 3 STATEMENT, 2 EXPRESSION", kinds)
	}
	// The emulator gives a DDL statement statementType SELECT (#698), so the
	// CREATE TABLE child is found by its frame's text.
	if k := byText["CREATE"]; k == nil {
		t.Errorf("no CREATE TABLE child: %v", byText)
	} else if f := k.LastStatus().Statistics.ScriptStatistics.StackFrames[0]; f.StartLine != 2 || f.StartColumn != 1 {
		t.Errorf("CREATE TABLE frame at %d:%d, want 2:1", f.StartLine, f.StartColumn)
	}
	if k := types["INSERT"]; k == nil {
		t.Errorf("no INSERT child: %v", types)
	} else if n := k.LastStatus().Statistics.Details.(*bigquery.QueryStatistics).NumDMLAffectedRows; n != 2 {
		t.Errorf("INSERT child numDmlAffectedRows = %d, want 2", n)
	}
	// A SELECT child is a jobs.query job of the emulator's, which names no
	// destination table (docs/compatibility.md); its query is its own.
	if k := byText["SELECT"]; k == nil {
		t.Errorf("no SELECT statement child: %v", byText)
	} else if cfg, err := k.Config(); err != nil {
		t.Error(err)
	} else if q, ok := cfg.(*bigquery.QueryConfig); !ok || !strings.HasPrefix(q.Q, "SELECT a FROM") {
		t.Errorf("SELECT child's query = %+v", cfg)
	}

	// Top-level jobs.list leaves the children out.
	lit := c.Jobs(ctx)
	lit.ProjectID = c.Project()
	kidIDs := map[string]bool{}
	for _, k := range kids {
		kidIDs[k.ID()] = true
	}
	for {
		j, err := lit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if kidIDs[j.ID()] {
			t.Errorf("child %s listed with the top-level jobs", j.ID())
		}
	}

	// INFORMATION_SCHEMA.JOBS has them, with parent_job_id.
	rit, err := c.Query("SELECT COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS WHERE parent_job_id = '" + job.ID() + "'").Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var row []bigquery.Value
	if err := rit.Next(&row); err != nil {
		t.Fatal(err)
	}
	if row[0].(int64) != int64(len(kids)) {
		t.Errorf("INFORMATION_SCHEMA.JOBS children = %v, want %d", row[0], len(kids))
	}
}
