package bigqueryfront

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/apache/arrow/go/v15/parquet/file"
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
