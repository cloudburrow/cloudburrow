//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	icebergio "github.com/apache/iceberg-go/io"
	"github.com/apache/iceberg-go/table"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryIcebergTablesWriteIcebergFiles (#1427), through the
// official Go client: a BigQuery table for Apache Iceberg
// (https://cloud.google.com/bigquery/docs/iceberg-tables) made both ways,
// by CREATE TABLE ... WITH CONNECTION ... OPTIONS (file_format =
// 'PARQUET', table_format = 'ICEBERG', storage_uri = 'gs://...') and by
// tables.insert with a biglakeConfiguration, keeps its rows as Parquet
// files under storage_uri in the instance's Cloud Storage. Rows are
// written by DML (INSERT, then UPDATE) into the first, by a load job and
// tabledata.insertAll into the second; EXPORT TABLE METADATA FROM each
// writes its Iceberg metadata, and the files are read back from Cloud
// Storage with Apache Iceberg's Go library: the current snapshot's rows
// are the table's, one snapshot a write. A column of a type CloudBurrow
// does not write is 501 by either way, nothing made; a write whose files
// cannot be written (the bucket missing) fails, not only logged.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.insert, bigquery.tables.get, bigquery.tabledata.insertAll
func TestBigQueryIcebergTablesWriteIcebergFiles(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-iceberg")
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

	// The CREATE TABLE form, rows by DML.
	if err := bqRun(ctx, c, "CREATE TABLE "+name("ddl")+" (i INT64 NOT NULL, s STRING, ts TIMESTAMP, n NUMERIC) "+
		"WITH CONNECTION `"+conn+"` OPTIONS (file_format = 'PARQUET', table_format = 'ICEBERG', storage_uri = '"+uri("ddl")+"')", false); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if md, err := ds.Table("ddl").Metadata(ctx); err != nil || md.BigLakeConfiguration == nil ||
		md.BigLakeConfiguration.TableFormat != bigquery.IcebergBigLakeTableFormat || md.BigLakeConfiguration.StorageURI != uri("ddl") ||
		md.BigLakeConfiguration.ConnectionID != conn {
		t.Errorf("tables.get of the CREATE TABLE form: %+v, %v", md, err)
	}
	if err := bqRun(ctx, c, "INSERT "+name("ddl")+" (i, s, ts, n) VALUES (1, 'a', TIMESTAMP '2024-01-02 03:04:05.123456 UTC', 1.5), "+
		"(2, 'b', NULL, NULL)", false); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if err := bqRun(ctx, c, "UPDATE "+name("ddl")+" SET s = 'B' WHERE i = 2", false); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	// The tables.insert form, rows by a load job and insertAll.
	schema := bigquery.Schema{{Name: "i", Type: bigquery.IntegerFieldType, Required: true}, {Name: "s", Type: bigquery.StringFieldType},
		{Name: "d", Type: bigquery.DateFieldType}}
	if err := ds.Table("api").Create(ctx, &bigquery.TableMetadata{Schema: schema, BigLakeConfiguration: &bigquery.BigLakeConfiguration{
		ConnectionID: conn, StorageURI: uri("api"), FileFormat: bigquery.ParquetBigLakeFileFormat,
		TableFormat: bigquery.IcebergBigLakeTableFormat}}); err != nil {
		t.Fatalf("tables.insert: %v", err)
	}
	w := bucket.Object("load/rows.csv").NewWriter(ctx)
	_, _ = io.WriteString(w, "10,x,2024-05-06\n11,y,\n")
	if err := w.Close(); err != nil {
		t.Fatalf("write the CSV: %v", err)
	}
	load := ds.Table("api").LoaderFrom(bigquery.NewGCSReference(uri("load/rows.csv")))
	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	job, err := load.Run(lctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = job.Wait(lctx); err == nil {
			err = st.Err()
		}
	}
	cancel()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	type row struct {
		I int64  `bigquery:"i"`
		S string `bigquery:"s"`
	}
	if err := ds.Table("api").Inserter().Put(ctx, []*row{{I: 12, S: "z"}}); err != nil {
		t.Fatalf("insertAll: %v", err)
	}

	// EXPORT TABLE METADATA, then the files read back.
	for _, tbl := range []string{"ddl", "api"} {
		q := c.Query("EXPORT TABLE METADATA FROM " + name(tbl))
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		job, err := q.Run(qctx)
		if err == nil {
			var st *bigquery.JobStatus
			if st, err = job.Wait(qctx); err == nil {
				err = st.Err()
			}
			if err == nil {
				if qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics); !ok || qs.StatementType != "EXPORT_DATA" {
					t.Errorf("EXPORT TABLE METADATA FROM %s: statistics %+v", tbl, st.Statistics.Details)
				}
			}
		}
		cancel()
		if err != nil {
			t.Fatalf("EXPORT TABLE METADATA FROM %s: %v", tbl, err)
		}
	}
	root := t.TempDir()
	download(t, ctx, bucket, root)
	gotDDL, snapsDDL := readIceberg(t, root, bucket.BucketName(), "ddl")
	if want := "1|a|2024-01-02T03:04:05.123456Z|1.5\n2|B|(null)|(null)"; gotDDL != want {
		t.Errorf("the CREATE TABLE form's Iceberg rows:\n%s\nwant\n%s", gotDDL, want)
	}
	gotAPI, snapsAPI := readIceberg(t, root, bucket.BucketName(), "api")
	if want := "10|x|2024-05-06\n11|y|(null)\n12|z|(null)"; gotAPI != want {
		t.Errorf("the tables.insert form's Iceberg rows:\n%s\nwant\n%s", gotAPI, want)
	}
	// One snapshot a write: INSERT, UPDATE; the load, insertAll.
	if snapsDDL != 2 || snapsAPI != 2 {
		t.Errorf("snapshots: %d of the CREATE TABLE form, %d of the tables.insert form; want 2 each", snapsDDL, snapsAPI)
	}

	// A column of a type not written: 501 either way, nothing made.
	notImplemented := func(what string, err error, table string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented || !strings.Contains(ge.Message, "j (JSON)") {
			t.Errorf("%s: %v, want 501 naming j (JSON)", what, err)
		}
		if tableExists(t, ctx, ds.Table(table)) {
			t.Errorf("%s: the table %s was made", what, table)
		}
	}
	notImplemented("CREATE TABLE with a JSON column", bqRun(ctx, c, "CREATE TABLE "+name("json_ddl")+" (i INT64, j JSON) WITH CONNECTION `"+
		conn+"` OPTIONS (file_format = 'PARQUET', table_format = 'ICEBERG', storage_uri = '"+uri("json_ddl")+"')", false), "json_ddl")
	notImplemented("CREATE TABLE AS SELECT of a JSON column", bqRun(ctx, c, "CREATE TABLE "+name("json_ctas")+" WITH CONNECTION `"+
		conn+"` OPTIONS (file_format = 'PARQUET', table_format = 'ICEBERG', storage_uri = '"+uri("json_ctas")+"') AS SELECT 1 AS i, JSON '{}' AS j", false), "json_ctas")
	notImplemented("tables.insert with a JSON column", ds.Table("json_api").Create(ctx, &bigquery.TableMetadata{
		Schema: bigquery.Schema{{Name: "i", Type: bigquery.IntegerFieldType}, {Name: "j", Type: bigquery.JSONFieldType}},
		BigLakeConfiguration: &bigquery.BigLakeConfiguration{ConnectionID: conn, StorageURI: uri("json_api"),
			FileFormat: bigquery.ParquetBigLakeFileFormat, TableFormat: bigquery.IcebergBigLakeTableFormat}}), "json_api")

	// A bucket that does not exist: the write fails, naming the URI.
	missing := "gs://" + bucket.BucketName() + "-missing/t"
	if err := bqRun(ctx, c, "CREATE TABLE "+name("nobucket")+" (i INT64) WITH CONNECTION `"+conn+
		"` OPTIONS (file_format = 'PARQUET', table_format = 'ICEBERG', storage_uri = '"+missing+"')", false); err != nil {
		t.Fatalf("CREATE TABLE of a missing bucket: %v", err)
	}
	err = bqRun(ctx, c, "INSERT "+name("nobucket")+" (i) VALUES (1)", false)
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotFound || !strings.Contains(ge.Message, missing) {
		t.Errorf("INSERT into a table of a missing bucket: %v, want 404 naming %s", err, missing)
	}
}

// download writes every object of bucket under root/<bucket>/<name>.
func download(t *testing.T, ctx context.Context, bucket *storage.BucketHandle, root string) {
	t.Helper()
	it := bucket.Objects(ctx, nil)
	for {
		o, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return
		}
		if err != nil {
			t.Fatalf("list the bucket: %v", err)
		}
		r, err := bucket.Object(o.Name).NewReader(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", o.Name, err)
		}
		b, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatalf("read %s: %v", o.Name, err)
		}
		p := filepath.Join(root, bucket.BucketName(), filepath.FromSlash(o.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// gsFiles is an Iceberg IO of gs:// names on the objects download wrote.
type gsFiles string

func (g gsFiles) Open(name string) (icebergio.File, error) {
	rest, ok := strings.CutPrefix(name, "gs://")
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return os.Open(filepath.Join(string(g), filepath.FromSlash(rest)))
}

func (g gsFiles) Remove(name string) error {
	return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
}

// readIceberg opens the Iceberg table of gs://bucket/prefix by its newest
// metadata/vN.metadata.json, with iceberg-go: its current snapshot's rows,
// sorted, a line each, and its number of snapshots.
func readIceberg(t *testing.T, root, bucket, prefix string) (string, int) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, bucket, prefix, "metadata"))
	if err != nil {
		t.Fatalf("%s: no metadata was written: %v", prefix, err)
	}
	newest, version := "", -1
	for _, e := range entries {
		v, ok := strings.CutSuffix(strings.TrimPrefix(e.Name(), "v"), ".metadata.json")
		if n, err := strconv.Atoi(v); ok && err == nil && n > version {
			newest, version = e.Name(), n
		}
	}
	if newest == "" {
		t.Fatalf("%s: no vN.metadata.json among %v", prefix, entries)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tbl, err := table.NewFromLocation(ctx, table.Identifier{"compat", prefix}, "gs://"+bucket+"/"+prefix+"/metadata/"+newest,
		func(context.Context) (icebergio.IO, error) { return gsFiles(root), nil }, nil)
	if err != nil {
		t.Fatalf("%s: open the Iceberg table: %v", prefix, err)
	}
	if tbl.CurrentSnapshot() == nil {
		t.Fatalf("%s: no current snapshot", prefix)
	}
	at, err := tbl.Scan().ToArrowTable(ctx)
	if err != nil {
		t.Fatalf("%s: scan: %v", prefix, err)
	}
	defer at.Release()
	var lines []string
	tr := array.NewTableReader(at, 0)
	defer tr.Release()
	for tr.Next() {
		rec := tr.RecordBatch()
		for i := 0; i < int(rec.NumRows()); i++ {
			var vals []string
			for j := 0; j < int(rec.NumCols()); j++ {
				vals = append(vals, icebergValueStr(rec.Column(j), i))
			}
			lines = append(lines, strings.Join(vals, "|"))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n"), len(tbl.Metadata().Snapshots())
}

// icebergValueStr is the value at i of col as Arrow prints it, a decimal
// without its trailing zeros.
func icebergValueStr(col arrow.Array, i int) string {
	if d, ok := col.(*array.Decimal128); ok && !col.IsNull(i) {
		s := d.Value(i).ToString(d.DataType().(*arrow.Decimal128Type).Scale)
		if strings.Contains(s, ".") {
			s = strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
		}
		return s
	}
	return fmt.Sprint(col.ValueStr(i))
}
