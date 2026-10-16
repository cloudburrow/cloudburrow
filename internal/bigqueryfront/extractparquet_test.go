package bigqueryfront

import (
	"bytes"
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// TestParquetExtractWrites (#957): a PARQUET extract the front writes
// reads back with its columns, NULLs and a negative NUMERIC.
func TestParquetExtractWrites(t *testing.T) {
	fields := []field{{Name: "i", Type: "INT64", Mode: "REQUIRED"}, {Name: "n", Type: "NUMERIC"},
		{Name: "r", Type: "RECORD", Fields: []field{{Name: "s", Type: "STRING"}}}}
	rows := [][]any{{"1", "-1.5", map[string]any{"f": []any{map[string]any{"v": "x"}}}}, {"2", nil, nil}}
	data, msg := encodeExtract(writtenExtract{parquet: true, parquetCodec: "SNAPPY", fields: fields}, rows)
	if msg != "" {
		t.Fatal(msg)
	}
	pf, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if n := pf.NumRows(); n != 2 {
		t.Fatalf("%d rows", n)
	}
	if got := pf.MetaData().Schema.Column(2).Path(); got != "r.s" {
		t.Errorf("column 3 is %s", got)
	}
	if b := twosComplementBytes(big.NewInt(-1500000000), 16); b[0] != 0xff || b[15] != 0x00 {
		t.Errorf("-1.5 as %x", b)
	}
	if _, msg := encodeExtract(writtenExtract{parquet: true, fields: fields}, [][]any{{nil, nil, nil}}); msg == "" {
		t.Errorf("a NULL in a REQUIRED column was written")
	}
}

// TestParquetExtractOfRepeatedFloatJSON (#957): FLOAT64 as DOUBLE, JSON
// as a STRING, and REPEATED columns, of values and of RECORDs with their
// own REPEATED field, in the LIST form, read back by Arrow.
func TestParquetExtractOfRepeatedFloatJSON(t *testing.T) {
	fields := []field{{Name: "f", Type: "FLOAT64"}, {Name: "j", Type: "JSON"}, {Name: "a", Type: "INT64", Mode: "REPEATED"},
		{Name: "r", Type: "RECORD", Mode: "REPEATED", Fields: []field{{Name: "s", Type: "STRING"}, {Name: "n", Type: "INT64", Mode: "REPEATED"}}}}
	v := func(x any) map[string]any { return map[string]any{"v": x} }
	rec := func(s any, n ...any) map[string]any {
		items := []any{}
		for _, x := range n {
			items = append(items, v(x))
		}
		return map[string]any{"f": []any{v(s), v(items)}}
	}
	rows := [][]any{
		{"1.5", `{"a":1}`, []any{v("1"), v("2")}, []any{v(rec("x", "7", "8")), v(rec(nil))}},
		{"1e300", nil, []any{}, []any{}},
		{nil, "2", []any{v("3")}, []any{v(rec("y", "9"))}},
	}
	data, msg := encodeExtract(writtenExtract{parquet: true, fields: fields}, rows)
	if msg != "" {
		t.Fatal(msg)
	}
	pf, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sc := pf.MetaData().Schema
	if got := sc.Column(0).PhysicalType(); got != parquet.Types.Double {
		t.Errorf("FLOAT64 as %s", got)
	}
	if got := sc.Column(2).Path(); got != "a.list.element" {
		t.Errorf("column 3 is %s", got)
	}
	if got := sc.Column(4).Path(); got != "r.list.element.n.list.element" {
		t.Errorf("column 5 is %s", got)
	}
	fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := fr.ReadTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Release()
	want := map[string]string{
		"f": `[1.5,1e+300,null]`,
		"j": `["{\"a\":1}",null,"2"]`,
		"a": `[[1,2],[],[3]]`,
		"r": `[[{"n":[7,8],"s":"x"},{"n":[],"s":null}],[],[{"n":[9],"s":"y"}]]`,
	}
	for i, fl := range tbl.Schema().Fields() {
		col := tbl.Column(i).Data().Chunk(0)
		b, err := col.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.ReplaceAll(string(b), "\n", "")
		if w := want[fl.Name]; got != w {
			t.Errorf("%s (%s): %s, want %s", fl.Name, fl.Type, got, w)
		}
	}
}
