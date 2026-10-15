//go:build compat

package compat

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryStreamingBuffer (#1273): a row streamed into an
// ingestion-time partitioned table is in its streaming buffer first, as
// BigQuery documents: "Recently streamed rows to an ingestion time
// partitioned table temporarily have a NULL value for the _PARTITIONTIME
// pseudocolumn", and the rows are "temporarily placed in the
// __UNPARTITIONED__ partition"; tables.get answers a streamingBuffer
// section, and numRows leaves them out; a copy job does not copy them.
// The buffer flushes in the background (BigQuery gives no SLA; CloudBurrow
// keeps a row 20 seconds), and the rows read their partition's time. A
// daily partitioned table takes a partition decorator, within 31 days in
// the past and 16 in the future; an hourly one does not; a table
// partitioned by a column is 501.
// Measured first: a streamed row read its partition's time at once, and
// tabledata.insertAll into `t$YYYYMMDD` failed.
//
// covers: bigquery.tabledata.insertAll, bigquery.tables.get, bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryStreamingBuffer(t *testing.T) {
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
	type row struct {
		N int64 `bigquery:"n"`
	}
	schema := bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}}
	day := ds.Table("buf")
	if err := day.Create(ctx, &bigquery.TableMetadata{Schema: schema,
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType}}); err != nil {
		t.Fatal(err)
	}
	if err := day.Inserter().Put(ctx, []row{{1}, {2}}); err != nil {
		t.Fatalf("insertAll: %v", err)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("buf") + " WHERE _PARTITIONTIME IS NULL AND _PARTITIONDATE IS NULL"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("rows in the buffer: %v, want [1 2]", got)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("buf$__UNPARTITIONED__")); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("buf$__UNPARTITIONED__: %v, want [1 2]", got)
	}
	md, err := day.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.StreamingBuffer == nil || md.StreamingBuffer.EstimatedRows != 2 || md.StreamingBuffer.OldestEntryTime.IsZero() || md.NumRows != 0 {
		t.Errorf("tables.get: streamingBuffer %+v, numRows %d; want 2 rows buffered, numRows 0", md.StreamingBuffer, md.NumRows)
	}
	// A copy job does not copy the streaming buffer's rows.
	cpj, err := ds.Table("bufcopy").CopierFrom(day).Run(ctx)
	if err := waitJob(ctx, cpj, err); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got := rows("SELECT COUNT(*) FROM " + p("bufcopy")); !reflect.DeepEqual(got, []string{"0"}) {
		t.Errorf("copy of the buffer: %v rows, want 0", got)
	}

	// A decorator: the row goes to that partition once the buffer flushes.
	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1).Format("20060102")
	if err := ds.Table("buf$"+yesterday).Inserter().Put(ctx, []row{{3}}); err != nil {
		t.Fatalf("insertAll into buf$%s: %v", yesterday, err)
	}
	for _, dec := range []string{now.AddDate(0, 0, -40).Format("20060102"), now.AddDate(0, 0, 20).Format("20060102"), "2024013"} {
		wantAPIError(t, "insertAll into buf$"+dec, ds.Table("buf$"+dec).Inserter().Put(ctx, []row{{9}}), http.StatusBadRequest, "")
	}
	hour := ds.Table("bufhour")
	if err := hour.Create(ctx, &bigquery.TableMetadata{Schema: schema,
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.HourPartitioningType}}); err != nil {
		t.Fatal(err)
	}
	wantAPIError(t, "insertAll into an hourly partition", ds.Table("bufhour$"+now.Format("2006010215")).Inserter().Put(ctx, []row{{1}}),
		http.StatusBadRequest, "daily")
	if err := bqRun(ctx, c, "CREATE TABLE "+p("bufcol")+" (n INT64, d DATE) PARTITION BY d", false); err != nil {
		t.Fatal(err)
	}
	wantAPIError(t, "insertAll into a column partition", ds.Table("bufcol$20240101").Inserter().Put(ctx, []row{{1}}),
		http.StatusNotImplemented, "")

	// The buffer flushes.
	want := []string{"1 " + now.Format("2006-01-02"), "2 " + now.Format("2006-01-02"), "3 " + now.AddDate(0, 0, -1).Format("2006-01-02")}
	var got []string
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		got = rows("SELECT CONCAT(CAST(n AS STRING), ' ', IFNULL(CAST(_PARTITIONDATE AS STRING), 'null')) FROM " + p("buf"))
		if reflect.DeepEqual(got, want) {
			break
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after the buffer flushed: %v, want %v", got, want)
	}
	if md, err := day.Metadata(ctx); err != nil || md.StreamingBuffer != nil || md.NumRows != 3 {
		t.Errorf("tables.get after the flush: %v, %+v; want no streamingBuffer, numRows 3", err, md)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("buf$"+yesterday)); !reflect.DeepEqual(got, []string{"3"}) {
		t.Errorf("buf$%s: %v, want [3]", yesterday, got)
	}
}

// TestBigQueryPartitionDestinationsRest (#1273): `$__NULL__` and
// `$__UNPARTITIONED__` of a column-partitioned table as a query or load
// job's destination take only rows of that partition, WRITE_TRUNCATE_DATA
// into a partition replaces its rows and keeps the table's schema, and a
// copy job of an ingestion-time partitioned table keeps each row's
// _PARTITIONTIME and, into a table it makes, the partitioning. An
// ingestion-time table's `$__UNPARTITIONED__`, its streaming buffer, is
// 501 as a destination. Measured first: each was 501, and a copy gave
// every row the copy's time.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get
func TestBigQueryPartitionDestinationsRest(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	exec := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rows := func(sql string) []string {
		t.Helper()
		got := queryRows(t, ctx, c, "", "", sql)
		sort.Strings(got)
		return got
	}
	query := func(sql, dst string, write bigquery.TableWriteDisposition) error {
		q := c.Query(sql)
		q.Dst, q.WriteDisposition = ds.Table(dst), write
		j, err := q.Run(ctx)
		return waitJob(ctx, j, err)
	}
	exec("CREATE TABLE " + p("pd") + " (n INT64, d DATE) PARTITION BY d")
	exec("INSERT INTO " + p("pd") + " (n, d) VALUES (1, DATE '2024-01-01'), (2, DATE '2024-01-02')")
	all := func() string {
		return strings.Join(rows("SELECT CONCAT(CAST(n AS STRING), ' ', IFNULL(CAST(d AS STRING), 'null')) FROM "+p("pd")), ",")
	}

	if err := query("SELECT 3 AS n, CAST(NULL AS DATE) AS d", "pd$__NULL__", bigquery.WriteAppend); err != nil {
		t.Fatalf("query into pd$__NULL__: %v", err)
	}
	if err := query("SELECT 4 AS n, DATE '1950-06-01' AS d", "pd$__UNPARTITIONED__", bigquery.WriteAppend); err != nil {
		t.Fatalf("query into pd$__UNPARTITIONED__: %v", err)
	}
	if err := query("SELECT 5 AS n, DATE '2024-01-01' AS d", "pd$__NULL__", bigquery.WriteAppend); err == nil ||
		!strings.Contains(err.Error(), "invalid") {
		t.Errorf("a dated row into pd$__NULL__: %v, want invalid", err)
	}
	ld := ds.Table("pd$__NULL__").LoaderFrom(bigquery.NewReaderSource(bytes.NewReader([]byte("6,\n"))))
	ld.WriteDisposition = bigquery.WriteAppend
	j, err := ld.Run(ctx)
	if err := waitJob(ctx, j, err); err != nil {
		t.Fatalf("load into pd$__NULL__: %v", err)
	}
	if got, want := all(), "1 2024-01-01,2 2024-01-02,3 null,4 1950-06-01,6 null"; got != want {
		t.Errorf("after the writes: %s, want %s", got, want)
	}
	if err := query("SELECT 7 AS n, DATE '2024-01-01' AS d", "pd$20240101", "WRITE_TRUNCATE_DATA"); err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA into pd$20240101: %v", err)
	}
	if got, want := all(), "2 2024-01-02,3 null,4 1950-06-01,6 null,7 2024-01-01"; got != want {
		t.Errorf("after WRITE_TRUNCATE_DATA: %s, want %s", got, want)
	}
	exec("CREATE TABLE " + p("pi") + " (n INT64) PARTITION BY _PARTITIONDATE")
	wantAPIError(t, "query into pi$__UNPARTITIONED__", query("SELECT 1 AS n", "pi$__UNPARTITIONED__", bigquery.WriteAppend),
		http.StatusNotImplemented, "")

	// A copy keeps _PARTITIONTIME.
	exec("INSERT INTO " + p("pi") + " (_PARTITIONTIME, n) VALUES (TIMESTAMP '2024-03-01', 1), (TIMESTAMP '2024-03-02', 2)")
	cp := ds.Table("picopy").CopierFrom(ds.Table("pi"))
	j, err = cp.Run(ctx)
	if err := waitJob(ctx, j, err); err != nil {
		t.Fatalf("copy into a new table: %v", err)
	}
	md, err := ds.Table("picopy").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.TimePartitioning == nil || md.TimePartitioning.Field != "" {
		t.Errorf("picopy partitioning: %+v, want by ingestion time", md.TimePartitioning)
	}
	exec("CREATE TABLE " + p("piappend") + " (n INT64) PARTITION BY _PARTITIONDATE")
	cp = ds.Table("piappend").CopierFrom(ds.Table("pi"))
	cp.WriteDisposition = bigquery.WriteAppend
	j, err = cp.Run(ctx)
	if err := waitJob(ctx, j, err); err != nil {
		t.Fatalf("copy into an existing table: %v", err)
	}
	for _, table := range []string{"picopy", "piappend"} {
		if got, want := rows("SELECT CONCAT(CAST(n AS STRING), ' ', CAST(_PARTITIONDATE AS STRING)) FROM "+p(table)),
			[]string{"1 2024-03-01", "2 2024-03-02"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", table, got, want)
		}
	}
}

// waitJob waits for a job that was started with err.
func waitJob(ctx context.Context, j *bigquery.Job, err error) error {
	if err != nil {
		return err
	}
	st, err := j.Wait(ctx)
	if err != nil {
		return err
	}
	return st.Err()
}
