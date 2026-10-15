//go:build compat

package compat

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryPartitionDestinations (#1273): tabledata.list of `t$P`
// (Table.Read) reads that partition's rows only, a page at a time; a query
// job whose destination is `t$P` appends to the partition, WRITE_TRUNCATE
// replaces that partition's rows and no other's, WRITE_EMPTY into a
// partition with rows fails "duplicate", and a result with a row of
// another partition fails the job and changes nothing, as BigQuery
// documents ("the partition ID specified in the decorator must match the
// data being written. Otherwise, an error occurs"). Load and copy jobs
// into a partition do the same, and a copy job reads a partition as its
// source. A partition of an ingestion-time partitioned table is 501.
// Measured first: every decorated name was "Table not found".
//
// covers: bigquery.tabledata.list, bigquery.tables.get, bigquery.jobs.insert, bigquery.jobs.get
func TestBigQueryPartitionDestinations(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for _, sql := range []string{
		"CREATE TABLE " + p("byday") + " (n INT64, d DATE) PARTITION BY d",
		"INSERT INTO " + p("byday") + " (n, d) VALUES (1, DATE '2024-01-01'), (2, DATE '2024-01-01'), (3, DATE '2024-01-02'), (4, NULL)",
		"CREATE TABLE " + p("byrange") + " (n INT64, s STRING) PARTITION BY RANGE_BUCKET(n, GENERATE_ARRAY(0, 100, 10))",
		"INSERT INTO " + p("byrange") + " (n, s) VALUES (5, 'a'), (15, 'b'), (19, 'c'), (150, 'd')",
		"CREATE TABLE " + p("ingest") + " (n INT64) PARTITION BY _PARTITIONDATE",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	read := func(table string, page int) []string {
		t.Helper()
		it := ds.Table(table).Read(ctx)
		if page > 0 {
			it.PageInfo().MaxSize = page
		}
		var got []string
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("read %s: %v", table, err)
			}
			var b strings.Builder
			for _, v := range row {
				fmt.Fprint(&b, v)
			}
			got = append(got, b.String())
		}
		if total := it.TotalRows; total != uint64(len(got)) {
			t.Errorf("read %s: TotalRows %d, read %d", table, total, len(got))
		}
		sort.Strings(got)
		return got
	}
	// tabledata.list of a partition.
	if got, want := read("byday$20240101", 0), []string{"12024-01-01", "22024-01-01"}; !reflect.DeepEqual(got, want) {
		t.Errorf("byday$20240101: %v, want %v", got, want)
	}
	if got, want := read("byday$20240101", 1), []string{"12024-01-01", "22024-01-01"}; !reflect.DeepEqual(got, want) {
		t.Errorf("byday$20240101 a row a page: %v, want %v", got, want)
	}
	if got, want := read("byrange$10", 0), []string{"15b", "19c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("byrange$10: %v, want %v", got, want)
	}
	if got, want := read("byrange$__UNPARTITIONED__", 0), []string{"150d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("byrange$__UNPARTITIONED__: %v, want %v", got, want)
	}
	if got := read("byday$20240105", 0); len(got) != 0 {
		t.Errorf("an empty partition: %v", got)
	}
	it := ds.Table("ingest$20240101").Read(ctx)
	var row []bigquery.Value
	wantAPIError(t, "ingestion-time partition", it.Next(&row), http.StatusNotImplemented, "ingestion-time")

	run := func(sql, dst, write string) error {
		q := c.Query(sql)
		q.Dst = ds.Table(dst)
		q.WriteDisposition = bigquery.TableWriteDisposition(write)
		j, err := q.Run(ctx)
		if err != nil {
			return err
		}
		st, err := j.Wait(ctx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	all := func(table string) []string {
		got := queryRows(t, ctx, c, "", "", "SELECT CONCAT(CAST(n AS STRING), ' ', IFNULL(CAST(d AS STRING), 'NULL')) FROM "+p(table))
		sort.Strings(got)
		return got
	}
	// WRITE_APPEND adds to the partition.
	if err := run("SELECT 5 AS n, DATE '2024-01-01' AS d", "byday$20240101", "WRITE_APPEND"); err != nil {
		t.Fatalf("append: %v", err)
	}
	// WRITE_TRUNCATE replaces that partition's rows only.
	if err := run("SELECT 6 AS n, DATE '2024-01-01' AS d UNION ALL SELECT 7, DATE '2024-01-01'", "byday$20240101", "WRITE_TRUNCATE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	want := []string{"3 2024-01-02", "4 NULL", "6 2024-01-01", "7 2024-01-01"}
	if got := all("byday"); !reflect.DeepEqual(got, want) {
		t.Errorf("after append and truncate: %v, want %v", got, want)
	}
	// A row of another partition fails the job; nothing changes.
	wantJobError(t, "another partition", run("SELECT 8 AS n, DATE '2024-01-03' AS d", "byday$20240101", "WRITE_TRUNCATE"),
		"invalid", "different partitions")
	// WRITE_EMPTY into a partition with rows fails; into an empty one it writes.
	wantJobError(t, "WRITE_EMPTY", run("SELECT 9 AS n, DATE '2024-01-02' AS d", "byday$20240102", "WRITE_EMPTY"),
		"duplicate", "Already Exists")
	if err := run("SELECT 10 AS n, DATE '2024-01-04' AS d", "byday$20240104", "WRITE_EMPTY"); err != nil {
		t.Fatalf("WRITE_EMPTY into an empty partition: %v", err)
	}
	want = append(want, "10 2024-01-04")
	sort.Strings(want)
	if got := all("byday"); !reflect.DeepEqual(got, want) {
		t.Errorf("after the refused jobs: %v, want %v", got, want)
	}
	// An integer-range partition, replaced.
	if err := run("SELECT 11 AS n, 'z' AS s", "byrange$10", "WRITE_TRUNCATE"); err != nil {
		t.Fatalf("range truncate: %v", err)
	}
	if got, want := read("byrange", 0), []string{"11z", "150d", "5a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("byrange after truncate: %v, want %v", got, want)
	}
	// An ingestion-time partition is not built.
	wantAPIError(t, "ingestion-time destination", run("SELECT 1 AS n", "ingest$20240101", "WRITE_APPEND"),
		http.StatusNotImplemented, "ingestion-time")

	// Load jobs into a partition (an upload, a CSV with the table's schema).
	load := func(csv, dst, write string) error {
		src := bigquery.NewReaderSource(bytes.NewReader([]byte(csv)))
		ld := ds.Table(dst).LoaderFrom(src)
		ld.WriteDisposition = bigquery.TableWriteDisposition(write)
		j, err := ld.Run(ctx)
		if err != nil {
			return err
		}
		st, err := j.Wait(ctx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	if err := load("20,2024-01-02\n21,2024-01-02\n", "byday$20240102", "WRITE_TRUNCATE"); err != nil {
		t.Fatalf("load truncate: %v", err)
	}
	if err := load("22,2024-01-02\n", "byday$20240102", "WRITE_APPEND"); err != nil {
		t.Fatalf("load append: %v", err)
	}
	wantJobError(t, "load of another partition", load("23,2024-01-09\n", "byday$20240102", "WRITE_APPEND"),
		"invalid", "different partitions")
	wantJobError(t, "load WRITE_EMPTY", load("24,2024-01-02\n", "byday$20240102", "WRITE_EMPTY"),
		"duplicate", "Already Exists")
	want = []string{"10 2024-01-04", "20 2024-01-02", "21 2024-01-02", "22 2024-01-02", "4 NULL", "6 2024-01-01", "7 2024-01-01"}
	if got := all("byday"); !reflect.DeepEqual(got, want) {
		t.Errorf("after the loads: %v, want %v", got, want)
	}

	// Copy jobs: a partition into another table's partition, replacing it.
	if err := bqRun(ctx, c, "CREATE TABLE "+p("copy")+" (n INT64, d DATE) PARTITION BY d", false); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+p("copy")+" (n, d) VALUES (30, DATE '2024-01-02'), (31, DATE '2024-01-01')", false); err != nil {
		t.Fatal(err)
	}
	copyJob := func(dst, src, write string) error {
		cp := ds.Table(dst).CopierFrom(ds.Table(src))
		cp.WriteDisposition = bigquery.TableWriteDisposition(write)
		j, err := cp.Run(ctx)
		if err != nil {
			return err
		}
		st, err := j.Wait(ctx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	if err := copyJob("copy$20240102", "byday$20240102", "WRITE_TRUNCATE"); err != nil {
		t.Fatalf("copy truncate: %v", err)
	}
	if got, want := all("copy"), []string{"20 2024-01-02", "21 2024-01-02", "22 2024-01-02", "31 2024-01-01"}; !reflect.DeepEqual(got, want) {
		t.Errorf("copy after copying a partition: %v, want %v", got, want)
	}
	wantJobError(t, "copy of another partition", copyJob("copy$20240101", "byday$20240102", "WRITE_APPEND"),
		"invalid", "different partitions")
	wantJobError(t, "copy WRITE_EMPTY", copyJob("copy$20240101", "byday$20240101", "WRITE_EMPTY"),
		"duplicate", "Already Exists")

	// No scratch table is left in the dataset.
	tit := ds.Tables(ctx)
	var names []string
	for {
		tb, err := tit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("tables.list: %v", err)
		}
		names = append(names, tb.TableID)
	}
	sort.Strings(names)
	if want := []string{"byday", "byrange", "copy", "ingest"}; !reflect.DeepEqual(names, want) {
		t.Errorf("tables: %v, want %v", names, want)
	}
}

// wantJobError fails t unless err is the failure of a job, of reason, whose
// message holds want: the Go client returns a job that jobs.insert
// answers as done with an errorResult as a googleapi error, and one that
// fails later as a *bigquery.Error.
func wantJobError(t *testing.T, what string, err error, reason, want string) {
	t.Helper()
	var e *bigquery.Error
	var g *googleapi.Error
	switch {
	case errors.As(err, &e) && e.Reason == reason && strings.Contains(e.Message, want):
	case errors.As(err, &g) && len(g.Errors) > 0 && g.Errors[0].Reason == reason && strings.Contains(g.Message, want):
	default:
		t.Errorf("%s: got %v, want %s %q", what, err, reason, want)
	}
}

// TestConsoleBigQueryPartitions (#1273): a partitioned table's console
// page lists its partitions with their rows, and each opens the table's
// page by the partition's decorated name, whose Preview (tabledata.list)
// shows that partition's rows only.
//
// covers: bigquery.tabledata.list, bigquery.tables.get, bigquery.jobs.query
func TestConsoleBigQueryPartitions(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for _, sql := range []string{
		"CREATE TABLE " + p("byday") + " (n INT64, d DATE) PARTITION BY d",
		"INSERT INTO " + p("byday") + " (n, d) VALUES (1, DATE '2024-01-01'), (2, DATE '2024-01-01'), (3, DATE '2024-01-02'), (4, NULL)",
		"CREATE TABLE " + p("byrange") + " (n INT64) PARTITION BY RANGE_BUCKET(n, GENERATE_ARRAY(0, 100, 10))",
		"INSERT INTO " + p("byrange") + " (n) VALUES (5), (15), (19), (150)",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	q := "?project=" + url.QueryEscape(project)
	page := func(table string) consoleDetail {
		var d consoleDetail
		consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery"+q+"&name="+ds.DatasetID+"&name="+url.QueryEscape(table), "", &d)
		if d.Unavailable != "" {
			t.Fatalf("%s: %s", table, d.Unavailable)
		}
		return d
	}
	for table, want := range map[string][]string{
		"byday":   {"20240101:2", "20240102:1", "__NULL__:1"},
		"byrange": {"0:1", "10:2", "__UNPARTITIONED__:1"},
	} {
		var got []string
		for _, it := range page(table).section(t, "partitions").Items {
			got = append(got, it.Name+":"+it.Fields["Rows"])
			if len(it.Opens) != 2 || it.Opens[1] != table+"$"+it.Name {
				t.Errorf("%s partition %s opens %v", table, it.Name, it.Opens)
			}
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s partitions: %v, want %v", table, got, want)
		}
	}
	part := page("byday$20240101")
	var rows []string
	for _, it := range part.section(t, "preview").Items {
		rows = append(rows, it.Name)
	}
	sort.Strings(rows)
	if !reflect.DeepEqual(rows, []string{"1", "2"}) {
		t.Errorf("the partition's preview shows %v, want [1 2]", rows)
	}
}
