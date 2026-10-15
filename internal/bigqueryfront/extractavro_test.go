package bigqueryfront

import (
	"bytes"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/linkedin/goavro/v2"
)

// TestAvroExtractReadsBack: an AVRO extract the front writes (#957) reads
// back, through an Avro reader, with the documented types and a NULLABLE
// column as a union with null, for each codec.
func TestAvroExtractReadsBack(t *testing.T) {
	fields := []field{{Name: "i", Type: "INT64", Mode: "REQUIRED"}, {Name: "s", Type: "STRING"},
		{Name: "f", Type: "FLOAT64"}, {Name: "b", Type: "BOOL"}, {Name: "y", Type: "BYTES"}}
	rows := [][]any{{"9007199254740993", "x", "1.5", "true", "YWI="}, {"2", nil, nil, nil, nil}}
	for _, codec := range []string{"", "deflate", "snappy"} {
		data, msg := encodeExtract(writtenExtract{avro: true, avroCodec: codec, fields: fields}, rows)
		if msg != "" {
			t.Fatalf("%q: %s", codec, msg)
		}
		r, err := goavro.NewOCFReader(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%q: %v", codec, err)
		}
		var got []any
		for r.Scan() {
			v, err := r.Read()
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, v)
		}
		want := []any{
			map[string]any{"i": int64(9007199254740993), "s": map[string]any{"string": "x"}, "f": map[string]any{"double": 1.5},
				"b": map[string]any{"boolean": true}, "y": map[string]any{"bytes": []byte("ab")}},
			map[string]any{"i": int64(2), "s": nil, "f": nil, "b": nil, "y": nil},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %#v", codec, got)
		}
	}
	if _, msg := encodeExtract(writtenExtract{avro: true, fields: fields}, [][]any{{nil, "x", nil, nil, nil}}); msg == "" {
		t.Error("a NULL in a REQUIRED column was written")
	}
}

// TestAvroExtractLogicalAndNested (#957): TIMESTAMP as timestamp-micros,
// DATE as a string (or int date with useAvroLogicalTypes), NUMERIC as
// decimal(38, 9), and RECORD and REPEATED columns as records and arrays.
func TestAvroExtractLogicalAndNested(t *testing.T) {
	fields := []field{{Name: "ts", Type: "TIMESTAMP"}, {Name: "d", Type: "DATE"}, {Name: "n", Type: "NUMERIC"},
		{Name: "r", Type: "RECORD", Fields: []field{{Name: "a", Type: "INT64"}, {Name: "tags", Type: "STRING", Mode: "REPEATED"}}},
		{Name: "xs", Type: "INT64", Mode: "REPEATED"}}
	rows := [][]any{
		{"1.577934245123456E9", "2020-01-02", "12.5",
			map[string]any{"f": []any{map[string]any{"v": "7"}, map[string]any{"v": []any{map[string]any{"v": "p"}}}}},
			[]any{map[string]any{"v": "1"}, map[string]any{"v": "2"}}},
		{nil, nil, nil, nil, []any{}},
	}
	for _, logical := range []bool{false, true} {
		data, msg := encodeExtract(writtenExtract{avro: true, avroLogical: logical, fields: fields}, rows)
		if msg != "" {
			t.Fatalf("logical %v: %s", logical, msg)
		}
		r, err := goavro.NewOCFReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		var got []map[string]any
		for r.Scan() {
			v, err := r.Read()
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, v.(map[string]any))
		}
		if len(got) != 2 {
			t.Fatalf("%d rows", len(got))
		}
		row := got[0]
		if ts := row["ts"].(map[string]any)["long.timestamp-micros"].(time.Time); ts.UnixMicro() != 1577934245123456 {
			t.Errorf("ts %v", ts)
		}
		if logical {
			if d := row["d"].(map[string]any)["int.date"].(time.Time); d.Format("2006-01-02") != "2020-01-02" {
				t.Errorf("d %v", d)
			}
		} else if d := row["d"].(map[string]any)["string"]; d != "2020-01-02" {
			t.Errorf("d %v", d)
		}
		if n := row["n"].(map[string]any)["bytes.decimal"].(*big.Rat); n.FloatString(1) != "12.5" {
			t.Errorf("n %v", n)
		}
		rec := row["r"].(map[string]any)["Root__r"].(map[string]any)
		if !reflect.DeepEqual(rec["a"], map[string]any{"long": int64(7)}) || !reflect.DeepEqual(rec["tags"], []any{"p"}) {
			t.Errorf("r %v", rec)
		}
		if !reflect.DeepEqual(row["xs"], []any{int64(1), int64(2)}) {
			t.Errorf("xs %v", row["xs"])
		}
		if want := (map[string]any{"ts": nil, "d": nil, "n": nil, "r": nil, "xs": []any{}}); !reflect.DeepEqual(got[1], want) {
			t.Errorf("row 2 %v", got[1])
		}
	}
}
