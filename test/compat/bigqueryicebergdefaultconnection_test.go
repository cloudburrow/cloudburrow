//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
)

// TestBigQueryIcebergDefaultConnection (#1456): WITH CONNECTION DEFAULT of
// a BigQuery table for Apache Iceberg takes the project's default
// connection, `region-REGION.default_cloud_resource_connection_id` set by
// ALTER PROJECT SET OPTIONS ("Only specify the connection's ID"), and with
// none the one BigQuery makes, "Name: __default_cloudresource_connection__",
// in the dataset's region (Default connection overview,
// https://docs.cloud.google.com/bigquery/docs/default-connections, read
// 2026-10-17), read back as biglakeConfiguration.connectionId; the
// table's files are written under its storage_uri. Before, the
// connectionId read "DEFAULT". storage_uri with no WITH CONNECTION at all
// is still kept only (TestBigQueryStorageURIMakesIcebergTable): the DDL
// reference's managed table takes WITH CONNECTION {CONNECTION_NAME |
// DEFAULT}, and Google documents no connection for one without it.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryIcebergDefaultConnection(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-icedefconn")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		it := bucket.Objects(context.Background(), nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = bucket.Object(o.Name).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	uri := func(p string) string { return "gs://" + bucket.BucketName() + "/" + p }
	name := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	run := func(sql string, insert bool) {
		t.Helper()
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("insert=%v: %s: %v", insert, sql, err)
		}
	}
	conn := func(table string) string {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil || md.BigLakeConfiguration == nil {
			t.Fatalf("tables.get of %s: %+v, %v", table, md, err)
		}
		return md.BigLakeConfiguration.ConnectionID
	}

	run("CREATE TABLE "+name("made")+" (i INT64, s STRING) WITH CONNECTION DEFAULT OPTIONS (storage_uri = '"+uri("made")+"')", false)
	if got, want := conn("made"), project+".us.__default_cloudresource_connection__"; got != want {
		t.Errorf("WITH CONNECTION DEFAULT, no default set: connection %q, want %q", got, want)
	}
	alter := "ALTER PROJECT `" + project + "` SET OPTIONS (`region-us.default_cloud_resource_connection_id` = "
	run(alter+"'lake')", false)
	t.Cleanup(func() { _ = bqRun(context.Background(), c, alter+"NULL)", false) })
	run("CREATE TABLE "+name("set")+" WITH CONNECTION DEFAULT OPTIONS (storage_uri = '"+uri("set")+"') AS SELECT 1 AS i, 'a' AS s", true)
	if got, want := conn("set"), project+".us.lake"; got != want {
		t.Errorf("WITH CONNECTION DEFAULT, default lake: connection %q, want %q", got, want)
	}
	run("INSERT "+name("made")+" (i, s) VALUES (1, 'x')", true)
	for _, tbl := range []string{"made", "set"} {
		run("EXPORT TABLE METADATA FROM "+name(tbl), true)
	}
	root := t.TempDir()
	download(t, ctx, bucket, root)
	for tbl, want := range map[string]string{"made": "1|x", "set": "1|a"} {
		if got, _ := readIceberg(t, root, bucket.BucketName(), tbl); got != want {
			t.Errorf("%s's Iceberg rows:\n%s\nwant\n%s", tbl, got, want)
		}
	}
}
