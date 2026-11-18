//go:build compat

package compat

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryPartitionsFilter1763: require_partition_filter refuses
// queries on the table, not INFORMATION_SCHEMA's views of it. BigQuery
// documents the option as "whether queries on this table must include a
// predicate filter that filters on the partitioning column" (DDL
// table_option_list), "queries on views and materialized views that
// reference the partitioned table" too (querying-partitioned-tables), and
// INFORMATION_SCHEMA views as views that "retrieve metadata about your
// BigQuery objects" (information-schema-intro; all re-read 2026-11-17):
//   - the dataset's PARTITIONS, COLUMNS and TABLE_OPTIONS and the region's
//     TABLE_STORAGE answer for a table with require_partition_filter, with
//     no filter on its partitioning column, whether DDL, tables.insert or a
//     copy job made it;
//   - a query of the table with no such filter, alone, joined to PARTITIONS,
//     or run by EXECUTE IMMEDIATE in a script that reads PARTITIONS, is
//     still 400 "Cannot query over table ... without a filter"; one with
//     the filter answers;
//   - PARTITIONS of a table with an aggregation threshold privacy policy
//     answers, while a SELECT * of it is still refused.
//
// Measured first: the PARTITIONS query was refused 400 (#1763).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryPartitionsFilter1763(t *testing.T) {
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
	refused := func(sql, want string) {
		t.Helper()
		for _, insert := range []bool{false, true} {
			err := bqRun(ctx, c, sql, insert)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s (job %v): %v, want an error with %q", sql, insert, err, want)
			}
		}
	}

	exec("CREATE TABLE " + p("t") + " (n INT64, d DATE) PARTITION BY d OPTIONS (require_partition_filter = TRUE)")
	exec("INSERT INTO " + p("t") + " (n, d) VALUES (1, DATE '2026-11-01'), (2, DATE '2026-11-02'), (3, DATE '2026-11-02')")

	if got := rows("SELECT partition_id || ':' || CAST(total_rows AS STRING) FROM " + p("INFORMATION_SCHEMA.PARTITIONS") +
		" WHERE table_name = 't'"); !reflect.DeepEqual(got, []string{"20261101:1", "20261102:2"}) {
		t.Errorf("PARTITIONS of t %v, want 20261101:1 20261102:2", got)
	}
	if got := rows("SELECT column_name FROM " + p("INFORMATION_SCHEMA.COLUMNS") + " WHERE table_name = 't'"); !reflect.DeepEqual(got, []string{"d", "n"}) {
		t.Errorf("COLUMNS of t %v, want d n", got)
	}
	if got := rows("SELECT option_value FROM " + p("INFORMATION_SCHEMA.TABLE_OPTIONS") +
		" WHERE table_name = 't' AND option_name = 'require_partition_filter'"); !reflect.DeepEqual(got, []string{"true"}) {
		t.Errorf("TABLE_OPTIONS of t %v, want true", got)
	}
	if got := rows("SELECT CAST(total_rows AS STRING) FROM `region-us`.INFORMATION_SCHEMA.TABLE_STORAGE WHERE table_schema = '" +
		ds.DatasetID + "' AND table_name = 't'"); !reflect.DeepEqual(got, []string{"3"}) {
		t.Errorf("TABLE_STORAGE of t %v, want 3", got)
	}

	const noFilter = "without a filter over column(s) 'd' that can be used for partition elimination"
	refused("SELECT n FROM "+p("t"), noFilter)
	refused("SELECT x.n FROM "+p("t")+" AS x JOIN "+p("INFORMATION_SCHEMA.PARTITIONS")+" AS q ON q.table_name = 't'", noFilter)
	refused("SELECT COUNT(*) FROM "+p("INFORMATION_SCHEMA.PARTITIONS")+"; EXECUTE IMMEDIATE 'SELECT n FROM "+
		ds.DatasetID+".t'", noFilter)
	if got := rows("SELECT CAST(n AS STRING) FROM " + p("t") + " WHERE d = DATE '2026-11-02'"); !reflect.DeepEqual(got, []string{"2", "3"}) {
		t.Errorf("t with a filter %v, want 2 3", got)
	}

	// A table made by tables.insert, and one made by a copy job, each
	// with require_partition_filter.
	if err := ds.Table("api").Create(ctx, &bigquery.TableMetadata{
		Schema:                 bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}, {Name: "d", Type: bigquery.DateFieldType}},
		TimePartitioning:       &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d", Expiration: 400 * 24 * time.Hour},
		Clustering:             &bigquery.Clustering{Fields: []string{"n"}},
		RequirePartitionFilter: true,
	}); err != nil {
		t.Fatalf("create api: %v", err)
	}
	exec("INSERT INTO " + p("api") + " (n, d) VALUES (1, CURRENT_DATE())")
	for src, dst := range map[string]string{"t": "tcopy", "api": "apicopy"} {
		cp := ds.Table(dst).CopierFrom(ds.Table(src))
		j, err := cp.Run(ctx)
		if err := waitJob(ctx, j, err); err != nil {
			t.Fatalf("copy %s into %s: %v", src, dst, err)
		}
	}
	today := time.Now().UTC().Format("20060102")
	for table, want := range map[string][]string{"api": {today + ":1"}, "apicopy": {today + ":1"}, "tcopy": {"20261101:1", "20261102:2"}} {
		if got := rows("SELECT partition_id || ':' || CAST(total_rows AS STRING) FROM " + p("INFORMATION_SCHEMA.PARTITIONS") +
			" WHERE table_name = '" + table + "'"); !reflect.DeepEqual(got, want) {
			t.Errorf("PARTITIONS of %s %v, want %v", table, got, want)
		}
		refused("SELECT n FROM "+p(table), noFilter)
	}

	exec("CREATE TABLE " + p("priv") + " (n INT64, d DATE) PARTITION BY d OPTIONS (privacy_policy = " +
		"'{\"aggregation_threshold_policy\": {\"threshold\": 2, \"privacy_unit_columns\": [\"n\"]}}')")
	exec("INSERT INTO " + p("priv") + " (n, d) VALUES (1, DATE '2026-11-01')")
	if got := rows("SELECT partition_id FROM " + p("INFORMATION_SCHEMA.PARTITIONS") + " WHERE table_name = 'priv'"); !reflect.DeepEqual(got, []string{"20261101"}) {
		t.Errorf("PARTITIONS of priv %v, want 20261101", got)
	}
	refused("SELECT * FROM "+p("priv"), "SELECT WITH AGGREGATION_THRESHOLD")
}
