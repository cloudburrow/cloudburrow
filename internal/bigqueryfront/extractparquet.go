package bigqueryfront

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	pqschema "github.com/apache/arrow-go/v18/parquet/schema"
)

// PARQUET extracts (#957).
//
// The emulator refuses them (400 "unsupported destination format", an
// empty object left, measured), so the front writes them itself, of the
// column types whose Parquet form BigQuery's documentation gives ("Parquet
// export details", https://cloud.google.com/bigquery/docs/exporting-data#parquet_export_details):
// INTEGER as INT64; BOOLEAN as BOOLEAN; STRING as BYTE_ARRAY with the
// STRING (UTF8) annotation; BYTES as BYTE_ARRAY; NUMERIC and BIGNUMERIC as
// FIXED_LEN_BYTE_ARRAY DECIMAL (38, 9) and (76, 38), a parameterized
// NUMERIC(P[, S]) or BIGNUMERIC(P[, S]) as DECIMAL (P, S) (parquetFieldLeaf); DATE as INT32 DATE;
// DATETIME and TIMESTAMP as INT64 TIMESTAMP (isAdjustedToUTC false,
// MICROS); TIME as INT64 TIME (isAdjustedToUTC true, MICROS); a RECORD as
// a group ("represents nested data as a group"); JSON as a STRING ("it is
// exported as a STRING field"); FLOAT64 as DOUBLE (parquetExportType);
// a REPEATED column in Parquet's LIST form (parquetNodes); a REQUIRED
// column required, any other optional; compressed with SNAPPY, GZIP or
// ZSTD, as the job names, or not. GEOGRAPHY only with the job's
// nativeGeographyExportEnabled ("BigQuery to Parquet export will write the
// native Parquet Geography type instead of the default GeoParquet type",
// the REST reference): BYTE_ARRAY GEOGRAPHY "(edges = spherical)", as the
// table gives it, with values in WKB (parquetGeography). 501 for the rest:
// GEOGRAPHY without it (GeoParquet metadata, whose bytes the
// documentation does not give), and INTERVAL and RANGE (not in the
// table). The file's writer metadata, page and row group sizes and
// encodings are the writer's, not BigQuery's (UNVERIFIED): a Parquet
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
// reader reads the same rows and types, not the same bytes.

// parquetLeaf is how the front writes a column of a BigQuery type to
// Parquet (above): its physical and logical type, and a FIXED_LEN_BYTE_ARRAY's
// length.
type parquetLeaf struct {
	phys    parquet.Type
	logical pqschema.LogicalType
	length  int
	// precision and scale are a DECIMAL's; parameterized, a
	// NUMERIC(P[, S]) or BIGNUMERIC(P[, S]) column's (parquetFieldLeaf).
	precision, scale int
	parameterized    bool
	// geography is a GEOGRAPHY column: BYTE_ARRAY written with no logical
	// type, which parquetGeography then sets to GEOGRAPHY in the footer.
	geography bool
}

// parquetExportType is the Parquet type the front writes a column of the
// BigQuery type typ as, and whether it writes one; a RECORD is not here.
func parquetExportType(typ string) (parquetLeaf, bool) {
	switch strings.ToUpper(typ) {
	case "INTEGER", "INT64":
		return parquetLeaf{phys: parquet.Types.Int64, logical: pqschema.NoLogicalType{}}, true
	case "BOOLEAN", "BOOL":
		return parquetLeaf{phys: parquet.Types.Boolean, logical: pqschema.NoLogicalType{}}, true
	case "STRING", "JSON":
		// "When you export a JSON column to Parquet format, it is exported
		// as a STRING field."
		return parquetLeaf{phys: parquet.Types.ByteArray, logical: pqschema.StringLogicalType{}}, true
	case "FLOAT", "FLOAT64":
		// The table gives "FLOAT", but a Parquet FLOAT (32 bits) cannot
		// hold a FLOAT64, and BigQuery's names for FLOAT64 include FLOAT
		// (the legacy and REST name): DOUBLE, the 64-bit type, which
		// BigQuery's Parquet load also reads as FLOAT64. UNVERIFIED
		// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
		// against a file BigQuery exported.
		return parquetLeaf{phys: parquet.Types.Double, logical: pqschema.NoLogicalType{}}, true
	case "BYTES":
		return parquetLeaf{phys: parquet.Types.ByteArray, logical: pqschema.NoLogicalType{}}, true
	case "GEOGRAPHY":
		return parquetLeaf{phys: parquet.Types.ByteArray, logical: pqschema.NoLogicalType{}, geography: true}, true
	case "NUMERIC":
		return parquetLeaf{phys: parquet.Types.FixedLenByteArray, logical: pqschema.NewDecimalLogicalType(38, 9), length: 16, precision: 38, scale: 9}, true
	case "BIGNUMERIC":
		return parquetLeaf{phys: parquet.Types.FixedLenByteArray, logical: pqschema.NewDecimalLogicalType(76, 38), length: 32, precision: 76, scale: 38}, true
	case "DATE":
		return parquetLeaf{phys: parquet.Types.Int32, logical: pqschema.DateLogicalType{}}, true
	case "DATETIME", "TIMESTAMP":
		return parquetLeaf{phys: parquet.Types.Int64, logical: pqschema.NewTimestampLogicalType(false, pqschema.TimeUnitMicros)}, true
	case "TIME":
		return parquetLeaf{phys: parquet.Types.Int64, logical: pqschema.NewTimeLogicalType(true, pqschema.TimeUnitMicros)}, true
	}
	return parquetLeaf{}, false
}

// parquetFieldLeaf is parquetExportType of fl's type, a parameterized
// NUMERIC(P[, S]) or BIGNUMERIC(P[, S]) with its own precision and scale:
// the Parquet export details give "NUMERIC(P[, S])" and "BIGNUMERIC(P[,
// S])" as DECIMAL (precision = P, scale = S). It stays in the
// unparameterized type's FIXED_LEN_BYTE_ARRAY length (16 or 32 bytes),
// which holds any such precision; the length BigQuery writes is not
// documented (UNVERIFIED against BigQuery).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
func parquetFieldLeaf(fl field) (parquetLeaf, bool) {
	leaf, ok := parquetExportType(fl.Type)
	if p, sc, param := decimalParams(fl); ok && param {
		leaf.precision, leaf.scale, leaf.parameterized = p, sc, true
		leaf.logical = pqschema.NewDecimalLogicalType(int32(p), int32(sc))
	}
	return leaf, ok
}

// parquetUnwritten returns the first column of fields, by its path below
// prefix, and its type, that the front does not write to Parquet (above,
// and parquetFields); "" when it writes them all.
func parquetUnwritten(fields []field, prefix string) (string, string) {
	for _, fl := range fields {
		typ := strings.ToUpper(fl.Type)
		switch typ {
		case "RECORD", "STRUCT":
			if loc, t := parquetUnwritten(fl.Fields, prefix+fl.Name+"."); loc != "" {
				return loc, t
			}
			continue
		case "INTERVAL":
			continue
		case "RANGE":
			if _, ok := parquetExportType(rangeElement(fl)); !ok {
				return prefix + fl.Name, typ
			}
			continue
		}
		if _, ok := parquetExportType(typ); !ok {
			return prefix + fl.Name, typ
		}
	}
	return "", ""
}

// parquetFields is the schema the front writes fields in, and each row's
// values for it (#957):
//
//   - GEOGRAPHY without nativeGeographyExportEnabled is a STRING of WKT:
//     "When you export spatial data from BigQuery, GEOGRAPHY column values
//     are always formatted as WKT strings" (gis-data). The export page
//     says GeoParquet metadata is added, but GeoParquet 1.x holds WKB, not
//     WKT, and its version and CRS are not given: none is written
//     (UNVERIFIED against BigQuery).
//
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
//   - INTERVAL, which the Parquet table does not list, is a STRING of its
//     canonical text "[sign]Y-M [sign]D [sign]H:M:S[.F]" (UNVERIFIED).
//
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
//   - RANGE, which the table does not list, is a group of two fields of
//     its element type, start and end, a NULL for an UNBOUNDED bound: the
//     form BigQuery's Storage Read API gives a RANGE in Arrow (a struct of
//     start and end), UNVERIFIED for Parquet.
//
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
func parquetFields(fields []field, nativeGeo bool) []field {
	out := make([]field, len(fields))
	for i, fl := range fields {
		switch strings.ToUpper(fl.Type) {
		case "GEOGRAPHY":
			if !nativeGeo {
				fl.Type = "STRING"
			}
		case "INTERVAL":
			fl.Type = "STRING"
		case "RANGE":
			elem := rangeElement(fl)
			fl.Type, fl.RangeElementType = "RECORD", nil
			fl.Fields = []field{{Name: "start", Type: elem, Mode: "NULLABLE"}, {Name: "end", Type: elem, Mode: "NULLABLE"}}
		case "RECORD", "STRUCT":
			fl.Fields = parquetFields(fl.Fields, nativeGeo)
		}
		out[i] = fl
	}
	return out
}

// parquetRow is a row's values, as tabledata.list gives them, for
// parquetFields: each RANGE "[start, end)" as a RECORD value of its bounds.
func parquetRow(fields []field, row []any) ([]any, error) {
	out := make([]any, len(row))
	for j, v := range row {
		if j >= len(fields) || v == nil {
			out[j] = v
			continue
		}
		fl := fields[j]
		one := func(v any) (any, error) { return parquetOne(fl, v) }
		if strings.EqualFold(fl.Mode, "REPEATED") {
			items, _ := v.([]any)
			vals := make([]any, len(items))
			for i, it := range items {
				x, err := one(cellValue(it))
				if err != nil {
					return nil, err
				}
				vals[i] = map[string]any{"v": x}
			}
			out[j] = vals
			continue
		}
		x, err := one(v)
		if err != nil {
			return nil, err
		}
		out[j] = x
	}
	return out, nil
}

func parquetOne(fl field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch strings.ToUpper(fl.Type) {
	case "RANGE":
		s, _ := v.(string)
		if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ')' {
			return nil, fmt.Errorf("column %s: unreadable RANGE %q", fl.Name, s)
		}
		parts := strings.Split(s[1:len(s)-1], ", ")
		if len(parts) != 2 {
			return nil, fmt.Errorf("column %s: unreadable RANGE %q", fl.Name, s)
		}
		cells := make([]any, 2)
		for i, p := range parts {
			var b any = p
			if p == "UNBOUNDED" || p == "NULL" {
				b = nil
			}
			cells[i] = map[string]any{"v": b}
		}
		return map[string]any{"f": cells}, nil
	case "RECORD", "STRUCT":
		cells, err := recordCells(v) // storagerows.go
		if err != nil {
			return nil, err
		}
		vals := make([]any, len(cells))
		for i, c := range cells {
			vals[i] = cellValue(c)
		}
		conv, err := parquetRow(fl.Fields, vals)
		if err != nil {
			return nil, err
		}
		out := make([]any, len(conv))
		for i, c := range conv {
			out[i] = map[string]any{"v": c}
		}
		return map[string]any{"f": out}, nil
	}
	return v, nil
}

// parquetColumn is one leaf column of the file: its type, and its values
// and definition and repetition levels.
type parquetColumn struct {
	name   string
	typ    string
	leaf   parquetLeaf
	maxRep int16
	values []any
	defs   []int16
	reps   []int16
}

// parquetNode is a BigQuery field as the front writes it: a leaf column,
// or a RECORD's fields.
type parquetNode struct {
	fl   field
	col  *parquetColumn
	kids []*parquetNode
}

// columns is every leaf column below n.
func (n *parquetNode) columns() []*parquetColumn {
	if n.col != nil {
		return []*parquetColumn{n.col}
	}
	var out []*parquetColumn
	for _, k := range n.kids {
		out = append(out, k.columns()...)
	}
	return out
}

// parquetNodes is the Parquet schema nodes of fields, and appends their
// leaf columns, in order, to cols. rep is the number of repeated nodes
// above. A REPEATED field is written in Parquet's LIST form, an optional
// group (LIST) holding a repeated group "list" of one required field
// "element": BigQuery writes "repeated records as repeated groups", and a
// file it exported, loaded back without list inference, adds a field
// <column>.list (googleapis/google-cloud-php#6308), which is that form.
// The names "list" and "element" are Parquet's (LogicalTypes.md); the
// element's and the outer group's repetitions are UNVERIFIED against
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
// BigQuery.
func parquetNodes(fields []field, rep int16, cols *[]*parquetColumn) (pqschema.FieldList, []*parquetNode, error) {
	var out pqschema.FieldList
	var nodes []*parquetNode
	for _, fl := range fields {
		repeated := strings.EqualFold(fl.Mode, "REPEATED")
		r := parquet.Repetitions.Optional
		if strings.EqualFold(fl.Mode, "REQUIRED") || repeated {
			r = parquet.Repetitions.Required
		}
		name := fl.Name
		elemRep := rep
		if repeated {
			name = "element"
			elemRep++
		}
		n := &parquetNode{fl: fl}
		var pn pqschema.Node
		if t := strings.ToUpper(fl.Type); t == "RECORD" || t == "STRUCT" {
			kids, kn, err := parquetNodes(fl.Fields, elemRep, cols)
			if err != nil {
				return nil, nil, err
			}
			n.kids = kn
			g, err := pqschema.NewGroupNode(name, r, kids, -1)
			if err != nil {
				return nil, nil, err
			}
			pn = g
		} else {
			leaf, _ := parquetFieldLeaf(fl)
			prim, err := pqschema.NewPrimitiveNodeLogical(name, r, leaf.logical, leaf.phys, leaf.length, -1)
			if err != nil {
				return nil, nil, err
			}
			pn = prim
			n.col = &parquetColumn{name: fl.Name, typ: fl.Type, leaf: leaf, maxRep: elemRep}
			*cols = append(*cols, n.col)
		}
		if repeated {
			list, err := pqschema.NewGroupNode("list", parquet.Repetitions.Repeated, pqschema.FieldList{pn}, -1)
			if err != nil {
				return nil, nil, err
			}
			outer, err := pqschema.NewGroupNodeLogical(fl.Name, parquet.Repetitions.Optional, pqschema.FieldList{list}, pqschema.ListLogicalType{}, -1)
			if err != nil {
				return nil, nil, err
			}
			pn = outer
		}
		out = append(out, pn)
		nodes = append(nodes, n)
	}
	return out, nodes, nil
}

// shred adds a row's value v of n, as tabledata.list gives it, to n's
// columns: def is the definition level so far, rep the repetition level
// of its first leaf value and depth the number of repeated nodes above.
func (n *parquetNode) shred(v any, def, rep, depth int16) error {
	if strings.EqualFold(n.fl.Mode, "REPEATED") {
		items, _ := v.([]any)
		if len(items) == 0 { // the outer group, defined; no element
			for _, c := range n.columns() {
				c.defs = append(c.defs, def+1)
				c.reps = append(c.reps, rep)
			}
			return nil
		}
		for i, it := range items {
			r := rep
			if i > 0 {
				r = depth + 1
			}
			if err := n.shredOne(cellValue(it), def+2, r, depth+1, true); err != nil {
				return err
			}
		}
		return nil
	}
	return n.shredOne(v, def, rep, depth, strings.EqualFold(n.fl.Mode, "REQUIRED"))
}

// shredOne adds one value (not an array) of n.
func (n *parquetNode) shredOne(v any, def, rep, depth int16, required bool) error {
	if v == nil {
		if required {
			return fmt.Errorf("a NULL in the REQUIRED field %s", n.fl.Name)
		}
		for _, c := range n.columns() {
			c.defs = append(c.defs, def)
			c.reps = append(c.reps, rep)
		}
		return nil
	}
	if !required {
		def++
	}
	if n.col == nil {
		cells, err := recordCells(v) // storagerows.go
		if err != nil {
			return err
		}
		for j, k := range n.kids {
			var kv any
			if j < len(cells) {
				kv = cellValue(cells[j])
			}
			if err := k.shred(kv, def, rep, depth); err != nil {
				return err
			}
		}
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("a value of %T", v)
	}
	val, err := parquetValue(n.col.typ, n.col.leaf, s)
	if err != nil {
		return err
	}
	c := n.col
	c.values = append(c.values, val)
	c.defs = append(c.defs, def)
	c.reps = append(c.reps, rep)
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
	case "STRING", "JSON":
		return parquet.ByteArray(s), nil
	case "FLOAT", "FLOAT64":
		switch s {
		case "Infinity":
			s = "+Inf"
		case "-Infinity":
			s = "-Inf"
		}
		return strconv.ParseFloat(s, 64)
	case "BYTES":
		b, err := base64.StdEncoding.DecodeString(s)
		return parquet.ByteArray(b), err
	case "GEOGRAPHY":
		b, err := wktToWKB(s) // wkb.go
		return parquet.ByteArray(b), err
	case "NUMERIC", "BIGNUMERIC":
		// An unparameterized BIGNUMERIC can hold 39 digits before the
		// point, past the documented precision 76, which its 32 bytes hold.
		if leaf.parameterized {
			if err := decimalFits(s, leaf.precision, leaf.scale); err != nil {
				return nil, err
			}
		}
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
	conv := make([][]any, len(rows))
	for i, row := range rows {
		r, err := parquetRow(x.fields, row)
		if err != nil {
			return nil, fmt.Sprintf("row %d, %v", i+1, err)
		}
		conv[i] = r
	}
	rows = conv
	x.fields = parquetFields(x.fields, x.parquetNativeGeography)
	var cols []*parquetColumn
	kids, nodes, err := parquetNodes(x.fields, 0, &cols)
	if err != nil {
		return nil, "its schema: " + err.Error()
	}
	root, err := pqschema.NewGroupNode("schema", parquet.Repetitions.Required, kids, -1)
	if err != nil {
		return nil, "its schema: " + err.Error()
	}
	for i, row := range rows {
		for j, n := range nodes {
			var v any
			if j < len(row) {
				v = row[j]
			}
			if err := n.shred(v, 0, 0, 0); err != nil {
				return nil, fmt.Sprintf("row %d, column %s: %v", i+1, n.fl.Name, err)
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
	opts := []parquet.WriterProperty{parquet.WithCompression(codec)}
	var geo []bool // by leaf column, in order
	for _, c := range cols {
		geo = append(geo, c.leaf.geography)
	}
	sch := pqschema.NewSchema(root)
	for i, g := range geo {
		if g {
			// Parquet: "the sort order for GEOGRAPHY is undefined", so no
			// min and max statistics.
			opts = append(opts, parquet.WithStatsPath(sch.Column(i).ColumnPath(), false))
		}
	}
	var buf bytes.Buffer
	fw := file.NewParquetWriter(&buf, root, file.WithWriterProps(parquet.NewWriterProperties(opts...)))
	rg := fw.AppendRowGroup()
	for _, c := range cols {
		cw, err := rg.NextColumn()
		if err != nil {
			return nil, err.Error()
		}
		reps := c.reps
		if c.maxRep == 0 {
			reps = nil
		}
		switch w := cw.(type) {
		case *file.Int64ColumnChunkWriter:
			vals := make([]int64, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(int64)
			}
			_, err = w.WriteBatch(vals, c.defs, reps)
		case *file.Float64ColumnChunkWriter:
			vals := make([]float64, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(float64)
			}
			_, err = w.WriteBatch(vals, c.defs, reps)
		case *file.Int32ColumnChunkWriter:
			vals := make([]int32, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(int32)
			}
			_, err = w.WriteBatch(vals, c.defs, reps)
		case *file.BooleanColumnChunkWriter:
			vals := make([]bool, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(bool)
			}
			_, err = w.WriteBatch(vals, c.defs, reps)
		case *file.ByteArrayColumnChunkWriter:
			vals := make([]parquet.ByteArray, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(parquet.ByteArray)
			}
			_, err = w.WriteBatch(vals, c.defs, reps)
		case *file.FixedLenByteArrayColumnChunkWriter:
			vals := make([]parquet.FixedLenByteArray, len(c.values))
			for i, v := range c.values {
				vals[i] = v.(parquet.FixedLenByteArray)
			}
			_, err = w.WriteBatch(vals, c.defs, reps)
		default:
			err = fmt.Errorf("no writer for the column %s", c.name)
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
	out, err := parquetGeography(buf.Bytes(), geo)
	if err != nil {
		return nil, err.Error()
	}
	return out, ""
}
