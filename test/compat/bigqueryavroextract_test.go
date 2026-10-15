//go:build compat

package compat

import (
	"bytes"
	"context"
	"io"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/apache/arrow/go/v15/arrow/array"
	"github.com/apache/arrow/go/v15/arrow/memory"
	"github.com/apache/arrow/go/v15/parquet/file"
	"github.com/apache/arrow/go/v15/parquet/pqarrow"
	"github.com/linkedin/goavro/v2"
)

// TestBigQueryAvroExtractOfOtherTypes (#957), through the official Go
// client: an AVRO extract writes TIMESTAMP as timestamp-micros, DATE as a
// string (an int date with UseAvroLogicalTypes), NUMERIC as decimal(38,
// 9), a STRUCT as a nested record and an ARRAY as an Avro array, as
// BigQuery's "Avro export details" give them; a TIME without
// UseAvroLogicalTypes is 501, and nothing is written.
func TestBigQueryAvroExtractOfOtherTypes(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-avro")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		for _, n := range []string{"plain.avro", "logical.avro", "time.avro", "time-logical.avro"} {
			_ = bucket.Object(n).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	for _, sql := range []string{
		"CREATE TABLE `" + ds.DatasetID + ".src` AS SELECT TIMESTAMP '2020-01-02 03:04:05.123456 UTC' AS ts, DATE '2020-01-02' AS d, " +
			"NUMERIC '12.5' AS n, STRUCT(7 AS a, ['p', 'q'] AS tags) AS r, [1, 2] AS xs",
		"CREATE TABLE `" + ds.DatasetID + ".tm` AS SELECT TIME '03:04:05.5' AS t",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	extract := func(table, name string, logical bool) error {
		ref := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + name)
		ref.DestinationFormat = bigquery.Avro
		e := ds.Table(table).ExtractorTo(ref)
		e.UseAvroLogicalTypes = logical
		ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := e.Run(ectx)
		if err != nil {
			return err
		}
		st, err := job.Wait(ectx)
		if err == nil {
			err = st.Err()
		}
		return err
	}
	read := func(name string) map[string]any {
		t.Helper()
		r, err := bucket.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		ocf, err := goavro.NewOCFReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var rows []map[string]any
		for ocf.Scan() {
			v, err := ocf.Read()
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, v.(map[string]any))
		}
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows, want 1", name, len(rows))
		}
		return rows[0]
	}
	for _, logical := range []bool{false, true} {
		name := map[bool]string{false: "plain.avro", true: "logical.avro"}[logical]
		if err := extract("src", name, logical); err != nil {
			t.Fatalf("an AVRO extract (logical types %v): %v", logical, err)
		}
		row := read(name)
		if ts, _ := row["ts"].(map[string]any)["long.timestamp-micros"].(time.Time); ts.UnixMicro() != 1577934245123456 {
			t.Errorf("%s: ts %v", name, row["ts"])
		}
		if logical {
			if d, _ := row["d"].(map[string]any)["int.date"].(time.Time); d.Format("2006-01-02") != "2020-01-02" {
				t.Errorf("%s: d %v", name, row["d"])
			}
		} else if d := row["d"].(map[string]any)["string"]; d != "2020-01-02" {
			t.Errorf("%s: d %v", name, row["d"])
		}
		if n, _ := row["n"].(map[string]any)["bytes.decimal"].(*big.Rat); n == nil || n.Cmp(big.NewRat(25, 2)) != 0 {
			t.Errorf("%s: n %v", name, row["n"])
		}
		var rec map[string]any
		for _, v := range row["r"].(map[string]any) {
			rec, _ = v.(map[string]any)
		}
		if !reflect.DeepEqual(rec["a"], map[string]any{"long": int64(7)}) || !reflect.DeepEqual(rec["tags"], []any{"p", "q"}) {
			t.Errorf("%s: r %v", name, row["r"])
		}
		if !reflect.DeepEqual(row["xs"], []any{int64(1), int64(2)}) {
			t.Errorf("%s: xs %v", name, row["xs"])
		}
	}
	if err := extract("tm", "time-logical.avro", true); err != nil {
		t.Fatalf("an AVRO extract of a TIME with logical types: %v", err)
	}
	if tm, _ := read("time-logical.avro")["t"].(map[string]any)["long.time-micros"].(time.Duration); tm != 3*time.Hour+4*time.Minute+5500*time.Millisecond {
		t.Errorf("TIME read back %v", tm)
	}
	wantReason(t, "an AVRO extract of a TIME without logical types", extract("tm", "time.avro", false), 501, "notImplemented")
	if _, err := bucket.Object("time.avro").Attrs(ctx); err == nil {
		t.Errorf("a 501 extract wrote time.avro")
	}
}

// TestBigQueryParquetExtract (#957), through the official Go client: a
// PARQUET extract writes each column of the Parquet type and logical type
// BigQuery's "Parquet export details" give, a RECORD as a group, with the
// job's compression; a FLOAT64 or REPEATED column is 501, and nothing is
// written.
func TestBigQueryParquetExtract(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-parquet")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	names := []string{"plain.parquet", "snappy.parquet", "zstd.parquet", "float.parquet", "rep.parquet"}
	t.Cleanup(func() {
		for _, n := range names {
			_ = bucket.Object(n).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	for _, sql := range []string{
		"CREATE TABLE `" + ds.DatasetID + ".src` AS SELECT 1 AS i, TRUE AS b, 'x' AS s, b'ab' AS y, NUMERIC '12.5' AS n, " +
			"DATE '2020-01-02' AS d, DATETIME '2020-01-02 03:04:05.5' AS dt, TIMESTAMP '2020-01-02 03:04:05.123456 UTC' AS ts, " +
			"TIME '03:04:05' AS tm, STRUCT(7 AS a, 'q' AS t) AS r " +
			"UNION ALL SELECT 2, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL",
		"CREATE TABLE `" + ds.DatasetID + ".fl` AS SELECT 1.5 AS f",
		"CREATE TABLE `" + ds.DatasetID + ".rep` AS SELECT [1, 2] AS xs",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	extract := func(table, name string, comp bigquery.Compression) error {
		ref := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + name)
		ref.DestinationFormat = bigquery.Parquet
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
	for name, comp := range map[string]bigquery.Compression{"plain.parquet": bigquery.None, "snappy.parquet": bigquery.Snappy, "zstd.parquet": "ZSTD"} {
		if err := extract("src", name, comp); err != nil {
			t.Fatalf("a PARQUET extract (%s): %v", name, err)
		}
		r, err := bucket.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		r.Close()
		pf, err := file.NewParquetReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The Parquet schema, as the documentation gives it.
		sch := pf.MetaData().Schema
		want := map[string]string{
			"i": "INT64 None", "b": "BOOLEAN None", "s": "BYTE_ARRAY String", "y": "BYTE_ARRAY None",
			"n": "FIXED_LEN_BYTE_ARRAY Decimal(precision=38, scale=9)", "d": "INT32 Date",
			"dt": "INT64 Timestamp(isAdjustedToUTC=false, timeUnit=microseconds, is_from_converted_type=false, force_set_converted_type=false)",
			"ts": "INT64 Timestamp(isAdjustedToUTC=false, timeUnit=microseconds, is_from_converted_type=false, force_set_converted_type=false)",
			"tm": "INT64 Time(isAdjustedToUTC=true, timeUnit=microseconds)", "r.a": "INT64 None", "r.t": "BYTE_ARRAY String",
		}
		for i := 0; i < sch.NumColumns(); i++ {
			col := sch.Column(i)
			got := col.PhysicalType().String() + " " + col.LogicalType().String()
			if w, ok := want[col.Path()]; !ok || w != got {
				t.Errorf("%s: column %s is %s, want %s", name, col.Path(), got, w)
			}
		}
		if sch.NumColumns() != len(want) {
			t.Errorf("%s: %d columns, want %d", name, sch.NumColumns(), len(want))
		}
		// The rows, through Arrow.
		fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
		if err != nil {
			t.Fatal(err)
		}
		tbl, err := fr.ReadTable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tbl.NumRows() != 2 {
			t.Errorf("%s: %d rows, want 2", name, tbl.NumRows())
		}
		var rows []string
		tr := array.NewTableReader(tbl, 10)
		for tr.Next() {
			rec := tr.Record()
			for i := 0; i < int(rec.NumRows()); i++ {
				var cells []string
				for j := 0; j < int(rec.NumCols()); j++ {
					cells = append(cells, rec.Column(j).ValueStr(i))
				}
				rows = append(rows, strings.Join(cells, "|"))
			}
		}
		tr.Release()
		tbl.Release()
		sort.Strings(rows)
		want2 := []string{
			`1|true|x|YWI=|12.5|2020-01-02|2020-01-02 03:04:05.5Z|2020-01-02 03:04:05.123456Z|03:04:05.000000|{"a":7,"t":"q"}`,
			"2|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)|(null)",
		}
		if !reflect.DeepEqual(rows, want2) {
			t.Errorf("%s: rows %q, want %q", name, rows, want2)
		}
	}
	wantReason(t, "a PARQUET extract of a FLOAT64", extract("fl", "float.parquet", bigquery.None), 501, "notImplemented")
	wantReason(t, "a PARQUET extract of a REPEATED column", extract("rep", "rep.parquet", bigquery.None), 501, "notImplemented")
	for _, n := range []string{"float.parquet", "rep.parquet"} {
		if _, err := bucket.Object(n).Attrs(ctx); err == nil {
			t.Errorf("a 501 extract wrote %s", n)
		}
	}
}
