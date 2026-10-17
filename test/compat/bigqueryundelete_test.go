//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
)

// TestBigQueryUndeleteAndRestore (#1460): what BigQuery documents of
// deleted data within the time travel window
// (https://cloud.google.com/bigquery/docs/time-travel,
// https://cloud.google.com/bigquery/docs/restore-deleted-datasets,
// https://cloud.google.com/bigquery/docs/restore-deleted-tables):
//
//   - a table deleted by tables.delete, or by DROP TABLE, is restored by a
//     copy job of table@<milliseconds> at a time it existed, with its rows
//     as they were then; a time after the deletion fails "Invalid snapshot
//     time";
//   - a copy of table@<milliseconds> of a table that exists reads it as it
//     was then;
//   - datasets.undelete restores a dataset deleted with its contents, with
//     its description, its tables (their rows and descriptions) and views;
//     an undelete of a dataset that exists fails 409, of one never deleted
//     404;
//   - UNDROP SCHEMA after DROP SCHEMA CASCADE, through jobs.query and
//     jobs.insert; IF NOT EXISTS of a dataset that exists does nothing;
//   - CREATE SNAPSHOT TABLE and CREATE TABLE … CLONE FOR SYSTEM_TIME AS OF
//     a past time copy the table as it was then;
//   - a deleted dataset whose window (48 hours) has passed, by the front's
//     clock (advanceBigQueryClock), is no longer undeleted.
//
// Measured first: datasets.undelete and UNDROP SCHEMA were 501, a copy of
// a deleted table@time failed notFound, and a snapshot or clone at a past
// time was 501. Times are the engine's CURRENT_TIMESTAMP(), the clock its
// changes are recorded by.
//
// covers: bigquery.datasets.undelete, bigquery.datasets.delete, bigquery.tables.delete, bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryUndeleteAndRestore(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ctx := h.Context()
	base := validationDataset(t, h, c)
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	now := func() time.Time {
		t.Helper()
		got := rows(t, ctx, c, "SELECT CURRENT_TIMESTAMP()")
		ts, ok := got[0][0].(time.Time)
		if !ok {
			t.Fatalf("CURRENT_TIMESTAMP(): %v", got)
		}
		return ts
	}
	read := func(sql string) []string {
		t.Helper()
		it, err := c.Query(sql).Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return readRows(t, it, sql)
	}
	copyAt := func(src, dest string, at int64) error {
		t.Helper()
		job, err := base.Table(dest).CopierFrom(base.Table(fmt.Sprintf("%s@%d", src, at))).Run(ctx)
		if err != nil {
			return err
		}
		st, err := job.Wait(ctx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	q := func(table string) string { return "`" + base.DatasetID + "." + table + "`" }
	lit := func(ts time.Time) string { return fmt.Sprintf("TIMESTAMP_MICROS(%d)", ts.UnixMicro()) }

	// A table deleted by tables.delete, restored at two times.
	run("CREATE TABLE " + q("gone") + " (n INT64, s STRING)")
	run("INSERT INTO " + q("gone") + " VALUES (1, 'a'), (2, 'b')")
	t1 := now()
	run("UPDATE " + q("gone") + " SET s = 'c' WHERE n = 1")
	t2 := now()
	if err := base.Table("gone").Delete(ctx); err != nil {
		t.Fatalf("tables.delete: %v", err)
	}
	if err := copyAt("gone", "gone_t1", t1.UnixMilli()); err != nil {
		t.Fatalf("copy gone@t1: %v", err)
	}
	if got := read("SELECT n, s FROM " + q("gone_t1") + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1|a", "2|b"}) {
		t.Errorf("gone@t1: %v, want [1|a 2|b]", got)
	}
	if err := copyAt("gone", "gone", t2.UnixMilli()); err != nil {
		t.Fatalf("copy gone@t2 into gone: %v", err)
	}
	if got := read("SELECT n, s FROM " + q("gone") + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1|c", "2|b"}) {
		t.Errorf("gone@t2: %v, want [1|c 2|b]", got)
	}

	// A table dropped by DROP TABLE; a time after the drop is refused.
	run("CREATE TABLE " + q("dropped") + " (n INT64)")
	run("INSERT INTO " + q("dropped") + " VALUES (7)")
	t3 := now()
	run("DROP TABLE " + q("dropped"))
	if err := copyAt("dropped", "dropped_back", t3.UnixMilli()); err != nil {
		t.Fatalf("copy dropped@t3: %v", err)
	}
	if got := read("SELECT n FROM " + q("dropped_back")); !reflect.DeepEqual(got, []string{"7"}) {
		t.Errorf("dropped@t3: %v, want [7]", got)
	}
	wantAPIError(t, "a time after the drop", copyAt("dropped", "dropped_late", -1), http.StatusBadRequest, "Invalid snapshot time")

	// A table that exists, at a past time; a snapshot and a clone of then.
	run("CREATE TABLE " + q("live") + " (n INT64, s STRING)")
	run("INSERT INTO " + q("live") + " VALUES (1, 'a')")
	t4 := now()
	run("INSERT INTO " + q("live") + " VALUES (2, 'b')")
	if err := copyAt("live", "live_t4", t4.UnixMilli()); err != nil {
		t.Fatalf("copy live@t4: %v", err)
	}
	if got := read("SELECT n, s FROM " + q("live_t4")); !reflect.DeepEqual(got, []string{"1|a"}) {
		t.Errorf("live@t4: %v, want [1|a]", got)
	}
	run("CREATE SNAPSHOT TABLE " + q("live_snap") + " CLONE " + q("live") + " FOR SYSTEM_TIME AS OF " + lit(t4))
	run("CREATE TABLE " + q("live_clone") + " CLONE " + q("live") + " FOR SYSTEM_TIME AS OF " + lit(t4))
	for _, tb := range []string{"live_snap", "live_clone"} {
		if got := read("SELECT n, s FROM " + q(tb)); !reflect.DeepEqual(got, []string{"1|a"}) {
			t.Errorf("%s: %v, want [1|a]", tb, got)
		}
	}
	if md, err := base.Table("live_snap").Metadata(ctx); err != nil || md.SnapshotDefinition == nil ||
		md.SnapshotDefinition.SnapshotTime.UnixMilli() != t4.UnixMilli() {
		t.Errorf("live_snap's snapshot definition: %+v, %v; want its time %v", md, err, t4)
	}

	// datasets.undelete of a dataset deleted with its contents.
	ds := c.Dataset(base.DatasetID + "_undelete")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Description: "kept"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	run("CREATE TABLE " + p("t") + " (n INT64, s STRING) OPTIONS (description = 'rows')")
	run("INSERT INTO " + p("t") + " VALUES (1, 'a'), (2, 'b')")
	run("UPDATE " + p("t") + " SET s = 'c' WHERE n = 1")
	run("CREATE VIEW " + p("v") + " AS SELECT n FROM " + p("t"))
	check := func(what string) {
		t.Helper()
		if md, err := ds.Metadata(ctx); err != nil || md.Description != "kept" {
			t.Errorf("%s: dataset %+v, %v; want its description", what, md, err)
		}
		if md, err := ds.Table("t").Metadata(ctx); err != nil || md.Description != "rows" {
			t.Errorf("%s: table %+v, %v; want its description", what, md, err)
		}
		if got := read("SELECT n, s FROM " + p("t") + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1|c", "2|b"}) {
			t.Errorf("%s: rows %v, want [1|c 2|b]", what, got)
		}
		if got := read("SELECT n FROM " + p("v") + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1", "2"}) {
			t.Errorf("%s: view %v, want [1 2]", what, got)
		}
	}
	if err := ds.DeleteWithContents(ctx); err != nil {
		t.Fatalf("DeleteWithContents: %v", err)
	}
	got, err := svc.Datasets.Undelete(project, ds.DatasetID, &bq.UndeleteDatasetRequest{}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("datasets.undelete: %v", err)
	}
	if got.Description != "kept" || got.DatasetReference == nil || got.DatasetReference.DatasetId != ds.DatasetID {
		t.Errorf("datasets.undelete answered %+v", got)
	}
	check("after datasets.undelete")
	_, err = svc.Datasets.Undelete(project, ds.DatasetID, &bq.UndeleteDatasetRequest{}).Context(ctx).Do()
	wantAPIError(t, "undelete of a dataset that exists", err, http.StatusConflict, "Already Exists")
	_, err = svc.Datasets.Undelete(project, ds.DatasetID+"_never", &bq.UndeleteDatasetRequest{}).Context(ctx).Do()
	wantAPIError(t, "undelete of a dataset never deleted", err, http.StatusNotFound, "Not found")

	// DROP SCHEMA CASCADE, then UNDROP SCHEMA.
	for _, insert := range []bool{false, true} {
		run("DROP SCHEMA " + ds.DatasetID + " CASCADE")
		if err := bqRun(ctx, c, "UNDROP SCHEMA "+ds.DatasetID, insert); err != nil {
			t.Fatalf("UNDROP SCHEMA (insert %v): %v", insert, err)
		}
		check(fmt.Sprintf("after UNDROP SCHEMA (insert %v)", insert))
	}
	if err := bqRun(ctx, c, "UNDROP SCHEMA IF NOT EXISTS "+ds.DatasetID, false); err != nil {
		t.Errorf("UNDROP SCHEMA IF NOT EXISTS of a dataset that exists: %v", err)
	}
	wantAPIError(t, "UNDROP SCHEMA of a dataset never dropped", bqRun(ctx, c, "UNDROP SCHEMA "+ds.DatasetID+"_never", false),
		http.StatusNotFound, "Not found")

	// After the dataset's time travel window, nothing is undeleted.
	short := c.Dataset(base.DatasetID + "_short")
	if err := short.Create(ctx, &bigquery.DatasetMetadata{MaxTimeTravel: 48 * time.Hour}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = short.DeleteWithContents(h.Context()) })
	run("CREATE TABLE `" + short.DatasetID + ".t` (n INT64)")
	if err := short.DeleteWithContents(ctx); err != nil {
		t.Fatalf("DeleteWithContents: %v", err)
	}
	advanceBigQueryClock(t, h, 49*time.Hour)
	_, err = svc.Datasets.Undelete(project, short.DatasetID, &bq.UndeleteDatasetRequest{}).Context(ctx).Do()
	wantAPIError(t, "undelete after the window", err, http.StatusNotFound, "Not found")
}
