//go:build ignore

// Writes enum.parquet with Apache Arrow's Parquet writer for Go (the
// module's github.com/apache/arrow/go/v15), as pyarrow writes no ENUM
// column. Run by hand from this directory, never in CI:
//
//	go run genenum.go
package main

import (
	"log"
	"os"

	"github.com/apache/arrow/go/v15/parquet"
	"github.com/apache/arrow/go/v15/parquet/compress"
	"github.com/apache/arrow/go/v15/parquet/file"
	"github.com/apache/arrow/go/v15/parquet/schema"
)

func main() {
	// e: an optional BYTE_ARRAY ENUM column, of "red", "", NULL and a byte
	// that is not UTF-8.
	e, err := schema.NewPrimitiveNodeLogical("e", parquet.Repetitions.Optional, schema.EnumLogicalType{},
		parquet.Types.ByteArray, -1, -1)
	if err != nil {
		log.Fatal(err)
	}
	root, err := schema.NewGroupNode("schema", parquet.Repetitions.Required, schema.FieldList{e}, -1)
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range []struct {
		name string
		vals []parquet.ByteArray
		defs []int16
	}{
		{"enum.parquet", []parquet.ByteArray{[]byte("red"), []byte("")}, []int16{1, 1, 0}},
		{"enum_bytes.parquet", []parquet.ByteArray{{0xff}}, []int16{1}},
	} {
		out, err := os.Create(f.name)
		if err != nil {
			log.Fatal(err)
		}
		w := file.NewParquetWriter(out, root, file.WithWriterProps(parquet.NewWriterProperties(
			parquet.WithCompression(compress.Codecs.Uncompressed), parquet.WithCreatedBy("parquet-go (Apache Arrow) genenum.go"))))
		rg := w.AppendRowGroup()
		cw, err := rg.NextColumn()
		if err != nil {
			log.Fatal(err)
		}
		if _, err := cw.(*file.ByteArrayColumnChunkWriter).WriteBatch(f.vals, f.defs, nil); err != nil {
			log.Fatal(err)
		}
		if err := cw.Close(); err != nil {
			log.Fatal(err)
		}
		if err := rg.Close(); err != nil {
			log.Fatal(err)
		}
		if err := w.Close(); err != nil {
			log.Fatal(err)
		}
	}
}
