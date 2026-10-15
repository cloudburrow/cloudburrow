//go:build compat

package compat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"github.com/apache/arrow/go/v15/arrow/memory"
	"github.com/apache/arrow/go/v15/parquet/file"
	"github.com/apache/arrow/go/v15/parquet/pqarrow"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryExtractCompressionsEachFormatAllows (#957), through the
// official Go client: BigQuery's export documentation lists the supported
// compressions of each format (GZIP for CSV and JSON; DEFLATE and SNAPPY
// for Avro; SNAPPY, GZIP and ZSTD for Parquet), and an extract with any
// other is 400 invalid, nothing written: each URI is then read from the
// instance's Cloud Storage, and none was. (An AVRO extract of a RANGE
// column, which "RANGE types aren't supported in Avro export" refuses, is a
// unit only: the emulator cannot make a table with a RANGE column,
// measured: 500 "Type not found: TYPE_UNKNOWN".)
//
// covers: bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryExtractCompressionsEachFormatAllows(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-extract-codecs")
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
	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".src` AS SELECT 1 AS i, 'x' AS s", false); err != nil {
		t.Fatal(err)
	}
	extract := func(table, name string, format bigquery.DataFormat, comp bigquery.Compression) error {
		ref := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + name)
		ref.DestinationFormat = format
		ref.Compression = comp
		ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := ds.Table(table).ExtractorTo(ref).Run(ectx)
		if err != nil {
			return err
		}
		st, err := job.Wait(ectx)
		if err == nil {
			err = st.Err()
		}
		return err
	}
	refused := func(what, name string, err error, want string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest || !strings.Contains(ge.Message, want) {
			t.Errorf("%s: %v, want 400 with %q", what, err, want)
		}
		if _, err := bucket.Object(name).Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Errorf("%s: the object %s: %v, want none written", what, name, err)
		}
	}
	for _, c := range []struct {
		name   string
		format bigquery.DataFormat
		comp   bigquery.Compression
		want   string
	}{
		{"snappy.csv", bigquery.CSV, bigquery.Snappy, "Supported: NONE, GZIP"},
		{"deflate.csv", bigquery.CSV, bigquery.Deflate, "Supported: NONE, GZIP"},
		{"zstd.json", bigquery.JSON, "ZSTD", "Supported: NONE, GZIP"},
		{"zstd.avro", bigquery.Avro, "ZSTD", "Supported: NONE, DEFLATE, SNAPPY"},
		{"deflate.parquet", bigquery.Parquet, bigquery.Deflate, "Supported: NONE, SNAPPY, GZIP, ZSTD"},
	} {
		refused(string(c.format)+" with "+string(c.comp), c.name, extract("src", c.name, c.format, c.comp), c.want)
	}
	// Each allowed one is still written.
	for _, c := range []struct {
		name   string
		format bigquery.DataFormat
		comp   bigquery.Compression
	}{
		{"gzip.csv", bigquery.CSV, bigquery.Gzip},
		{"snappy.avro", bigquery.Avro, bigquery.Snappy},
		{"gzip.parquet", bigquery.Parquet, bigquery.Gzip},
	} {
		if err := extract("src", c.name, c.format, c.comp); err != nil {
			t.Errorf("%s with %s: %v", c.format, c.comp, err)
		} else if _, err := bucket.Object(c.name).Attrs(ctx); err != nil {
			t.Errorf("%s with %s: the object: %v", c.format, c.comp, err)
		}
	}

}

// TestConsoleBigQueryParquetAndSeveralURIExports (#957), through the
// console API, each object read back from the instance's Cloud Storage and
// decoded: Export to Cloud Storage as PARQUET with ZSTD to a URI and a More
// destination URI writes one Parquet file to each, read by Arrow, every row
// in one of them once; as AVRO with Use Avro logical types writes a DATE
// column as an Avro int of logicalType date, read by goavro; a compression
// the format does not allow is refused on the form, nothing written.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get
func TestConsoleBigQueryParquetAndSeveralURIExports(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	d := ds.DatasetID
	if err := bqRun(ctx, c, "CREATE TABLE `"+d+".src` AS SELECT x AS id, CONCAT('n', CAST(x AS STRING)) AS name, "+
		"DATE_ADD(DATE '2020-01-01', INTERVAL x DAY) AS day FROM UNNEST([1, 2, 3, 4, 5]) AS x", false); err != nil {
		t.Fatal(err)
	}
	bucketName := strings.ReplaceAll(h.Project(), "_", "-") + "-bq-console-pq"
	bucket := sc.Bucket(bucketName)
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
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
	read := func(name string) []byte {
		t.Helper()
		r, err := bucket.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatalf("the object %s: %v", name, err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return b
	}

	_, fields, _ := consoleJobResult(t, addr, project, []string{d, "src"}, "export", map[string]string{
		"uri": "gs://" + bucketName + "/a-*.parquet", "moreUris": "gs://" + bucketName + "/b-*.parquet\n",
		"format": "PARQUET", "compression": "ZSTD", "fieldDelimiter": ",", "header": "true"})
	if fields["State"] != "Succeeded" {
		t.Errorf("the Parquet export answered %v", fields)
	}
	var ids []string
	for _, name := range []string{"a-000000000000.parquet", "b-000000000000.parquet"} {
		pf, err := file.NewParquetReader(bytes.NewReader(read(name)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cc, err := pf.MetaData().RowGroup(0).ColumnChunk(0); err != nil || !strings.EqualFold(cc.Compression().String(), "zstd") {
			t.Errorf("%s: the first column chunk %v, %v, want ZSTD", name, cc, err)
		}
		fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
		if err != nil {
			t.Fatal(err)
		}
		tbl, err := fr.ReadTable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := tbl.Schema().Field(2).Type.String(); got != "date32" {
			t.Errorf("%s: day is %s, want date32", name, got)
		}
		for i := 0; i < int(tbl.NumRows()); i++ {
			ids = append(ids, tbl.Column(0).Data().Chunk(0).ValueStr(i))
		}
		tbl.Release()
		pf.Close()
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "1,2,3,4,5" {
		t.Errorf("the two Parquet files hold ids %v, want each of 1..5 once", ids)
	}

	_, fields, _ = consoleJobResult(t, addr, project, []string{d, "src"}, "export", map[string]string{
		"uri": "gs://" + bucketName + "/logical.avro", "format": "AVRO", "compression": "SNAPPY",
		"useAvroLogicalTypes": "true", "fieldDelimiter": ",", "header": "true"})
	if fields["State"] != "Succeeded" {
		t.Errorf("the Avro export answered %v", fields)
	}
	ocf, err := goavro.NewOCFReader(bytes.NewReader(read("logical.avro")))
	if err != nil {
		t.Fatal(err)
	}
	if s := ocf.Codec().Schema(); !strings.Contains(s, `"logicalType":"date"`) {
		t.Errorf("the Avro export's schema is %s, want a date logical type", s)
	}
	n := 0
	for ocf.Scan() {
		rec, err := ocf.Read()
		if err != nil {
			t.Fatal(err)
		}
		if day, ok := rec.(map[string]any)["day"].(map[string]any)["int.date"].(time.Time); !ok || day.Year() != 2020 {
			t.Errorf("the Avro row %v has no date day", rec)
		}
		n++
	}
	if n != 5 {
		t.Errorf("the Avro export holds %d rows, want 5", n)
	}

	code, out := consoleAct(t, addr, "bigquery", project, []string{d, "src"}, "export", map[string]string{
		"uri": "gs://" + bucketName + "/deflate.parquet", "format": "PARQUET", "compression": "DEFLATE"})
	if code == http.StatusOK {
		t.Errorf("a Parquet export with DEFLATE = %d: %s, want a refusal", code, out)
	}
	it := bucket.Objects(ctx, &storage.Query{Prefix: "deflate"})
	if _, err := it.Next(); err != iterator.Done {
		t.Errorf("the refused export wrote an object: %v", err)
	}
}
