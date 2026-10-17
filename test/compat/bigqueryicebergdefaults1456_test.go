//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryStorageURIMakesIcebergTable (#1456): the table option
// storage_uri, with WITH CONNECTION and neither file_format nor
// table_format, makes a BigQuery table for Apache Iceberg, its data files
// written under storage_uri in the instance's Cloud Storage: the DDL
// reference's table_option_list (read 2026-10-17) says each of the three
// is "Required for managed tables", and of file_format "The default is
// PARQUET" and of table_format "The default is ICEBERG". The same of
// tables.insert with a biglakeConfiguration of a connection and a
// storageUri. Measured first: storage_uri was kept only, no files were
// written, and WITH CONNECTION without the formats was 400. storage_uri
// with no connection is still kept only.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.insert, bigquery.tables.get
func TestBigQueryStorageURIMakesIcebergTable(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-icedefaults")
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
	conn := h.Project() + ".us.iceberg"
	uri := func(p string) string { return "gs://" + bucket.BucketName() + "/" + p }
	name := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	run := func(sql string, insert bool) {
		t.Helper()
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("insert=%v: %s: %v", insert, sql, err)
		}
	}
	check := func(table string) {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil || md.BigLakeConfiguration == nil || md.BigLakeConfiguration.StorageURI != uri(table) ||
			md.BigLakeConfiguration.ConnectionID != conn {
			t.Fatalf("tables.get of %s: %+v, %v", table, md, err)
		}
		if b := md.BigLakeConfiguration; b.TableFormat != bigquery.IcebergBigLakeTableFormat || b.FileFormat != bigquery.ParquetBigLakeFileFormat {
			t.Errorf("%s: formats %q, %q, want ICEBERG and PARQUET, the defaults", table, b.TableFormat, b.FileFormat)
		}
	}

	run("CREATE TABLE "+name("dq")+" (i INT64, s STRING) WITH CONNECTION `"+conn+"` OPTIONS (storage_uri = '"+uri("dq")+"')", false)
	run("CREATE TABLE "+name("dj")+" WITH CONNECTION `"+conn+"` OPTIONS (storage_uri = '"+uri("dj")+"') AS SELECT 1 AS i, 'a' AS s", true)
	if err := ds.Table("api").Create(ctx, &bigquery.TableMetadata{
		Schema:               bigquery.Schema{{Name: "i", Type: bigquery.IntegerFieldType}, {Name: "s", Type: bigquery.StringFieldType}},
		BigLakeConfiguration: &bigquery.BigLakeConfiguration{ConnectionID: conn, StorageURI: uri("api")}}); err != nil {
		t.Fatalf("tables.insert: %v", err)
	}
	run("INSERT "+name("dq")+" (i, s) VALUES (1, 'x'), (2, 'y')", false)
	run("INSERT "+name("api")+" (i, s) VALUES (3, 'z')", true)
	for _, tbl := range []string{"dq", "dj", "api"} {
		check(tbl)
		run("EXPORT TABLE METADATA FROM "+name(tbl), true)
	}
	root := t.TempDir()
	download(t, ctx, bucket, root)
	for tbl, want := range map[string]string{"dq": "1|x\n2|y", "dj": "1|a", "api": "3|z"} {
		if got, _ := readIceberg(t, root, bucket.BucketName(), tbl); got != want {
			t.Errorf("%s's Iceberg rows:\n%s\nwant\n%s", tbl, got, want)
		}
	}

	// No connection: storage_uri is kept, no table for Apache Iceberg.
	run("CREATE TABLE "+name("plain")+" (i INT64) OPTIONS (storage_uri = '"+uri("plain")+"')", false)
	run("INSERT "+name("plain")+" (i) VALUES (1)", false)
	time.Sleep(100 * time.Millisecond)
	it := bucket.Objects(ctx, nil)
	for {
		o, err := it.Next()
		if err != nil {
			break
		}
		if strings.HasPrefix(o.Name, "plain/") {
			t.Errorf("storage_uri with no connection wrote %s", o.Name)
		}
	}
}
