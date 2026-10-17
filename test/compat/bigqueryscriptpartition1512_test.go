//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryScriptCreateTableKeepsPartitioning (#1512): a CREATE TABLE
// with PARTITION BY, CLUSTER BY and table OPTIONS inside a multi-statement
// script makes the table a lone CREATE TABLE makes, as BigQuery's script
// runs each statement as it runs alone
// (https://cloud.google.com/bigquery/docs/multi-statement-queries): it is
// partitioned and clustered, its partitions are counted in
// INFORMATION_SCHEMA.PARTITIONS, and its options are kept; so in a script
// with a TEMP table too. Reported (#1512) on land/104: the issue's script
// made an unpartitioned table (INFORMATION_SCHEMA.TABLES' ddl had no
// PARTITION BY, one PARTITIONS row of partition_id NULL), which the
// scripts carried out a statement at a time (#1459) cure; on this branch,
// CREATE TABLE ... PARTITION BY ... OPTIONS (...) AS SELECT, alone or in
// a script, failed 400 "Unrecognized name: day".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryScriptCreateTableKeepsPartitioning(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	advanceBigQueryClock(t, h, 0)
	ds := validationDataset(t, h, c)
	t.Run("alone", func(t *testing.T) {
		// A lone CREATE TABLE ... PARTITION BY ... OPTIONS (...) AS
		// SELECT, which failed "Unrecognized name: day" (the query after
		// OPTIONS was not read for its columns).
		if err := bqRun(ctx, c, "CREATE TABLE "+ds.DatasetID+".lone PARTITION BY day OPTIONS (description = 'd') "+
			"AS SELECT 1 AS n, DATE '2024-01-01' AS day", false); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		md, err := ds.Table("lone").Metadata(ctx)
		if err != nil {
			t.Fatalf("metadata: %v", err)
		}
		if md.TimePartitioning == nil || md.TimePartitioning.Field != "day" || md.Description != "d" {
			t.Errorf("partitioning %+v, description %q", md.TimePartitioning, md.Description)
		}
	})
	t.Run("issue", func(t *testing.T) {
		// The issue's script: a new dataset, the table, then rows.
		schema := ds.DatasetID + "_s1512"
		t.Cleanup(func() { _ = c.Dataset(schema).DeleteWithContents(ctx) })
		if err := bqRun(ctx, c, "CREATE SCHEMA "+schema+";\nCREATE TABLE "+schema+".t3 (n INT64, s STRING, day DATE) PARTITION BY day;\n"+
			"INSERT INTO "+schema+".t3 VALUES (1,'a','2024-01-01'),(2,'bb','2024-01-02'),(3,NULL,'2024-01-03')", false); err != nil {
			t.Fatalf("script: %v", err)
		}
		got := rows(t, ctx, c, "SELECT partition_id FROM "+schema+".INFORMATION_SCHEMA.PARTITIONS WHERE table_name = 't3' ORDER BY partition_id")
		if fmt.Sprint(got) != "[[20240101] [20240102] [20240103]]" {
			t.Errorf("PARTITIONS: %v", got)
		}
		got = rows(t, ctx, c, "SELECT total_partitions FROM `region-us`.INFORMATION_SCHEMA.TABLE_STORAGE WHERE table_schema = '"+schema+"' AND table_name = 't3'")
		if fmt.Sprint(got) != "[[3]]" {
			t.Errorf("TABLE_STORAGE total_partitions: %v", got)
		}
		ddl := rows(t, ctx, c, "SELECT ddl FROM "+schema+".INFORMATION_SCHEMA.TABLES WHERE table_name = 't3'")
		if len(ddl) != 1 || !strings.Contains(fmt.Sprint(ddl[0][0]), "PARTITION BY day") {
			t.Errorf("ddl: %v", ddl)
		}
	})
	for _, tc := range []struct {
		name   string
		temp   bool
		insert bool
	}{{"plain", false, false}, {"job", false, true}, {"withtemp", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := ds.DatasetID + ".p_" + tc.name
			rg := ds.DatasetID + ".r_" + tc.name
			ing := ds.DatasetID + ".i_" + tc.name
			script := ""
			if tc.temp {
				script += "CREATE TEMP TABLE tmp AS SELECT 1 AS x;\n"
			}
			script += fmt.Sprintf("CREATE TABLE %s (n INT64, s STRING, day DATE) PARTITION BY day CLUSTER BY s "+
				"OPTIONS (description = 'parted', partition_expiration_days = 10, require_partition_filter = FALSE);\n", p)
			script += fmt.Sprintf("INSERT INTO %s VALUES (1, 'a', '2024-01-01'), (2, 'bb', '2024-01-02'), (3, NULL, '2024-01-03');\n", p)
			script += fmt.Sprintf("CREATE TABLE %s (k INT64) PARTITION BY RANGE_BUCKET(k, GENERATE_ARRAY(0, 100, 10));\n", rg)
			script += fmt.Sprintf("CREATE TABLE %s (v STRING) PARTITION BY _PARTITIONDATE;\n", ing)
			script += fmt.Sprintf("CREATE OR REPLACE TABLE %s.c_%s PARTITION BY day CLUSTER BY n OPTIONS (labels = [('k', 'v')]) "+
				"AS SELECT 1 AS n, DATE '2024-01-01' AS day;\n", ds.DatasetID, tc.name)
			script += fmt.Sprintf("SELECT COUNT(*) FROM %s", p)
			if err := bqRun(ctx, c, script, tc.insert); err != nil {
				t.Fatalf("script: %v", err)
			}
			md, err := ds.Table("p_" + tc.name).Metadata(ctx)
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}
			if md.TimePartitioning == nil || md.TimePartitioning.Field != "day" || md.TimePartitioning.Type != bigquery.DayPartitioningType ||
				md.TimePartitioning.Expiration != 10*24*time.Hour {
				t.Errorf("time partitioning: %+v", md.TimePartitioning)
			}
			if md.Clustering == nil || strings.Join(md.Clustering.Fields, ",") != "s" {
				t.Errorf("clustering: %+v", md.Clustering)
			}
			if md.Description != "parted" {
				t.Errorf("description: %q", md.Description)
			}
			got := rows(t, ctx, c, fmt.Sprintf("SELECT partition_id, total_rows FROM %s.INFORMATION_SCHEMA.PARTITIONS "+
				"WHERE table_name = 'p_%s' ORDER BY partition_id", ds.DatasetID, tc.name))
			if fmt.Sprint(got) != "[[20240101 1] [20240102 1] [20240103 1]]" {
				t.Errorf("PARTITIONS: %v", got)
			}
			ddl := rows(t, ctx, c, fmt.Sprintf("SELECT ddl FROM %s.INFORMATION_SCHEMA.TABLES WHERE table_name = 'p_%s'", ds.DatasetID, tc.name))
			if len(ddl) != 1 || !strings.Contains(fmt.Sprint(ddl[0][0]), "PARTITION BY day") || !strings.Contains(fmt.Sprint(ddl[0][0]), "CLUSTER BY s") {
				t.Errorf("ddl: %v", ddl)
			}
			rmd, err := ds.Table("r_" + tc.name).Metadata(ctx)
			if err != nil {
				t.Fatalf("range metadata: %v", err)
			}
			if rmd.RangePartitioning == nil || rmd.RangePartitioning.Field != "k" || rmd.RangePartitioning.Range == nil ||
				rmd.RangePartitioning.Range.End != 100 || rmd.RangePartitioning.Range.Interval != 10 {
				t.Errorf("range partitioning: %+v", rmd.RangePartitioning)
			}
			cmd, err := ds.Table("c_" + tc.name).Metadata(ctx)
			if err != nil {
				t.Fatalf("CREATE TABLE AS SELECT metadata: %v", err)
			}
			if cmd.TimePartitioning == nil || cmd.TimePartitioning.Field != "day" || cmd.Clustering == nil ||
				strings.Join(cmd.Clustering.Fields, ",") != "n" || cmd.Labels["k"] != "v" {
				t.Errorf("CREATE TABLE AS SELECT: %+v %+v %v", cmd.TimePartitioning, cmd.Clustering, cmd.Labels)
			}
			imd, err := ds.Table("i_" + tc.name).Metadata(ctx)
			if err != nil {
				t.Fatalf("ingestion metadata: %v", err)
			}
			if imd.TimePartitioning == nil || imd.TimePartitioning.Field != "" || imd.TimePartitioning.Type != bigquery.DayPartitioningType {
				t.Errorf("ingestion-time partitioning: %+v", imd.TimePartitioning)
			}
		})
	}
}
