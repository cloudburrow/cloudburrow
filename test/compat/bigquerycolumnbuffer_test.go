//go:build compat

package compat

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryColumnStreamingBuffer (#1273): a row streamed into a table
// partitioned by a column is in the table's streaming buffer, as BigQuery
// documents: "each partition has a streaming buffer"; a row "is initially
// placed in the __UNPARTITIONED__ partition", but a daily table's within
// the last 7 days to 3 days ahead stays in its own partition; a value more
// than 10 years back or 1 year ahead "is rejected". Buffered rows are read
// by queries at once, tables.get answers streamingBuffer and leaves them
// out of numRows, a copy leaves them out, and DML that would change one
// fails ("rows that were recently written ... can't be modified"), while
// the table's other rows stay modifiable. After the flush (20 seconds) the
// rows are in their partitions. The engine's mark of a buffered row is in
// neither the schema nor SELECT *. Measured first: a streamed row was in its
// partition at once, counted and copied, and DML changed it.
//
// covers: bigquery.tabledata.insertAll, bigquery.tables.get, bigquery.tabledata.list, bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryColumnStreamingBuffer(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	rows := func(sql string) []string {
		t.Helper()
		got := queryRows(t, ctx, c, "", "", sql)
		sort.Strings(got)
		return got
	}
	exec := func(sql string) error { return bqRun(ctx, c, sql, true) }
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	type row struct {
		N int64  `bigquery:"n"`
		D string `bigquery:"d"`
	}

	// A daily table partitioned by a DATE column, made by tables.insert.
	day := ds.Table("cbday")
	if err := day.Create(ctx, &bigquery.TableMetadata{
		Schema:           bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}, {Name: "d", Type: bigquery.DateFieldType}},
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"}}); err != nil {
		t.Fatal(err)
	}
	if err := exec("INSERT INTO " + p("cbday") + " (n, d) VALUES (0, DATE '" + today + "')"); err != nil {
		t.Fatal(err)
	}
	if err := day.Inserter().Put(ctx, []row{{1, today}, {2, "2020-01-01"}}); err != nil {
		t.Fatalf("insertAll: %v", err)
	}
	var multi bigquery.PutMultiError
	if err := day.Inserter().Put(ctx, []row{{3, "2010-01-01"}}); !errors.As(err, &multi) || len(multi) != 1 {
		t.Errorf("insertAll of a date more than 10 years back: %v, want the row rejected", err)
	}
	all := rows("SELECT CAST(n AS STRING) FROM " + p("cbday"))
	if !reflect.DeepEqual(all, []string{"0", "1", "2"}) {
		t.Errorf("rows: %v, want [0 1 2]", all)
	}
	read := func(dec string) []string {
		t.Helper()
		return rows("SELECT CAST(n AS STRING) FROM " + p("cbday$"+dec))
	}
	todayDec := now.Format("20060102")
	if got := read(todayDec); !reflect.DeepEqual(got, []string{"0", "1"}) {
		t.Errorf("cbday$%s: %v, want [0 1] (today's buffer is the partition's)", todayDec, got)
	}
	if got := read("__UNPARTITIONED__"); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("cbday$__UNPARTITIONED__: %v, want [2]", got)
	}
	if got := read("20200101"); len(got) != 0 {
		t.Errorf("cbday$20200101 while buffered: %v, want none", got)
	}
	md, err := day.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.StreamingBuffer == nil || md.StreamingBuffer.EstimatedRows != 2 || md.NumRows != 1 || len(md.Schema) != 2 {
		t.Errorf("tables.get: streamingBuffer %+v, numRows %d, %d columns; want 2 buffered, numRows 1, 2 columns",
			md.StreamingBuffer, md.NumRows, len(md.Schema))
	}
	// tabledata.list reads the table's columns only.
	it := day.Read(ctx)
	var vals []bigquery.Value
	if err := it.Next(&vals); err != nil || len(vals) != 2 || len(it.Schema) != 2 {
		t.Errorf("tabledata.list: %v, %v (%d columns); want 2 columns", err, vals, len(it.Schema))
	}

	// A copy leaves the buffer out.
	cpj, err := ds.Table("cbcopy").CopierFrom(day).Run(ctx)
	if err := waitJob(ctx, cpj, err); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("cbcopy")); !reflect.DeepEqual(got, []string{"0"}) {
		t.Errorf("copy: %v, want [0]", got)
	}

	// DML of a buffered row fails; of the others, it runs.
	for _, sql := range []string{
		"UPDATE " + p("cbday") + " SET n = 10 WHERE n = 1",
		"DELETE FROM " + p("cbday") + " t WHERE t.n >= 2",
		"MERGE " + p("cbday") + " t USING (SELECT 2 AS n) s ON t.n = s.n WHEN MATCHED THEN DELETE",
		"TRUNCATE TABLE " + p("cbday"),
	} {
		if err := exec(sql); err == nil || !strings.Contains(err.Error(), "streaming buffer") {
			t.Errorf("%s: %v, want the streaming buffer's error", sql, err)
		}
	}
	if err := exec("UPDATE " + p("cbday") + " SET n = 5 WHERE n = 0"); err != nil {
		t.Errorf("UPDATE of a row not in the buffer: %v", err)
	}

	// A monthly table made by DDL, and an integer-range one: every
	// buffered row is in __UNPARTITIONED__.
	if err := exec("CREATE TABLE " + p("cbmonth") + " (n INT64, ts TIMESTAMP) PARTITION BY TIMESTAMP_TRUNC(ts, MONTH)"); err != nil {
		t.Fatal(err)
	}
	type tsRow struct {
		N  int64     `bigquery:"n"`
		TS time.Time `bigquery:"ts"`
	}
	if err := ds.Table("cbmonth").Inserter().Put(ctx, []tsRow{{1, now}}); err != nil {
		t.Fatalf("insertAll into cbmonth: %v", err)
	}
	month := now.Format("200601")
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("cbmonth$__UNPARTITIONED__")); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("cbmonth$__UNPARTITIONED__: %v, want [1]", got)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("cbmonth$"+month)); len(got) != 0 {
		t.Errorf("cbmonth$%s while buffered: %v, want none", month, got)
	}
	rng := ds.Table("cbrange")
	if err := rng.Create(ctx, &bigquery.TableMetadata{
		Schema: bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}},
		RangePartitioning: &bigquery.RangePartitioning{Field: "n",
			Range: &bigquery.RangePartitioningRange{Start: 0, End: 100, Interval: 10}}}); err != nil {
		t.Fatal(err)
	}
	type nRow struct {
		N int64 `bigquery:"n"`
	}
	if err := rng.Inserter().Put(ctx, []nRow{{5}}); err != nil {
		t.Fatalf("insertAll into cbrange: %v", err)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("cbrange$__UNPARTITIONED__")); !reflect.DeepEqual(got, []string{"5"}) {
		t.Errorf("cbrange$__UNPARTITIONED__: %v, want [5]", got)
	}

	// The buffer flushes.
	var got []string
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if got = read("__UNPARTITIONED__"); len(got) == 0 {
			break
		}
	}
	if len(got) != 0 {
		t.Fatalf("cbday$__UNPARTITIONED__ after the flush: %v, want none", got)
	}
	if got := read("20200101"); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("cbday$20200101 after the flush: %v, want [2]", got)
	}
	if md, err := day.Metadata(ctx); err != nil || md.StreamingBuffer != nil || md.NumRows != 3 {
		t.Errorf("tables.get after the flush: %v, %+v; want no streamingBuffer, numRows 3", err, md)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("cbmonth$"+month)); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("cbmonth$%s after the flush: %v, want [1]", month, got)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("cbrange$0")); !reflect.DeepEqual(got, []string{"5"}) {
		t.Errorf("cbrange$0 after the flush: %v, want [5]", got)
	}
	// Flushed, the rows stay unmodifiable for 30 minutes from their
	// stream (#1370, TestBigQueryDMLOfRecentlyStreamedRows).
	if err := exec("UPDATE " + p("cbday") + " SET n = 10 WHERE n = 1"); err == nil || !strings.Contains(err.Error(), "streaming buffer") {
		t.Errorf("UPDATE after the flush: %v, want the streaming buffer's error", err)
	}
	advanceBigQueryClock(t, h, 31*time.Minute)
	if err := exec("UPDATE " + p("cbday") + " SET n = 10 WHERE n = 1"); err != nil {
		t.Errorf("UPDATE 31 minutes after the stream: %v", err)
	}
}

// TestBigQueryPartitionColumnLeftOut (#1273): a query job into a
// partition whose result leaves out a REQUIRED column of the table, with
// ALLOW_FIELD_RELAXATION, relaxes the column and writes it NULL, as
// BigQuery's example appends a query whose "results will omit the second
// column" with that option; without it the job fails and the table is
// unchanged. Columns in another order stay 501. Measured first: each was
// 501.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get
func TestBigQueryPartitionColumnLeftOut(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	tbl := ds.Table("lo")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{
		Schema: bigquery.Schema{{Name: "d", Type: bigquery.DateFieldType}, {Name: "a", Type: bigquery.StringFieldType, Required: true},
			{Name: "b", Type: bigquery.IntegerFieldType}},
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"}}); err != nil {
		t.Fatal(err)
	}
	query := func(sql string, opts ...string) error {
		q := c.Query(sql)
		q.Dst, q.WriteDisposition, q.SchemaUpdateOptions = ds.Table("lo$20240101"), bigquery.WriteAppend, opts
		j, err := q.Run(ctx)
		return waitJob(ctx, j, err)
	}
	if err := query("SELECT DATE '2024-01-01' AS d, 1 AS b"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("a REQUIRED column left out without ALLOW_FIELD_RELAXATION: %v, want the job to fail", err)
	}
	if err := query("SELECT DATE '2024-01-01' AS d, 2 AS b", "ALLOW_FIELD_RELAXATION"); err != nil {
		t.Fatalf("with ALLOW_FIELD_RELAXATION: %v", err)
	}
	md, err := tbl.Metadata(ctx)
	if err != nil || len(md.Schema) != 3 || md.Schema[1].Required {
		t.Errorf("schema after the relaxing write: %v, %+v; want a NULLABLE", err, md)
	}
	if got := queryRows(t, ctx, c, "", "", "SELECT CONCAT(CAST(b AS STRING), ' ', IFNULL(a, 'null')) FROM "+p("lo")); !reflect.DeepEqual(got, []string{"2 null"}) {
		t.Errorf("rows: %v, want [2 null]", got)
	}
	var apiErr error = query("SELECT DATE '2024-01-01' AS d, 3 AS b, 'x' AS a", "ALLOW_FIELD_RELAXATION")
	if apiErr == nil || !strings.Contains(apiErr.Error(), "order") {
		t.Errorf("columns in another order: %v, want 501", apiErr)
	}
}
