package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/decimal256"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// arrowIPC writes rec as BigQuery frames arrow_rows: the schema's IPC
// message and the batch's.
func arrowIPC(t *testing.T, rec arrow.Record) (schema, batch []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(rec.Schema()))
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	msgs, isSchema, ok := arrowMessages(buf.Bytes())
	if !ok || len(msgs) != 2 || !isSchema[0] {
		t.Fatalf("the IPC stream: %d messages", len(msgs))
	}
	return msgs[0], msgs[1]
}

func arrowReq(stream string, schema, batch []byte, offset *wrapperspb.Int64Value) *storagepb.AppendRowsRequest {
	ad := &storagepb.AppendRowsRequest_ArrowData{Rows: &storagepb.ArrowRecordBatch{SerializedRecordBatch: batch}}
	if schema != nil {
		ad.WriterSchema = &storagepb.ArrowSchema{SerializedSchema: schema}
	}
	return &storagepb.AppendRowsRequest{WriteStream: stream, Offset: offset, Rows: &storagepb.AppendRowsRequest_ArrowRows{ArrowRows: ad}}
}

// testArrowRecord is rows of writeTestFields: i, s, r (a list, NULL in
// the second row) and rec{ts, d}.
func testArrowRecord(t *testing.T, ids ...int64) arrow.Record {
	t.Helper()
	recType := arrow.StructOf(
		arrow.Field{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}, Nullable: true},
		arrow.Field{Name: "d", Type: arrow.FixedWidthTypes.Date32, Nullable: true})
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "i", Type: arrow.PrimitiveTypes.Int64},
		{Name: "S", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "r", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: true},
		{Name: "rec", Type: recType, Nullable: true},
	}, nil)
	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	for n, id := range ids {
		b.Field(0).(*array.Int64Builder).Append(id)
		sb := b.Field(1).(*array.StringBuilder)
		lb := b.Field(2).(*array.ListBuilder)
		rb := b.Field(3).(*array.StructBuilder)
		if n%2 == 0 {
			sb.Append("s")
			lb.Append(true)
			lb.ValueBuilder().(*array.Int64Builder).AppendValues([]int64{1, 2}, nil)
			rb.Append(true)
			rb.FieldBuilder(0).(*array.TimestampBuilder).Append(arrow.Timestamp(1_700_000_000_123_456))
			rb.FieldBuilder(1).(*array.Date32Builder).Append(arrow.Date32(19000))
		} else {
			sb.AppendNull()
			lb.AppendNull()
			rb.AppendNull()
		}
	}
	return b.NewRecord()
}

// TestStorageWriteArrowRows: arrow_rows are written as proto_rows are, to
// the default, COMMITTED, PENDING and BUFFERED streams, with the
// connection's writer schema in later requests.
func TestStorageWriteArrowRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, tables := storageWriteFront(t, ctx)
	schema, batch := arrowIPC(t, testArrowRecord(t, 1, 2))
	_, batch2 := arrowIPC(t, testArrowRecord(t, 3))

	conn := openAppend(t, ctx, c)
	def := "projects/p-1/datasets/ds/tables/t/streams/_default"
	if resp, err := conn.send(arrowReq(def, schema, batch, nil)); respCode(resp, err) != codes.OK {
		t.Fatalf("append to the default stream: %v, %v", resp, err)
	}
	if resp, err := conn.send(arrowReq("", nil, batch2, nil)); respCode(resp, err) != codes.OK {
		t.Fatalf("the second append, with the connection's schema: %v, %v", resp, err)
	}
	tables.mu.Lock()
	got, _ := json.Marshal(tables.rows["ds.t"])
	tables.mu.Unlock()
	// The column is written by the table's name, s, not the field's, S.
	want := `[{"i":"1","r":["1","2"],"rec":{"d":"2022-01-08","ts":"2023-11-14 22:13:20.123456 UTC"},"s":"s"},` +
		`{"i":"2","r":[],"rec":null,"s":null},{"i":"3","r":["1","2"],"rec":{"d":"2022-01-08","ts":"2023-11-14 22:13:20.123456 UTC"},"s":"s"}]`
	if string(got) != want {
		t.Errorf("rows:\n got %s\nwant %s", got, want)
	}

	for _, typ := range []storagepb.WriteStream_Type{storagepb.WriteStream_COMMITTED, storagepb.WriteStream_PENDING, storagepb.WriteStream_BUFFERED} {
		ws, err := c.CreateWriteStream(ctx, &storagepb.CreateWriteStreamRequest{Parent: "projects/p-1/datasets/ds/tables/u",
			WriteStream: &storagepb.WriteStream{Type: typ}})
		if err != nil {
			t.Fatal(err)
		}
		before := len(tables.ids("ds.u"))
		conn := openAppend(t, ctx, c)
		resp, err := conn.send(arrowReq(ws.GetName(), schema, batch, wrapperspb.Int64(0)))
		if respCode(resp, err) != codes.OK || resp.GetAppendResult().GetOffset().GetValue() != 0 {
			t.Fatalf("%s: %v, %v", typ, resp, err)
		}
		resp, err = conn.send(arrowReq("", nil, batch2, wrapperspb.Int64(2)))
		if respCode(resp, err) != codes.OK || resp.GetAppendResult().GetOffset().GetValue() != 2 {
			t.Fatalf("%s, second append: %v, %v", typ, resp, err)
		}
		if got := respCode(conn.send(arrowReq("", nil, batch2, wrapperspb.Int64(2)))); got != codes.AlreadyExists {
			t.Errorf("%s, an offset written: %s", typ, got)
		}
		switch typ {
		case storagepb.WriteStream_PENDING:
			if n := len(tables.ids("ds.u")) - before; n != 0 {
				t.Errorf("PENDING rows in the table before the commit: %d", n)
			}
			if _, err := c.FinalizeWriteStream(ctx, &storagepb.FinalizeWriteStreamRequest{Name: ws.GetName()}); err != nil {
				t.Fatal(err)
			}
			r, err := c.BatchCommitWriteStreams(ctx, &storagepb.BatchCommitWriteStreamsRequest{Parent: "projects/p-1/datasets/ds/tables/u",
				WriteStreams: []string{ws.GetName()}})
			if err != nil || len(r.GetStreamErrors()) > 0 {
				t.Fatalf("commit: %v, %v", r, err)
			}
		case storagepb.WriteStream_BUFFERED:
			if _, err := c.FlushRows(ctx, &storagepb.FlushRowsRequest{WriteStream: ws.GetName(), Offset: wrapperspb.Int64(2)}); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := tables.ids("ds.u")[before:], []string{"1", "2", "3"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: rows %v, want %v", typ, got, want)
		}
	}
}

// TestStorageWriteArrowTypes: every Arrow type BigQuery documents for its
// column types, as insertAll's JSON.
func TestStorageWriteArrowTypes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, tables := storageWriteFront(t, ctx)
	tables.schemas["ds.all"] = `[{"name":"b","type":"BOOLEAN"},{"name":"by","type":"BYTES"},{"name":"d","type":"DATE"},` +
		`{"name":"d2","type":"DATE"},{"name":"dt","type":"DATETIME"},{"name":"f4","type":"FLOAT"},{"name":"f8","type":"FLOAT"},` +
		`{"name":"g","type":"GEOGRAPHY"},{"name":"i8","type":"INTEGER"},{"name":"i32","type":"INTEGER"},{"name":"j","type":"JSON"},` +
		`{"name":"n","type":"NUMERIC"},{"name":"bn","type":"BIGNUMERIC"},{"name":"s","type":"STRING"},{"name":"ts","type":"TIMESTAMP"},` +
		`{"name":"tm","type":"TIME"},{"name":"iv","type":"INTERVAL"},{"name":"iv2","type":"INTERVAL"}]`
	fields := []arrow.Field{
		{Name: "b", Type: arrow.FixedWidthTypes.Boolean},
		{Name: "by", Type: arrow.BinaryTypes.Binary},
		{Name: "d", Type: arrow.FixedWidthTypes.Date32},
		{Name: "d2", Type: arrow.BinaryTypes.String},
		{Name: "dt", Type: &arrow.TimestampType{Unit: arrow.Microsecond}},
		{Name: "f4", Type: arrow.PrimitiveTypes.Float32},
		{Name: "f8", Type: arrow.PrimitiveTypes.Float64},
		{Name: "g", Type: arrow.BinaryTypes.String},
		{Name: "i8", Type: arrow.PrimitiveTypes.Int8},
		{Name: "i32", Type: arrow.PrimitiveTypes.Int32},
		{Name: "j", Type: arrow.BinaryTypes.String},
		{Name: "n", Type: &arrow.Decimal128Type{Precision: 38, Scale: 9}},
		{Name: "bn", Type: &arrow.Decimal256Type{Precision: 76, Scale: 38}},
		{Name: "s", Type: arrow.BinaryTypes.String},
		{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}},
		{Name: "tm", Type: arrow.FixedWidthTypes.Time64us},
		{Name: "iv", Type: arrow.FixedWidthTypes.MonthDayNanoInterval},
		{Name: "iv2", Type: arrow.FixedWidthTypes.MonthInterval},
	}
	b := array.NewRecordBuilder(memory.DefaultAllocator, arrow.NewSchema(fields, nil))
	b.Field(0).(*array.BooleanBuilder).Append(true)
	b.Field(1).(*array.BinaryBuilder).Append([]byte("hi"))
	b.Field(2).(*array.Date32Builder).Append(arrow.Date32(-719162))
	b.Field(3).(*array.StringBuilder).Append("2024-02-29")
	b.Field(4).(*array.TimestampBuilder).Append(arrow.Timestamp(0))
	b.Field(5).(*array.Float32Builder).Append(1.5)
	b.Field(6).(*array.Float64Builder).Append(-0.25)
	b.Field(7).(*array.StringBuilder).Append("POINT(1 2)")
	b.Field(8).(*array.Int8Builder).Append(-8)
	b.Field(9).(*array.Int32Builder).Append(32)
	b.Field(10).(*array.StringBuilder).Append(`{"a":1}`)
	b.Field(11).(*array.Decimal128Builder).Append(decimal128.FromI64(-1234567891))
	b.Field(12).(*array.Decimal256Builder).Append(decimal256.FromI64(5))
	b.Field(13).(*array.StringBuilder).Append("str")
	b.Field(14).(*array.TimestampBuilder).Append(arrow.Timestamp(-1))
	b.Field(15).(*array.Time64Builder).Append(arrow.Time64(3_723_000_004))
	b.Field(16).(*array.MonthDayNanoIntervalBuilder).Append(arrow.MonthDayNanoInterval{Months: 14, Days: -3, Nanoseconds: 3_661_500_000_000})
	b.Field(17).(*array.MonthIntervalBuilder).Append(arrow.MonthInterval(-13))
	schema, batch := arrowIPC(t, b.NewRecord())
	resp, err := openAppend(t, ctx, c).send(arrowReq("projects/p-1/datasets/ds/tables/all/streams/_default", schema, batch, nil))
	if respCode(resp, err) != codes.OK {
		t.Fatalf("append: %v, %v", resp, err)
	}
	tables.mu.Lock()
	got := tables.rows["ds.all"][0]
	tables.mu.Unlock()
	want := map[string]any{"b": true, "by": "aGk=", "d": "0001-01-01", "d2": "2024-02-29", "dt": "1970-01-01 00:00:00.000000",
		"f4": 1.5, "f8": -0.25, "g": "POINT(1 2)", "i8": "-8", "i32": "32", "j": `{"a":1}`, "n": "-1.234567891",
		"bn": "0.00000000000000000000000000000000000005", "s": "str", "ts": "1969-12-31 23:59:59.999999 UTC",
		"tm": "01:02:03.000004", "iv": "1-2 -3 1:1:1.5", "iv2": "-1-1 0 0:0:0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("row:\n got %v\nwant %v", got, want)
	}
}

// TestStorageWriteArrowRefusesSchemas: an Arrow writer schema the table
// does not take is refused before any row is written.
func TestStorageWriteArrowRefusesSchemas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, tables := storageWriteFront(t, ctx)
	def := "projects/p-1/datasets/ds/tables/t/streams/_default"
	one := func(f arrow.Field, build func(array.Builder)) ([]byte, []byte) {
		b := array.NewRecordBuilder(memory.DefaultAllocator, arrow.NewSchema([]arrow.Field{{Name: "i", Type: arrow.PrimitiveTypes.Int64}, f}, nil))
		b.Field(0).(*array.Int64Builder).Append(1)
		build(b.Field(1))
		return arrowIPC(t, b.NewRecord())
	}
	for name, tc := range map[string]struct {
		f     arrow.Field
		build func(array.Builder)
		code  codes.Code
	}{
		"an extra field": {arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Int64}, func(b array.Builder) { b.(*array.Int64Builder).Append(1) }, codes.InvalidArgument},
		"a type the column does not take": {arrow.Field{Name: "s", Type: arrow.PrimitiveTypes.Float64},
			func(b array.Builder) { b.(*array.Float64Builder).Append(1) }, codes.InvalidArgument},
		"a list to a column not REPEATED": {arrow.Field{Name: "s", Type: arrow.ListOf(arrow.BinaryTypes.String)},
			func(b array.Builder) { b.AppendNull() }, codes.InvalidArgument},
		"a REPEATED column not a list": {arrow.Field{Name: "r", Type: arrow.PrimitiveTypes.Int64},
			func(b array.Builder) { b.(*array.Int64Builder).Append(1) }, codes.InvalidArgument},
		"a timestamp in nanoseconds": {arrow.Field{Name: "rec", Type: arrow.StructOf(arrow.Field{Name: "ts", Type: arrow.FixedWidthTypes.Timestamp_ns})},
			func(b array.Builder) { b.AppendNull() }, codes.InvalidArgument},
		"a NULL element of a list": {arrow.Field{Name: "r", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64)}, func(b array.Builder) {
			lb := b.(*array.ListBuilder)
			lb.Append(true)
			lb.ValueBuilder().AppendNull()
		}, codes.InvalidArgument},
	} {
		schema, batch := one(tc.f, tc.build)
		if got := respCode(openAppend(t, ctx, c).send(arrowReq(def, schema, batch, nil))); got != tc.code {
			t.Errorf("%s: %s, want %s", name, got, tc.code)
		}
	}
	if got := respCode(openAppend(t, ctx, c).send(arrowReq(def, []byte("not arrow"), []byte("x"), nil))); got != codes.InvalidArgument {
		t.Errorf("a schema that is not Arrow: %s", got)
	}
	if got := tables.ids("ds.t"); len(got) != 0 {
		t.Errorf("rows written: %v", got)
	}
}
