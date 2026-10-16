//go:build compat

package compat

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
)

// wantCode fails t unless err is a googleapi error of code whose message
// holds want.
func wantAPIError(t *testing.T, what string, err error, code int, want string) {
	t.Helper()
	var e *googleapi.Error
	if !errors.As(err, &e) || e.Code != code || !strings.Contains(e.Message, want) {
		t.Errorf("%s: got %v, want %d %q", what, err, code, want)
	}
}

// TestBigQueryPartitionPseudoColumns (#1277): _PARTITIONTIME and
// _PARTITIONDATE exist only on an ingestion-time partitioned table. On any
// other the query fails 400 "Unrecognized name", as BigQuery fails it
// (measured first: the emulator answered the time of the query); on an
// ingestion-time partitioned table they read each row's partition
// (TestBigQueryIngestionTime).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryPartitionPseudoColumns(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for _, sql := range []string{
		"CREATE TABLE " + p("plain") + " (n INT64)",
		"INSERT INTO " + p("plain") + " (n) VALUES (1)",
		"CREATE TABLE " + p("bycol") + " (n INT64, d DATE) PARTITION BY d",
		"CREATE TABLE " + p("ingest") + " (n INT64) PARTITION BY _PARTITIONDATE",
		"CREATE TABLE " + p("ingestrows") + " (n INT64) PARTITION BY _PARTITIONDATE",
		"INSERT INTO " + p("ingestrows") + " (n) VALUES (1)",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, insert := range []bool{false, true} {
		wantAPIError(t, "plain", bqRun(ctx, c, "SELECT _PARTITIONTIME FROM "+p("plain"), insert),
			http.StatusBadRequest, "Unrecognized name: _PARTITIONTIME at [1:8]")
		wantAPIError(t, "bycol", bqRun(ctx, c, "SELECT n FROM "+p("bycol")+" WHERE _PARTITIONDATE = CURRENT_DATE()", insert),
			http.StatusBadRequest, "Unrecognized name: _PARTITIONDATE")
		if got := queryRows(t, ctx, c, "", "", "SELECT COUNT(*) FROM "+p("ingestrows")+
			" WHERE _PARTITIONTIME = TIMESTAMP(CURRENT_DATE())"); !reflect.DeepEqual(got, []string{"1"}) {
			t.Errorf("ingestion-time table with rows: %v, want [1]", got)
		}
		if err := bqRun(ctx, c, "SELECT n, _PARTITIONTIME FROM "+p("ingest")+" WHERE _PARTITIONDATE = CURRENT_DATE()", insert); err != nil {
			t.Errorf("empty ingestion-time table: %v", err)
		}
	}
	// Text in a string is not the pseudo-column.
	if got := queryRows(t, ctx, c, "", "", "SELECT '_PARTITIONTIME' AS s FROM "+p("plain")); !reflect.DeepEqual(got, []string{"_PARTITIONTIME"}) {
		t.Errorf("string literal: %v", got)
	}
}

// TestBigQueryPartitionDecorators (#1273): a query reads one partition of a
// day-, month- or integer-range-partitioned table by its decorator, and
// $__NULL__ / $__UNPARTITIONED__; tables.delete of a decorated name
// deletes that partition only, and tables.get of one reads the table with
// the partition's numRows. A decorator on a table that is not
// partitioned is 400. A partition as a job's destination is
// TestBigQueryPartitionDestinations.
// Measured first: every decorated name was 400 "Table not found".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.delete, bigquery.tables.get
func TestBigQueryPartitionDecorators(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for _, sql := range []string{
		"CREATE TABLE " + p("byday") + " (n INT64, d DATE) PARTITION BY d",
		"INSERT INTO " + p("byday") + " (n, d) VALUES (1, DATE '2024-01-01'), (2, DATE '2024-01-01'), (3, DATE '2024-01-02'), (4, NULL)",
		"CREATE TABLE " + p("bymonth") + " (n INT64, ts TIMESTAMP) PARTITION BY TIMESTAMP_TRUNC(ts, MONTH)",
		"INSERT INTO " + p("bymonth") + " (n, ts) VALUES (1, TIMESTAMP '2024-01-31 23:59:59'), (2, TIMESTAMP '2024-02-01 00:00:00')",
		"CREATE TABLE " + p("byrange") + " (n INT64) PARTITION BY RANGE_BUCKET(n, GENERATE_ARRAY(0, 100, 10))",
		"INSERT INTO " + p("byrange") + " (n) VALUES (5), (15), (19), (150)",
		"CREATE TABLE " + p("plain") + " (n INT64)",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{"SELECT n FROM " + p("byday$20240101"), []string{"1", "2"}},
		{"SELECT x.n FROM " + p("byday$20240102") + " AS x", []string{"3"}},
		{"SELECT n FROM " + p("byday$__NULL__"), []string{"4"}},
		{"SELECT n FROM `" + project + "." + ds.DatasetID + ".bymonth$202401`", []string{"1"}},
		{"SELECT n FROM " + p("byrange$10"), []string{"15", "19"}},
		{"SELECT n FROM " + p("byrange$__UNPARTITIONED__"), []string{"150"}},
	} {
		if got := queryRows(t, ctx, c, "", "", tc.sql); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.sql, got, tc.want)
		}
	}
	for _, insert := range []bool{false, true} {
		wantAPIError(t, "plain", bqRun(ctx, c, "SELECT n FROM "+p("plain$20240101"), insert),
			http.StatusBadRequest, "not partitioned")
		wantAPIError(t, "bad decorator", bqRun(ctx, c, "SELECT n FROM "+p("byday$2024"), insert),
			http.StatusBadRequest, "Invalid partition decorator")
	}
	// tables.delete of a partition deletes its rows only.
	if err := ds.Table("byday$20240101").Delete(ctx); err != nil {
		t.Fatalf("delete partition: %v", err)
	}
	if got := queryRows(t, ctx, c, "", "", "SELECT n FROM "+p("byday")); !reflect.DeepEqual(got, []string{"3", "4"}) {
		t.Errorf("after deleting the partition: %v, want [3 4]", got)
	}
	md, err := ds.Table("byday$20240102").Metadata(ctx)
	if err != nil || md.NumRows != 1 || md.TimePartitioning == nil || len(md.Schema) != 2 {
		t.Errorf("tables.get of a partition: %+v, %v; want 1 row, the table's partitioning and schema", md, err)
	}
	wantAPIError(t, "delete of plain$", ds.Table("plain$20240101").Delete(ctx), http.StatusBadRequest, "not partitioned")
}

// TestBigQueryTimeTravelReads (#1276): FOR SYSTEM_TIME AS OF the current
// time reads the table as it is; a time before the table was made, or
// before the dataset's time travel window, fails as BigQuery documents it
// ("Invalid snapshot time ... Cannot read before ..."); any other past time
// reads the table's history (#1460, TestBigQueryTimeTravelHistory). Measured first: every form was
// 400 "FOR SYSTEM_TIME AS OF is not supported".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryTimeTravelReads(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := "`" + ds.DatasetID + ".tt`"
	for _, sql := range []string{"CREATE TABLE " + p + " (n INT64)", "INSERT INTO " + p + " (n) VALUES (1), (2)"} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, sql := range []string{
		"SELECT n FROM " + p + " FOR SYSTEM_TIME AS OF CURRENT_TIMESTAMP()",
		"SELECT x.n FROM " + p + " AS x FOR SYSTEM_TIME AS OF CURRENT_TIMESTAMP() WHERE x.n > 0",
	} {
		if got := queryRows(t, ctx, c, "", "", sql); !reflect.DeepEqual(got, []string{"1", "2"}) {
			t.Errorf("%s: %v, want [1 2]", sql, got)
		}
	}
	for _, insert := range []bool{false, true} {
		wantAPIError(t, "before creation", bqRun(ctx, c, "SELECT n FROM "+p+
			" FOR SYSTEM_TIME AS OF TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 DAY)", insert),
			http.StatusBadRequest, "Invalid snapshot time")
		wantAPIError(t, "out of window", bqRun(ctx, c, "SELECT n FROM "+p+
			" FOR SYSTEM_TIME AS OF TIMESTAMP '2001-01-01'", insert), http.StatusBadRequest, "Cannot read before")
		if err := bqRun(ctx, c, "SELECT n FROM "+p+
			" FOR SYSTEM_TIME AS OF TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 MICROSECOND)", insert); err != nil {
			t.Errorf("in window: %v", err) // the table's history, bigquerytimetravel_test.go
		}
	}
}
