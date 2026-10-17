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

// TestBigQueryUndeleteKeepsMore (#1483): what #1460 left of undelete and
// time travel, as BigQuery documents it
// (https://cloud.google.com/bigquery/docs/time-travel,
// https://cloud.google.com/bigquery/docs/restore-deleted-datasets,
// https://cloud.google.com/bigquery/docs/restore-deleted-tables):
//
//   - FOR SYSTEM_TIME AS OF, and a copy of table@time, at a time before an
//     ALTER TABLE that rebuilt the table (ADD COLUMN) read the rows of then,
//     of the table and of it once deleted;
//   - an undeleted dataset's table keeps its change history: a past time
//     before the deletion reads its rows of then, and APPENDS gives its
//     rows as inserted when they were, not at the restore;
//   - an undelete keeps a table's enable_change_history (CHANGES reads
//     it), its IAM policy and row access policies, a materialized view
//     and a table snapshot;
//   - UNDROP SCHEMA in a multi-statement query, through jobs.query and
//     jobs.insert; 501 when another statement names the dataset.
//
// Measured first: each was 501 or lost (the rebuild 501 "starts it over",
// a restored table's history began at the restore, CHANGES of it was
// refused, its policies, the materialized view and the snapshot were
// gone, and UNDROP SCHEMA in a script was 501). Times are the engine's
// CURRENT_TIMESTAMP(), which follows the front's clock (#1466).
//
// covers: bigquery.datasets.undelete, bigquery.datasets.delete, bigquery.tables.delete, bigquery.jobs.insert, bigquery.jobs.query, bigquery.tables.getIamPolicy, bigquery.tables.setIamPolicy, bigquery.rowAccessPolicies.list
func TestBigQueryUndeleteKeepsMore(t *testing.T) {
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
	copyFrom := func(ds *bigquery.Dataset, src, dest string) error {
		t.Helper()
		job, err := ds.Table(dest).CopierFrom(ds.Table(src)).Run(ctx)
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

	// A past time before an ALTER TABLE rebuild, live and deleted.
	run("CREATE TABLE " + q("rebuilt") + " (n INT64)")
	run("INSERT INTO " + q("rebuilt") + " VALUES (1), (2)")
	t1 := now()
	run("ALTER TABLE " + q("rebuilt") + " ADD COLUMN s STRING")
	run("INSERT INTO " + q("rebuilt") + " VALUES (3, 'c')")
	if got := read("SELECT n, s FROM " + q("rebuilt") + " FOR SYSTEM_TIME AS OF " + lit(t1) + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1|<nil>", "2|<nil>"}) {
		t.Errorf("rebuilt AS OF before the ALTER: %v, want [1|<nil> 2|<nil>]", got)
	}
	if err := copyFrom(base, fmt.Sprintf("rebuilt@%d", t1.UnixMilli()), "rebuilt_t1"); err != nil {
		t.Fatalf("copy rebuilt@t1: %v", err)
	}
	if got := read("SELECT n FROM " + q("rebuilt_t1") + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("rebuilt@t1: %v, want [1 2]", got)
	}
	run("DROP TABLE " + q("rebuilt"))
	if err := copyFrom(base, fmt.Sprintf("rebuilt@%d", t1.UnixMilli()), "rebuilt_back"); err != nil {
		t.Fatalf("copy of the deleted rebuilt@t1: %v", err)
	}
	if got := read("SELECT n FROM " + q("rebuilt_back") + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("deleted rebuilt@t1: %v, want [1 2]", got)
	}

	// A dataset with a table of every kind of thing an undelete keeps.
	ds := c.Dataset(base.DatasetID + "_keeps")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	run("CREATE TABLE " + p("t") + " (n INT64, s STRING) OPTIONS (enable_change_history = TRUE)")
	run("INSERT INTO " + p("t") + " VALUES (1, 'a'), (2, 'b')")
	inserted := now()
	run("UPDATE " + p("t") + " SET s = 'c' WHERE n = 1")
	updated := now()
	run("CREATE MATERIALIZED VIEW " + p("mv") + " AS SELECT n, COUNT(*) AS c FROM " + p("t") + " GROUP BY n")
	run("CREATE SNAPSHOT TABLE " + p("snap") + " CLONE " + p("t"))
	// The policies on a table of their own: a snapshot or materialized
	// view of a table with row access policies is 501 (#1218).
	run("CREATE TABLE " + p("pt") + " (n INT64)")
	pol, err := ds.Table("pt").IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("getIamPolicy: %v", err)
	}
	pol.Add("user:bob@example.com", "roles/bigquery.dataViewer")
	if err := ds.Table("pt").IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatalf("setIamPolicy: %v", err)
	}
	run("CREATE ROW ACCESS POLICY everyone ON " + p("pt") + " GRANT TO ('allAuthenticatedUsers') FILTER USING (TRUE)")
	check := func(what string) {
		t.Helper()
		if got := read("SELECT n, s FROM " + p("t") + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1|c", "2|b"}) {
			t.Errorf("%s: rows %v, want [1|c 2|b]", what, got)
		}
		if got := read("SELECT n, s FROM " + p("t") + " FOR SYSTEM_TIME AS OF " + lit(inserted) + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1|a", "2|b"}) {
			t.Errorf("%s: AS OF before the UPDATE %v, want [1|a 2|b]", what, got)
		}
		if got := read(fmt.Sprintf("SELECT n FROM APPENDS(TABLE %s, NULL, %s) ORDER BY n", p("t"), lit(updated))); !reflect.DeepEqual(got, []string{"1", "2"}) {
			t.Errorf("%s: APPENDS up to before the deletion %v, want [1 2]", what, got)
		}
		if got := read(fmt.Sprintf("SELECT n, _CHANGE_TYPE, _CHANGE_IS_FOR_UPDATE FROM CHANGES(TABLE %s, %s, %s) ORDER BY n, _CHANGE_TYPE", p("t"), lit(inserted), lit(updated))); !reflect.DeepEqual(got, []string{"1|DELETE|true", "1|UPDATE|false"}) {
			// An UPDATE is a DELETE for the update and the UPDATE (BigQuery's change history).
			t.Errorf("%s: CHANGES of the UPDATE %v, want [1|DELETE|true 1|UPDATE|false]", what, got)
		}
		if got, err := ds.Table("pt").IAM().Policy(ctx); err != nil || !got.HasRole("user:bob@example.com", "roles/bigquery.dataViewer") {
			t.Errorf("%s: IAM policy %v, %v; want bob's role", what, got, err)
		}
		if list, err := svc.RowAccessPolicies.List(project, ds.DatasetID, "pt").Context(ctx).Do(); err != nil || len(list.RowAccessPolicies) != 1 {
			t.Errorf("%s: row access policies %+v, %v; want 1", what, list, err)
		}
		if md, err := ds.Table("mv").Metadata(ctx); err != nil || md.Type != bigquery.MaterializedView {
			t.Errorf("%s: materialized view %+v, %v", what, md, err)
		}
		if got := read("SELECT n, c FROM " + p("mv") + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1|1", "2|1"}) {
			t.Errorf("%s: materialized view rows %v, want [1|1 2|1]", what, got)
		}
		if md, err := ds.Table("snap").Metadata(ctx); err != nil || md.Type != bigquery.Snapshot {
			t.Errorf("%s: snapshot %+v, %v", what, md, err)
		}
		if got := read("SELECT n, s FROM " + p("snap") + " ORDER BY n"); !reflect.DeepEqual(got, []string{"1|c", "2|b"}) {
			t.Errorf("%s: snapshot rows %v, want [1|c 2|b]", what, got)
		}
	}
	check("before the deletion")
	if err := ds.DeleteWithContents(ctx); err != nil {
		t.Fatalf("DeleteWithContents: %v", err)
	}
	if _, err := svc.Datasets.Undelete(project, ds.DatasetID, &bq.UndeleteDatasetRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("datasets.undelete: %v", err)
	}
	check("after datasets.undelete")

	// UNDROP SCHEMA in a multi-statement query.
	for _, insert := range []bool{false, true} {
		run("DROP SCHEMA " + ds.DatasetID + " CASCADE")
		if err := bqRun(ctx, c, "SELECT 1; UNDROP SCHEMA "+ds.DatasetID+"; SELECT 2", insert); err != nil {
			t.Fatalf("UNDROP SCHEMA in a script (insert %v): %v", insert, err)
		}
		check(fmt.Sprintf("after UNDROP SCHEMA in a script (insert %v)", insert))
	}
	run("DROP SCHEMA " + ds.DatasetID + " CASCADE")
	wantAPIError(t, "UNDROP SCHEMA in a script that reads the dataset",
		bqRun(ctx, c, "UNDROP SCHEMA "+ds.DatasetID+"; SELECT * FROM "+p("t"), false), http.StatusNotImplemented, "Not implemented")
	if err := bqRun(ctx, c, "UNDROP SCHEMA "+ds.DatasetID, false); err != nil {
		t.Fatalf("UNDROP SCHEMA: %v", err)
	}
}

// TestBigQueryTableDecorators (#1483): a copy job of table@0 and of a
// range decorator, as https://cloud.google.com/bigquery/docs/table-decorators
// (read 2026-10-16) documents them: "Table decorators can be used whenever
// a table is read, such as when copying a table"; "@0 is a special case
// that references the oldest data available for the table"; a range
// decorator references "table data added between" its two times, the
// second "optional and defaults to 'now'", relative when negative. Of a
// table that exists and of one deleted. A range that ends in the future is
// 501 (not documented). Measured first: @0 and every range were 501.
//
// covers: bigquery.jobs.insert
func TestBigQueryTableDecorators(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
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
	copyFrom := func(src, dest string) error {
		t.Helper()
		job, err := base.Table(dest).CopierFrom(base.Table(src)).Run(ctx)
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
	want := func(src, dest string, rows []string) {
		t.Helper()
		if err := copyFrom(src, dest); err != nil {
			t.Fatalf("copy %s: %v", src, err)
		}
		if got := read("SELECT n FROM " + q(dest) + " ORDER BY n"); !reflect.DeepEqual(got, rows) && !(len(got) == 0 && len(rows) == 0) {
			t.Errorf("%s: %v, want %v", src, got, rows)
		}
	}

	run("CREATE TABLE " + q("dec") + " (n INT64)")
	run("INSERT INTO " + q("dec") + " VALUES (1)")
	a := now()
	run("INSERT INTO " + q("dec") + " VALUES (2), (3)")
	run("UPDATE " + q("dec") + " SET n = 30 WHERE n = 3")
	b := now()
	run("INSERT INTO " + q("dec") + " VALUES (4)")

	for _, gone := range []bool{false, true} {
		if gone {
			run("DROP TABLE " + q("dec"))
		}
		sfx := fmt.Sprint("_", gone)
		// The oldest data: the table as it was made, before any row.
		want("dec@0", "zero"+sfx, nil)
		want(fmt.Sprintf("dec@%d-%d", a.UnixMilli()+1, b.UnixMilli()), "range"+sfx, []string{"2", "3"})
		want(fmt.Sprintf("dec@%d-", a.UnixMilli()+1), "open"+sfx, []string{"2", "3", "4"})
		want("dec@-3600000-", "recent"+sfx, []string{"1", "2", "3", "4"})
	}
	wantAPIError(t, "a range ending in the future", copyFrom(fmt.Sprintf("dec@%d-%d", a.UnixMilli(), b.UnixMilli()+3600000), "future"),
		http.StatusNotImplemented, "Not implemented")
}
