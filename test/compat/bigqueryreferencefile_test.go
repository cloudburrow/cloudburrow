//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"google.golang.org/api/googleapi"
)

// TestBigQueryLoadReferenceFileSchema (#1457): an Avro or Parquet load
// from Cloud Storage with referenceFileSchemaUri, through the official Go
// client's LoadConfig.ReferenceFileSchemaURI. BigQuery: "The user can
// provide a reference file with the reader schema. This file is only
// loaded if it is part of source URIs, but is not loaded otherwise"
// (JobConfigurationLoad). The fixtures, written in the test by goavro and
// Arrow's Parquet writer, are put in the instance's Cloud Storage: a
// reference file and two data files of its schema. A load of the data
// files makes the table with the reference file's schema and loads only
// their rows; one whose sourceUris name the reference file too loads its
// rows as well. A reference file that does not exist is 404 notFound, as
// any source URI that names no object; a data file whose schema is not
// the reference file's is 501 (BigQuery's documentation does not say how
// a file is read by another reader schema), and so is a wildcard
// reference. Nothing is loaded by any of them.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.jobs.query
func TestBigQueryLoadReferenceFileSchema(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-reffile")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	var objects []string
	t.Cleanup(func() {
		for _, o := range objects {
			_ = bucket.Object(o).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	put := func(name, data string) string {
		t.Helper()
		w := bucket.Object(name).NewWriter(ctx)
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write object: %v", err)
		}
		objects = append(objects, name)
		return "gs://" + bucket.BucketName() + "/" + name
	}

	// Avro: the reference has id 0, the data files 1 and 2; another file
	// has a column more.
	avroSchema := `{"type":"record","name":"Row","fields":[{"name":"id","type":"long"},{"name":"s","type":["null","string"]}]}`
	avroOther := `{"type":"record","name":"Row","fields":[{"name":"id","type":"long"},{"name":"s","type":["null","string"]},{"name":"x","type":"long"}]}`
	avroRow := func(id int64, s string) map[string]any {
		return map[string]any{"id": id, "s": map[string]any{"string": s}}
	}
	// Parquet: the same, by Arrow's writer.
	pqSchema := arrow.NewSchema([]arrow.Field{{Name: "id", Type: arrow.PrimitiveTypes.Int64}, {Name: "s", Type: arrow.BinaryTypes.String, Nullable: true}}, nil)
	pqOther := arrow.NewSchema([]arrow.Field{{Name: "id", Type: arrow.PrimitiveTypes.Int64}, {Name: "s", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "x", Type: arrow.PrimitiveTypes.Int64}}, nil)
	pqRow := func(schema *arrow.Schema, id int64, s string) string {
		return arrowParquet(t, schema, func(b *array.RecordBuilder) {
			b.Field(0).(*array.Int64Builder).Append(id)
			b.Field(1).(*array.StringBuilder).Append(s)
			if len(schema.Fields()) > 2 {
				b.Field(2).(*array.Int64Builder).Append(9)
			}
		})
	}
	type fileSet struct{ ref, one, two, other string }
	formats := map[string]struct {
		format bigquery.DataFormat
		files  fileSet
	}{
		"avro": {bigquery.Avro, fileSet{
			put("avro/ref.avro", avroFile(t, avroSchema, "", avroRow(0, "ref"))),
			put("avro/data/1.avro", avroFile(t, avroSchema, "", avroRow(1, "a"))),
			put("avro/data/2.avro", avroFile(t, avroSchema, "", avroRow(2, "b"))),
			put("avro/other.avro", avroFile(t, avroOther, "", map[string]any{"id": int64(3), "s": nil, "x": int64(9)})),
		}},
		"parquet": {bigquery.Parquet, fileSet{
			put("parquet/ref.parquet", pqRow(pqSchema, 0, "ref")),
			put("parquet/data/1.parquet", pqRow(pqSchema, 1, "a")),
			put("parquet/data/2.parquet", pqRow(pqSchema, 2, "b")),
			put("parquet/other.parquet", pqRow(pqOther, 3, "c")),
		}},
	}
	run := func(table string, format bigquery.DataFormat, ref string, uris ...string) error {
		gcs := bigquery.NewGCSReference(uris...)
		gcs.SourceFormat = format
		l := ds.Table(table).LoaderFrom(gcs)
		l.ReferenceFileSchemaURI = ref
		lctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		job, err := l.Run(lctx)
		if err != nil {
			return err
		}
		st, err := job.Wait(lctx)
		if err == nil {
			err = st.Err()
		}
		return err
	}
	read := func(table string) string {
		return strings.Join(queryRows(t, ctx, c, h.Project(), ds.DatasetID, "SELECT id, s FROM `"+ds.DatasetID+"."+table+"` ORDER BY id"), " / ")
	}
	wantCode := func(name string, err error, code int, text string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != code || !strings.Contains(ge.Message, text) {
			t.Errorf("%s: %v, want HTTP %d naming %q", name, err, code, text)
		}
	}
	for name, f := range formats {
		fs := f.files
		dir := fs.one[:strings.LastIndex(fs.one, "/")+1]
		// The data files, by the reference's schema; the reference's own
		// row is not loaded.
		table := name + "_data"
		if err := run(table, f.format, fs.ref, fs.one, fs.two); err != nil {
			t.Errorf("%s: the load: %v", table, err)
		} else {
			md, err := ds.Table(table).Metadata(ctx)
			if err != nil || !strings.HasPrefix(describeSchema(md.Schema), "id:INTEGER") || len(md.Schema) != 2 {
				t.Errorf("%s: the table's schema: %+v %v", table, md, err)
			}
			if got := read(table); got != "1|a / 2|b" {
				t.Errorf("%s: the rows read back %q, want the data files' only", table, got)
			}
		}
		// A wildcard of the data files.
		table = name + "_wildcard"
		if err := run(table, f.format, fs.ref, dir+"*"); err != nil {
			t.Errorf("%s: the load: %v", table, err)
		} else if got := read(table); got != "1|a / 2|b" {
			t.Errorf("%s: the rows read back %q", table, got)
		}
		// The reference among the source URIs is loaded too.
		table = name + "_with_ref"
		if err := run(table, f.format, fs.ref, fs.ref, fs.one); err != nil {
			t.Errorf("%s: the load: %v", table, err)
		} else if got := read(table); got != "0|ref / 1|a" {
			t.Errorf("%s: the rows read back %q", table, got)
		}
		// Refused: nothing loaded, no table.
		for _, c := range []struct {
			table, ref string
			uris       []string
			code       int
			text       string
		}{
			{name + "_missing", "gs://" + bucket.BucketName() + "/" + name + "/none", []string{fs.one}, http.StatusNotFound, "Not found: URI"},
			{name + "_differs", fs.ref, []string{fs.other}, http.StatusNotImplemented, "reference file"},
			{name + "_ref_wildcard", dir + "*", []string{fs.one}, http.StatusNotImplemented, "wildcard"},
		} {
			wantCode(c.table, run(c.table, f.format, c.ref, c.uris...), c.code, c.text)
			if _, err := ds.Table(c.table).Metadata(ctx); err == nil {
				t.Errorf("%s: the refused load made its table", c.table)
			}
		}
	}
}
