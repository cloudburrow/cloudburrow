//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryWriteTimesByTheInstanceClock (#1487), official Go client:
// after the instance's clock is moved forward (clock:advance, a minute),
// the change times of rows written by DML, tabledata.insertAll and a load
// job (APPENDS and CHANGES), and a table's creationTime and
// lastModifiedTime, read that clock, not this machine's: each is between
// the instance's clock before the write and after it.
//
// covers: bigquery.tables.insert, bigquery.tables.get, bigquery.tabledata.insertAll, bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryWriteTimesByTheInstanceClock(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	ref := "`" + ds.DatasetID + ".oneclock`"
	before := advanceBigQueryClock(t, h, time.Minute)
	if err := bqRun(ctx, c, "CREATE TABLE "+ref+" (a INT64) OPTIONS (enable_change_history = TRUE)", false); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ref+" VALUES (1)", false); err != nil {
		t.Fatal(err)
	}
	type row struct {
		A int64 `bigquery:"a"`
	}
	if err := ds.Table("oneclock").Inserter().Put(ctx, []*row{{A: 2}}); err != nil {
		t.Fatal(err)
	}
	src := bigquery.NewReaderSource(strings.NewReader("3\n"))
	src.Schema = bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}
	job, err := ds.Table("oneclock").LoaderFrom(src).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := job.Wait(ctx); err != nil || status.Err() != nil {
		t.Fatalf("load: %v, %v", err, status)
	}
	after := advanceBigQueryClock(t, h, 0)
	within := func(what string, got time.Time) {
		t.Helper()
		if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
			t.Errorf("%s = %v, want between %v and %v by the instance's clock (this machine's: %v)", what, got, before, after, time.Now().UTC())
		}
	}
	md, err := ds.Table("oneclock").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	within("CreationTime", md.CreationTime)
	within("LastModifiedTime", md.LastModifiedTime)
	got := rows(t, ctx, c, "SELECT a, _CHANGE_TIMESTAMP FROM APPENDS(TABLE "+ref+", NULL, NULL) UNION ALL "+
		"SELECT a, _CHANGE_TIMESTAMP FROM CHANGES(TABLE "+ref+", NULL, TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 0 SECOND))")
	if len(got) != 6 {
		t.Fatalf("APPENDS and CHANGES: %v, want three rows each", got)
	}
	for _, r := range got {
		ts, _ := r[1].(time.Time)
		within(fmt.Sprintf("the _CHANGE_TIMESTAMP of a = %v", r[0]), ts)
	}
}
