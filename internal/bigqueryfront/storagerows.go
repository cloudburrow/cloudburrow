package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"github.com/apache/arrow/go/v15/arrow"
	"github.com/apache/arrow/go/v15/arrow/array"
	"github.com/apache/arrow/go/v15/arrow/decimal128"
	"github.com/apache/arrow/go/v15/arrow/decimal256"
	"github.com/apache/arrow/go/v15/arrow/ipc"
	"github.com/apache/arrow/go/v15/arrow/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReadRows, written by the front (#1095, #1098; storageread.go says why).
//
// The rows are read with jobs.query of the emulator's REST API, as the
// emulator's own ReadRows reads them with a query: SELECT the session
// schema's columns FROM `dataset.table` (the whole name, so the table's
// own rows, #1032) WHERE (the session's row_restriction), with
// formatOptions.useInt64Timestamp. The query is the front's own, left out
// of jobs.list (jobrecords.go). Each row is then written in the session's
// schema, which the front gave the client: Arrow, as BigQuery frames it
// (the record batch's IPC message alone, arrowframes.go), a REPEATED column
// as a list and a RECORD as a struct, at any depth; Avro, in the binary
// encoding of the schema's record (avrorows.go). A value is read from the
// query's row as BigQuery's REST API gives it (TableCell: a string, a list
// of {"v": ...} for a REPEATED value, {"f": [...]} for a RECORD, null),
// by the schema's type: BYTES is base64, a TIMESTAMP microseconds since
// the epoch (a TIMESTAMP inside a RECORD or REPEATED value is the engine's
// text, "2020-01-01 00:00:00+00", as the emulator writes it there, read
// too), a DATETIME "2006-01-02T15:04:05.999999", a TIME "15:04:05.999999".
//
// BigQuery's ReadRowsResponse.schema "is only populated in the first
// ReadRowsResponse RPC" (storage.proto); rows are
// sent readRowsBatch at a time, and a read of no rows is one answer with
// none, as the emulator's was. ReadRowsRequest.offset skips that many rows
// of the stream. Rows are read when ReadRows asks for them, as the
// emulator read them (not as of the session's creation, as BigQuery's are).

// readRowsBatch is the most rows one ReadRowsResponse carries.
const readRowsBatch = 1000

// serveRows answers ReadRows of st from offset (above).
func (s *storageRead) serveRows(ctx context.Context, ss grpc.ServerStream, st *readStream, offset int64) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = status.Errorf(codes.Internal, "CloudBurrow could not write the rows of %s.%s: %v", st.table.dataset, st.table.table, p)
		}
	}()
	if offset < 0 {
		return status.Errorf(codes.InvalidArgument, "offset %d is negative", offset)
	}
	var enc rowEncoder
	var columns []string
	switch {
	case st.arrowSchema != nil:
		a, err := newArrowRows(st.arrowSchema)
		if err != nil {
			return status.Errorf(codes.Internal, "read the session's Arrow schema: %v", err)
		}
		enc = a
		for _, f := range a.schema.Fields() {
			columns = append(columns, f.Name)
		}
	case st.avroSchema != "":
		a, err := newAvroRows(st.avroSchema)
		if err != nil {
			return status.Errorf(codes.Internal, "read the session's Avro schema: %v", err)
		}
		enc = a
		for _, f := range a.root.fields {
			columns = append(columns, f.name)
		}
	default:
		return status.Errorf(codes.Internal, "the session of %s.%s has no schema", st.table.dataset, st.table.table)
	}
	if len(columns) == 0 {
		return status.Errorf(codes.InvalidArgument, "the session of %s.%s reads no column: read_options.selected_fields names "+
			"none of the table's top-level columns", st.table.dataset, st.table.table)
	}
	rows, err := s.queryRows(ctx, st, columns)
	if err != nil {
		return err
	}
	if offset > int64(len(rows)) {
		return status.Errorf(codes.OutOfRange, "offset %d is past the stream's %d rows", offset, len(rows))
	}
	rows = rows[offset:]
	first := true
	for start := 0; first || start < len(rows); start += readRowsBatch {
		end := min(start+readRowsBatch, len(rows))
		resp, err := enc.encode(rows[start:end])
		if err != nil {
			return status.Errorf(codes.Internal, "CloudBurrow could not write the rows of %s.%s: %v", st.table.dataset, st.table.table, err)
		}
		resp.RowCount = int64(end - start)
		if first {
			enc.schemaOf(resp)
			first = false
		}
		if err := ss.SendMsg(resp); err != nil {
			return err
		}
	}
	return nil
}

// rowEncoder writes rows in a session's format.
type rowEncoder interface {
	// encode is a ReadRowsResponse of rows, without its schema.
	encode(rows []queryRow) (*storagepb.ReadRowsResponse, error)
	// schemaOf sets resp's schema, the session's.
	schemaOf(resp *storagepb.ReadRowsResponse)
}

// queryRow is a row as the REST API gives it: {"f": [{"v": ...}, ...]}.
type queryRow struct {
	F []struct {
		V any `json:"v"`
	} `json:"f"`
}

// queryRows reads st's rows, of columns, through the emulator's jobs.query
// (above).
func (s *storageRead) queryRows(ctx context.Context, st *readStream, columns []string) ([]queryRow, error) {
	ctx, cancel := context.WithTimeout(ctx, storageReadTimeout)
	defer cancel()
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = quotePath([]string{c})
	}
	sql := "SELECT " + strings.Join(quoted, ", ") + " FROM " + quotePath([]string{st.table.dataset, st.table.table})
	if strings.TrimSpace(st.restriction) != "" {
		sql += " WHERE (" + st.restriction + ")"
	}
	legacy := false
	body, err := json.Marshal(map[string]any{
		"query":         sql,
		"useLegacySql":  &legacy,
		"formatOptions": map[string]bool{"useInt64Timestamp": true},
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://bigquery/", nil)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	f := front{next: s.rest, base: "/bigquery/v2/projects/" + url.PathEscape(st.table.project), records: s.records}
	code, got := f.send(r, http.MethodPost, "/queries", body)
	var res struct {
		Rows []queryRow `json:"rows"`
	}
	if code == http.StatusOK || code == 0 {
		if err := json.Unmarshal(got, &res); err != nil {
			return nil, status.Errorf(codes.Internal, "read the rows of %s.%s: %v", st.table.dataset, st.table.table, err)
		}
		for _, row := range res.Rows {
			if len(row.F) != len(columns) {
				return nil, status.Errorf(codes.Internal, "the rows of %s.%s have %d columns, the session's schema %d",
					st.table.dataset, st.table.table, len(row.F), len(columns))
			}
		}
		return res.Rows, nil
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(got))
	if json.Unmarshal(got, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	c := codes.Unavailable
	switch code {
	case http.StatusBadRequest:
		c = codes.InvalidArgument
	case http.StatusNotFound:
		c = codes.NotFound
	case http.StatusNotImplemented:
		c = codes.Unimplemented
	}
	return nil, status.Errorf(c, "reading the rows of %s.%s: %s", st.table.dataset, st.table.table, msg)
}

// arrowRows writes rows as Arrow record batches of schema.
type arrowRows struct {
	schema  *arrow.Schema
	message []byte
}

func newArrowRows(message []byte) (*arrowRows, error) {
	r, err := ipc.NewReader(bytes.NewReader(message))
	if err != nil {
		return nil, err
	}
	defer r.Release()
	return &arrowRows{schema: r.Schema(), message: message}, nil
}

func (a *arrowRows) schemaOf(resp *storagepb.ReadRowsResponse) {
	resp.Schema = &storagepb.ReadRowsResponse_ArrowSchema{ArrowSchema: &storagepb.ArrowSchema{SerializedSchema: a.message}}
}

func (a *arrowRows) encode(rows []queryRow) (*storagepb.ReadRowsResponse, error) {
	mem := memory.NewGoAllocator()
	b := array.NewRecordBuilder(mem, a.schema)
	defer b.Release()
	for i, row := range rows {
		for j, f := range a.schema.Fields() {
			if err := appendArrow(b.Field(j), f, row.F[j].V); err != nil {
				return nil, fmt.Errorf("row %d, column %s: %w", i, f.Name, err)
			}
		}
	}
	rec := b.NewRecord()
	defer rec.Release()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithAllocator(mem), ipc.WithSchema(a.schema))
	if err := w.Write(rec); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &storagepb.ReadRowsResponse{Rows: &storagepb.ReadRowsResponse_ArrowRecordBatch{ArrowRecordBatch: &storagepb.ArrowRecordBatch{
		SerializedRecordBatch: arrowBatchMessages(buf.Bytes()), // arrowframes.go
		RowCount:              int64(len(rows)),
	}}}, nil
}

// appendArrow appends v, a value of the field f as the REST API gives it
// (above), to b, f's builder.
func appendArrow(b array.Builder, f arrow.Field, v any) error {
	if lb, ok := b.(*array.ListBuilder); ok {
		// BigQuery has no NULL array: an empty one reads as NULL.
		items, ok := v.([]any)
		if v != nil && !ok {
			return fmt.Errorf("a REPEATED value of %T", v)
		}
		lb.Append(true)
		elem := f.Type.(*arrow.ListType).ElemField()
		for _, it := range items {
			if err := appendArrow(lb.ValueBuilder(), elem, cellValue(it)); err != nil {
				return err
			}
		}
		return nil
	}
	if v == nil {
		b.AppendNull()
		return nil
	}
	if sb, ok := b.(*array.StructBuilder); ok {
		cells, err := recordCells(v)
		if err != nil {
			return err
		}
		st := f.Type.(*arrow.StructType)
		if len(cells) != st.NumFields() {
			return fmt.Errorf("a RECORD of %d fields, want %d", len(cells), st.NumFields())
		}
		sb.Append(true)
		for i := range cells {
			if err := appendArrow(sb.FieldBuilder(i), st.Field(i), cellValue(cells[i])); err != nil {
				return err
			}
		}
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("a value of %T", v)
	}
	switch b := b.(type) {
	case *array.Int64Builder:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		b.Append(n)
	case *array.Float64Builder:
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		b.Append(x)
	case *array.BooleanBuilder:
		x, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		b.Append(x)
	case *array.StringBuilder:
		b.Append(s)
	case *array.BinaryBuilder:
		x, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return err
		}
		b.Append(x)
	case *array.Decimal128Builder:
		// Exactly: arrow's FromString goes through a float.
		n, err := unscaled(s, int(b.Type().(*arrow.Decimal128Type).Scale)) // avrorows.go
		if err != nil {
			return err
		}
		b.Append(decimal128.FromBigInt(n))
	case *array.Decimal256Builder:
		n, err := unscaled(s, int(b.Type().(*arrow.Decimal256Type).Scale))
		if err != nil {
			return err
		}
		b.Append(decimal256.FromBigInt(n))
	case *array.Date32Builder:
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			return err
		}
		b.Append(arrow.Date32FromTime(d))
	case *array.Time64Builder:
		us, err := parseTimeOfDay(s)
		if err != nil {
			return err
		}
		if b.Type().(*arrow.Time64Type).Unit == arrow.Nanosecond {
			us *= 1000
		}
		b.Append(arrow.Time64(us))
	case *array.TimestampBuilder:
		var t time.Time
		var err error
		if name, _ := f.Metadata.GetValue("ARROW:extension:name"); name == "google:sqlType:datetime" {
			t, err = parseDateTime(s)
		} else {
			t, err = parseTimestamp(s)
		}
		if err != nil {
			return err
		}
		ts, err := arrow.TimestampFromTime(t, b.Type().(*arrow.TimestampType).Unit)
		if err != nil {
			return err
		}
		b.Append(ts)
	default:
		return fmt.Errorf("unsupported Arrow type %s", f.Type)
	}
	return nil
}

// bigQueryArrowSchema is the emulator's Arrow schema message with a
// DATETIME column's type BigQuery's: a timestamp with no time zone, which
// is how a client tells a DATETIME from a TIMESTAMP (the Go client reads a
// timestamp with no zone as a civil.DateTime: cloud.google.com/go/bigquery
// v1.85.0, arrow.go). The emulator gives it the zone UTC (types/arrow.go,
// Timestamp_us), measured: the Go client's Table.Read through the Storage
// Read API read a DATETIME as a time.Time. A message it cannot read, or
// with no DATETIME, is kept as it is.
func bigQueryArrowSchema(msg []byte) []byte {
	r, err := ipc.NewReader(bytes.NewReader(msg))
	if err != nil {
		return msg
	}
	schema := r.Schema()
	r.Release()
	changed := false
	fields := make([]arrow.Field, len(schema.Fields()))
	for i, f := range schema.Fields() {
		fields[i] = datetimeField(f, &changed)
	}
	if !changed {
		return msg
	}
	md := schema.Metadata()
	out := arrow.NewSchema(fields, &md)
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(out))
	if err := w.Close(); err != nil {
		return msg
	}
	return arrowSchemaMessage(buf.Bytes()) // arrowframes.go
}

// datetimeField is f with each DATETIME within it a timestamp with no
// time zone (above).
func datetimeField(f arrow.Field, changed *bool) arrow.Field {
	switch t := f.Type.(type) {
	case *arrow.TimestampType:
		if name, _ := f.Metadata.GetValue("ARROW:extension:name"); name == "google:sqlType:datetime" && t.TimeZone != "" {
			f.Type = &arrow.TimestampType{Unit: t.Unit}
			*changed = true
		}
	case *arrow.ListType:
		f.Type = arrow.ListOfField(datetimeField(t.ElemField(), changed))
	case *arrow.StructType:
		fields := make([]arrow.Field, t.NumFields())
		for i := range fields {
			fields[i] = datetimeField(t.Field(i), changed)
		}
		f.Type = arrow.StructOf(fields...)
	}
	return f
}

// cellValue is the value of a REST TableCell, {"v": ...}.
func cellValue(c any) any {
	if m, ok := c.(map[string]any); ok {
		return m["v"]
	}
	return nil
}

// recordCells is the cells of a RECORD value as the REST API gives it,
// {"f": [...]}.
func recordCells(v any) ([]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("a RECORD value of %T", v)
	}
	cells, ok := m["f"].([]any)
	if !ok {
		return nil, fmt.Errorf("a RECORD value without its fields")
	}
	return cells, nil
}

// parseTimestamp reads a TIMESTAMP value of the REST API: microseconds
// since the epoch (useInt64Timestamp), seconds as a float, or the engine's
// text (above).
func parseTimestamp(s string) (time.Time, error) {
	if us, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMicro(us).UTC(), nil
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999 MST",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	if r, ok := new(big.Rat).SetString(s); ok {
		us := new(big.Rat).Mul(r, big.NewRat(1e6, 1))
		n := new(big.Int).Quo(us.Num(), us.Denom())
		if n.IsInt64() {
			return time.UnixMicro(n.Int64()).UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unreadable TIMESTAMP %q", s)
}

// parseDateTime reads a DATETIME value, as the time of that clock in UTC.
func parseDateTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unreadable DATETIME %q", s)
}

// parseTimeOfDay reads a TIME value as microseconds since midnight.
func parseTimeOfDay(s string) (int64, error) {
	t, err := time.Parse("15:04:05.999999999", s)
	if err != nil {
		return 0, fmt.Errorf("unreadable TIME %q", s)
	}
	return int64(t.Hour())*3600e6 + int64(t.Minute())*60e6 + int64(t.Second())*1e6 + int64(t.Nanosecond()/1000), nil
}
