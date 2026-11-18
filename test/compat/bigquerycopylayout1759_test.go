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

// TestBigQueryCopyLayout1759: a copy job and the destination's
// partitioning and clustering, as managing-partitioned-tables documents
// them ("Copy a partitioned table", read 2026-11-17):
//   - into a new table, "all of the partitioning information is copied with
//     the table" and "all of the clustering information": a DATE column
//     partitioned table with a partition expiration, require_partition_filter
//     and clustering, an integer-range partitioned one and an HOUR
//     ingestion-time one each give a new table partitioned as they are, with
//     the schema's descriptions, and INFORMATION_SCHEMA.PARTITIONS lists the
//     range-partitioned copy's partitions;
//   - several sources with the same partitioning are copied into one table
//     partitioned so; "a mixture of partitioned and non-partitioned tables",
//     or sources clustered differently, fail the job;
//   - into a table that exists: partitioned otherwise ("must match"), a
//     non-partitioned source into a column-partitioned table ("only
//     supported for ingestion-time partitioning"), and clustered otherwise
//     fail the job; a non-partitioned source into an ingestion-time
//     partitioned table lands in today's partition; a partitioned source into
//     a non-partitioned table, WRITE_TRUNCATE, "remains unpartitioned", but
//     fails the job, reason "invalid" (error-messages), when the source is
//     clustered: "the cluster specifications ... must match" (#1762);
//   - a SNAPSHOT does not keep the partition expiration
//     (table-snapshots-intro).
//
// Measured first: the new table was not partitioned (#1759).
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get
func TestBigQueryCopyLayout1759(t *testing.T) {
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
	copyJob := func(dst string, write bigquery.TableWriteDisposition, op bigquery.TableCopyOperationType, srcs ...string) error {
		t.Helper()
		tables := make([]*bigquery.Table, len(srcs))
		for i, s := range srcs {
			tables[i] = ds.Table(s)
		}
		cp := ds.Table(dst).CopierFrom(tables...)
		cp.WriteDisposition, cp.OperationType = write, op
		j, err := cp.Run(ctx)
		return waitJob(ctx, j, err)
	}
	meta := func(table string) *bigquery.TableMetadata {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("tables.get %s: %v", table, err)
		}
		return md
	}

	// A DATE column-partitioned, clustered table with an expiration and
	// require_partition_filter, and descriptions.
	schema := bigquery.Schema{
		{Name: "n", Type: bigquery.IntegerFieldType, Description: "the number"},
		{Name: "d", Type: bigquery.DateFieldType, Description: "the day"},
		{Name: "s", Type: bigquery.StringFieldType},
	}
	if err := ds.Table("pd").Create(ctx, &bigquery.TableMetadata{
		Schema:                 schema,
		TimePartitioning:       &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d", Expiration: 400 * 24 * time.Hour},
		Clustering:             &bigquery.Clustering{Fields: []string{"s"}},
		RequirePartitionFilter: true,
	}); err != nil {
		t.Fatalf("create pd: %v", err)
	}
	exec("INSERT INTO " + p("pd") + " (n, d, s) VALUES (1, CURRENT_DATE(), 'a'), (2, DATE_SUB(CURRENT_DATE(), INTERVAL 1 DAY), 'b')")
	if err := copyJob("pdcopy", "", "", "pd"); err != nil {
		t.Fatalf("copy pd into a new table: %v", err)
	}
	md := meta("pdcopy")
	if tp := md.TimePartitioning; tp == nil || tp.Field != "d" || tp.Type != bigquery.DayPartitioningType || tp.Expiration != 400*24*time.Hour {
		t.Errorf("pdcopy timePartitioning %+v, want DAY on d, 400 days", tp)
	}
	if md.Clustering == nil || !reflect.DeepEqual(md.Clustering.Fields, []string{"s"}) {
		t.Errorf("pdcopy clustering %+v, want [s]", md.Clustering)
	}
	if !md.RequirePartitionFilter {
		t.Errorf("pdcopy requirePartitionFilter false, want true")
	}
	if len(md.Schema) != 3 || md.Schema[0].Description != "the number" || md.Schema[1].Description != "the day" {
		t.Errorf("pdcopy schema %+v, want pd's descriptions", md.Schema)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("pdcopy") + " WHERE d >= DATE_SUB(CURRENT_DATE(), INTERVAL 7 DAY)"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("pdcopy rows %v, want 1 2", got)
	}

	// Integer-range and HOUR ingestion-time partitioning.
	exec("CREATE TABLE " + p("pr") + " (n INT64) PARTITION BY RANGE_BUCKET(n, GENERATE_ARRAY(0, 100, 10))")
	exec("INSERT INTO " + p("pr") + " (n) VALUES (5), (15)")
	if err := copyJob("prcopy", "", "", "pr"); err != nil {
		t.Fatalf("copy pr into a new table: %v", err)
	}
	if rp := meta("prcopy").RangePartitioning; rp == nil || rp.Field != "n" || rp.Range == nil || rp.Range.Start != 0 ||
		rp.Range.End != 100 || rp.Range.Interval != 10 {
		t.Errorf("prcopy rangePartitioning %+v, want n 0..100 by 10", rp)
	}
	if got := rows("SELECT partition_id FROM " + p("INFORMATION_SCHEMA.PARTITIONS") +
		" WHERE table_name = 'prcopy'"); !reflect.DeepEqual(got, []string{"0", "10"}) {
		t.Errorf("prcopy partitions %v, want 0 10", got)
	}
	if err := ds.Table("ph").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}},
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.HourPartitioningType}}); err != nil {
		t.Fatalf("create ph: %v", err)
	}
	if err := copyJob("phcopy", "", "", "ph"); err != nil {
		t.Fatalf("copy ph into a new table: %v", err)
	}
	if tp := meta("phcopy").TimePartitioning; tp == nil || tp.Field != "" || tp.Type != bigquery.HourPartitioningType {
		t.Errorf("phcopy timePartitioning %+v, want HOUR ingestion-time", tp)
	}

	// Several sources.
	exec("CREATE TABLE " + p("pd2") + " (n INT64 OPTIONS (description = 'the number'), d DATE OPTIONS (description = 'the day'), s STRING) " +
		"PARTITION BY d CLUSTER BY s OPTIONS (partition_expiration_days = 400, require_partition_filter = TRUE)")
	exec("INSERT INTO " + p("pd2") + " (n, d, s) VALUES (3, CURRENT_DATE(), 'c')")
	if err := copyJob("both", "", "", "pd", "pd2"); err != nil {
		t.Fatalf("copy pd and pd2 into a new table: %v", err)
	}
	if tp := meta("both").TimePartitioning; tp == nil || tp.Field != "d" {
		t.Errorf("both timePartitioning %+v, want DAY on d", tp)
	}
	exec("CREATE TABLE " + p("plain") + " (n INT64, d DATE, s STRING)")
	exec("INSERT INTO " + p("plain") + " (n, d, s) VALUES (9, CURRENT_DATE(), 'z')")
	if err := copyJob("mixed", "", "", "pd", "plain"); err == nil || !strings.Contains(err.Error(), "mixture") {
		t.Errorf("copy of a partitioned and a non-partitioned table: %v, want the job to fail", err)
	}
	exec("CREATE TABLE " + p("pdn") + " (n INT64, d DATE, s STRING) PARTITION BY d CLUSTER BY n")
	if err := copyJob("mixed", "", "", "pd", "pdn"); err == nil || !strings.Contains(err.Error(), "clustering") {
		t.Errorf("copy of sources clustered otherwise: %v, want the job to fail", err)
	}
	if _, err := ds.Table("mixed").Metadata(ctx); err == nil {
		t.Errorf("the failed copies made mixed")
	}

	// Into a table that exists.
	exec("CREATE TABLE " + p("pmonth") + " (n INT64, d DATE, s STRING) PARTITION BY DATE_TRUNC(d, MONTH)")
	if err := copyJob("pmonth", bigquery.WriteAppend, "", "pd"); err == nil || !strings.Contains(err.Error(), "partitioning") {
		t.Errorf("copy into a table partitioned otherwise: %v, want the job to fail", err)
	}
	if err := copyJob("pmonth", bigquery.WriteTruncate, "", "plain"); err == nil || !strings.Contains(err.Error(), "ingestion-time") {
		t.Errorf("copy of a non-partitioned table into a column-partitioned one: %v, want the job to fail", err)
	}
	exec("CREATE TABLE " + p("pdother") + " (n INT64, d DATE, s STRING) PARTITION BY d CLUSTER BY n")
	if err := copyJob("pdother", bigquery.WriteAppend, "", "pd"); err == nil || !strings.Contains(err.Error(), "clustering") {
		t.Errorf("copy into a table clustered otherwise: %v, want the job to fail", err)
	}
	if got := rows("SELECT CAST(COUNT(*) AS STRING) FROM " + p("pmonth")); !reflect.DeepEqual(got, []string{"0"}) {
		t.Errorf("pmonth after the failed copies holds %v rows, want 0", got)
	}
	exec("CREATE TABLE " + p("pingest") + " (n INT64, d DATE, s STRING) PARTITION BY _PARTITIONDATE")
	if err := copyJob("pingest", bigquery.WriteAppend, "", "plain"); err != nil {
		t.Fatalf("copy of a non-partitioned table into an ingestion-time partitioned one: %v", err)
	}
	if got := rows("SELECT CAST(_PARTITIONDATE = CURRENT_DATE() AS STRING) FROM " + p("pingest")); !reflect.DeepEqual(got, []string{"true"}) {
		t.Errorf("pingest rows in today's partition: %v, want true", got)
	}
	exec("CREATE TABLE " + p("unpart") + " (n INT64, d DATE, s STRING)")
	// pd is clustered: "the cluster specifications for the source and
	// destination tables must match", unpart's being none (#1762); the
	// reason is "invalid", error-messages' "invalid input other than an
	// invalid query".
	err := copyJob("unpart", bigquery.WriteTruncate, "", "pd")
	var bqErr *bigquery.Error
	if err == nil || !strings.Contains(err.Error(), "clustering") || !errors.As(err, &bqErr) || bqErr.Reason != "invalid" {
		t.Errorf("copy of a partitioned, clustered table into a table not clustered: %v, want the job to fail invalid", err)
	}
	exec("CREATE TABLE " + p("pdu") + " (n INT64, d DATE, s STRING) PARTITION BY d")
	exec("INSERT INTO " + p("pdu") + " (n, d, s) VALUES (1, CURRENT_DATE(), 'a'), (2, DATE_SUB(CURRENT_DATE(), INTERVAL 1 DAY), 'b')")
	if err := copyJob("unpart", bigquery.WriteTruncate, "", "pdu"); err != nil {
		t.Fatalf("copy of a partitioned table into a non-partitioned one: %v", err)
	}
	if md := meta("unpart"); md.TimePartitioning != nil || md.RangePartitioning != nil || md.Clustering != nil {
		t.Errorf("unpart after WRITE_TRUNCATE: %+v %+v %+v, want it unpartitioned", md.TimePartitioning, md.RangePartitioning, md.Clustering)
	}
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("unpart")); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("unpart rows %v, want 1 2", got)
	}
	if err := copyJob("pd2", bigquery.WriteAppend, "", "pd"); err != nil {
		t.Fatalf("copy into a table partitioned and clustered as the source: %v", err)
	}

	// A snapshot keeps no partition expiration.
	if err := copyJob("pdsnap", "", bigquery.SnapshotOperation, "pd"); err != nil {
		t.Fatalf("snapshot of pd: %v", err)
	}
	if tp := meta("pdsnap").TimePartitioning; tp == nil || tp.Field != "d" || tp.Expiration != 0 {
		t.Errorf("pdsnap timePartitioning %+v, want DAY on d without an expiration", tp)
	}
}
