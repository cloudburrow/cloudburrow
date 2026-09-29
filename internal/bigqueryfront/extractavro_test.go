package bigqueryfront

import (
	"bytes"
	"reflect"
	"testing"

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
