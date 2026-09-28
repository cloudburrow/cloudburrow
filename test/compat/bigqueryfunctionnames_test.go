//go:build compat

package compat

import (
	"reflect"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryFunctionNameInTheDefaultDataset (#1033): a query's call of
// a persistent function without a dataset calls the one in its default
// dataset, as a table's name is read there: with one.fn(x) AS (x + 1) made
// first and two.fn(x) AS (x + 2), SELECT fn(0) reads 2 with the default
// dataset two and 1 with one, through jobs.query and a query job, whose
// jobs.get shows the client's text; a function a script's CREATE FUNCTION
// makes is called so in the same script; a built-in (UPPER) and a
// qualified call are left as they are. Measured first against the pinned
// emulator: SELECT fn(0) with the default dataset two failed 400 "sqlite3:
// SQL logic error: no such column: <project>". Not covered: a call of
// one.fn with the default dataset two, or one query that calls both one.fn
// and two.fn, which the engine fails so even with every name qualified
// (measured: #1107).
func TestBigQueryFunctionNameInTheDefaultDataset(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	base := validationDataset(t, h, c)
	ctx := h.Context()
	one, two := base.DatasetID+"_one", base.DatasetID+"_two"
	for _, d := range []string{one, two} {
		if err := c.Dataset(d).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Dataset(d).DeleteWithContents(ctx) })
	}
	if err := bqRun(ctx, c, "CREATE FUNCTION "+one+".fn(x INT64) AS (x + 1)", false); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "CREATE FUNCTION "+two+".fn(x INT64) AS (x + 2)", false); err != nil {
		t.Fatal(err)
	}
	for _, c2 := range []struct{ dataset, sql, want string }{
		{two, "SELECT fn(0)", "2"},
		{one, "SELECT fn(0)", "1"},
		{two, "SELECT FN(0), `fn`(1), UPPER('a')", "2|3|A"},
	} {
		if got := queryRows(t, ctx, c, project, c2.dataset, c2.sql); !reflect.DeepEqual(got, []string{c2.want}) {
			t.Errorf("%s with the default dataset %s: %v, want %s", c2.sql, c2.dataset, got, c2.want)
		}
	}

	// A query job, and jobs.get of it.
	q := c.Query("SELECT fn(10) AS v")
	q.DefaultProjectID, q.DefaultDatasetID = project, two
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("the query job: %v", err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("the query job's rows: %v", err)
	}
	if got := readRows(t, it, "the query job"); !reflect.DeepEqual(got, []string{"12"}) {
		t.Errorf("the query job read %v, want [12]", got)
	}
	again, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatalf("jobs.get: %v", err)
	}
	if cfg, err := again.Config(); err != nil {
		t.Errorf("the job's configuration: %v", err)
	} else if qc, ok := cfg.(*bigquery.QueryConfig); !ok || qc.Q != "SELECT fn(10) AS v" {
		t.Errorf("jobs.get shows %+v, want the client's text", cfg)
	}

	// A function the script makes, called without its dataset.
	sq := c.Query("CREATE FUNCTION " + two + ".g(x INT64) AS (x * 3);\nSELECT g(2) AS v")
	sq.DefaultProjectID, sq.DefaultDatasetID = project, two
	sj, err := sq.Run(ctx)
	if err != nil {
		t.Fatalf("the script: %v", err)
	}
	if st, err := sj.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("the script: %v %v", err, st.Err())
	}
	if got := queryRows(t, ctx, c, project, two, "SELECT g(3)"); !reflect.DeepEqual(got, []string{"9"}) {
		t.Errorf("SELECT g(3): %v, want [9]", got)
	}
}
