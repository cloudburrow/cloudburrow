//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
)

// TestBigQueryCreateOfAnExistingName (#1456): CREATE TABLE and CREATE VIEW
// (with or without a column list), without OR REPLACE or IF NOT EXISTS, of
// a name a table or view has fail with reason duplicate, "Already Exists:
// Table p:ds.t", 409 through jobs.query and the job's error through
// jobs.insert, as BigQuery's error table gives duplicate "when trying to
// create a job, dataset, or table that already exists"
// (https://cloud.google.com/bigquery/docs/error-messages); and a dry run of
// each, through jobs.insert (Query.DryRun) and jobs.query (the generated
// client), fails the same, as "an invalid query will return the same error
// it would if it wasn't a dry run" (JobConfiguration.dryRun). Nothing is
// changed. Measured first through the front: each failed 400
// jobInternalError "sqlite3: SQL logic error: table `p_ds_t` already
// exists" (the engine's text), and a dry run of a CREATE VIEW with a column
// list was 501.
//
// A dry run of a CREATE TABLE or CREATE VIEW of a new name makes nothing
// ("the query is not executed"): tables.get of it is 404 and a query of it
// 404 notFound, as of any table that does not exist. Measured first: the
// query failed 400 jobInternalError "no such table: p_ds_t", the engine's
// analyzer keeping the name its dry run rolled back.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryCreateOfAnExistingName(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	svc := generatedBigQuery(t, h)
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for _, sql := range []string{
		"CREATE TABLE " + path("t") + " AS SELECT 1 AS a",
		"CREATE VIEW " + path("v") + " AS SELECT a FROM " + path("t"),
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	want := func(name string) string { return "Already Exists: Table " + project + ":" + ds.DatasetID + "." + name }
	for _, st := range []struct{ sql, name string }{
		{"CREATE TABLE " + path("t") + " (z STRING)", "t"},
		{"CREATE TABLE " + path("t") + " AS SELECT 2 AS a", "t"},
		{"CREATE TABLE " + path("v") + " (z STRING)", "v"},
		{"CREATE VIEW " + path("v") + " AS SELECT 5 AS a", "v"},
		{"CREATE VIEW " + path("t") + " AS SELECT 5 AS a", "t"},
		{"CREATE VIEW " + path("v") + " (x) AS SELECT 5 AS a", "v"},
	} {
		// jobs.query
		err := bqRun(ctx, c, st.sql, false)
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusConflict || len(ge.Errors) != 1 ||
			ge.Errors[0].Reason != "duplicate" || ge.Errors[0].Message != want(st.name) {
			t.Errorf("%s through jobs.query: %v, want 409 duplicate %q", st.sql, err, want(st.name))
		}
		// jobs.insert: the job's error.
		err = bqRun(ctx, c, st.sql, true)
		if errReason(err) != "duplicate" || !strings.Contains(err.Error(), want(st.name)) {
			t.Errorf("%s through jobs.insert: %v, want duplicate %q", st.sql, err, want(st.name))
		}
		// A dry run, through jobs.insert and jobs.query.
		q := c.Query(st.sql)
		q.DryRun = true
		_, err = q.Run(ctx)
		wantReason(t, st.sql+" (dry run, jobs.insert)", err, http.StatusConflict, "duplicate")
		_, err = svc.Jobs.Query(project, &bq.QueryRequest{Query: st.sql, DryRun: true, UseLegacySql: new(bool)}).Context(ctx).Do()
		wantReason(t, st.sql+" (dry run, jobs.query)", err, http.StatusConflict, "duplicate")
	}
	// Nothing changed: the table and the view read as before.
	it, err := c.Query("SELECT a FROM " + path("v")).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil || len(row) != 1 || row[0] != int64(1) {
		t.Errorf("the view reads %v (%v), want [1]", row, err)
	}
	md, err := ds.Table("v").Metadata(ctx)
	if err != nil || md.Type != bigquery.ViewTable || len(md.Schema) != 1 || md.Schema[0].Name != "a" {
		t.Errorf("the view's metadata: %+v (%v)", md, err)
	}
	// A dry run of a new name makes nothing.
	for _, st := range []struct{ sql, name string }{
		{"CREATE TABLE " + path("drytable") + " (z STRING)", "drytable"},
		{"CREATE VIEW " + path("dryview") + " AS SELECT 1 AS a", "dryview"},
	} {
		q := c.Query(st.sql)
		q.DryRun = true
		if _, err := q.Run(ctx); err != nil {
			t.Fatalf("%s (dry run): %v", st.sql, err)
		}
		if tableExists(t, ctx, ds.Table(st.name)) {
			t.Errorf("the dry run of %s made it", st.sql)
		}
		_, err := c.Query("SELECT * FROM " + path(st.name)).Read(ctx)
		wantReason(t, "a query of "+st.name+" after a dry run", err, http.StatusNotFound, "notFound")
		if err := bqRun(ctx, c, st.sql, true); err != nil {
			t.Errorf("%s after its dry run: %v", st.sql, err)
		}
	}
}
