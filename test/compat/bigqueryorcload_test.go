//go:build compat

package compat

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/scritchley/orc"
	"google.golang.org/api/googleapi"
)

// orcFile writes rows of schema (Hive's type string) as an ORC file with
// the ORC module's writer, compressed by ZLIB when zlib is set.
func orcFile(t *testing.T, schema string, zlib bool, rows ...[]any) string {
	t.Helper()
	td, err := orc.ParseSchema(schema)
	if err != nil {
		t.Fatalf("orc schema: %v", err)
	}
	opts := []orc.WriterConfigFunc{orc.SetSchema(td)}
	if zlib {
		opts = append(opts, orc.SetCompression(orc.CompressionZlib{Level: 6}))
	}
	var b bytes.Buffer
	w, err := orc.NewWriter(&b, opts...)
	if err != nil {
		t.Fatalf("orc writer: %v", err)
	}
	for _, r := range rows {
		if err := w.Write(r...); err != nil {
			t.Fatalf("orc row: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("orc close: %v", err)
	}
	return b.String()
}

// orcFixture is one of Apache ORC's example files
// (internal/bigqueryfront/testdata/orc, NOTICE).
func orcFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), "internal", "bigqueryfront", "testdata", "orc", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const orcTypesSchema = "struct<id:bigint,i:int,sm:smallint,b:boolean,f:float,d:double,name:string,v:varchar(10)," +
	"day:date,at:timestamp,tags:array<string>,attrs:map<string,int>,rec:struct<x:bigint,ys:array<double>>," +
	"u:uniontype<int,string>,one:uniontype<string>>"

func orcTypesRows(first int64) [][]any {
	return [][]any{
		{first, int64(-7), int64(3), true, float32(1.5), 0.25, "héllo 'q'", "vc", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
			time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC), []any{"x", nil, "y"}, map[string]any{"k": int64(7)},
			[]any{int64(9), []any{1.5, 2.5}}, orc.UnionValue{Tag: 1, Value: "s"}, orc.UnionValue{Tag: 0, Value: "only"}},
		{first + 1, nil, nil, nil, nil, nil, nil, nil, time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), nil, nil, nil,
			[]any{nil, nil}, orc.UnionValue{Tag: 0, Value: int64(5)}, orc.UnionValue{Tag: 0, Value: nil}},
	}
}

// TestBigQueryLoadORC (#1713): an ORC load, which the pinned emulator
// answered 500, carried out by the front by BigQuery's ORC conversions
// (Loading ORC data from Cloud Storage): the schema from the file, every
// column NULLABLE; each primitive (tinyint and binary from Apache ORC's
// own test file); a date, a timestamp truncated to microseconds; a decimal
// NUMERIC; a struct RECORD; a list REPEATED with its NULL elements
// dropped; a map a REPEATED RECORD of key and value; a union of several
// types a RECORD of field_0, field_1, and of one type that type; an
// uncompressed upload and ZLIB files from Cloud Storage (two objects, one
// job); WRITE_APPEND, WRITE_TRUNCATE and WRITE_EMPTY; hive partitioning;
// and the documented refusals: a file that is not ORC, a list of lists, a
// list of maps (400), a decimal decimalTargetTypes makes STRING (501).
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.jobs.query
func TestBigQueryLoadORC(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	data := orcFile(t, orcTypesSchema, false, orcTypesRows(1)...)

	// An upload, into a new table.
	typed := ds.Table("typed")
	src := bigquery.NewReaderSource(strings.NewReader(data))
	src.SourceFormat = bigquery.ORC
	ls := avroLoadStats(t, ctx, typed.LoaderFrom(src))
	if ls.OutputRows != 2 || ls.InputFiles != 1 || ls.InputFileBytes != int64(len(data)) {
		t.Errorf("statistics: %+v", ls)
	}
	if got, want := loadedSchema(t, ctx, typed), "id INTEGER, i INTEGER, sm INTEGER, b BOOLEAN, f FLOAT, d FLOAT, "+
		"name STRING, v STRING, day DATE, at TIMESTAMP, tags STRING REPEATED, attrs RECORD REPEATED (key STRING, "+
		"value INTEGER), rec RECORD (x INTEGER, ys FLOAT REPEATED), u RECORD (field_0 INTEGER, field_1 STRING), "+
		"one STRING"; got != want {
		t.Errorf("schema:\n got %s\nwant %s", got, want)
	}
	if got, want := q("SELECT id, i, sm, b, f, d, name, v, CAST(day AS STRING), FORMAT_TIMESTAMP('%F %H:%M:%E*S', `at`), "+
		"ARRAY_TO_STRING(tags, '|'), (SELECT STRING_AGG(a.key || '=' || CAST(a.value AS STRING)) FROM UNNEST(attrs) a), "+
		"rec.x, ARRAY_LENGTH(rec.ys), u.field_0, u.field_1, one FROM DS.typed ORDER BY id"),
		"[[1 -7 3 true 1.5 0.25 héllo 'q' vc 2024-02-29 2024-01-02 03:04:05.123456 x|y k=7 9 2 <nil> s only] "+
			"[2 <nil> <nil> <nil> <nil> <nil> <nil> <nil> 1969-12-31 <nil>  <nil> <nil> 0 5 <nil> <nil>]]"; got != want {
		t.Errorf("rows:\n got %s\nwant %s", got, want)
	}

	// From Cloud Storage: two ZLIB files, one job, appended; then
	// WRITE_TRUNCATE; then WRITE_EMPTY into the table with rows fails.
	nested := orcFile(t, "struct<a:array<array<int>>>", false)
	bucket := loadBucket(t, h, "bq-orc", map[string]string{
		"orc/a.orc":           orcFile(t, orcTypesSchema, true, orcTypesRows(3)[0]),
		"orc/b.orc":           orcFile(t, orcTypesSchema, true, orcTypesRows(3)[1]),
		"java/test1.orc":      orcFixture(t, "TestOrcFile.test1.orc"),
		"java/decimal.orc":    orcFixture(t, "decimal.orc"),
		"hive/k=1/part.orc":   orcFile(t, "struct<v:bigint>", true, []any{int64(1)}),
		"hive/k=2/part.orc":   orcFile(t, "struct<v:bigint>", true, []any{int64(2)}, []any{int64(3)}),
		"bad/x.orc":           "not an ORC file",
		"nested/lists.orc":    nested,
		"nested/listmaps.orc": orcFile(t, "struct<a:array<map<string,int>>>", false),
	})
	gcs := func(name string) *bigquery.GCSReference {
		g := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + name)
		g.SourceFormat = bigquery.ORC
		return g
	}
	l := typed.LoaderFrom(gcs("orc/*"))
	if ls := avroLoadStats(t, ctx, l); ls.OutputRows != 2 || ls.InputFiles != 2 {
		t.Errorf("Cloud Storage statistics: %+v", ls)
	}
	if got, want := q("SELECT id, name FROM DS.typed ORDER BY id"),
		"[[1 héllo 'q'] [2 <nil>] [3 héllo 'q'] [4 <nil>]]"; got != want {
		t.Errorf("appended rows: got %s want %s", got, want)
	}
	l.WriteDisposition = bigquery.WriteTruncate
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("WRITE_TRUNCATE: %v", err)
	}
	if got, want := q("SELECT id FROM DS.typed ORDER BY id"), "[[3] [4]]"; got != want {
		t.Errorf("truncated: got %s want %s", got, want)
	}
	l.WriteDisposition = bigquery.WriteEmpty
	if err := runLoad(ctx, l); err == nil || !strings.Contains(err.Error(), "Already Exists") {
		t.Errorf("WRITE_EMPTY into a table with rows: %v", err)
	}

	// Apache ORC's own test file: tinyint, binary, a nested list, a map.
	if err := runLoad(ctx, ds.Table("java").LoaderFrom(gcs("java/test1.orc"))); err != nil {
		t.Fatalf("Apache ORC's test1.orc: %v", err)
	}
	if got, want := loadedSchema(t, ctx, ds.Table("java")), "boolean1 BOOLEAN, byte1 INTEGER, short1 INTEGER, "+
		"int1 INTEGER, long1 INTEGER, float1 FLOAT, double1 FLOAT, bytes1 BYTES, string1 STRING, middle RECORD "+
		"(list RECORD REPEATED (int1 INTEGER, string1 STRING)), list RECORD REPEATED (int1 INTEGER, string1 STRING), "+
		"map RECORD REPEATED (key STRING, value RECORD (int1 INTEGER, string1 STRING))"; got != want {
		t.Errorf("test1.orc's schema:\n got %s\nwant %s", got, want)
	}
	if got, want := q("SELECT byte1, long1, TO_HEX(bytes1), string1, ARRAY_LENGTH(middle.list), list[OFFSET(0)].string1, "+
		"ARRAY_LENGTH(map), (SELECT STRING_AGG(m.key ORDER BY m.key) FROM UNNEST(map) m) FROM DS.java ORDER BY byte1"),
		"[[1 9223372036854775807 0001020304 hi 2 good 0 <nil>] [100 9223372036854775807  bye 2 cat 2 chani,mauddib]]"; got != want {
		t.Errorf("test1.orc's rows:\n got %s\nwant %s", got, want)
	}
	if err := runLoad(ctx, ds.Table("dec").LoaderFrom(gcs("java/decimal.orc"))); err != nil {
		t.Fatalf("Apache ORC's decimal.orc: %v", err)
	}
	if got, want := loadedSchema(t, ctx, ds.Table("dec")), "_col0 NUMERIC"; got != want {
		t.Errorf("decimal.orc's schema: got %s want %s", got, want)
	}
	if got, want := q("SELECT COUNT(*), CAST(MIN(_col0) AS STRING), CAST(MAX(_col0) AS STRING) FROM DS.dec"),
		"[[6000 -1000.5 1999.2]]"; got != want {
		t.Errorf("decimal.orc's rows: got %s want %s", got, want)
	}

	// Hive partitioning, STRINGS: the key after the file's columns.
	hl := ds.Table("hive").LoaderFrom(gcs("hive/*"))
	hl.HivePartitioningOptions = &bigquery.HivePartitioningOptions{Mode: bigquery.StringHivePartitioningMode,
		SourceURIPrefix: "gs://" + bucket.BucketName() + "/hive/"}
	if err := runLoad(ctx, hl); err != nil {
		t.Fatalf("hive-partitioned: %v", err)
	}
	if got, want := loadedSchema(t, ctx, ds.Table("hive")), "v INTEGER, k STRING"; got != want {
		t.Errorf("hive-partitioned schema: got %s want %s", got, want)
	}
	if got, want := q("SELECT v, k FROM DS.hive ORDER BY v"), "[[1 1] [2 2] [3 2]]"; got != want {
		t.Errorf("hive-partitioned rows: got %s want %s", got, want)
	}

	// Refused as documented.
	for _, tc := range []struct {
		name string
		code int
		dec  []bigquery.DecimalTargetType
	}{
		{"bad/x.orc", http.StatusBadRequest, nil},
		{"nested/lists.orc", http.StatusBadRequest, nil},
		{"nested/listmaps.orc", http.StatusBadRequest, nil},
		{"java/decimal.orc", http.StatusNotImplemented, []bigquery.DecimalTargetType{bigquery.StringTargetType}},
	} {
		l := ds.Table("refused").LoaderFrom(gcs(tc.name))
		l.DecimalTargetTypes = tc.dec
		err := runLoad(ctx, l)
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != tc.code {
			t.Errorf("%s: got %v, want %d", tc.name, err, tc.code)
		}
	}
}
