//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryMaterializedViewsInTempScripts (#1434): CREATE, ALTER and
// DROP MATERIALIZED VIEW and CALL BQ.REFRESH_MATERIALIZED_VIEW in a
// multi-statement script that also makes a TEMP table and a TEMP function,
// before and after the materialized view statements, through jobs.query
// and jobs.insert. BigQuery keeps a TEMP table for the script
// (https://cloud.google.com/bigquery/docs/multi-statement-queries#temporary_tables),
// and runs each statement as a child job: the TEMP table, written and
// read on both sides of the materialized view's statements, gives the
// script's rows; the materialized view reads the base table the script
// filled from the TEMP table, its options set by the script; the child
// jobs have statementType CREATE_MATERIALIZED_VIEW,
// ALTER_MATERIALIZED_VIEW, CALL and DROP_MATERIALIZED_VIEW, and their
// texts name the TEMP table as the script does. The TEMP table is gone
// once the script ends. A materialized view of the TEMP table is 400,
// nothing run. Measured first: each such script was 501 notImplemented.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.list, bigquery.tables.get
func TestBigQueryMaterializedViewsInTempScripts(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(name string) string { return "`" + ds.DatasetID + "." + name + "`" }
	for _, insert := range []bool{false, true} {
		base, mv := fmt.Sprintf("tbase%v", insert), fmt.Sprintf("tmv%v", insert)
		script := "CREATE TEMP TABLE staged AS SELECT 1 AS v UNION ALL SELECT 2;\n" +
			"CREATE TEMP FUNCTION tenfold(x INT64) AS (x * 10);\n" +
			"CREATE TABLE " + path(base) + " AS SELECT tenfold(v) AS v FROM staged;\n" +
			"CREATE MATERIALIZED VIEW " + path(mv) + " OPTIONS (enable_refresh = FALSE) AS SELECT SUM(v) AS s FROM " + path(base) + ";\n" +
			"INSERT INTO staged (v) VALUES (3);\n" +
			"ALTER MATERIALIZED VIEW " + path(mv) + " SET OPTIONS (description = 'from a temp script');\n" +
			"INSERT INTO " + path(base) + " SELECT tenfold(v) FROM staged WHERE v = 3;\n" +
			"CALL BQ.REFRESH_MATERIALIZED_VIEW('" + ds.DatasetID + "." + mv + "');\n" +
			"SELECT (SELECT s FROM " + path(mv) + ") AS s, (SELECT COUNT(*) FROM staged) AS n, (SELECT SUM(staged.v) FROM staged) AS t"
		job, it := runScriptJob(t, c, script, insert)
		var out []bigquery.Value
		if err := it.Next(&out); err != nil || fmt.Sprint(out) != "[60 3 6]" {
			t.Errorf("insert %v: the script's rows %v (%v), want [60 3 6]", insert, out, err)
		}
		md, err := ds.Table(mv).Metadata(ctx)
		if err != nil || md.Type != bigquery.MaterializedView || md.Description != "from a temp script" || md.MaterializedView.EnableRefresh {
			t.Errorf("insert %v: the script's materialized view: %v %+v", insert, err, md)
		}
		types, texts := childTypes(t, job)
		for _, want := range []string{"CREATE_MATERIALIZED_VIEW", "ALTER_MATERIALIZED_VIEW", "CALL", "INSERT"} {
			if !types[want] {
				t.Errorf("insert %v: no child job of statementType %s: %v", insert, want, types)
			}
		}
		if !strings.Contains(texts, "INSERT INTO staged (v) VALUES (3)") || strings.Contains(texts, "_cloudburrow") {
			t.Errorf("insert %v: the child jobs' texts: %s", insert, texts)
		}
		if err := bqRun(ctx, c, "SELECT * FROM staged", insert); err == nil {
			t.Errorf("insert %v: the TEMP table outlived its script", insert)
		}

		// The reverse order: the materialized view's statements first,
		// then the TEMP table's.
		job, it = runScriptJob(t, c, "DROP MATERIALIZED VIEW "+path(mv)+";\n"+
			"CREATE TEMP TABLE kept AS SELECT COUNT(*) AS n FROM "+path(base)+";\n"+
			"DROP TABLE "+path(base)+";\n"+
			"SELECT n FROM kept", insert)
		if err := it.Next(&out); err != nil || fmt.Sprint(out) != "[3]" {
			t.Errorf("insert %v: the second script's rows %v (%v), want [3]", insert, out, err)
		}
		if tableExists(t, ctx, ds.Table(mv)) || tableExists(t, ctx, ds.Table(base)) {
			t.Errorf("insert %v: the second script left %s or %s", insert, mv, base)
		}
		if types, _ := childTypes(t, job); !types["DROP_MATERIALIZED_VIEW"] {
			t.Errorf("insert %v: no DROP_MATERIALIZED_VIEW child job: %v", insert, types)
		}
	}
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	run("CREATE TABLE " + path("tb") + " AS SELECT 1 AS v")
	err := bqRun(ctx, c, "CREATE TEMP TABLE x AS SELECT 1 AS v;\nCREATE MATERIALIZED VIEW "+path("tmvx")+
		" AS SELECT SUM(v) AS s FROM x", false)
	wantHTTPStatus(t, "a materialized view of a TEMP table", err, http.StatusBadRequest)
	if tableExists(t, ctx, ds.Table("tmvx")) {
		t.Error("a materialized view of a TEMP table was made")
	}
}

// runScriptJob runs sql through jobs.query (Query.Read) or jobs.insert
// (Query.Run), and returns its job and rows.
func runScriptJob(t *testing.T, c *bigquery.Client, sql string, insert bool) (*bigquery.Job, *bigquery.RowIterator) {
	t.Helper()
	ctx := t.Context()
	if !insert {
		it, err := c.Query(sql).Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		job := it.SourceJob()
		if job == nil {
			t.Fatalf("%s: no job", sql)
		}
		return job, it
	}
	job, err := c.Query(sql).Run(ctx)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return job, it
}

// childTypes is the statementTypes of job's child jobs, and their texts.
func childTypes(t *testing.T, job *bigquery.Job) (map[string]bool, string) {
	t.Helper()
	types := map[string]bool{}
	var texts []string
	kids := job.Children(t.Context())
	for {
		k, err := kids.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if qs, ok := k.LastStatus().Statistics.Details.(*bigquery.QueryStatistics); ok {
			types[qs.StatementType] = true
		}
		if cfg, err := k.Config(); err == nil {
			if qc, ok := cfg.(*bigquery.QueryConfig); ok {
				texts = append(texts, qc.Q)
			}
		}
	}
	return types, strings.Join(texts, "\n")
}
