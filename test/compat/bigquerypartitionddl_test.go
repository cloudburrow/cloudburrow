//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryPartitionByAndClusterByReadBack (#1272): the partitioning and
// clustering of CREATE TABLE ... PARTITION BY ... CLUSTER BY, with a column
// list and AS SELECT, through jobs.query and jobs.insert, read back through
// Table.Metadata (tables.get) as BigQuery keeps them: time-unit partitioning
// by a DATE column, DATE(ts), TIMESTAMP_TRUNC, DATETIME_TRUNC and
// DATE_TRUNC, ingestion-time partitioning, and integer-range partitioning by
// RANGE_BUCKET. Measured first: the emulator kept none of them, and made a
// table partitioned by a STRING column, which BigQuery refuses (400 here,
// nothing made). The table's console page shows its partitioning and
// clustering.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryPartitionByAndClusterByReadBack(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	const cols = "(n INT64, d DATE, ts TIMESTAMP, dt DATETIME, s STRING)"
	for _, insert := range []bool{false, true} {
		sfx := "_q"
		if insert {
			sfx = "_j"
		}
		for _, tc := range []struct {
			table, sql string
			time       *bigquery.TimePartitioning
			rng        *bigquery.RangePartitioning
			clustering []string
		}{
			{"bydate", "CREATE TABLE %s " + cols + " PARTITION BY d CLUSTER BY s, n",
				&bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"}, nil, []string{"s", "n"}},
			{"byts", "CREATE TABLE %s " + cols + " PARTITION BY DATE(ts)",
				&bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "ts"}, nil, nil},
			{"byhour", "CREATE OR REPLACE TABLE %s " + cols + " PARTITION BY TIMESTAMP_TRUNC(ts, HOUR) OPTIONS(description = 'x')",
				&bigquery.TimePartitioning{Type: bigquery.HourPartitioningType, Field: "ts"}, nil, nil},
			{"bymonth", "CREATE TABLE %s " + cols + " PARTITION BY DATETIME_TRUNC(dt, MONTH)",
				&bigquery.TimePartitioning{Type: bigquery.MonthPartitioningType, Field: "dt"}, nil, nil},
			{"byyear", "CREATE TABLE %s " + cols + " PARTITION BY DATE_TRUNC(d, YEAR)",
				&bigquery.TimePartitioning{Type: bigquery.YearPartitioningType, Field: "d"}, nil, nil},
			{"ingest", "CREATE TABLE %s " + cols + " PARTITION BY _PARTITIONDATE",
				&bigquery.TimePartitioning{Type: bigquery.DayPartitioningType}, nil, nil},
			{"range", "CREATE TABLE %s " + cols + " PARTITION BY RANGE_BUCKET(n, GENERATE_ARRAY(0, 100, 10)) CLUSTER BY d",
				nil, &bigquery.RangePartitioning{Field: "n", Range: &bigquery.RangePartitioningRange{Start: 0, End: 100, Interval: 10}}, []string{"d"}},
			{"ctas", "CREATE TABLE %s PARTITION BY d CLUSTER BY s AS SELECT DATE '2024-01-02' AS d, 'x' AS s",
				&bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"}, nil, []string{"s"}},
			{"clustered", "CREATE TABLE %s " + cols + " CLUSTER BY ts", nil, nil, []string{"ts"}},
		} {
			sql := fmt.Sprintf(tc.sql, path(tc.table+sfx))
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			md, err := ds.Table(tc.table + sfx).Metadata(ctx)
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			if !reflect.DeepEqual(md.TimePartitioning, tc.time) {
				t.Errorf("%s: time partitioning %+v, want %+v", sql, md.TimePartitioning, tc.time)
			}
			if !reflect.DeepEqual(md.RangePartitioning, tc.rng) {
				t.Errorf("%s: range partitioning %+v, want %+v", sql, md.RangePartitioning, tc.rng)
			}
			var got []string
			if md.Clustering != nil {
				got = md.Clustering.Fields
			}
			if !reflect.DeepEqual(got, tc.clustering) {
				t.Errorf("%s: clustering %v, want %v", sql, got, tc.clustering)
			}
		}

		// Refused before anything is made.
		for _, tc := range []struct{ table, sql, want string }{
			{"bystring", "CREATE TABLE %s (s STRING) PARTITION BY s", "PARTITION BY expression must be"},
			{"daytrunc", "CREATE TABLE %s (d DATE) PARTITION BY DATE_TRUNC(d, DAY)", "PARTITION BY expression must be"},
			{"nocol", "CREATE TABLE %s (d DATE) PARTITION BY x", "Unrecognized name: x"},
			{"float", "CREATE TABLE %s (f FLOAT64) CLUSTER BY f", "Clustering on a column of type FLOAT64"},
			{"five", "CREATE TABLE %s " + cols + " CLUSTER BY n, d, ts, dt, s", "Too many clustering columns"},
		} {
			sql := fmt.Sprintf(tc.sql, path(tc.table+sfx))
			err := bqRun(ctx, c, sql, insert)
			var e *googleapi.Error
			if !errors.As(err, &e) || e.Code != http.StatusBadRequest || !strings.Contains(e.Message, tc.want) {
				t.Errorf("%s: got %v, want 400 %q", sql, err, tc.want)
			}
			if tableExists(t, ctx, ds.Table(tc.table+sfx)) {
				t.Errorf("%s: the table was made", sql)
			}
		}
	}

	// The table's console page shows them (consolebigquery.go).
	addr := consoleAddr(t, h)
	for table, want := range map[string]map[string]string{
		"bydate_q": {"Partitioning": "DAY on d", "Clustered by": "s, n"},
		"range_q":  {"Partitioning": "RANGE_BUCKET on n: 0 to 100, interval 10", "Clustered by": "d"},
	} {
		var page consoleJobPage
		consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery?project="+url.QueryEscape(project)+
			"&name="+ds.DatasetID+"&name="+table, "", &page)
		for label, v := range want {
			if got := page.property("Table", label); got != v {
				t.Errorf("console page of %s: %s %q, want %q", table, label, got, v)
			}
		}
	}
}
