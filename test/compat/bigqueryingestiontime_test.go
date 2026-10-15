//go:build compat

package compat

import (
	"bytes"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryIngestionTime (#1277, #1273): an ingestion-time partitioned
// table keeps each row's ingestion time, truncated to its partitioning
// unit, whatever writes it: DML, tabledata.insertAll (once its streaming
// buffer flushes, #1273), a load. Its
// _PARTITIONTIME reads the start of the row's partition and _PARTITIONDATE
// (by day only) its date, as BigQuery documents
// (https://cloud.google.com/bigquery/docs/querying-partitioned-tables);
// SELECT * and the schema leave them out; an INSERT may give
// _PARTITIONTIME; a row filter on it reads one partition; a partition
// decorator reads that partition, and a query, load or copy job into one
// writes its rows with the partition's time; INFORMATION_SCHEMA.PARTITIONS
// counts the partitions. Measured first: every row read the time of the
// query (#1277), then the front refused them 501.
//
// covers: bigquery.tables.insert, bigquery.tables.get, bigquery.tabledata.insertAll, bigquery.tabledata.list, bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryIngestionTime(t *testing.T) {
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
	wait := func(j *bigquery.Job, err error) error {
		if err != nil {
			return err
		}
		st, err := j.Wait(ctx)
		if err != nil {
			return err
		}
		return st.Err()
	}

	// tables.insert with timePartitioning and no field, written by
	// tabledata.insertAll and by DML.
	day := ds.Table("ingday")
	if err := day.Create(ctx, &bigquery.TableMetadata{
		Schema:           bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}},
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType},
	}); err != nil {
		t.Fatal(err)
	}
	type row struct {
		N int64 `bigquery:"n"`
	}
	if err := day.Inserter().Put(ctx, []row{{1}, {2}}); err != nil {
		t.Fatalf("insertAll: %v", err)
	}
	exec("INSERT INTO " + p("ingday") + " (n) VALUES (3)")
	exec("INSERT INTO " + p("ingday") + " (_PARTITIONTIME, n) VALUES (TIMESTAMP '2024-01-01', 4)")

	md, err := day.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Schema) != 1 || md.Schema[0].Name != "n" {
		t.Errorf("schema: %v, want only n", md.Schema)
	}
	if got := rows("SELECT TO_JSON_STRING(t) FROM " + p("ingday") + " AS t WHERE n = 1"); !reflect.DeepEqual(got, []string{`{"n":1}`}) {
		t.Errorf("SELECT *: %v, want only n", got)
	}
	// The streamed rows are in the streaming buffer, _PARTITIONTIME NULL,
	// until it flushes (TestBigQueryStreamingBuffer, #1273).
	if got, want := rows("SELECT CONCAT(CAST(n AS STRING), ' ', IFNULL(CAST(_PARTITIONTIME = TIMESTAMP(CURRENT_DATE()) AS STRING), 'null'), ' ', "+
		"IFNULL(CAST(_PARTITIONDATE = CURRENT_DATE() AS STRING), 'null')) FROM "+p("ingday")+" WHERE n < 4"),
		[]string{"1 null null", "2 null null", "3 true true"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows of today: %v, want %v", got, want)
	}
	if got := rows("SELECT CONCAT(CAST(n AS STRING), ' ', FORMAT_TIMESTAMP('%F %T', _PARTITIONTIME), ' ', CAST(_PARTITIONDATE AS STRING)) FROM " +
		p("ingday") + " WHERE _PARTITIONTIME = TIMESTAMP('2024-01-01')"); !reflect.DeepEqual(got, []string{"4 2024-01-01 00:00:00 2024-01-01"}) {
		t.Errorf("WHERE _PARTITIONTIME = TIMESTAMP('2024-01-01'): %v", got)
	}

	// By hour: _PARTITIONTIME is the hour's start; there is no _PARTITIONDATE.
	exec("CREATE TABLE " + p("inghour") + " (n INT64) PARTITION BY TIMESTAMP_TRUNC(_PARTITIONTIME, HOUR)")
	exec("INSERT INTO " + p("inghour") + " (n) VALUES (1)")
	if got := rows("SELECT CAST(_PARTITIONTIME = TIMESTAMP_TRUNC(CURRENT_TIMESTAMP(), HOUR) AS STRING) FROM " + p("inghour")); !reflect.DeepEqual(got, []string{"true"}) {
		t.Errorf("hour: %v, want [true]", got)
	}
	wantAPIError(t, "hour _PARTITIONDATE", bqRun(ctx, c, "SELECT _PARTITIONDATE FROM "+p("inghour"), false),
		http.StatusBadRequest, "Unrecognized name: _PARTITIONDATE")

	// A partition decorator reads that partition.
	readN := func(table string) []string {
		t.Helper()
		it := ds.Table(table).Read(ctx)
		var got []string
		for {
			var r row
			err := it.Next(&r)
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("read %s: %v", table, err)
			}
			got = append(got, strconv.FormatInt(r.N, 10))
		}
		sort.Strings(got)
		return got
	}
	if got := readN("ingday$20240101"); !reflect.DeepEqual(got, []string{"4"}) {
		t.Errorf("ingday$20240101: %v, want [4]", got)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("ingday$20240101")); !reflect.DeepEqual(got, []string{"4"}) {
		t.Errorf("SELECT FROM ingday$20240101: %v, want [4]", got)
	}

	// A query job into a partition writes its rows with the partition's time.
	q := c.Query("SELECT 5 AS n")
	q.Dst, q.WriteDisposition = ds.Table("ingday$20240102"), bigquery.WriteAppend
	if err := wait(q.Run(ctx)); err != nil {
		t.Fatalf("query into ingday$20240102: %v", err)
	}
	q = c.Query("SELECT 6 AS n")
	q.Dst, q.WriteDisposition = ds.Table("ingday$20240101"), bigquery.WriteTruncate
	if err := wait(q.Run(ctx)); err != nil {
		t.Fatalf("query truncating ingday$20240101: %v", err)
	}
	// A load into a partition.
	ld := ds.Table("ingday$20240103").LoaderFrom(bigquery.NewReaderSource(bytes.NewReader([]byte("7\n"))))
	if err := wait(ld.Run(ctx)); err != nil {
		t.Fatalf("load into ingday$20240103: %v", err)
	}
	// A copy keeps each row's partition.
	exec("CREATE TABLE " + p("ingcopy") + " (n INT64) PARTITION BY _PARTITIONDATE")
	cp := ds.Table("ingcopy").CopierFrom(ds.Table("ingday$20240102"))
	if err := wait(cp.Run(ctx)); err != nil {
		t.Fatalf("copy of ingday$20240102: %v", err)
	}
	if got, want := rows("SELECT CONCAT(CAST(n AS STRING), ' ', CAST(_PARTITIONDATE AS STRING)) FROM "+p("ingday")+
		" WHERE _PARTITIONTIME < TIMESTAMP '2025-01-01'"), []string{"5 2024-01-02", "6 2024-01-01", "7 2024-01-03"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the writes: %v, want %v", got, want)
	}
	if got := rows("SELECT CONCAT(CAST(n AS STRING), ' ', CAST(_PARTITIONDATE AS STRING)) FROM " + p("ingcopy")); !reflect.DeepEqual(got, []string{"5 2024-01-02"}) {
		t.Errorf("ingcopy: %v, want [5 2024-01-02]", got)
	}

	// INFORMATION_SCHEMA.PARTITIONS.
	if got, want := rows("SELECT CONCAT(partition_id, ' ', CAST(total_rows AS STRING)) FROM "+p("INFORMATION_SCHEMA.PARTITIONS")+
		" WHERE table_name = 'ingday' AND partition_id < '2025'"), []string{"20240101 1", "20240102 1", "20240103 1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PARTITIONS: %v, want %v", got, want)
	}
}
