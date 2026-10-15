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
// BigQuery's "Avro export details" give them; TIME and DATETIME as
// strings (time-micros and a string of logicalType datetime with
// UseAvroLogicalTypes), JSON and GEOGRAPHY as strings with their sqlType;
// GZIP is 400, as documented.
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
		for _, n := range []string{"plain.avro", "logical.avro", "time.avro", "time-logical.avro", "dj.avro", "dj-logical.avro"} {
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
	// "In extract jobs, it's a string by default."
	if err := extract("tm", "time.avro", false); err != nil {
		t.Fatalf("an AVRO extract of a TIME without logical types: %v", err)
	}
	if tm := read("time.avro")["t"]; !reflect.DeepEqual(tm, map[string]any{"string": "03:04:05.5"}) {
		t.Errorf("TIME without logical types read back %v", tm)
	}
	// DATETIME a string (logicalType datetime with logical types), JSON
	// and GEOGRAPHY strings with their sqlType (extractavro.go).
	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".dj` AS SELECT DATETIME '2020-01-02 03:04:05.5' AS dt, "+
		"JSON '{\"a\": 1}' AS j, ST_GEOGPOINT(1, 2) AS g", false); err != nil {
		t.Fatal(err)
	}
	for _, logical := range []bool{false, true} {
		name := map[bool]string{false: "dj.avro", true: "dj-logical.avro"}[logical]
		if err := extract("dj", name, logical); err != nil {
			t.Fatalf("an AVRO extract of DATETIME, JSON, GEOGRAPHY (logical %v): %v", logical, err)
		}
		row := read(name)
		want := map[string]any{"dt": map[string]any{"string": "2020-01-02T03:04:05.5"},
			"j": map[string]any{"string": `{"a":1}`}, "g": map[string]any{"string": "POINT(1 2)"}}
		if !reflect.DeepEqual(row, want) {
			t.Errorf("%s: %v, want %v", name, row, want)
		}
	}
	gz := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/gzip.avro")
	gz.DestinationFormat = bigquery.Avro
	gz.Compression = bigquery.Gzip
	_, err := ds.Table("dj").ExtractorTo(gz).Run(ctx)
	// "The Avro format can't be used in combination with GZIP compression."
	wantReason(t, "an AVRO extract with GZIP", err, 400, "invalid")
	if _, err := bucket.Object("gzip.avro").Attrs(ctx); err == nil {
		t.Errorf("a refused extract wrote gzip.avro")
	}
}

// TestBigQueryParquetExtract (#957), through the official Go client: a
// PARQUET extract writes each column of the Parquet type and logical type
// BigQuery's "Parquet export details" give, a RECORD as a group, with the
// job's compression; FLOAT64 as DOUBLE, JSON as a STRING and REPEATED
// columns in Parquet's LIST form.
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
		"CREATE TABLE `" + ds.DatasetID + ".fl` AS SELECT 1.5 AS f, JSON '{\"a\": 1}' AS j",
		"CREATE TABLE `" + ds.DatasetID + ".rep` AS SELECT [1, 2] AS xs, [STRUCT('p' AS a, [3, 4] AS ns), STRUCT('q' AS a, ARRAY<INT64>[] AS ns)] AS rs",
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
	// FLOAT64 as DOUBLE, JSON as a STRING, REPEATED columns in the LIST
	// form (extractparquet.go).
	if err := extract("fl", "float.parquet", bigquery.None); err != nil {
		t.Fatalf("a PARQUET extract of FLOAT64 and JSON: %v", err)
	}
	if err := extract("rep", "rep.parquet", bigquery.Snappy); err != nil {
		t.Fatalf("a PARQUET extract of REPEATED columns: %v", err)
	}
	readBack := func(name string) (*file.Reader, string) {
		t.Helper()
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
		fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
		if err != nil {
			t.Fatal(err)
		}
		tbl, err := fr.ReadTable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tbl.Release()
		var cols []string
		for i := 0; i < int(tbl.NumCols()); i++ {
			j, _ := tbl.Column(i).Data().Chunk(0).MarshalJSON()
			cols = append(cols, tbl.Schema().Field(i).Name+"="+strings.ReplaceAll(string(j), "\n", ""))
		}
		return pf, strings.Join(cols, " ")
	}
	pf, got := readBack("float.parquet")
	if pt := pf.MetaData().Schema.Column(0).PhysicalType().String(); pt != "DOUBLE" {
		t.Errorf("FLOAT64 written as %s", pt)
	}
	if want := `f=[1.5] j=["{\"a\":1}"]`; got != want {
		t.Errorf("float.parquet: %s, want %s", got, want)
	}
	pf, got = readBack("rep.parquet")
	var paths []string
	for i := 0; i < pf.MetaData().Schema.NumColumns(); i++ {
		paths = append(paths, pf.MetaData().Schema.Column(i).Path())
	}
	if want := "xs.list.element rs.list.element.a rs.list.element.ns.list.element"; strings.Join(paths, " ") != want {
		t.Errorf("rep.parquet columns %v, want %s", paths, want)
	}
	if want := `xs=[[1,2]] rs=[[{"a":"p","ns":[3,4]},{"a":"q","ns":[]}]]`; got != want {
		t.Errorf("rep.parquet: %s, want %s", got, want)
	}
}
