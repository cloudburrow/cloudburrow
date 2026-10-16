package bigqueryfront

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow/go/v15/arrow"
	"github.com/apache/arrow/go/v15/arrow/array"
	"github.com/apache/arrow/go/v15/arrow/ipc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The rows of an AppendRows request in arrow_rows (#1327): an Apache
// Arrow schema and record batch, read by the front as proto_rows' are
// (storagewriterows.go), and written as insertAll's JSON.
//
// arrow_rows.writer_schema.serialized_schema is the IPC message of the
// schema and arrow_rows.rows.serialized_record_batch the IPC message of
// one record batch (storage.proto's ArrowSchema and ArrowRecordBatch), as
// the Storage Read API frames them (arrowframes.go): the batch is read
// after the schema. The schema's fields are matched to the table's columns
// by name, without regard to case; a field the table has no column for is
// INVALID_ARGUMENT, as for proto_rows. The Arrow types each column takes
// are BigQuery's table (https://cloud.google.com/bigquery/docs/supported-data-types,
// "Supported Apache Arrow data types"):
//
//	BOOL        Boolean
//	BYTES       Binary (and LargeBinary)
//	DATE        Date32 (unit DAY), Utf8, Int32 (days since 1970-01-01)
//	DATETIME    Timestamp, unit MICROSECOND, no time zone
//	FLOAT       Float32, Float64
//	GEOGRAPHY   Utf8 (WKT or GeoJSON)
//	INTEGER     Int8, Int16, Int32, Int64 (and their unsigned kinds; a
//	            value past INT64's range is a row error)
//	JSON        Utf8
//	NUMERIC     Decimal128
//	BIGNUMERIC  Decimal256 (and Decimal128)
//	STRING      Utf8 (and LargeUtf8)
//	TIMESTAMP   Timestamp, unit MICROSECOND, time zone UTC
//	INTERVAL    Interval (YEAR_MONTH, DAY_TIME, MONTH_DAY_NANO), Utf8
//	RANGE       Struct of start and end (storagewriterange.go, #1391)
//	REPEATED    List (and LargeList, FixedSizeList); a NULL list is the
//	            empty array
//	RECORD      Struct
//
// TIME is not in that table; a Time64 of unit MICROSECOND (or Time32 of
// unit SECOND or MILLISECOND) is taken for it (UNVERIFIED against
// BigQuery). Any other Arrow type is INVALID_ARGUMENT before a row is
// read. A NULL element of a list is a row error, as an array cannot hold
// one.

// arrowConv writes the rows of one Arrow writer schema for one table's
// columns.
type arrowConv struct {
	schema *arrow.Schema
	cols   []arrowCol
}

// arrowCol is one field of the writer schema (a struct's field) and the
// column it writes.
type arrowCol struct {
	idx      int
	col      field
	typ      string
	repeated bool
	// sub writes a RECORD column's struct.
	sub *arrowConv
	// rng writes a RANGE column's struct.
	rng *arrowRange
}

// arrowWriterSchema reads arrow_rows.writer_schema.serialized_schema.
func arrowWriterSchema(serialized []byte) (*arrow.Schema, error) {
	if len(serialized) == 0 {
		return nil, status.Error(codes.InvalidArgument, "arrow_rows.writer_schema.serialized_schema is required")
	}
	r, err := ipc.NewReader(bytes.NewReader(serialized))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid Arrow schema: arrow_rows.writer_schema.serialized_schema: %v", err)
	}
	defer r.Release()
	return r.Schema(), nil
}

// newArrowConv matches the schema's fields to the table's columns.
func newArrowConv(schema *arrow.Schema, fields []field) (*arrowConv, error) {
	cols, err := arrowCols(schema.Fields(), fields, "")
	if err != nil {
		return nil, err
	}
	return &arrowConv{schema: schema, cols: cols}, nil
}

func arrowCols(afs []arrow.Field, fields []field, path string) ([]arrowCol, error) {
	seen := map[string]bool{}
	out := make([]arrowCol, 0, len(afs))
	for i, af := range afs {
		col, ok := fieldNamed(fields, af.Name)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument,
				"Input schema has more fields than BigQuery schema, extra fields: '%s%s'", path, af.Name)
		}
		if seen[strings.ToLower(col.Name)] {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid Arrow schema: two fields write the column '%s%s'", path, col.Name)
		}
		seen[strings.ToLower(col.Name)] = true
		ac := arrowCol{idx: i, col: col, typ: legacyType(col.Type)}
		where := path + col.Name
		dt := af.Type
		if elem, ok := listElem(dt); ok {
			ac.repeated, dt = true, elem
		}
		if ac.repeated != strings.EqualFold(col.Mode, "REPEATED") {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid Arrow schema: field '%s' is %s in the Arrow schema and %s in the table",
				where, cardinality(ac.repeated), modeOf(col))
		}
		switch {
		case ac.typ == "RANGE":
			rng, err := newArrowRange(dt, col, where)
			if err != nil {
				return nil, err
			}
			ac.rng = rng
		case ac.typ == "RECORD":
			st, ok := dt.(*arrow.StructType)
			if !ok {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid Arrow schema: field '%s' is a RECORD in the table and %s in the Arrow schema",
					where, dt)
			}
			sub, err := arrowCols(st.Fields(), col.Fields, where+".")
			if err != nil {
				return nil, err
			}
			ac.sub = &arrowConv{cols: sub}
		default:
			if err := arrowTakes(ac.typ, dt); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid Arrow schema: field '%s' of Arrow type %s cannot be written to "+
					"the column's type %s%s", where, dt, ac.typ, err.Error())
			}
		}
		out = append(out, ac)
	}
	return out, nil
}

// listElem is the element type of a list type.
func listElem(dt arrow.DataType) (arrow.DataType, bool) {
	switch t := dt.(type) {
	case *arrow.ListType:
		return t.Elem(), true
	case *arrow.LargeListType:
		return t.Elem(), true
	case *arrow.FixedSizeListType:
		return t.Elem(), true
	}
	return nil, false
}

var errNoArrowType = errors.New("")

// arrowTakes reports, as nil, that a column of typ takes dt; its error is
// what the type lacks (or empty).
func arrowTakes(typ string, dt arrow.DataType) error {
	id := dt.ID()
	isStr := id == arrow.STRING || id == arrow.LARGE_STRING
	switch typ {
	case "BOOLEAN":
		if id == arrow.BOOL {
			return nil
		}
	case "BYTES":
		if id == arrow.BINARY || id == arrow.LARGE_BINARY {
			return nil
		}
	case "DATE":
		if id == arrow.DATE32 || id == arrow.INT32 || isStr {
			return nil
		}
	case "DATETIME", "TIMESTAMP":
		ts, ok := dt.(*arrow.TimestampType)
		if !ok {
			break
		}
		if ts.Unit != arrow.Microsecond {
			return fmt.Errorf(": the unit must be MICROSECOND")
		}
		if typ == "DATETIME" && ts.TimeZone != "" {
			return fmt.Errorf(": a DATETIME's Timestamp has no time zone")
		}
		if typ == "TIMESTAMP" && !strings.EqualFold(ts.TimeZone, "UTC") {
			return fmt.Errorf(": a TIMESTAMP's Timestamp has the time zone UTC")
		}
		return nil
	case "TIME":
		switch t := dt.(type) {
		case *arrow.Time64Type:
			if t.Unit == arrow.Microsecond {
				return nil
			}
			return fmt.Errorf(": the unit must be MICROSECOND")
		case *arrow.Time32Type:
			return nil
		}
	case "FLOAT":
		if id == arrow.FLOAT32 || id == arrow.FLOAT64 {
			return nil
		}
	case "GEOGRAPHY", "JSON", "STRING":
		if isStr {
			return nil
		}
	case "INTEGER":
		switch id {
		case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64, arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64:
			return nil
		}
	case "NUMERIC":
		if t, ok := dt.(*arrow.Decimal128Type); ok {
			if t.Scale > 9 || t.Precision-t.Scale > 29 {
				return fmt.Errorf(": NUMERIC holds a scale of at most 9 and 29 digits before the point")
			}
			return nil
		}
	case "BIGNUMERIC":
		switch t := dt.(type) {
		case *arrow.Decimal128Type:
			if t.Scale <= 38 {
				return nil
			}
		case *arrow.Decimal256Type:
			if t.Scale > 38 || t.Precision-t.Scale > 39 {
				return fmt.Errorf(": BIGNUMERIC holds a scale of at most 38 and 39 digits before the point")
			}
			return nil
		}
	case "INTERVAL":
		switch id {
		case arrow.INTERVAL_MONTHS, arrow.INTERVAL_DAY_TIME, arrow.INTERVAL_MONTH_DAY_NANO:
			return nil
		}
		if isStr {
			return nil
		}
	}
	return errNoArrowType
}

// arrowRows decodes the record batch, read after the schema, into
// insertAll's JSON objects. A batch that does not decode is
// INVALID_ARGUMENT; a value that does not convert is the row's error.
func (ac *arrowConv) rows(schemaBytes, batch []byte) ([]map[string]any, []error, error) {
	if len(batch) == 0 {
		return nil, nil, status.Error(codes.InvalidArgument, "arrow_rows.rows.serialized_record_batch is required: the rows to append")
	}
	stream := make([]byte, 0, len(schemaBytes)+len(batch))
	stream = append(append(stream, schemaBytes...), batch...)
	r, err := ipc.NewReader(bytes.NewReader(stream), ipc.WithSchema(ac.schema))
	if err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "the Arrow record batch does not decode after the writer schema: %v", err)
	}
	defer r.Release()
	var (
		out  []map[string]any
		errs []error
	)
	for r.Next() {
		rec := r.Record()
		for i := 0; i < int(rec.NumRows()); i++ {
			obj, err := ac.row(rec.Columns(), i)
			out, errs = append(out, obj), append(errs, err)
		}
	}
	if err := r.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, status.Errorf(codes.InvalidArgument, "the Arrow record batch does not decode after the writer schema: %v", err)
	}
	return out, errs, nil
}

// row writes row i of the columns (a record's, or a struct's fields).
func (ac *arrowConv) row(cols []arrow.Array, i int) (map[string]any, error) {
	out := make(map[string]any, len(ac.cols))
	for _, c := range ac.cols {
		v, err := c.field(cols[c.idx], i)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", c.col.Name, err)
		}
		out[c.col.Name] = v
	}
	return out, nil
}

func (c arrowCol) field(arr arrow.Array, i int) (any, error) {
	if !c.repeated {
		if arr.IsNull(i) {
			return nil, nil
		}
		return c.value(arr, i)
	}
	// "A NULL value must be represented by an empty list."
	out := []any{}
	if arr.IsNull(i) {
		return out, nil
	}
	var (
		values     arrow.Array
		start, end int64
	)
	switch l := arr.(type) {
	case *array.List:
		s, e := l.ValueOffsets(i)
		values, start, end = l.ListValues(), s, e
	case *array.LargeList:
		s, e := l.ValueOffsets(i)
		values, start, end = l.ListValues(), s, e
	case *array.FixedSizeList:
		n := int64(l.DataType().(*arrow.FixedSizeListType).Len())
		off := int64(l.Offset()+i) * n
		values, start, end = l.ListValues(), off, off+n
	default:
		return nil, fmt.Errorf("%s is not a list", arr.DataType())
	}
	for j := start; j < end; j++ {
		if values.IsNull(int(j)) {
			return nil, fmt.Errorf("element %d is NULL, which an array cannot hold", j-start)
		}
		v, err := c.value(values, int(j))
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", j-start, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// value writes the non-NULL value i of arr (an element of a list's
// values) to the column, as insertAll's JSON gives it.
func (c arrowCol) value(arr arrow.Array, i int) (any, error) {
	if c.sub != nil || c.rng != nil {
		s := arr.(*array.Struct)
		fields := make([]arrow.Array, s.NumField())
		for k := range fields {
			fields[k] = s.Field(k)
		}
		if c.rng != nil {
			return c.rng.value(fields, i)
		}
		return c.sub.row(fields, i)
	}
	switch a := arr.(type) {
	case *array.Boolean:
		return a.Value(i), nil
	case *array.Binary:
		return base64.StdEncoding.EncodeToString(a.Value(i)), nil
	case *array.LargeBinary:
		return base64.StdEncoding.EncodeToString(a.Value(i)), nil
	case *array.String:
		return c.text(a.Value(i))
	case *array.LargeString:
		return c.text(a.Value(i))
	case *array.Int8:
		return strconv.FormatInt(int64(a.Value(i)), 10), nil
	case *array.Int16:
		return strconv.FormatInt(int64(a.Value(i)), 10), nil
	case *array.Int32:
		if c.typ == "DATE" {
			return dateText(int64(a.Value(i)))
		}
		return strconv.FormatInt(int64(a.Value(i)), 10), nil
	case *array.Int64:
		return strconv.FormatInt(a.Value(i), 10), nil
	case *array.Uint8:
		return strconv.FormatUint(uint64(a.Value(i)), 10), nil
	case *array.Uint16:
		return strconv.FormatUint(uint64(a.Value(i)), 10), nil
	case *array.Uint32:
		return strconv.FormatUint(uint64(a.Value(i)), 10), nil
	case *array.Uint64:
		if v := a.Value(i); v > math.MaxInt64 {
			return nil, fmt.Errorf("%d is out of INT64's range", v)
		}
		return strconv.FormatUint(a.Value(i), 10), nil
	case *array.Float32:
		return floatValue(float64(a.Value(i)), 32), nil
	case *array.Float64:
		return floatValue(a.Value(i), 64), nil
	case *array.Date32:
		return dateText(int64(a.Value(i)))
	case *array.Timestamp:
		us := int64(a.Value(i))
		if c.typ == "TIMESTAMP" {
			return timestampText(us)
		}
		t := time.UnixMicro(us).UTC()
		if t.Year() < 1 || t.Year() > 9999 {
			return nil, fmt.Errorf("%d microseconds is out of DATETIME's range", us)
		}
		return t.Format("2006-01-02 15:04:05.000000"), nil
	case *array.Time64:
		return clockText(int64(a.Value(i)))
	case *array.Time32:
		v := int64(a.Value(i))
		if a.DataType().(*arrow.Time32Type).Unit == arrow.Millisecond {
			return clockText(v * 1000)
		}
		return clockText(v * 1_000_000)
	case *array.Decimal128:
		scale := a.DataType().(*arrow.Decimal128Type).Scale
		return decimalText(a.Value(i).BigInt(), scale), nil
	case *array.Decimal256:
		scale := a.DataType().(*arrow.Decimal256Type).Scale
		return decimalText(a.Value(i).BigInt(), scale), nil
	case *array.MonthInterval:
		return checkedIntervalText(int64(a.Value(i)), 0, 0)
	case *array.DayTimeInterval:
		v := a.Value(i)
		return checkedIntervalText(0, int64(v.Days), int64(v.Milliseconds)*1_000_000)
	case *array.MonthDayNanoInterval:
		v := a.Value(i)
		return checkedIntervalText(int64(v.Months), int64(v.Days), v.Nanoseconds)
	}
	return nil, fmt.Errorf("no conversion of %s to %s", arr.DataType(), c.typ)
}

// text is a Utf8 value written to the column. An INTERVAL's is read as
// the proto path reads a string (#1424), and one that is not an interval,
// or is out of range, fails its row (#1437); it was passed to the engine
// unchecked.
func (c arrowCol) text(s string) (any, error) {
	if c.typ == "INTERVAL" {
		return writeIntervalText(s)
	}
	return s, nil
}

// checkedIntervalText is an Arrow interval's canonical text, or an error
// when a part is out of INTERVAL's range (#1437).
func checkedIntervalText(months, days, nanos int64) (any, error) {
	if err := (interval{months: months, days: days, micros: nanos / 1000}).check(); err != nil {
		return nil, err
	}
	return intervalText(months, days, nanos), nil
}

func floatValue(f float64, bits int) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return json.Number(strconv.FormatFloat(f, 'g', -1, bits))
}

// dateText writes days since 1970-01-01 as a DATE.
func dateText(days int64) (string, error) {
	if days < -719162 || days > 2932896 {
		return "", fmt.Errorf("%d days since 1970-01-01 is out of DATE's range, -719162 to 2932896", days)
	}
	return time.Unix(days*86400, 0).UTC().Format("2006-01-02"), nil
}

// clockText writes microseconds since midnight as a TIME.
func clockText(us int64) (string, error) {
	if us < 0 || us >= 86400*1_000_000 {
		return "", fmt.Errorf("%d microseconds since midnight is not a TIME", us)
	}
	s := us / 1_000_000
	return fmt.Sprintf("%02d:%02d:%02d.%06d", s/3600, s/60%60, s%60, us%1_000_000), nil
}

// decimalText writes the unscaled n at scale exactly, in decimal.
func decimalText(n *big.Int, scale int32) string {
	r := new(big.Rat).SetFrac(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	text := r.FloatString(int(scale))
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	if text == "-0" {
		text = "0"
	}
	return text
}

// intervalText writes an interval as BigQuery's canonical form,
// "Y-M D H:M:S[.F]", each part with its own sign. Below a microsecond,
// which an INTERVAL does not hold, the nanoseconds are cut.
func intervalText(months, days, nanos int64) string {
	ym := ""
	if months < 0 {
		ym, months = "-", -months
	}
	ym += fmt.Sprintf("%d-%d", months/12, months%12)
	us := nanos / 1000
	hs := ""
	if us < 0 {
		hs, us = "-", -us
	}
	secs := us / 1_000_000
	clock := fmt.Sprintf("%s%d:%d:%d", hs, secs/3600, secs/60%60, secs%60)
	if f := us % 1_000_000; f != 0 {
		clock += strings.TrimRight(fmt.Sprintf(".%06d", f), "0")
	}
	return fmt.Sprintf("%s %d %s", ym, days, clock)
}
