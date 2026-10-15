package bigqueryfront

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow/go/v15/parquet"
	"github.com/apache/arrow/go/v15/parquet/compress"
	"github.com/apache/arrow/go/v15/parquet/file"
	pqschema "github.com/apache/arrow/go/v15/parquet/schema"
)

// PARQUET extracts (#957).
//
// The emulator refuses them (400 "unsupported destination format", an
// empty object left, measured), so the front writes them itself, of the
// column types whose Parquet form BigQuery's documentation gives ("Parquet
// export details", https://cloud.google.com/bigquery/docs/exporting-data#parquet_export_details):
// INTEGER as INT64; BOOLEAN as BOOLEAN; STRING as BYTE_ARRAY with the
// STRING (UTF8) annotation; BYTES as BYTE_ARRAY; NUMERIC and BIGNUMERIC as
// FIXED_LEN_BYTE_ARRAY DECIMAL (38, 9) and (76, 38); DATE as INT32 DATE;
// DATETIME and TIMESTAMP as INT64 TIMESTAMP (isAdjustedToUTC false,
// MICROS); TIME as INT64 TIME (isAdjustedToUTC true, MICROS); a RECORD as
// a group ("represents nested data as a group"); a REQUIRED column
// required, any other optional; compressed with SNAPPY, GZIP or ZSTD, as
// the job names, or not. 501 for the rest: FLOAT64 (the documentation
// gives Parquet FLOAT, a 32-bit type that cannot hold a FLOAT64, so its
// form is unclear), GEOGRAPHY and JSON (the form of their bytes is not
// given), INTERVAL and RANGE (not in the table), a REPEATED column
// ("repeated records as repeated groups", which Parquet's LIST structure,
// the one the writer has, is not given to be), and a parameterized
// NUMERIC. The file's writer metadata, page and row group sizes and
// encodings are the writer's, not BigQuery's (UNVERIFIED): a Parquet
// reader reads the same rows and types, not the same bytes.

// parquetLeaf is how the front writes a column of a BigQuery type to
// Parquet (above): its physical and logical type, and a FIXED_LEN_BYTE_ARRAY's
// length.
type parquetLeaf struct {
	phys    parquet.Type
	logical pqschema.LogicalType
	length  int
	scale   int
}

// parquetExportType is the Parquet type the front writes a column of the
// BigQuery type typ as, and whether it writes one; a RECORD is not here.
func parquetExportType(typ string) (parquetLeaf, bool) {
	switch strings.ToUpper(typ) {
	case "INTEGER", "INT64":
		return parquetLeaf{phys: parquet.Types.Int64, logical: pqschema.NoLogicalType{}}, true
	case "BOOLEAN", "BOOL":
		return parquetLeaf{phys: parquet.Types.Boolean, logical: pqschema.NoLogicalType{}}, true
	case "STRING":
		return parquetLeaf{phys: parquet.Types.ByteArray, logical: pqschema.StringLogicalType{}}, true
	case "BYTES":
		return parquetLeaf{phys: parquet.Types.ByteArray, logical: pqschema.NoLogicalType{}}, true
	case "NUMERIC":
		return parquetLeaf{phys: parquet.Types.FixedLenByteArray, logical: pqschema.NewDecimalLogicalType(38, 9), length: 16, scale: 9}, true
	case "BIGNUMERIC":
		return parquetLeaf{phys: parquet.Types.FixedLenByteArray, logical: pqschema.NewDecimalLogicalType(76, 38), length: 32, scale: 38}, true
	case "DATE":
		return parquetLeaf{phys: parquet.Types.Int32, logical: pqschema.DateLogicalType{}}, true
	case "DATETIME", "TIMESTAMP":
		return parquetLeaf{phys: parquet.Types.Int64, logical: pqschema.NewTimestampLogicalType(false, pqschema.TimeUnitMicros)}, true
	case "TIME":
		return parquetLeaf{phys: parquet.Types.Int64, logical: pqschema.NewTimeLogicalType(true, pqschema.TimeUnitMicros)}, true
	}
	return parquetLeaf{}, false
}

// parquetUnwritten returns the first column of fields, by its path below
// prefix, and its type, that the front does not write to Parquet (above);
// "" when it writes them all.
func parquetUnwritten(fields []field, prefix string) (string, string) {
	for _, fl := range fields {
		typ := strings.ToUpper(fl.Type)
		if strings.EqualFold(fl.Mode, "REPEATED") {
			return prefix + fl.Name, "REPEATED " + typ
		}
		if typ == "RECORD" || typ == "STRUCT" {
			if loc, t := parquetUnwritten(fl.Fields, prefix+fl.Name+"."); loc != "" {
				return loc, t
			}
			continue
		}
		if _, ok := parquetExportType(typ); !ok {
			return prefix + fl.Name, typ
		}
	}
	return "", ""
}

// parquetColumn is one leaf column of the file: the fields on its path
// from the root, its type, and its values and definition levels.
type parquetColumn struct {
	path   []field
	leaf   parquetLeaf
	values []any
	defs   []int16
}

// parquetNodes is the Parquet schema nodes of fields, and appends their
// leaf columns, in order, to cols.
func parquetNodes(fields []field, parent []field, cols *[]*parquetColumn) (pqschema.FieldList, error) {
	var out pqschema.FieldList
	for _, fl := range fields {
		rep := parquet.Repetitions.Optional
		if strings.EqualFold(fl.Mode, "REQUIRED") {
			rep = parquet.Repetitions.Required
		}
		path := append(append([]field{}, parent...), fl)
		if t := strings.ToUpper(fl.Type); t == "RECORD" || t == "STRUCT" {
			kids, err := parquetNodes(fl.Fields, path, cols)
			if err != nil {
				return nil, err
			}
			g, err := pqschema.NewGroupNode(fl.Name, rep, kids, -1)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
			continue
		}
		leaf, _ := parquetExportType(fl.Type)
		n, err := pqschema.NewPrimitiveNodeLogical(fl.Name, rep, leaf.logical, leaf.phys, leaf.length, -1)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
		*cols = append(*cols, &parquetColumn{path: path, leaf: leaf})
	}
	return out, nil
}

// add appends a row's value of c, from its top-level cell v: the leaf's
// value and its definition level, the number of optional fields on the
// path that are not NULL.
func (c *parquetColumn) add(v any) error {
	var def int16
	for i, fl := range c.path {
		if v == nil {
			if strings.EqualFold(fl.Mode, "REQUIRED") {
				return fmt.Errorf("a NULL in the REQUIRED field %s", fl.Name)
			}
			c.defs = append(c.defs, def)
			return nil
		}
		if !strings.EqualFold(fl.Mode, "REQUIRED") {
			def++
		}
		if i == len(c.path)-1 {
			break
		}
		cells, err := recordCells(v) // storagerows.go
		if err != nil {
			return err
		}
		next := c.path[i+1]
		idx := -1
		for j, kid := range fl.Fields {
			if kid.Name == next.Name {
				idx = j
			}
		}
		if idx < 0 || idx >= len(cells) {
			return fmt.Errorf("a RECORD %s without its field %s", fl.Name, next.Name)
		}
		v = cellValue(cells[idx])
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("a value of %T", v)
	}
	val, err := parquetValue(c.path[len(c.path)-1].Type, c.leaf, s)
	if err != nil {
		return err
	}
	c.values = append(c.values, val)
	c.defs = append(c.defs, def)
	return nil
}

// parquetValue is a tabledata.list value s of the BigQuery type typ as
// the Go value its Parquet column writer takes.
func parquetValue(typ string, leaf parquetLeaf, s string) (any, error) {
	switch strings.ToUpper(typ) {
	case "INTEGER", "INT64":
		return strconv.ParseInt(s, 10, 64)
	case "BOOLEAN", "BOOL":
		return strconv.ParseBool(s)
	case "STRING":
		return parquet.ByteArray(s), nil
	case "BYTES":
		b, err := base64.StdEncoding.DecodeString(s)
		return parquet.ByteArray(b), err
	case "NUMERIC", "BIGNUMERIC":
		n, err := unscaled(s, leaf.scale) // avrorows.go
		if err != nil {
			return nil, err
		}
		return parquet.FixedLenByteArray(twosComplementBytes(n, leaf.length)), nil
	case "DATE":
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			return nil, err
		}
		return int32(d.Unix() / 86400), nil
	case "DATETIME":
		t, err := parseDateTime(s) // storagerows.go
		return t.UnixMicro(), err
	case "TIMESTAMP":
		t, err := parseTimestamp(s)
		return t.UnixMicro(), err
	case "TIME":
		return parseTimeOfDay(s)
	}
	return nil, fmt.Errorf("a %s value", typ)
}

// twosComplementBytes writes n big-endian in two's complement, in size bytes.
func twosComplementBytes(n *big.Int, size int) []byte {
	out := make([]byte, size)
	v := new(big.Int).Set(n)
	if v.Sign() < 0 {
		v.Add(v, new(big.Int).Lsh(big.NewInt(1), uint(size*8)))
	}
	v.FillBytes(out)
	return out
}

// encodeParquetExtract writes rows, as tabledata.list gives them, as a
// Parquet file of one row group (above). It returns why a row cannot be
// written, or "".
func encodeParquetExtract(x writtenExtract, rows [][]any) ([]byte, string) {
	var cols []*parquetColumn
	kids, err := parquetNodes(x.fields, nil, &cols)
	if err != nil {
		return nil, "its schema: " + err.Error()
	}
	root, err := pqschema.NewGroupNode("schema", parquet.Repetitions.Required, kids, -1)
	if err != nil {
		return nil, "its schema: " + err.Error()
	}
	for i, row := range rows {
		for _, c := range cols {
			top := -1
			for j, fl := range x.fields {
				if fl.Name == c.path[0].Name {
					top = j
				}
			}
			var v any
			if top < len(row) {
				v = row[top]
			}
			if err := c.add(v); err != nil {
				return nil, fmt.Sprintf("row %d, column %s: %v", i+1, c.path[0].Name, err)
			}
		}
	}
	codec := compress.Codecs.Uncompressed
	switch x.parquetCodec {
	case "SNAPPY":
		codec = compress.Codecs.Snappy
	case "GZIP":
		codec = compress.Codecs.Gzip
	case "ZSTD":
		codec = compress.Codecs.Zstd
	}
	var buf bytes.Buffer
	fw := file.NewParquetWriter(&buf, root, file.WithWriterProps(parquet.NewWriterProperties(parquet.WithCompression(codec))))
	rg := fw.AppendRowGroup()
	for _, c := range cols {
		cw, err := rg.NextColumn()
		if err != nil {
			return nil, err.Error()
		}
		switch w := cw.(type) {
		case *file.Int64ColumnChunkWriter:
			vals := make([]int64, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(int64)
			}
			_, err = w.WriteBatch(vals, c.defs, nil)
		case *file.Int32ColumnChunkWriter:
			vals := make([]int32, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(int32)
			}
			_, err = w.WriteBatch(vals, c.defs, nil)
		case *file.BooleanColumnChunkWriter:
			vals := make([]bool, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(bool)
			}
			_, err = w.WriteBatch(vals, c.defs, nil)
		case *file.ByteArrayColumnChunkWriter:
			vals := make([]parquet.ByteArray, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(parquet.ByteArray)
			}
			_, err = w.WriteBatch(vals, c.defs, nil)
		case *file.FixedLenByteArrayColumnChunkWriter:
			vals := make([]parquet.FixedLenByteArray, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(parquet.FixedLenByteArray)
			}
			_, err = w.WriteBatch(vals, c.defs, nil)
		default:
			err = fmt.Errorf("no writer for the column %s", c.path[len(c.path)-1].Name)
		}
		if err == nil {
			err = cw.Close()
		}
		if err != nil {
			return nil, err.Error()
		}
	}
	if err := rg.Close(); err != nil {
		return nil, err.Error()
	}
	if err := fw.Close(); err != nil {
		return nil, err.Error()
	}
	return buf.Bytes(), ""
}
