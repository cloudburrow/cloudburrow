//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryIcebergNestedTypesAndRefusedTypes (#2104), through the
// official Go client: a BigQuery table for Apache Iceberg holds STRUCT and
// ARRAY columns, an ARRAY of STRUCT among them, made by CREATE TABLE and by
// tables.insert; its rows, written by DML and tabledata.insertAll, are read
// back from the Iceberg files in the instance's Cloud Storage with Apache
// Iceberg's Go library as Iceberg structs and lists. "Iceberg managed
// tables don't support the following table schema: Schema with
// BIGNUMERIC, INTERVAL, JSON, RANGE, or GEOGRAPHY data types"
// (https://docs.cloud.google.com/bigquery/docs/iceberg-tables#limitations,
// read 2026-11-21): a column of each, at the top level or in a STRUCT, is
// 400, nothing made.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.insert, bigquery.tables.get, bigquery.tabledata.insertAll
func TestBigQueryIcebergNestedTypesAndRefusedTypes(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-icetypes")
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
	opts := func(table string) string {
		return " WITH CONNECTION `" + conn + "` OPTIONS (file_format = 'PARQUET', table_format = 'ICEBERG', storage_uri = '" + uri(table) + "')"
	}

	if err := bqRun(ctx, c, "CREATE TABLE "+name("nested")+" (i INT64 NOT NULL, addr STRUCT<city STRING, zip INT64>, "+
		"tags ARRAY<STRING>, items ARRAY<STRUCT<sku STRING, n INT64>>)"+opts("nested"), false); err != nil {
		t.Fatalf("CREATE TABLE with STRUCT and ARRAY columns: %v", err)
	}
	md, err := ds.Table("nested").Metadata(ctx)
	if err != nil || md.BigLakeConfiguration == nil || md.BigLakeConfiguration.TableFormat != bigquery.IcebergBigLakeTableFormat {
		t.Fatalf("tables.get: %+v, %v", md, err)
	}
	if err := bqRun(ctx, c, "INSERT "+name("nested")+" (i, addr, tags, items) VALUES "+
		"(1, STRUCT('Paris', 75001), ['a', 'b'], [STRUCT('x', 2), STRUCT('y', NULL)]), (2, NULL, [], [])", false); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	schema := bigquery.Schema{{Name: "i", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "rec", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType},
			{Name: "b", Type: bigquery.BooleanFieldType}}},
		{Name: "nums", Type: bigquery.IntegerFieldType, Repeated: true}}
	if err := ds.Table("api").Create(ctx, &bigquery.TableMetadata{Schema: schema, BigLakeConfiguration: &bigquery.BigLakeConfiguration{
		ConnectionID: conn, StorageURI: uri("api"), FileFormat: bigquery.ParquetBigLakeFileFormat,
		TableFormat: bigquery.IcebergBigLakeTableFormat}}); err != nil {
		t.Fatalf("tables.insert with RECORD and REPEATED columns: %v", err)
	}
	type rec struct {
		A string `bigquery:"a"`
		B bool   `bigquery:"b"`
	}
	type row struct {
		I    int64   `bigquery:"i"`
		Rec  rec     `bigquery:"rec"`
		Nums []int64 `bigquery:"nums"`
	}
	if err := ds.Table("api").Inserter().Put(ctx, []*row{{I: 7, Rec: rec{A: "q", B: true}, Nums: []int64{3, 1, 4}}}); err != nil {
		t.Fatalf("insertAll: %v", err)
	}

	for _, tbl := range []string{"nested", "api"} {
		if err := bqRun(ctx, c, "EXPORT TABLE METADATA FROM "+name(tbl), false); err != nil {
			t.Fatalf("EXPORT TABLE METADATA FROM %s: %v", tbl, err)
		}
	}
	root := t.TempDir()
	download(t, ctx, bucket, root)
	got, _ := readIceberg(t, root, bucket.BucketName(), "nested")
	want := `1|{"city":"Paris","zip":75001}|["a","b"]|[{"n":2,"sku":"x"},{"n":null,"sku":"y"}]` + "\n" + `2|(null)|[]|[]`
	if got != want {
		t.Errorf("the CREATE TABLE form's Iceberg rows:\n%s\nwant\n%s", got, want)
	}
	got, _ = readIceberg(t, root, bucket.BucketName(), "api")
	if want := `7|{"a":"q","b":true}|[3,1,4]`; got != want {
		t.Errorf("the tables.insert form's Iceberg rows:\n%s\nwant\n%s", got, want)
	}

	refused := func(what string, err error, table, col string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest || !strings.Contains(ge.Message, col) {
			t.Errorf("%s: %v, want 400 naming %s", what, err, col)
		}
		if tableExists(t, ctx, ds.Table(table)) {
			t.Errorf("%s: the table %s was made", what, table)
		}
	}
	for i, ty := range []string{"BIGNUMERIC", "INTERVAL", "JSON", "RANGE<DATE>", "GEOGRAPHY"} {
		tbl := "refused" + string(rune('a'+i))
		base, _, _ := strings.Cut(ty, "<")
		refused("CREATE TABLE with a "+ty+" column", bqRun(ctx, c, "CREATE TABLE "+name(tbl)+" (i INT64, x "+ty+")"+opts(tbl), false),
			tbl, "x ("+base+")")
	}
	refused("CREATE TABLE with a GEOGRAPHY field in a STRUCT", bqRun(ctx, c, "CREATE TABLE "+name("refusedf")+
		" (i INT64, s STRUCT<g GEOGRAPHY>)"+opts("refusedf"), false), "refusedf", "s.g (GEOGRAPHY)")
	refused("tables.insert with a BIGNUMERIC column", ds.Table("refusedg").Create(ctx, &bigquery.TableMetadata{
		Schema: bigquery.Schema{{Name: "i", Type: bigquery.IntegerFieldType}, {Name: "x", Type: bigquery.BigNumericFieldType}},
		BigLakeConfiguration: &bigquery.BigLakeConfiguration{ConnectionID: conn, StorageURI: uri("refusedg"),
			FileFormat: bigquery.ParquetBigLakeFileFormat, TableFormat: bigquery.IcebergBigLakeTableFormat}}), "refusedg", "x (BIGNUMERIC)")
}
