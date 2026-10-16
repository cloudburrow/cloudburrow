package bigqueryfront

import (
	"fmt"
	"strings"

	"github.com/apache/arrow/go/v15/arrow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// RANGE columns of the Storage Write API (#1391).
//
// In proto_rows a RANGE<T> column is a message of two fields, start and
// end, each of a proto type T's column takes (the Go client's
// adapt.StorageSchemaToProto2Descriptor makes it rangemessage_range_date,
// _datetime or _timestamp, with optional fields start and end of T's
// type); in arrow_rows it is a Struct of start and end of an Arrow type T
// takes ("RANGE ... Struct", supported-data-types). A bound with no value,
// or NULL, is UNBOUNDED; the column is NULL when the message, or the
// struct, is. Each is written as the text insertAll takes for a RANGE,
// "[start, end)".

// rangeElem is a RANGE column's element type, or why it has none.
func rangeElem(col field, where string) (string, error) {
	if col.RangeElementType == nil {
		return "", status.Errorf(codes.Internal, "the RANGE column '%s' has no rangeElementType", where)
	}
	return legacyType(col.RangeElementType.Type), nil
}

// rangeConv writes a proto RANGE message.
type rangeConv struct {
	start, end colConv
}

func newRangeConv(fd protoreflect.FieldDescriptor, col field, where string) (*rangeConv, error) {
	elem, err := rangeElem(col, where)
	if err != nil {
		return nil, err
	}
	if fd.Kind() != protoreflect.MessageKind {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: field '%s' is a RANGE in the table and a %s in the "+
			"proto schema: a RANGE is a message of start and end", where, fd.Kind())
	}
	rc := &rangeConv{}
	md := fd.Message()
	for i := 0; i < md.Fields().Len(); i++ {
		b := md.Fields().Get(i)
		name := strings.ToLower(columnNameOf(b))
		var dst *colConv
		switch name {
		case "start":
			dst = &rc.start
		case "end":
			dst = &rc.end
		default:
			return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: the RANGE field '%s' has a field '%s': "+
				"a RANGE is a message of start and end", where, b.Name())
		}
		if b.IsList() || b.IsMap() || !kindTakes(elem, b.Kind()) {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: field '%s.%s' of proto type %s cannot be "+
				"written to the RANGE's element type %s", where, name, b.Kind(), elem)
		}
		*dst = colConv{fd: b, typ: elem}
	}
	return rc, nil
}

func (rc *rangeConv) value(m protoreflect.Message) (any, error) {
	bound := func(cc colConv) (any, error) {
		if cc.fd == nil || !m.Has(cc.fd) {
			return nil, nil
		}
		return scalarValue(cc.typ, cc.fd, m.Get(cc.fd))
	}
	s, err := bound(rc.start)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	e, err := bound(rc.end)
	if err != nil {
		return nil, fmt.Errorf("end: %w", err)
	}
	return rangeText(s, e), nil
}

// rangeText is insertAll's text of a RANGE of bounds start and end, each a
// bound's text or nil for UNBOUNDED.
func rangeText(start, end any) string {
	b := func(v any) string {
		s, ok := v.(string)
		if !ok {
			return "UNBOUNDED"
		}
		// A TIMESTAMP bound as GoogleSQL writes one ("+00"), which the
		// engine's RANGE text has and tabledata.list reads
		// (nestedtimestamps.go restRange): measured, a bound ending
		// " UTC" was read back as it was written, which no client parses.
		if t, ok := strings.CutSuffix(s, " UTC"); ok {
			return t + "+00"
		}
		return s
	}
	return "[" + b(start) + ", " + b(end) + ")"
}

// arrowRange is an Arrow RANGE struct's start and end, by field
// index (-1: absent).
type arrowRange struct {
	start, end int
	elem       arrowCol
}

func newArrowRange(dt arrow.DataType, col field, where string) (*arrowRange, error) {
	elem, err := rangeElem(col, where)
	if err != nil {
		return nil, err
	}
	st, ok := dt.(*arrow.StructType)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid Arrow schema: field '%s' is a RANGE in the table and %s in the "+
			"Arrow schema: a RANGE is a Struct of start and end", where, dt)
	}
	ar := &arrowRange{start: -1, end: -1, elem: arrowCol{typ: elem}}
	for i, f := range st.Fields() {
		switch strings.ToLower(f.Name) {
		case "start":
			ar.start = i
		case "end":
			ar.end = i
		default:
			return nil, status.Errorf(codes.InvalidArgument, "Invalid Arrow schema: the RANGE field '%s' has a field '%s': "+
				"a RANGE is a Struct of start and end", where, f.Name)
		}
		if err := arrowTakes(elem, f.Type); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid Arrow schema: field '%s.%s' of Arrow type %s cannot be "+
				"written to the RANGE's element type %s%s", where, f.Name, f.Type, elem, err.Error())
		}
	}
	return ar, nil
}

func (ar *arrowRange) value(fields []arrow.Array, i int) (any, error) {
	bound := func(k int) (any, error) {
		if k < 0 || fields[k].IsNull(i) {
			return nil, nil
		}
		return ar.elem.value(fields[k], i)
	}
	s, err := bound(ar.start)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	e, err := bound(ar.end)
	if err != nil {
		return nil, fmt.Errorf("end: %w", err)
	}
	return rangeText(s, e), nil
}
