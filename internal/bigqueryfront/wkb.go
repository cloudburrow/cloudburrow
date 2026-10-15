package bigqueryfront

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// GEOGRAPHY in a PARQUET extract with nativeGeographyExportEnabled (#957).
//
// BigQuery's Parquet type table gives GEOGRAPHY as "BYTE_ARRAY" with the
// logical type "GEOGRAPHY (edges = spherical)"
// (https://cloud.google.com/bigquery/docs/exporting-data#parquet_export_details),
// and nativeGeographyExportEnabled writes "the native Parquet Geography
// type instead of the default GeoParquet type" (REST reference,
// JobConfigurationExtract). Parquet's GEOGRAPHY holds WKB values
// (https://github.com/apache/parquet-format/blob/master/LogicalTypes.md#geography)
// and has a CRS, OGC:CRS84 (longitude, latitude on WGS84) when not given,
// which is BigQuery's, and an edge algorithm, which is set to SPHERICAL.
// UNVERIFIED against a file BigQuery exported: whether it sets the CRS,
// and the WKB byte order (written little-endian, as is usual; a reader
// takes either).
//
// The Arrow Go writer the front uses has no GEOGRAPHY logical type, so the
// column is written as plain BYTE_ARRAY, with no statistics, and the
// logical type is then added to its SchemaElement in the footer.

// parquetGeography adds the GEOGRAPHY logical type to the leaf columns of
// the Parquet file b marked in geo (by leaf, in schema order), which must
// have none. It returns b when none is marked.
func parquetGeography(b []byte, geo []bool) ([]byte, error) {
	marked := false
	for _, g := range geo {
		marked = marked || g
	}
	if !marked {
		return b, nil
	}
	if len(b) < 12 || string(b[len(b)-4:]) != "PAR1" {
		return nil, errNotParquet
	}
	n := int(binary.LittleEndian.Uint32(b[len(b)-8:]))
	start := len(b) - 8 - n
	if n <= 0 || start < 4 {
		return nil, errNotParquet
	}
	meta := b[start : len(b)-8]
	// The stop byte of each marked element, where its field 10 goes.
	var stops []int
	d := &compactReader{b: meta}
	err := d.readStruct(func(id int16, typ byte) error {
		if id != 2 || typ != ctList {
			return d.skip(typ, 0)
		}
		_, count, err := d.listHeader()
		if err != nil {
			return err
		}
		leaf := 0
		for range count {
			e, err := d.readSchemaElement()
			if err != nil {
				return err
			}
			if e.Children != pqNone || e.Type == pqNone {
				continue
			}
			if leaf < len(geo) && geo[leaf] {
				if e.Logical != nil || e.Type != pqByteArray {
					return fmt.Errorf("parquet metadata: the GEOGRAPHY column %s is not a plain BYTE_ARRAY", e.Name)
				}
				stops = append(stops, d.pos-1)
			}
			leaf++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// SchemaElement field 10, logicalType, a struct (long-form header:
	// type, then the zigzag field ID): LogicalType field 18, GEOGRAPHY, a
	// GeographyType of field 2, algorithm, SPHERICAL (0); then the stops.
	logical := []byte{0x0c, 10 << 1, 0x0c, 18 << 1, 0x25, 0x00, 0x00, 0x00}
	out := make([]byte, 0, len(b)+len(stops)*len(logical))
	out = append(out, b[:start]...)
	prev := 0
	for _, s := range stops {
		out = append(out, meta[prev:s]...)
		out = append(out, logical...)
		prev = s
	}
	out = append(out, meta[prev:]...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(out)-start))
	return append(out, "PAR1"...), nil
}

// WKB geometry types (ISO 13249-3, OGC Simple Features).
var wkbTypes = map[string]uint32{
	"POINT": 1, "LINESTRING": 2, "POLYGON": 3, "MULTIPOINT": 4, "MULTILINESTRING": 5, "MULTIPOLYGON": 6,
	"GEOMETRYCOLLECTION": 7,
}

// wktToWKB is the little-endian 2D WKB of the WKT s, as tabledata.list
// gives a GEOGRAPHY value. An empty POINT is NaN coordinates, as in
// GeoPackage.
func wktToWKB(s string) ([]byte, error) {
	p := &wktParser{s: s}
	out, err := p.geometry(nil)
	if err == nil && p.skipSpace() < len(p.s) {
		err = fmt.Errorf("trailing text in %q", s)
	}
	if err != nil {
		return nil, fmt.Errorf("a GEOGRAPHY value: %w", err)
	}
	return out, nil
}

type wktParser struct {
	s   string
	pos int
}

func (p *wktParser) skipSpace() int {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t' || p.s[p.pos] == '\n' || p.s[p.pos] == '\r') {
		p.pos++
	}
	return p.pos
}

func (p *wktParser) word() string {
	p.skipSpace()
	i := p.pos
	for p.pos < len(p.s) && (p.s[p.pos] >= 'A' && p.s[p.pos] <= 'Z' || p.s[p.pos] >= 'a' && p.s[p.pos] <= 'z') {
		p.pos++
	}
	return strings.ToUpper(p.s[i:p.pos])
}

func (p *wktParser) peek(c byte) bool {
	p.skipSpace()
	return p.pos < len(p.s) && p.s[p.pos] == c
}

func (p *wktParser) expect(c byte) error {
	if !p.peek(c) {
		return fmt.Errorf("want %q at %d of %q", c, p.pos, p.s)
	}
	p.pos++
	return nil
}

// empty reads "EMPTY", or "(" when the geometry has a body.
func (p *wktParser) empty() (bool, error) {
	if p.peek('(') {
		p.pos++
		return false, nil
	}
	save := p.pos
	if p.word() == "EMPTY" {
		return true, nil
	}
	p.pos = save
	return false, fmt.Errorf("want ( or EMPTY at %d of %q", p.pos, p.s)
}

func (p *wktParser) number() (float64, error) {
	p.skipSpace()
	i := p.pos
	for p.pos < len(p.s) && strings.IndexByte("+-.0123456789eE", p.s[p.pos]) >= 0 {
		p.pos++
	}
	return strconv.ParseFloat(p.s[i:p.pos], 64)
}

func (p *wktParser) point(out []byte) ([]byte, error) {
	for range 2 {
		f, err := p.number()
		if err != nil {
			return nil, err
		}
		out = binary.LittleEndian.AppendUint64(out, math.Float64bits(f))
	}
	if !p.peek(',') && !p.peek(')') {
		return nil, errors.New("only 2D coordinates are written")
	}
	return out, nil
}

// list reads "item, item, ...)" after its "(", appending the count and
// each item.
func (p *wktParser) list(out []byte, item func([]byte) ([]byte, error)) ([]byte, error) {
	at := len(out)
	out = append(out, 0, 0, 0, 0)
	n := uint32(0)
	for {
		var err error
		if out, err = item(out); err != nil {
			return nil, err
		}
		n++
		if p.peek(',') {
			p.pos++
			continue
		}
		if err := p.expect(')'); err != nil {
			return nil, err
		}
		binary.LittleEndian.PutUint32(out[at:], n)
		return out, nil
	}
}

func (p *wktParser) ring(out []byte) ([]byte, error) {
	if err := p.expect('('); err != nil {
		return nil, err
	}
	return p.list(out, p.point)
}

func (p *wktParser) header(out []byte, typ uint32) []byte {
	return binary.LittleEndian.AppendUint32(append(out, 1), typ)
}

// geometry reads one tagged geometry.
func (p *wktParser) geometry(out []byte) ([]byte, error) {
	name := p.word()
	typ, ok := wkbTypes[name]
	if !ok {
		return nil, fmt.Errorf("an unknown geometry %q", name)
	}
	out = p.header(out, typ)
	empty, err := p.empty()
	if err != nil {
		return nil, err
	}
	if empty {
		if typ == 1 {
			nan := math.Float64bits(math.NaN())
			return binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint64(out, nan), nan), nil
		}
		return append(out, 0, 0, 0, 0), nil
	}
	switch name {
	case "POINT":
		if out, err = p.point(out); err != nil {
			return nil, err
		}
		return out, p.expect(')')
	case "LINESTRING":
		return p.list(out, p.point)
	case "POLYGON":
		return p.list(out, p.ring)
	case "MULTIPOINT":
		return p.list(out, func(o []byte) ([]byte, error) {
			o = p.header(o, 1)
			if p.peek('(') { // MULTIPOINT((1 2), (3 4))
				p.pos++
				o, err := p.point(o)
				if err != nil {
					return nil, err
				}
				return o, p.expect(')')
			}
			return p.point(o)
		})
	case "MULTILINESTRING":
		return p.list(out, func(o []byte) ([]byte, error) { return p.ring(p.header(o, 2)) })
	case "MULTIPOLYGON":
		return p.list(out, func(o []byte) ([]byte, error) {
			if err := p.expect('('); err != nil {
				return nil, err
			}
			return p.list(p.header(o, 3), p.ring)
		})
	}
	return p.list(out, p.geometry) // GEOMETRYCOLLECTION
}
