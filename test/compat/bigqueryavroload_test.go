//go:build compat

package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/api/googleapi"
)

// avroFile writes records as an Avro object container file of schema.
func avroFile(t *testing.T, schema string, codec string, records ...map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	if codec == "" {
		codec = goavro.CompressionNullLabel
	}
	w, err := goavro.NewOCFWriter(goavro.OCFConfig{W: &buf, Schema: schema, CompressionName: codec})
	if err != nil {
		t.Fatalf("avro schema: %v", err)
	}
	if len(records) > 0 {
		rs := make([]any, len(records))
		for i, r := range records {
			rs[i] = r
		}
		if err := w.Append(rs); err != nil {
			t.Fatalf("avro records: %v", err)
		}
	}
	return buf.String()
}

func avroUpload(data string) *bigquery.ReaderSource {
	s := bigquery.NewReaderSource(strings.NewReader(data))
	s.SourceFormat = bigquery.Avro
	return s
}

const avroTypesSchema = `{"type": "record", "name": "Row", "fields": [
	{"name": "id", "type": "long", "doc": "the row"},
	{"name": "i", "type": "int"},
	{"name": "b", "type": ["null", "boolean"]},
	{"name": "f", "type": "float"},
	{"name": "d", "type": "double"},
	{"name": "s", "type": ["null", "string"]},
	{"name": "by", "type": "bytes"},
	{"name": "e", "type": {"type": "enum", "name": "Color", "symbols": ["RED", "GREEN"]}},
	{"name": "fx", "type": {"type": "fixed", "name": "Two", "size": 2}},
	{"name": "nothing", "type": "null"},
	{"name": "day", "type": {"type": "int", "logicalType": "date"}},
	{"name": "tm", "type": {"type": "int", "logicalType": "time-millis"}},
	{"name": "tu", "type": {"type": "long", "logicalType": "time-micros"}},
	{"name": "tsm", "type": {"type": "long", "logicalType": "timestamp-millis"}},
	{"name": "tsu", "type": ["null", {"type": "long", "logicalType": "timestamp-micros"}]},
	{"name": "lt", "type": {"type": "long", "logicalType": "local-timestamp-micros"}},
	{"name": "dec", "type": {"type": "bytes", "logicalType": "decimal", "precision": 10, "scale": 2}},
	{"name": "big", "type": {"type": "bytes", "logicalType": "decimal", "precision": 40, "scale": 3}},
	{"name": "tags", "type": {"type": "array", "items": "string"}},
	{"name": "rec", "type": ["null", {"type": "record", "name": "Inner", "fields": [
		{"name": "x", "type": "long"},
		{"name": "ys", "type": {"type": "array", "items": "double"}}]}]},
	{"name": "recs", "type": {"type": "array", "items": "Inner"}}
]}`

func avroTypesRecords() []map[string]any {
	ts := time.Date(2024, 1, 2, 3, 4, 5, 123000000, time.UTC)
	return []map[string]any{{
		"id": int64(1), "i": int32(-7), "b": goavro.Union("boolean", true), "f": float32(1.5), "d": 0.25,
		"s": goavro.Union("string", "héllo 'q'"), "by": []byte{0xff, 0x00}, "e": "GREEN", "fx": []byte("ab"),
		"nothing": nil, "day": time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), "tm": 3723004 * time.Millisecond,
		"tu": 3723004567 * time.Microsecond, "tsm": ts, "tsu": goavro.Union("long.timestamp-micros", ts.Add(456*time.Microsecond)),
		"lt": int64(1704164645123456), "dec": big.NewRat(-12345, 100), "big": big.NewRat(1234567890123456789, 1000),
		"tags": []any{"a", "b"},
		"rec":  goavro.Union("Inner", map[string]any{"x": int64(9), "ys": []any{1.5, 2.5}}),
		"recs": []any{map[string]any{"x": int64(1), "ys": []any{}}, map[string]any{"x": int64(2), "ys": []any{3.0}}},
	}, {
		"id": int64(2), "i": int32(0), "b": nil, "f": float32(0), "d": -1.0, "s": nil, "by": []byte{}, "e": "RED",
		"fx": []byte{0, 1}, "nothing": nil, "day": time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), "tm": time.Duration(0),
		"tu": time.Duration(0), "tsm": time.Unix(0, 0).UTC(), "tsu": nil, "lt": int64(0), "dec": big.NewRat(0, 1),
		"big": big.NewRat(0, 1), "tags": []any{}, "rec": nil, "recs": []any{},
	}}
}

// TestBigQueryAvroLoadTypes (#1457): an Avro load, which the pinned
// emulator does not have ("not support sourceFormat: AVRO"), carried out
// by the front by BigQuery's Avro conversions: each primitive, enum,
// fixed, a null field (no column), a union with null (NULLABLE), a field
// of one type (REQUIRED), arrays (REPEATED), records (RECORD, a doc its
// description), the logical types with useAvroLogicalTypes (DATE, TIME,
// TIMESTAMP, DATETIME) and without (INTEGER), and decimal NUMERIC and, by
// decimalTargetTypes, BIGNUMERIC; from an upload and from the instance's
// Cloud Storage (a deflate and a snappy file, two objects one job),
// appended and with WRITE_TRUNCATE; and a file that is not Avro, a
// union of several types and a map are refused as documented.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.jobs.query
func TestBigQueryAvroLoadTypes(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	data := avroFile(t, avroTypesSchema, "", avroTypesRecords()...)

	// With logical types.
	typed := ds.Table("typed")
	l := typed.LoaderFrom(avroUpload(data))
	l.UseAvroLogicalTypes = true
	l.DecimalTargetTypes = []bigquery.DecimalTargetType{bigquery.NumericTargetType, bigquery.BigNumericTargetType}
	ls := avroLoadStats(t, ctx, l)
	if ls.OutputRows != 2 || ls.InputFiles != 1 || ls.InputFileBytes != int64(len(data)) {
		t.Errorf("statistics: %+v", ls)
	}
	if got, want := loadedSchema(t, ctx, typed), "id INTEGER REQUIRED, i INTEGER REQUIRED, b BOOLEAN, f FLOAT REQUIRED, "+
		"d FLOAT REQUIRED, s STRING, by BYTES REQUIRED, e STRING REQUIRED, fx BYTES REQUIRED, day DATE REQUIRED, "+
		"tm TIME REQUIRED, tu TIME REQUIRED, tsm TIMESTAMP REQUIRED, tsu TIMESTAMP, lt DATETIME REQUIRED, "+
		"dec NUMERIC REQUIRED, big BIGNUMERIC REQUIRED, tags STRING REPEATED, rec RECORD (x INTEGER REQUIRED, "+
		"ys FLOAT REPEATED), recs RECORD REPEATED (x INTEGER REQUIRED, ys FLOAT REPEATED)"; got != want {
		t.Errorf("schema:\n got %s\nwant %s", got, want)
	}
	if md, err := typed.Metadata(ctx); err != nil || md.Schema[0].Description != "the row" {
		t.Errorf("the doc as the description: %v %+v", err, md)
	}
	if got, want := q("SELECT id, i, b, f, d, s, TO_HEX(`by`), e, TO_HEX(fx), CAST(day AS STRING), CAST(tm AS STRING), "+
		"CAST(tu AS STRING), FORMAT_TIMESTAMP('%F %H:%M:%E6S', tsm), FORMAT_TIMESTAMP('%F %H:%M:%E6S', tsu), "+
		"CAST(lt AS STRING), CAST(dec AS STRING), CAST(big AS STRING), ARRAY_TO_STRING(tags, '|'), rec.x, "+
		"ARRAY_LENGTH(rec.ys), ARRAY_LENGTH(recs), (SELECT SUM(r.x) FROM UNNEST(recs) r) FROM DS.typed ORDER BY id"),
		"[[1 -7 true 1.5 0.25 héllo 'q' ff00 GREEN 6162 2024-02-29 01:02:03.004 01:02:03.004567 2024-01-02 03:04:05.123000 "+
			"2024-01-02 03:04:05.123456 2024-01-02 03:04:05.123456 -123.45 1234567890123456.789 a|b 9 2 2 3] "+
			"[2 0 <nil> 0 -1 <nil>  RED 0001 1969-12-31 00:00:00 00:00:00 1970-01-01 00:00:00.000000 <nil> "+
			"1970-01-01 00:00:00 0 0  <nil> <nil> 0 <nil>]]"; got != want {
		t.Errorf("rows:\n got %s\nwant %s", got, want)
	}

	// Without logical types: the underlying numbers; decimal NUMERIC by
	// default.
	raw := ds.Table("raw")
	narrow := avroFile(t, strings.Replace(avroTypesSchema, `"precision": 40, "scale": 3`, `"precision": 12, "scale": 3`, 1),
		"", avroTypesRecords()[1])
	if err := runLoad(ctx, raw.LoaderFrom(avroUpload(narrow))); err != nil {
		t.Fatalf("load without logical types: %v", err)
	}
	if got, want := loadedSchema(t, ctx, raw), "id INTEGER REQUIRED, i INTEGER REQUIRED, b BOOLEAN, f FLOAT REQUIRED, "+
		"d FLOAT REQUIRED, s STRING, by BYTES REQUIRED, e STRING REQUIRED, fx BYTES REQUIRED, day INTEGER REQUIRED, "+
		"tm INTEGER REQUIRED, tu INTEGER REQUIRED, tsm INTEGER REQUIRED, tsu INTEGER, lt INTEGER REQUIRED, "+
		"dec NUMERIC REQUIRED, big NUMERIC REQUIRED, tags STRING REPEATED, rec RECORD (x INTEGER REQUIRED, "+
		"ys FLOAT REPEATED), recs RECORD REPEATED (x INTEGER REQUIRED, ys FLOAT REPEATED)"; got != want {
		t.Errorf("schema without logical types:\n got %s\nwant %s", got, want)
	}
	if got, want := q("SELECT day, tm, tu, tsm, lt FROM DS.raw"), "[[-1 0 0 0 0]]"; got != want {
		t.Errorf("rows without logical types: got %s want %s", got, want)
	}

	// From Cloud Storage: two files, deflate and snappy, one job, appended
	// to the typed table, then WRITE_TRUNCATE.
	recs := avroTypesRecords()
	recs[0]["id"], recs[1]["id"] = int64(3), int64(4)
	bucket := loadBucket(t, h, "bq-avro", map[string]string{
		"avro/a.avro": avroFile(t, avroTypesSchema, goavro.CompressionDeflateLabel, recs[0]),
		"avro/b.avro": avroFile(t, avroTypesSchema, goavro.CompressionSnappyLabel, recs[1]),
		"bad/x.avro":  "not an Avro file",
		"union.avro": avroFile(t, `{"type":"record","name":"R","fields":[{"name":"u","type":["null","long","string"]}]}`,
			"", map[string]any{"u": goavro.Union("long", int64(1))}),
		"map.avro": avroFile(t, `{"type":"record","name":"R","fields":[{"name":"m","type":{"type":"map","values":"long"}}]}`,
			"", map[string]any{"m": map[string]any{"k": int64(1)}}),
	})
	gcs := func(name string) *bigquery.GCSReference {
		g := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + name)
		g.SourceFormat = bigquery.Avro
		return g
	}
	l = typed.LoaderFrom(gcs("avro/*"))
	l.UseAvroLogicalTypes = true
	l.DecimalTargetTypes = []bigquery.DecimalTargetType{bigquery.NumericTargetType, bigquery.BigNumericTargetType}
	if ls := avroLoadStats(t, ctx, l); ls.OutputRows != 2 || ls.InputFiles != 2 {
		t.Errorf("Cloud Storage statistics: %+v", ls)
	}
	if got, want := q("SELECT id, s, CAST(big AS STRING) FROM DS.typed ORDER BY id"),
		"[[1 héllo 'q' 1234567890123456.789] [2 <nil> 0] [3 héllo 'q' 1234567890123456.789] [4 <nil> 0]]"; got != want {
		t.Errorf("appended rows: got %s want %s", got, want)
	}
	l.WriteDisposition = bigquery.WriteTruncate
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("WRITE_TRUNCATE: %v", err)
	}
	if got, want := q("SELECT id FROM DS.typed ORDER BY id"), "[[3] [4]]"; got != want {
		t.Errorf("truncated: got %s want %s", got, want)
	}

	// Refused: a file that is not Avro fails the job (400 BigQuery's
	// invalid); a union of several types and a map are 501.
	if err := runLoad(ctx, ds.Table("bad").LoaderFrom(gcs("bad/x.avro"))); err == nil {
		t.Error("a load of a file that is not Avro succeeded")
	}
	for _, name := range []string{"union.avro", "map.avro"} {
		err := runLoad(ctx, ds.Table("n").LoaderFrom(gcs(name)))
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
			t.Errorf("%s: got %v, want 501", name, err)
		}
	}
}

func avroLoadStats(t *testing.T, ctx context.Context, l *bigquery.Loader) *bigquery.LoadStatistics {
	t.Helper()
	job, err := l.Run(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	st, err := job.Wait(ctx)
	if err != nil || st.Err() != nil {
		t.Fatalf("load: %v %v", err, st.Err())
	}
	ls, _ := st.Statistics.Details.(*bigquery.LoadStatistics)
	if ls == nil {
		t.Fatalf("load: no load statistics")
	}
	return ls
}
