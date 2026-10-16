//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/linkedin/goavro/v2"
)

// TestBigQueryExtractOfParameterizedDecimals (#957), through the official
// Go client: an AVRO or PARQUET extract of a NUMERIC(P, S), NUMERIC(P)
// (scale 0) or BIGNUMERIC(P, S) column writes a decimal of the column's own precision
// and scale, as BigQuery's export details give it ("Parameterized
// NUMERIC(P[, S]) and BIGNUMERIC(P[, S]) data types transfer their
// precision and scale type parameters to the Avro decimal logical type";
// the Parquet table's DECIMAL (precision = P, scale = S)). Each file is
// read back from the instance's Cloud Storage with goavro and Arrow. A
// value written with more digits after the point is stored rounded
// (#1401), and extracts.
//
// covers: bigquery.tables.insert, bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get
func TestBigQueryExtractOfParameterizedDecimals(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-decimal")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	names := []string{"dec.avro", "dec.parquet", "wide.avro", "wide.parquet"}
	t.Cleanup(func() {
		for _, n := range names {
			_ = bucket.Object(n).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	schema := bigquery.Schema{
		{Name: "n", Type: bigquery.NumericFieldType, Precision: 5, Scale: 2},
		{Name: "b", Type: bigquery.BigNumericFieldType, Precision: 40, Scale: 5},
		{Name: "i", Type: bigquery.NumericFieldType, Precision: 10}, // scale 0
	}
	for _, tbl := range []string{"dec", "wide"} {
		if err := ds.Table(tbl).Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatalf("create %s: %v", tbl, err)
		}
	}
	for _, sql := range []string{
		"INSERT `" + ds.DatasetID + ".dec` (n, b, i) VALUES (NUMERIC '-123.45', BIGNUMERIC '12345678901234567890123456789012345.12345', NUMERIC '42')",
		// 1.234 has three digits after the point: stored rounded to 1.23,
		// as BigQuery's NUMERIC(5, 2) holds two (#1401).
		"INSERT `" + ds.DatasetID + ".wide` (n, b, i) VALUES (NUMERIC '1.234', NULL, NULL)",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	extract := func(table, name string, format bigquery.DataFormat) error {
		ref := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + name)
		ref.DestinationFormat = format
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
	read := func(name string) []byte {
		t.Helper()
		r, err := bucket.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return b
	}

	// AVRO: the writer schema's decimals, and the values.
	if err := extract("dec", "dec.avro", bigquery.Avro); err != nil {
		t.Fatalf("an AVRO extract of parameterized decimals: %v", err)
	}
	ocf, err := goavro.NewOCFReader(bytes.NewReader(read("dec.avro")))
	if err != nil {
		t.Fatal(err)
	}
	var ws struct {
		Fields []struct {
			Name string            `json:"name"`
			Type []json.RawMessage `json:"type"`
		} `json:"fields"`
	}
	if err := json.Unmarshal([]byte(ocf.Codec().Schema()), &ws); err != nil || len(ws.Fields) != 3 {
		t.Fatalf("writer schema %s: %v", ocf.Codec().Schema(), err)
	}
	for i, want := range []map[string]any{
		{"type": "bytes", "logicalType": "decimal", "precision": 5.0, "scale": 2.0},
		{"type": "bytes", "logicalType": "decimal", "precision": 40.0, "scale": 5.0},
		{"type": "bytes", "logicalType": "decimal", "precision": 10.0, "scale": 0.0},
	} {
		var got map[string]any
		if len(ws.Fields[i].Type) != 2 || string(ws.Fields[i].Type[0]) != `"null"` || json.Unmarshal(ws.Fields[i].Type[1], &got) != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("AVRO field %s: %s, want a union of null and %v", ws.Fields[i].Name, ws.Fields[i].Type, want)
		}
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
		t.Fatalf("dec.avro: %d rows, want 1", len(rows))
	}
	wantB, _ := new(big.Rat).SetString("12345678901234567890123456789012345.12345")
	if n, _ := rows[0]["n"].(map[string]any)["bytes.decimal"].(*big.Rat); n == nil || n.Cmp(big.NewRat(-12345, 100)) != 0 {
		t.Errorf("AVRO n read back %v, want -123.45", rows[0]["n"])
	}
	if b, _ := rows[0]["b"].(map[string]any)["bytes.decimal"].(*big.Rat); b == nil || b.Cmp(wantB) != 0 {
		t.Errorf("AVRO b read back %v, want %s", rows[0]["b"], wantB.FloatString(5))
	}
	if i, _ := rows[0]["i"].(map[string]any)["bytes.decimal"].(*big.Rat); i == nil || i.Cmp(big.NewRat(42, 1)) != 0 {
		t.Errorf("AVRO i read back %v, want 42", rows[0]["i"])
	}

	// PARQUET: DECIMAL(5, 2), (40, 5) and (10, 0), read back by Arrow.
	if err := extract("dec", "dec.parquet", bigquery.Parquet); err != nil {
		t.Fatalf("a PARQUET extract of parameterized decimals: %v", err)
	}
	pf, err := file.NewParquetReader(bytes.NewReader(read("dec.parquet")))
	if err != nil {
		t.Fatal(err)
	}
	sch := pf.MetaData().Schema
	want := map[string]string{
		"n": "FIXED_LEN_BYTE_ARRAY Decimal(precision=5, scale=2)",
		"b": "FIXED_LEN_BYTE_ARRAY Decimal(precision=40, scale=5)",
		"i": "FIXED_LEN_BYTE_ARRAY Decimal(precision=10, scale=0)",
	}
	for i := 0; i < sch.NumColumns(); i++ {
		col := sch.Column(i)
		if got := col.PhysicalType().String() + " " + col.LogicalType().String(); want[col.Path()] != got {
			t.Errorf("PARQUET column %s is %s, want %s", col.Path(), got, want[col.Path()])
		}
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
	tr := array.NewTableReader(tbl, 10)
	defer tr.Release()
	var cells []string
	for tr.Next() {
		rec := tr.Record()
		for j := 0; j < int(rec.NumCols()); j++ {
			for i := 0; i < int(rec.NumRows()); i++ {
				// The unscaled integer, exactly: Arrow's ValueStr of a
				// Decimal256 goes through a float.
				var n *big.Int
				var scale int32
				switch col := rec.Column(j).(type) {
				case *array.Decimal128:
					n, scale = col.Value(i).BigInt(), col.DataType().(arrow.DecimalType).GetScale()
				case *array.Decimal256:
					n, scale = col.Value(i).BigInt(), col.DataType().(arrow.DecimalType).GetScale()
				default:
					t.Fatalf("PARQUET column %d read back as %s", j, col.DataType())
				}
				cells = append(cells, new(big.Rat).SetFrac(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)).FloatString(int(scale)))
			}
		}
	}
	if w := []string{"-123.45", "12345678901234567890123456789012345.12345", "42"}; !reflect.DeepEqual(cells, w) {
		t.Errorf("PARQUET values %q, want %q", cells, w)
	}

	// 1.234 written to the NUMERIC(5, 2) column is stored rounded, 1.23
	// (#1401), so its extracts succeed as BigQuery's would.
	for name, format := range map[string]bigquery.DataFormat{"wide.avro": bigquery.Avro, "wide.parquet": bigquery.Parquet} {
		if err := extract("wide", name, format); err != nil {
			t.Errorf("an extract of a NUMERIC(5, 2) written 1.234, stored 1.23 (%s): %v", name, err)
		}
	}
}
