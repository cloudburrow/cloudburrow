//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// parquetFixture reads a Parquet file written by Apache Arrow's Parquet
// writer (internal/bigqueryfront/testdata/parquet, gen.py).
func parquetFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), "internal", "bigqueryfront", "testdata", "parquet", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func parquetUpload(data string, schema bigquery.Schema) bigquery.LoadSource {
	s := bigquery.NewReaderSource(strings.NewReader(data))
	s.SourceFormat = bigquery.Parquet
	s.Schema = schema
	return s
}

// TestBigQueryParquetLoadColumns (#988): a Parquet load's columns, read
// from the file, are held to its table's as BigQuery documents, through
// the official Go client. Measured first (#988), each of the loads the
// front now refuses loaded: into a table (a INT64) the file's column b
// was dropped; into (z STRING) three rows of NULL; and into (a STRING, b
// STRING) the INT64 column a as text. The schema changes and names in
// another case that were 501 here are loaded since #1006
// (TestBigQueryParquetSchemaChanges).
func TestBigQueryParquetLoadColumns(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	ab := parquetFixture(t, "ab.parquet")
	required := parquetFixture(t, "ab_required.parquet")
	i, s := bigquery.IntegerFieldType, bigquery.StringFieldType
	col := func(name string, typ bigquery.FieldType) *bigquery.FieldSchema {
		return &bigquery.FieldSchema{Name: name, Type: typ}
	}
	for n, c2 := range []struct {
		name   string
		table  bigquery.Schema // nil: no table
		data   string
		load   func(*bigquery.Loader)
		code   int
		reason string
		msg    string
	}{
		{"a column the table lacks", bigquery.Schema{col("a", i)}, ab, nil, 400, "invalid", "Cannot add fields (field: b)"},
		{"no column of the table's", bigquery.Schema{col("z", s)}, ab, nil, 400, "invalid", "Cannot add fields (field: a)"},
		{"another type", bigquery.Schema{col("a", s), col("b", s)}, ab, nil, 400, "invalid", "Field a has changed type from STRING to INTEGER"},
		{"NULLABLE into REQUIRED", bigquery.Schema{{Name: "a", Type: i, Required: true}, col("b", s)}, ab, nil, 400, "invalid",
			"Field a has changed mode from REQUIRED to NULLABLE"},
		{"a REQUIRED column the file lacks", bigquery.Schema{col("a", i), col("b", s), {Name: "c", Type: s, Required: true}}, ab, nil,
			400, "invalid", "Field c is missing in new schema"},
		{"not a Parquet file", bigquery.Schema{col("a", i), col("b", s)}, "a,b\n1,x\n", nil, 400, "invalid", "not a Parquet file"},
		{"ALLOW_FIELD_ADDITION of a REQUIRED column", bigquery.Schema{{Name: "a", Type: i, Required: true}}, required,
			func(l *bigquery.Loader) { l.SchemaUpdateOptions = []string{"ALLOW_FIELD_ADDITION"} }, 501, "notImplemented",
			"adds a REQUIRED column, b"},
		{"a schema that is not the file's", nil, "", nil, 501, "notImplemented", "a schema that is not the file's"},
		{"TIMESTAMP(NANOS)", nil, parquetFixture(t, "ts_ns.parquet"), nil, 501, "notImplemented", "does not list"},
	} {
		table := ds.Table(fmt.Sprintf("refused_%d", n))
		if c2.table != nil {
			if err := table.Create(ctx, &bigquery.TableMetadata{Schema: c2.table}); err != nil {
				t.Fatal(err)
			}
			// A row the refused load must leave alone.
			if _, err := c.Query("INSERT " + ds.DatasetID + "." + table.TableID + " (" + c2.table[0].Name + ") VALUES (" +
				map[bool]string{true: "1", false: "'1'"}[c2.table[0].Type == i] + ")").Read(ctx); err != nil {
				t.Fatalf("%s: insert: %v", c2.name, err)
			}
		}
		src := parquetUpload(c2.data, nil)
		if c2.data == "" {
			src = parquetUpload(ab, bigquery.Schema{col("z", s)})
		}
		l := table.LoaderFrom(src)
		if c2.load != nil {
			c2.load(l)
		}
		err := runLoad(ctx, l)
		wantReason(t, c2.name, err, c2.code, c2.reason)
		if err == nil || !strings.Contains(err.Error(), c2.msg) || !strings.Contains(err.Error(), "Nothing was loaded") {
			t.Errorf("%s: %v, want %q", c2.name, err, c2.msg)
		}
		if c2.table == nil {
			if tableExists(t, ctx, table) {
				t.Errorf("%s: the table was made", c2.name)
			}
			continue
		}
		if got := rows(t, ctx, c, "SELECT COUNT(*) FROM "+ds.DatasetID+"."+table.TableID); fmt.Sprint(got) != "[[1]]" {
			t.Errorf("%s: the table holds %v rows, want its one", c2.name, got)
		}
		if md, err := table.Metadata(ctx); err != nil || len(md.Schema) != len(c2.table) {
			t.Errorf("%s: the table's schema is now %v (%v)", c2.name, md, err)
		}
	}

	// Loaded: a table column the file lacks is NULL; REQUIRED file columns
	// into NULLABLE ones; the file's own schema given; a new table takes
	// the file's schema, REQUIRED columns REQUIRED; WRITE_TRUNCATE of a
	// table whose schema is the file's.
	abc := ds.Table("abc")
	if err := abc.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{col("a", i), col("b", s), col("c", s)}}); err != nil {
		t.Fatal(err)
	}
	if err := runLoad(ctx, abc.LoaderFrom(parquetUpload(ab, nil))); err != nil {
		t.Fatalf("into (a, b, c): %v", err)
	}
	if err := runLoad(ctx, abc.LoaderFrom(parquetUpload(required, nil))); err != nil {
		t.Fatalf("REQUIRED into NULLABLE: %v", err)
	}
	if err := runLoad(ctx, abc.LoaderFrom(parquetUpload(ab, bigquery.Schema{col("a", i), col("b", s)}))); err != nil {
		t.Fatalf("with the file's schema: %v", err)
	}
	if got := rows(t, ctx, c, "SELECT a, b, c FROM "+ds.DatasetID+".abc ORDER BY a, b LIMIT 3"); fmt.Sprint(got) != "[[1 x <nil>] [1 x <nil>] [1 x <nil>]]" {
		t.Errorf("into (a, b, c): %v", got)
	}
	if got := rows(t, ctx, c, "SELECT COUNT(*) FROM "+ds.DatasetID+".abc"); fmt.Sprint(got) != "[[9]]" {
		t.Errorf("into (a, b, c): %v rows, want 9", got)
	}
	fresh := ds.Table("fresh")
	if err := runLoad(ctx, fresh.LoaderFrom(parquetUpload(required, nil))); err != nil {
		t.Fatalf("into a new table: %v", err)
	}
	md, err := fresh.Metadata(ctx)
	if err != nil || len(md.Schema) != 2 || md.Schema[0].Name != "a" || md.Schema[0].Type != i || !md.Schema[0].Required ||
		md.Schema[1].Name != "b" || md.Schema[1].Type != s || !md.Schema[1].Required {
		t.Errorf("the new table's schema: %+v %v", md, err)
	}
	l := fresh.LoaderFrom(parquetUpload(required, nil))
	l.WriteDisposition = bigquery.WriteTruncate
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("WRITE_TRUNCATE with the table's schema: %v", err)
	}
	if got := rows(t, ctx, c, "SELECT a, b FROM "+ds.DatasetID+".fresh ORDER BY a"); fmt.Sprint(got) != "[[1 x] [2 y] [3 z]]" {
		t.Errorf("the new table, replaced: %v", got)
	}
	_ = project

	// From Cloud Storage: the objects' schemas are read there.
	bucket := loadBucket(t, h, "bq-parquet-cols", map[string]string{"ab.parquet": ab, "ab2.parquet": ab, "req.parquet": required,
		"upper.parquet": parquetFixture(t, "upper.parquet")})
	gcs := func(names ...string) bigquery.LoadSource {
		uris := make([]string, len(names))
		for k, n := range names {
			uris[k] = "gs://" + bucket.BucketName() + "/" + n
		}
		r := bigquery.NewGCSReference(uris...)
		r.SourceFormat = bigquery.Parquet
		return r
	}
	narrow := ds.Table("narrow")
	if err := narrow.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{col("a", i)}}); err != nil {
		t.Fatal(err)
	}
	err = runLoad(ctx, narrow.LoaderFrom(gcs("ab.parquet")))
	wantReason(t, "from Cloud Storage, a column the table lacks", err, 400, "invalid")
	err = runLoad(ctx, ds.Table("gcs_modes").LoaderFrom(gcs("ab.parquet", "req.parquet")))
	wantReason(t, "from Cloud Storage, files whose modes differ", err, 400, "invalid")
	err = runLoad(ctx, ds.Table("gcs_differ").LoaderFrom(gcs("ab.parquet", "upper.parquet")))
	wantReason(t, "from Cloud Storage, files whose columns differ", err, http.StatusNotImplemented, "notImplemented")
	if err := runLoad(ctx, ds.Table("gcs_new").LoaderFrom(gcs("ab.parquet", "ab2.parquet"))); err != nil {
		t.Fatalf("from Cloud Storage, two files into a new table: %v", err)
	}
	if got := rows(t, ctx, c, "SELECT COUNT(*), SUM(a) FROM "+ds.DatasetID+".gcs_new"); fmt.Sprint(got) != "[[6 12]]" {
		t.Errorf("from Cloud Storage, two files: %v", got)
	}
}

// TestBigQueryParquetLoadTypes (#988): each Parquet type the emulator
// loads, written by Apache Arrow, loads into a new table with the type
// BigQuery's conversion table gives it, and reads back as written, NULLs
// included. Each type or value CloudBurrow does not load is 501 and makes
// no table: the types the table does not list, a NaN (which the
// emulator's engine stores as NULL), an INT96 with nanoseconds and a
// DECIMAL NUMERIC would have to round. The types the emulator loads
// wrongly are converted by the front since #1005
// (TestBigQueryParquetConvertedTypes).
func TestBigQueryParquetLoadTypes(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := ds.Table("loadable")
	if err := runLoad(ctx, tbl.LoaderFrom(parquetUpload(parquetFixture(t, "loadable.parquet"), nil))); err != nil {
		t.Fatalf("load: %v", err)
	}
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var schema []string
	for _, f := range md.Schema {
		schema = append(schema, f.Name+" "+string(f.Type))
	}
	if got, want := strings.Join(schema, ", "), "bool BOOLEAN, int32 INTEGER, int8 INTEGER, uint16 INTEGER, uint32 INTEGER, "+
		"int64 INTEGER, float FLOAT, double FLOAT, string STRING, date DATE, ts_us TIMESTAMP"; got != want {
		t.Errorf("schema:\n got %s\nwant %s", got, want)
	}
	got := rows(t, ctx, c, "SELECT bool, int32, int8, uint16, uint32, int64, float, double, string, CAST(date AS STRING), "+
		"FORMAT_TIMESTAMP('%Y-%m-%d %H:%M:%E6S', ts_us) FROM "+ds.DatasetID+".loadable ORDER BY int32 DESC")
	want := "[[true 7 8 16 32 64 1.5 2.25 s 2024-01-02 2024-01-02 03:04:05.123456] " +
		"[false -7 -8 65535 4294967295 -9223372036854775808 -1.5 -2.25 é☃ 1969-12-31 1969-12-31 23:59:59.999999] " +
		"[<nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil>]]"
	if fmt.Sprint(got) != want {
		t.Errorf("rows:\n got %v\nwant %s", got, want)
	}

	for _, c2 := range []struct{ file, msg string }{
		{"ts_ns.parquet", "does not list"},
		{"json.parquet", "does not list"},
		{"nan.parquet", "has a NaN"},
		{"int96_ns.parquet", "part of a microsecond"},
		{"decimal_frac.parquet", "more than the 9 fractional digits of NUMERIC"},
	} {
		table := ds.Table("t_" + strings.TrimSuffix(c2.file, ".parquet"))
		err := runLoad(ctx, table.LoaderFrom(parquetUpload(parquetFixture(t, c2.file), nil)))
		wantReason(t, c2.file, err, http.StatusNotImplemented, "notImplemented")
		if err == nil || !strings.Contains(err.Error(), c2.msg) {
			t.Errorf("%s: %v, want %q", c2.file, err, c2.msg)
		}
		if tableExists(t, ctx, table) {
			t.Errorf("%s: the table was made", c2.file)
		}
	}
}
