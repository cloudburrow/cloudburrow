//go:build compat

package compat

import (
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
)

// TestBigQueryScriptDryRunStops (#1456): the dry run of a script stops
// where BigQuery documents it stops
// (https://docs.cloud.google.com/bigquery/docs/multi-statement-queries,
// "Dry-run a multi-statement query", docs re-read 2026-10-17): at its
// first DDL statement, which it validates, and before an IF, FOR or WHILE.
// So a script whose dry run reaches an ALTER TABLE has the statements
// before it validated and the ALTER TABLE checked as a lone one's dry run
// checks it (a column the table has 400 invalidQuery), what follows not
// read, and nothing changed; one with an ALTER PROJECT inside an IF has
// the statements before the IF validated. Each answers statementType
// SCRIPT, through jobs.insert (the Go client) and jobs.query. An ALTER
// TABLE in legacy SQL, which has no DDL
// (https://docs.cloud.google.com/bigquery/docs/legacy-sql-feature-availability),
// fails 400 invalidQuery through jobs.query and jobs.insert. Measured
// first: each was 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryScriptDryRunStops(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	svc := generatedBigQuery(t, h)
	tbl := ds.Table("drystop")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "a", Type: bigquery.IntegerFieldType},
	}}); err != nil {
		t.Fatal(err)
	}
	ref := "`" + ds.DatasetID + ".drystop`"
	dry := func(sql string) (*bigquery.QueryStatistics, error) {
		q := c.Query(sql)
		q.DryRun = true
		job, err := q.Run(ctx)
		if err != nil {
			return nil, err
		}
		st := job.LastStatus()
		if st == nil || st.Statistics == nil {
			return nil, nil
		}
		qs, _ := st.Statistics.Details.(*bigquery.QueryStatistics)
		return qs, nil
	}
	for _, sql := range []string{
		"SELECT a FROM " + ref + "; ALTER TABLE " + ref + " ADD COLUMN b STRING; SELECT nope FROM nowhere",
		"ALTER TABLE " + ref + " ADD COLUMN b STRING; ALTER TABLE " + ref + " ADD COLUMN b STRING",
		"DECLARE x INT64 DEFAULT 1; IF x = 1 THEN ALTER PROJECT `" + project +
			"` SET OPTIONS (`region-us.default_time_zone` = 'America/Chicago'); END IF",
	} {
		qs, err := dry(sql)
		if err != nil || qs == nil || qs.StatementType != "SCRIPT" {
			t.Errorf("%s: %+v (%v)", sql, qs, err)
		}
		gq, err := svc.Jobs.Query(project, &bq.QueryRequest{Query: sql, DryRun: true, UseLegacySql: new(bool)}).Context(ctx).Do()
		if err != nil || len(gq.Rows) != 0 {
			t.Errorf("%s through jobs.query: %+v (%v)", sql, gq, err)
		}
	}
	md, err := tbl.Metadata(ctx)
	if err != nil || len(md.Schema) != 1 {
		t.Errorf("a dry run changed the table: %+v (%v)", md, err)
	}
	for sql, want := range map[string]struct {
		code   int
		reason string
	}{
		"SELECT 1; ALTER TABLE " + ref + " ADD COLUMN a STRING":                     {http.StatusBadRequest, "invalidQuery"},
		"SELECT 1; ALTER TABLE `" + ds.DatasetID + ".nosuch` ADD COLUMN b STRING":   {http.StatusNotFound, "notFound"},
		"SELECT nope FROM " + ref + "; ALTER TABLE " + ref + " ADD COLUMN b STRING": {http.StatusBadRequest, "invalidQuery"},
	} {
		_, err := dry(sql)
		wantReason(t, sql, err, want.code, want.reason)
	}

	legacy := "ALTER TABLE [" + ds.DatasetID + ".drystop] ADD COLUMN b STRING"
	yes := true
	_, err = svc.Jobs.Query(project, &bq.QueryRequest{Query: legacy, UseLegacySql: &yes}).Context(ctx).Do()
	wantReason(t, "legacy SQL through jobs.query", err, http.StatusBadRequest, "invalidQuery")
	job, err := svc.Jobs.Insert(project, &bq.Job{Configuration: &bq.JobConfiguration{
		Query: &bq.JobConfigurationQuery{Query: legacy, UseLegacySql: &yes}}}).Context(ctx).Do()
	if err == nil {
		if job.Status == nil || job.Status.ErrorResult == nil || job.Status.ErrorResult.Reason != "invalidQuery" {
			t.Errorf("legacy SQL through jobs.insert: %+v", job.Status)
		}
	} else {
		wantReason(t, "legacy SQL through jobs.insert", err, http.StatusBadRequest, "invalidQuery")
	}
	if md, err := tbl.Metadata(ctx); err != nil || len(md.Schema) != 1 {
		t.Errorf("legacy SQL changed the table: %+v (%v)", md, err)
	}
}
