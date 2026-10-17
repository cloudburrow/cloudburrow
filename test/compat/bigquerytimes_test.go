//go:build compat

package compat

import (
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryResourceTimesInMilliseconds (#1310), official Go client: a
// dataset's, a table's, a view's and a routine's CreationTime and
// LastModifiedTime, and a query job's statistics' CreationTime, StartTime
// and EndTime, are within a minute of now by the instance's clock
// (advanceBigQueryClock, which earlier tests move forward, #1487). BigQuery
// gives them in
// milliseconds since the epoch; the engine gave a table's and a job's in
// seconds, which the client read as January 1970, and a dataset none.
//
// covers: bigquery.datasets.insert, bigquery.datasets.get, bigquery.tables.insert, bigquery.tables.get, bigquery.tables.patch, bigquery.routines.insert, bigquery.routines.get, bigquery.jobs.insert, bigquery.jobs.get
func TestBigQueryResourceTimesInMilliseconds(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	near := func(what string, got time.Time) {
		t.Helper()
		now := advanceBigQueryClock(t, h, 0) // the instance's clock, #1487
		if d := now.Sub(got); d < -time.Minute || d > time.Minute {
			t.Errorf("%s = %v, want within a minute of now, %v", what, got, now)
		}
	}

	ds := validationDataset(t, h, c)
	dmd, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatalf("datasets.get: %v", err)
	}
	near("Dataset CreationTime", dmd.CreationTime)
	near("Dataset LastModifiedTime", dmd.LastModifiedTime)

	table := ds.Table("timed")
	if err := table.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatalf("tables.insert: %v", err)
	}
	tmd, err := table.Metadata(ctx)
	if err != nil {
		t.Fatalf("tables.get: %v", err)
	}
	near("Table CreationTime", tmd.CreationTime)
	near("Table LastModifiedTime", tmd.LastModifiedTime)
	umd, err := table.Update(ctx, bigquery.TableMetadataToUpdate{Description: "described"}, "")
	if err != nil {
		t.Fatalf("tables.patch: %v", err)
	}
	near("updated Table CreationTime", umd.CreationTime)
	near("updated Table LastModifiedTime", umd.LastModifiedTime)

	view := ds.Table("timedview")
	if err := view.Create(ctx, &bigquery.TableMetadata{ViewQuery: "SELECT id FROM `" + ds.ProjectID + "." + ds.DatasetID + ".timed`"}); err != nil {
		t.Fatalf("tables.insert of a view: %v", err)
	}
	vmd, err := view.Metadata(ctx)
	if err != nil {
		t.Fatalf("tables.get of a view: %v", err)
	}
	near("view CreationTime", vmd.CreationTime)
	near("view LastModifiedTime", vmd.LastModifiedTime)

	if err := ds.Routine("timedfn").Create(ctx, &bigquery.RoutineMetadata{
		Type: "SCALAR_FUNCTION", Language: "SQL", Body: "x + 1",
		Arguments:  []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}},
		ReturnType: &bigquery.StandardSQLDataType{TypeKind: "INT64"},
	}); err != nil {
		t.Fatalf("routines.insert: %v", err)
	}
	rmd, err := ds.Routine("timedfn").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get: %v", err)
	}
	near("Routine CreationTime", rmd.CreationTime)
	near("Routine LastModifiedTime", rmd.LastModifiedTime)

	job, err := c.Query("SELECT 1").Run(ctx)
	if err != nil {
		t.Fatalf("jobs.insert: %v", err)
	}
	if status, err := job.Wait(ctx); err != nil || status.Err() != nil {
		t.Fatalf("query: %v, %v", err, status)
	}
	got, err := c.JobFromIDLocation(ctx, job.ID(), job.Location())
	if err != nil {
		t.Fatalf("jobs.get: %v", err)
	}
	stats := got.LastStatus().Statistics
	near("Job CreationTime", stats.CreationTime)
	near("Job StartTime", stats.StartTime)
	near("Job EndTime", stats.EndTime)
}
