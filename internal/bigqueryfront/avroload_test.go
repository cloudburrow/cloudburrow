package bigqueryfront

import (
	"bytes"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/linkedin/goavro/v2"
)

// TestAvroColumnsAndLiterals (#1457): an Avro file's schema read by
// BigQuery's Avro conversions, and its values as literals.
func TestAvroColumnsAndLiterals(t *testing.T) {
	schema := `{"type":"record","name":"R","fields":[
		{"name":"id","type":"long"},
		{"name":"n","type":"null"},
		{"name":"s","type":["null","string"]},
		{"name":"d","type":{"type":"int","logicalType":"date"}},
		{"name":"ts","type":{"type":"long","logicalType":"timestamp-millis"}},
		{"name":"lt","type":{"type":"long","logicalType":"local-timestamp-micros"}},
		{"name":"dec","type":{"type":"bytes","logicalType":"decimal","precision":5,"scale":2}},
		{"name":"e","type":{"type":"enum","name":"E","symbols":["A","B"]}},
		{"name":"in","type":{"type":"record","name":"I","fields":[{"name":"x","type":"double"}]}},
		{"name":"is","type":{"type":"array","items":"I"}}]}`
	var buf bytes.Buffer
	w, err := goavro.NewOCFWriter(goavro.OCFConfig{W: &buf, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{"id": int64(1), "n": nil, "s": goavro.Union("string", "a'b"),
		"d": time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), "ts": time.UnixMilli(1500).UTC(), "lt": int64(1),
		"dec": big.NewRat(-125, 100), "e": "B", "in": map[string]any{"x": 1.5},
		"is": []any{map[string]any{"x": 2.0}}}
	if err := w.Append([]any{rec}); err != nil {
		t.Fatal(err)
	}
	for _, logical := range []bool{true, false} {
		r, err := goavro.NewOCFReader(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		cols, err := avColumns(r.Codec().Schema(), logical, pqOptions{})
		if err != nil {
			t.Fatal(err)
		}
		fields := avTableFields(cols)
		var desc []string
		for _, f := range fields {
			desc = append(desc, f.Name+" "+f.Type+" "+f.Mode)
		}
		want := "id INTEGER REQUIRED, s STRING NULLABLE, d DATE REQUIRED, ts TIMESTAMP REQUIRED, lt DATETIME REQUIRED, " +
			"dec NUMERIC REQUIRED, e STRING REQUIRED, in RECORD REQUIRED, is RECORD REPEATED"
		if !logical {
			want = "id INTEGER REQUIRED, s STRING NULLABLE, d INTEGER REQUIRED, ts INTEGER REQUIRED, lt INTEGER REQUIRED, " +
				"dec NUMERIC REQUIRED, e STRING REQUIRED, in RECORD REQUIRED, is RECORD REPEATED"
		}
		if got := strings.Join(desc, ", "); got != want {
			t.Errorf("logical %v: columns\n got %s\nwant %s", logical, got, want)
		}
		r.Scan()
		v, err := r.Read()
		if err != nil {
			t.Fatal(err)
		}
		vals, err := avRow(cols, v.(map[string]any), "")
		if err != nil {
			t.Fatal(err)
		}
		var lits []string
		for _, f := range fields {
			s, err := literalFor(f, vals[strings.ToLower(f.Name)])
			if err != nil {
				t.Fatal(err)
			}
			lits = append(lits, s)
		}
		want = "1|'a\\'b'|DATE '2024-01-02'|TIMESTAMP '1970-01-01 00:00:01.500000+00'|DATETIME '1970-01-01 00:00:00.000001'|" +
			"NUMERIC '-1.25'|'B'|STRUCT<`x` FLOAT64>(1.5)|ARRAY<STRUCT<`x` FLOAT64>>[STRUCT<`x` FLOAT64>(2.0)]"
		if !logical {
			want = "1|'a\\'b'|19724|1500|1|NUMERIC '-1.25'|'B'|STRUCT<`x` FLOAT64>(1.5)|ARRAY<STRUCT<`x` FLOAT64>>[STRUCT<`x` FLOAT64>(2.0)]"
		}
		if got := strings.Join(lits, "|"); got != want {
			t.Errorf("logical %v: literals\n got %s\nwant %s", logical, got, want)
		}
	}
	for _, bad := range []string{
		`{"type":"record","name":"R","fields":[{"name":"u","type":["long","string"]}]}`,
		`{"type":"record","name":"R","fields":[{"name":"m","type":{"type":"map","values":"long"}}]}`,
		`{"type":"record","name":"R","fields":[{"name":"r","type":["null","R"]}]}`,
	} {
		if _, err := avColumns(bad, true, pqOptions{}); err == nil || err.(*avSchemaError).status != 501 {
			t.Errorf("%s: %v, want 501", bad, err)
		}
	}
	if _, err := avColumns(`{"type":"record","name":"R","fields":[{"name":"a","type":{"type":"array","items":{"type":"array","items":"long"}}}]}`, true, pqOptions{}); err == nil || err.(*avSchemaError).status != 400 {
		t.Errorf("an array of arrays: %v, want 400", err)
	}
}
