//go:build compat

package compat

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
)

// TestBigQueryPartitionLast (#1273), the last of partition decorators:
//   - a schema update through a partition of an ingestion-time partitioned
//     table (ALLOW_FIELD_ADDITION, as BigQuery documents schemaUpdateOptions
//     for "a partition of a table, specified by partition decorators"),
//     where the other partitions' rows keep their _PARTITIONTIME; a column
//     retyped through a partition fails, invalid;
//   - a query job into a partition that is a DML statement or a script
//     fails, invalid, as BigQuery refuses a destination for both;
//   - a dry run into a partition answers the query's statistics and schema
//     and writes nothing;
//   - tabledata.list of a partition takes a nested selectedFields (a.b);
//   - CLONE, SNAPSHOT and RESTORE of a partition, and `$__UNPARTITIONED__`
//     of an ingestion-time partitioned table as a destination, which
//     BigQuery does not document, stay 501.
//
// Measured first: each was 501.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tabledata.list, bigquery.tables.get
func TestBigQueryPartitionLast(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	exec := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	done := func(j *bigquery.Job, err error) error { return waitJob(ctx, j, err) }
	rows := func(sql string) []string {
		t.Helper()
		got := queryRows(t, ctx, c, "", "", sql)
		sort.Strings(got)
		return got
	}

	// A column added through a partition of an ingestion-time table.
	exec("CREATE TABLE " + p("pi") + " (n INT64) PARTITION BY _PARTITIONDATE")
	exec("INSERT INTO " + p("pi") + " (_PARTITIONTIME, n) VALUES (TIMESTAMP '2024-03-01', 1), (TIMESTAMP '2024-03-02', 2)")
	q := c.Query("SELECT 3 AS n, 'x' AS s")
	q.Dst, q.WriteDisposition = ds.Table("pi$20240303"), bigquery.WriteAppend
	if err := done(q.Run(ctx)); err == nil || !strings.Contains(err.Error(), "Cannot add fields") {
		t.Errorf("a new column without ALLOW_FIELD_ADDITION: %v, want invalid", err)
	}
	q.SchemaUpdateOptions = []string{"ALLOW_FIELD_ADDITION"}
	if err := done(q.Run(ctx)); err != nil {
		t.Fatalf("ALLOW_FIELD_ADDITION into pi$20240303: %v", err)
	}
	if got, want := rows("SELECT CONCAT(CAST(n AS STRING), ' ', CAST(_PARTITIONDATE AS STRING), ' ', IFNULL(s, 'null')) FROM "+p("pi")),
		[]string{"1 2024-03-01 null", "2 2024-03-02 null", "3 2024-03-03 x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pi after the column was added: %v, want %v", got, want)
	}
	if md, err := ds.Table("pi").Metadata(ctx); err != nil || len(md.Schema) != 2 || md.TimePartitioning == nil || md.TimePartitioning.Field != "" {
		t.Errorf("pi after the column was added: %v, %+v", err, md)
	}
	q = c.Query("SELECT 'four' AS n, 'y' AS s")
	q.Dst, q.WriteDisposition = ds.Table("pi$20240303"), bigquery.WriteAppend
	if err := done(q.Run(ctx)); err == nil || !strings.Contains(err.Error(), "changed type") {
		t.Errorf("a column retyped through a partition: %v, want invalid", err)
	}

	// DML and scripts take no destination.
	exec("CREATE TABLE " + p("pd") + " (n INT64, d DATE, r STRUCT<a INT64, b STRING>) PARTITION BY d")
	exec("INSERT INTO " + p("pd") + " (n, d, r) VALUES (1, DATE '2024-01-01', STRUCT(10, 'ten')), (2, DATE '2024-01-02', STRUCT(20, 'twenty'))")
	for what, sql := range map[string]string{
		"DML":    "INSERT INTO " + p("pd") + " (n, d) VALUES (3, DATE '2024-01-01')",
		"script": "SELECT 1; SELECT 2",
	} {
		q := c.Query(sql)
		q.Dst = ds.Table("pd$20240101")
		wantAPIError(t, what+" into pd$20240101", done(q.Run(ctx)), http.StatusBadRequest, "destination")
	}

	// A dry run.
	q = c.Query("SELECT n, d, r FROM " + p("pd") + " WHERE d = DATE '2024-01-01'")
	q.Dst, q.WriteDisposition, q.DryRun = ds.Table("pd$20240101"), bigquery.WriteAppend, true
	j, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("dry run into pd$20240101: %v", err)
	}
	st := j.LastStatus()
	if st == nil || st.Statistics == nil {
		t.Fatalf("dry run into pd$20240101: no statistics")
	}
	if qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics); !ok || len(qs.Schema) != 3 || qs.Schema[2].Name != "r" {
		t.Errorf("dry run statistics: %+v, want the result's schema", st.Statistics.Details)
	}
	if got := rows("SELECT COUNT(*) FROM " + p("pd")); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("rows after the dry run: %v, want 2", got)
	}

	// A nested selectedFields of a partition.
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.Tabledata.List(project, ds.DatasetID, "pd$20240102").SelectedFields("n,r.b").Context(ctx).Do()
	if err != nil {
		t.Fatalf("tabledata.list pd$20240102 selectedFields n,r.b: %v", err)
	}
	if len(list.Rows) != 1 || len(list.Rows[0].F) != 2 {
		t.Fatalf("tabledata.list pd$20240102 selectedFields n,r.b: %+v", list.Rows)
	}
	rec, _ := list.Rows[0].F[1].V.(map[string]any)
	cells, _ := rec["f"].([]any)
	if list.Rows[0].F[0].V != "2" || len(cells) != 1 || cells[0].(map[string]any)["v"] != "twenty" {
		t.Errorf("tabledata.list pd$20240102 selectedFields n,r.b: %v %v, want 2 {twenty}", list.Rows[0].F[0].V, list.Rows[0].F[1].V)
	}

	// Not documented by BigQuery: 501.
	for _, op := range []bigquery.TableCopyOperationType{bigquery.CloneOperation, bigquery.SnapshotOperation} {
		cp := ds.Table("pdcopy" + string(op)).CopierFrom(ds.Table("pd$20240101"))
		cp.OperationType = op
		wantAPIError(t, string(op)+" of pd$20240101", done(cp.Run(ctx)), http.StatusNotImplemented, "")
	}
	q = c.Query("SELECT 1 AS n")
	q.Dst, q.WriteDisposition = ds.Table("pi$__UNPARTITIONED__"), bigquery.WriteAppend
	wantAPIError(t, "query into pi$__UNPARTITIONED__", done(q.Run(ctx)), http.StatusNotImplemented, "")
}
