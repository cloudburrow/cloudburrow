//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
)

// TestBqIcebergNestedTypesAndRefusedTypes (#2104), through the bq CLI
// (`bq query --nouse_legacy_sql`, the configuration `cloudburrow env`
// exports, egress blocked): a BigQuery table for Apache Iceberg with a
// STRUCT and an ARRAY column is made and written, its schema read back by
// `bq show`, and one with a JSON column ("Iceberg managed tables don't
// support ... JSON") is refused, bq exiting non-zero with the column named.
// INFORMATION_SCHEMA.TABLES' upsert_stream_apply_watermark is NULL for a
// table that received no change data capture.
//
// covers: bigquery.jobs.insert, bigquery.jobs.getQueryResults, bigquery.tables.get
func TestBqIcebergNestedTypesAndRefusedTypes(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	vars := bqEnv(t, h)
	b := newBqSession(t, vars, "--nouse_google_auth", bqToken)
	ds := strings.ReplaceAll(h.Project(), "-", "_") + "_bqice2104"
	b.must("mk", "--dataset", project+":"+ds)
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(context.Background()) })
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bqice2104")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bucket) })
	opts := func(table string) string {
		return " WITH CONNECTION `" + project + ".us.iceberg` OPTIONS (file_format = 'PARQUET', table_format = 'ICEBERG', " +
			"storage_uri = 'gs://" + bucket.BucketName() + "/" + table + "')"
	}
	b.must("query", "--nouse_legacy_sql", "CREATE TABLE `"+ds+".nested` (i INT64, s STRUCT<a STRING, n INT64>, tags ARRAY<STRING>)"+opts("nested"))
	b.must("query", "--nouse_legacy_sql", "INSERT `"+ds+".nested` (i, s, tags) VALUES (1, STRUCT('x', 2), ['p', 'q'])")
	shown := b.must("show", "--format=prettyjson", project+":"+ds+".nested")
	for _, want := range []string{`"tableFormat": "ICEBERG"`, `"type": "RECORD"`, `"mode": "REPEATED"`} {
		if !strings.Contains(shown, want) {
			t.Errorf("bq show of the table: no %s in\n%s", want, shown)
		}
	}
	out, errOut, err := b.run(nil, "query", "--nouse_legacy_sql", "CREATE TABLE `"+ds+".refused` (i INT64, j JSON)"+opts("refused"))
	if err == nil || !strings.Contains(out+errOut, "j (JSON)") {
		t.Errorf("bq query of a JSON Iceberg column: %v\n%s%s", err, out, errOut)
	}
	if tableExists(t, ctx, c.Dataset(ds).Table("refused")) {
		t.Error("the refused CREATE made the table")
	}
	wm := b.must("query", "--nouse_legacy_sql", "--format=json",
		"SELECT table_name, upsert_stream_apply_watermark FROM `"+ds+".INFORMATION_SCHEMA.TABLES` WHERE table_name = 'nested'")
	if !strings.Contains(wm, `"table_name":"nested"`) || strings.Contains(wm, `"upsert_stream_apply_watermark":"`) {
		t.Errorf("the watermark of a table with no change data capture: %s", wm)
	}
}
