package bigqueryfront

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// A Parquet file's schema, read from its footer (#988).
//
// A Parquet file ends with its metadata, a Thrift FileMetaData in the
// compact protocol, then the metadata's length as a 4-byte little-endian
// integer, then the magic "PAR1"; it also starts with "PAR1"
// (https://parquet.apache.org/docs/file-format/). The schema is
// FileMetaData field 2, a list of SchemaElement: the root, then the
// columns depth first, a group giving its number of children
// (https://github.com/apache/parquet-format/blob/master/src/main/thrift/parquet.thrift).
// Only the schema is read: the front checks a load's columns against its
// table's, and the emulator reads the data.
//
// The module has no Parquet or Thrift dependency, and the schema is all
// that is needed, so this reads the few Thrift structures involved itself;
// it is tested against files written by Apache Arrow's Parquet writer
// (testdata/parquet, gen.py).

// Parquet physical types (parquet.thrift, enum Type).
const (
	pqBoolean = iota
	pqInt32
	pqInt64
	pqInt96
	pqFloat
	pqDouble
	pqByteArray
	pqFixedLenByteArray
)

// Parquet repetitions (parquet.thrift, enum FieldRepetitionType).
const (
	pqRequired = iota
	pqOptional
	pqRepeated
)

// pqNone marks an absent optional integer of a SchemaElement.
const pqNone = -1

// pqElement is one SchemaElement: the root, a group or a column.
type pqElement struct {
	Name       string
	Type       int // a physical type, or pqNone for a group
	TypeLength int
	Repetition int // pqNone for the root, which may have none
	Children   int // pqNone for a column
	// Converted is the ConvertedType (parquet.thrift), or pqNone.
	Converted int
	// Logical is the LogicalType, when the writer gave one.
	Logical *pqLogical
}

// pqLogical is a LogicalType: which member of the union is set (its field
// ID in parquet.thrift) and, for the members that have them, the unit
// (1 MILLIS, 2 MICROS, 3 NANOS), the integer width and signedness, and
// whether a time is adjusted to UTC.
type pqLogical struct {
	Kind     int
	Unit     int
	BitWidth int
	Signed   bool
	UTC      bool
}

// LogicalType members (parquet.thrift, union LogicalType).
const (
	pqLogicalString    = 1
	pqLogicalMap       = 2
	pqLogicalList      = 3
	pqLogicalEnum      = 4
	pqLogicalDecimal   = 5
	pqLogicalDate      = 6
	pqLogicalTime      = 7
	pqLogicalTimestamp = 8
	pqLogicalInteger   = 10
	pqLogicalUnknown   = 11
	pqLogicalJSON      = 12
	pqLogicalBSON      = 13
	pqLogicalUUID      = 14
)

// ConvertedType values (parquet.thrift, enum ConvertedType).
const (
	pqConvUTF8            = 0
	pqConvMap             = 1
	pqConvMapKeyValue     = 2
	pqConvList            = 3
	pqConvEnum            = 4
	pqConvDecimal         = 5
	pqConvDate            = 6
	pqConvTimeMillis      = 7
	pqConvTimeMicros      = 8
	pqConvTimestampMillis = 9
	pqConvTimestampMicros = 10
	pqConvUint8           = 11
	pqConvUint16          = 12
	pqConvUint32          = 13
	pqConvUint64          = 14
	pqConvInt8            = 15
	pqConvInt16           = 16
	pqConvInt32           = 17
	pqConvInt64           = 18
	pqConvJSON            = 19
	pqConvBSON            = 20
	pqConvInterval        = 21
)

// maxParquetFooter bounds the metadata the front reads. A schema is a
// small part of it; a file whose metadata is larger is not read.
const maxParquetFooter = 64 << 20

// errNotParquet is a file that is not a Parquet file: no "PAR1" at its
// start or end, or a footer that does not fit in it.
var errNotParquet = errors.New("not a Parquet file")

// readParquetSchema reads the schema of the Parquet file r, of size bytes.
func readParquetSchema(r io.ReaderAt, size int64) ([]pqElement, error) {
	if size < 12 {
		return nil, errNotParquet
	}
	var head, tail [4]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return nil, err
	}
	var end [8]byte
	if _, err := r.ReadAt(end[:], size-8); err != nil {
		return nil, err
	}
	copy(tail[:], end[4:])
	if string(head[:]) != "PAR1" || string(tail[:]) != "PAR1" {
		return nil, errNotParquet
	}
	n := int64(binary.LittleEndian.Uint32(end[:4]))
	if n <= 0 || n > size-12 {
		return nil, errNotParquet
	}
	if n > maxParquetFooter {
		return nil, fmt.Errorf("the file's metadata is %d bytes, more than the %d read", n, maxParquetFooter)
	}
	meta := make([]byte, n)
	if _, err := r.ReadAt(meta, size-8-n); err != nil {
		return nil, err
	}
	return parseFileMetaData(meta)
}

// parseFileMetaData reads FileMetaData's schema (field 2) from b.
func parseFileMetaData(b []byte) ([]pqElement, error) {
	d := &compactReader{b: b}
	var schema []pqElement
	found := false
	err := d.readStruct(func(id int16, typ byte) error {
		if id != 2 || typ != ctList {
			return d.skip(typ, 0)
		}
		et, n, err := d.listHeader()
		if err != nil {
			return err
		}
		if et != ctStruct {
			return fmt.Errorf("parquet metadata: schema is a list of type %d", et)
		}
		for range n {
			e, err := d.readSchemaElement()
			if err != nil {
				return err
			}
			schema = append(schema, e)
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found || len(schema) == 0 {
		return nil, errors.New("parquet metadata: no schema")
	}
	return schema, nil
}

func (d *compactReader) readSchemaElement() (pqElement, error) {
	e := pqElement{Type: pqNone, TypeLength: pqNone, Repetition: pqNone, Children: pqNone, Converted: pqNone}
	hasName := false
	err := d.readStruct(func(id int16, typ byte) error {
		var err error
		switch {
		case id == 4 && typ == ctBinary:
			var s []byte
			s, err = d.binary()
			e.Name, hasName = string(s), true
		case id == 10 && typ == ctStruct:
			var l pqLogical
			l, err = d.readLogical()
			e.Logical = &l
		case typ == ctI32 && id >= 1 && id <= 6:
			var v int64
			if v, err = d.varint(); err != nil {
				return err
			}
			if v < 0 || v > math.MaxInt32 {
				return fmt.Errorf("parquet metadata: field %d is %d", id, v)
			}
			switch id {
			case 1:
				e.Type = int(v)
			case 2:
				e.TypeLength = int(v)
			case 3:
				e.Repetition = int(v)
			case 5:
				e.Children = int(v)
			case 6:
				e.Converted = int(v)
			}
		default:
			err = d.skip(typ, 0)
		}
		return err
	})
	if err == nil && !hasName {
		err = errors.New("parquet metadata: a schema element has no name")
	}
	return e, err
}

// readLogical reads a LogicalType union: one member, a struct.
func (d *compactReader) readLogical() (pqLogical, error) {
	var l pqLogical
	err := d.readStruct(func(id int16, typ byte) error {
		if typ != ctStruct {
			return d.skip(typ, 0)
		}
		l.Kind = int(id)
		return d.readStruct(func(fid int16, ft byte) error {
			switch {
			case (l.Kind == pqLogicalTime || l.Kind == pqLogicalTimestamp) && fid == 1 && (ft == ctTrue || ft == ctFalse):
				l.UTC = ft == ctTrue
			case (l.Kind == pqLogicalTime || l.Kind == pqLogicalTimestamp) && fid == 2 && ft == ctStruct:
				// TimeUnit, a union of empty structs.
				return d.readStruct(func(uid int16, ut byte) error {
					l.Unit = int(uid)
					return d.skip(ut, 0)
				})
			case l.Kind == pqLogicalInteger && fid == 1 && ft == ctByte:
				v, err := d.byte()
				l.BitWidth = int(int8(v))
				return err
			case l.Kind == pqLogicalInteger && fid == 2 && (ft == ctTrue || ft == ctFalse):
				l.Signed = ft == ctTrue
			default:
				return d.skip(ft, 0)
			}
			return nil
		})
	})
	return l, err
}

// Thrift compact protocol types
// (https://github.com/apache/thrift/blob/master/doc/specs/thrift-compact-protocol.md).
const (
	ctStop   = 0
	ctTrue   = 1
	ctFalse  = 2
	ctByte   = 3
	ctI16    = 4
	ctI32    = 5
	ctI64    = 6
	ctDouble = 7
	ctBinary = 8
	ctList   = 9
	ctSet    = 10
	ctMap    = 11
	ctStruct = 12
)

// maxThriftDepth bounds the nesting skip follows, so a malformed footer
// cannot exhaust the stack.
const maxThriftDepth = 64

var errShortFooter = errors.New("parquet metadata: truncated")

// compactReader reads the Thrift compact protocol from b.
type compactReader struct {
	b   []byte
	pos int
}

func (d *compactReader) byte() (byte, error) {
	if d.pos >= len(d.b) {
		return 0, errShortFooter
	}
	c := d.b[d.pos]
	d.pos++
	return c, nil
}

func (d *compactReader) uvarint() (uint64, error) {
	var v uint64
	for shift := uint(0); shift < 64; shift += 7 {
		c, err := d.byte()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, nil
		}
	}
	return 0, errors.New("parquet metadata: varint too long")
}

// varint reads a zigzag varint: an i16, i32 or i64.
func (d *compactReader) varint() (int64, error) {
	u, err := d.uvarint()
	return int64(u>>1) ^ -int64(u&1), err
}

func (d *compactReader) binary() ([]byte, error) {
	n, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(d.b)-d.pos) {
		return nil, errShortFooter
	}
	s := d.b[d.pos : d.pos+int(n)]
	d.pos += int(n)
	return s, nil
}

// listHeader reads a list's or set's header: its element type and size.
func (d *compactReader) listHeader() (byte, int, error) {
	c, err := d.byte()
	if err != nil {
		return 0, 0, err
	}
	n := int(c >> 4)
	if n == 15 {
		u, err := d.uvarint()
		if err != nil {
			return 0, 0, err
		}
		// Every element takes a byte at least.
		if u > uint64(len(d.b)-d.pos) {
			return 0, 0, errShortFooter
		}
		n = int(u)
	}
	return c & 0x0f, n, nil
}

// readStruct reads a struct's fields up to its stop, calling field for
// each, which must read or skip the field's value. A boolean field's value
// is its type (ctTrue or ctFalse).
func (d *compactReader) readStruct(field func(id int16, typ byte) error) error {
	var last int16
	for {
		c, err := d.byte()
		if err != nil {
			return err
		}
		typ := c & 0x0f
		if typ == ctStop {
			return nil
		}
		id := last + int16(c>>4)
		if c>>4 == 0 {
			v, err := d.varint()
			if err != nil {
				return err
			}
			id = int16(v)
		}
		last = id
		if err := field(id, typ); err != nil {
			return err
		}
	}
}

// skip reads past a value of type typ.
func (d *compactReader) skip(typ byte, depth int) error {
	if depth > maxThriftDepth {
		return errors.New("parquet metadata: nested too deeply")
	}
	switch typ {
	case ctTrue, ctFalse:
		return nil
	case ctByte:
		_, err := d.byte()
		return err
	case ctI16, ctI32, ctI64:
		_, err := d.uvarint()
		return err
	case ctDouble:
		if len(d.b)-d.pos < 8 {
			return errShortFooter
		}
		d.pos += 8
		return nil
	case ctBinary:
		_, err := d.binary()
		return err
	case ctList, ctSet:
		et, n, err := d.listHeader()
		if err != nil {
			return err
		}
		for range n {
			// A boolean in a list is a byte of its own.
			if et == ctTrue || et == ctFalse {
				et = ctByte
			}
			if err := d.skip(et, depth+1); err != nil {
				return err
			}
		}
		return nil
	case ctMap:
		n, err := d.uvarint()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if n > uint64(len(d.b)-d.pos) {
			return errShortFooter
		}
		kv, err := d.byte()
		if err != nil {
			return err
		}
		kt, vt := kv>>4, kv&0x0f
		for range n {
			for _, t := range []byte{kt, vt} {
				if t == ctTrue || t == ctFalse {
					t = ctByte
				}
				if err := d.skip(t, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	case ctStruct:
		return d.readStruct(func(_ int16, t byte) error { return d.skip(t, depth+1) })
	}
	return fmt.Errorf("parquet metadata: unknown Thrift type %d", typ)
}
