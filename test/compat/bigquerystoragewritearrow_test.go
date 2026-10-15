//go:build compat

package compat

import (
	"bytes"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"github.com/apache/arrow/go/v15/arrow"
	"github.com/apache/arrow/go/v15/arrow/array"
	"github.com/apache/arrow/go/v15/arrow/decimal128"
	"github.com/apache/arrow/go/v15/arrow/decimal256"
	"github.com/apache/arrow/go/v15/arrow/ipc"
	"github.com/apache/arrow/go/v15/arrow/memory"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The Storage Write API's arrow_rows (#1327), through the official
// generated BigQueryWriteClient (cloud.google.com/go/bigquery/storage/apiv1):
// the managed writer of the pinned cloud.google.com/go/bigquery v1.85.0
// writes proto_rows only, so each request is an AppendRowsRequest whose
// arrow_rows are the Arrow IPC messages of the schema and of one record
// batch (storage.proto's ArrowSchema and ArrowRecordBatch), as pyarrow's
// Schema.serialize and RecordBatch.serialize write them. The rows are read
// back with the Go client's Table.Read.

// arrowWriteSchema is storageWriteDDL's table as an Arrow schema, in the
// Arrow types BigQuery's supported-data-types page gives for each column.
func arrowWriteSchema() *arrow.Schema {
	tsUTC := &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}
	return arrow.NewSchema([]arrow.Field{
		{Name: "i", Type: arrow.PrimitiveTypes.Int64},
		{Name: "f", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "n", Type: &arrow.Decimal128Type{Precision: 38, Scale: 9}, Nullable: true},
		{Name: "bn", Type: &arrow.Decimal256Type{Precision: 76, Scale: 38}, Nullable: true},
		{Name: "b", Type: arrow.FixedWidthTypes.Boolean, Nullable: true},
		{Name: "s", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "y", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "d", Type: arrow.FixedWidthTypes.Date32, Nullable: true},
		{Name: "dt", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: true},
		{Name: "tm", Type: arrow.FixedWidthTypes.Time64us, Nullable: true},
		{Name: "ts", Type: tsUTC, Nullable: true},
		{Name: "j", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "g", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "rec", Type: arrow.StructOf(
			arrow.Field{Name: "a", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
			arrow.Field{Name: "ts", Type: tsUTC, Nullable: true},
			arrow.Field{Name: "r", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true}), Nullable: true},
		{Name: "r", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: true},
		{Name: "rts", Type: arrow.ListOf(tsUTC), Nullable: true},
		{Name: "rr", Type: arrow.ListOf(arrow.StructOf(
			arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
			arrow.Field{Name: "y", Type: arrow.BinaryTypes.String, Nullable: true})), Nullable: true},
	}, nil)
}

// arrowWriteRecord is a batch of fullRow's values (row ids full) and of
// idRow's (the other ids: every column NULL, every list NULL, which
// BigQuery writes as the empty array).
func arrowWriteRecord(t *testing.T, ids []int64, full map[int64]bool) arrow.Record {
	t.Helper()
	b := array.NewRecordBuilder(memory.DefaultAllocator, arrowWriteSchema())
	defer b.Release()
	us := func(tm time.Time) arrow.Timestamp { return arrow.Timestamp(tm.UnixMicro()) }
	for _, id := range ids {
		b.Field(0).(*array.Int64Builder).Append(id)
		if !full[id] {
			for k := 1; k < len(b.Fields()); k++ {
				b.Field(k).AppendNull()
			}
			continue
		}
		b.Field(1).(*array.Float64Builder).Append(1.5)
		b.Field(2).(*array.Decimal128Builder).Append(decimal128.FromI64(123456789012)) // 123.456789012
		bn, _ := new(big.Int).SetString("-25"+strings.Repeat("0", 37), 10)             // -2.5
		b.Field(3).(*array.Decimal256Builder).Append(decimal256.FromBigInt(bn))
		b.Field(4).(*array.BooleanBuilder).Append(true)
		b.Field(5).(*array.StringBuilder).Append("x")
		b.Field(6).(*array.BinaryBuilder).Append([]byte("ab"))
		b.Field(7).(*array.Date32Builder).Append(arrow.Date32(19724)) // 2024-01-02
		b.Field(8).(*array.TimestampBuilder).Append(us(time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)))
		b.Field(9).(*array.Time64Builder).Append(arrow.Time64((3*3600+4*60+5)*1_000_000 + 500000))
		b.Field(10).(*array.TimestampBuilder).Append(arrow.Timestamp(1704164645123456))
		b.Field(11).(*array.StringBuilder).Append(`{"k":1}`)
		b.Field(12).(*array.StringBuilder).Append("POINT(1 2)")
		rec := b.Field(13).(*array.StructBuilder)
		rec.Append(true)
		rec.FieldBuilder(0).(*array.Int64Builder).Append(3)
		rec.FieldBuilder(1).(*array.TimestampBuilder).Append(arrow.Timestamp(1577836800000000))
		rl := rec.FieldBuilder(2).(*array.ListBuilder)
		rl.Append(true)
		rl.ValueBuilder().(*array.StringBuilder).AppendValues([]string{"p", "q"}, nil)
		r := b.Field(14).(*array.ListBuilder)
		r.Append(true)
		r.ValueBuilder().(*array.Int64Builder).AppendValues([]int64{7, 8}, nil)
		rts := b.Field(15).(*array.ListBuilder)
		rts.Append(true)
		rts.ValueBuilder().(*array.TimestampBuilder).Append(arrow.Timestamp(1609459201500000))
		rr := b.Field(16).(*array.ListBuilder)
		rr.Append(true)
		e := rr.ValueBuilder().(*array.StructBuilder)
		e.Append(true)
		e.FieldBuilder(0).(*array.Int64Builder).Append(1)
		e.FieldBuilder(1).(*array.StringBuilder).Append("a")
	}
	return b.NewRecord()
}

// arrowMessagesOf writes rec as an Arrow IPC stream and returns the
// schema's message and the record batch's, the end-of-stream marker left
// out.
func arrowMessagesOf(t *testing.T, rec arrow.Record) (schema, batch []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(rec.Schema()))
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	stream := buf.Bytes()
	var at []int
	rd := bytes.NewReader(stream)
	mr := ipc.NewMessageReader(rd)
	defer mr.Release()
	for i := 0; i < 2; i++ {
		if _, err := mr.Message(); err != nil {
			t.Fatalf("the IPC stream's message %d: %v", i, err)
		}
		at = append(at, len(stream)-rd.Len())
	}
	return stream[:at[0]], stream[at[0]:at[1]]
}

func arrowAppend(stream string, schema, batch []byte, offset *wrapperspb.Int64Value) *storagepb.AppendRowsRequest {
	ad := &storagepb.AppendRowsRequest_ArrowData{Rows: &storagepb.ArrowRecordBatch{SerializedRecordBatch: batch}}
	if schema != nil {
		ad.WriterSchema = &storagepb.ArrowSchema{SerializedSchema: schema}
	}
	return &storagepb.AppendRowsRequest{WriteStream: stream, Offset: offset, Rows: &storagepb.AppendRowsRequest_ArrowRows{ArrowRows: ad}}
}

// TestBigQueryStorageWriteArrowRows: arrow_rows of every column type
// storageWriteDDL has, nested and repeated, written to the default stream
// and to COMMITTED, PENDING and BUFFERED streams, the connection's writer
// schema left out of each request after its first; Table.Read reads each
// value as fullRow's proto row writes it, a NULL list as the empty array.
// PENDING rows are in the table at the commit, BUFFERED rows at the
// flush; an offset already written is ALREADY_EXISTS.
//
// covers: bigquery.tabledata.list
func TestBigQueryStorageWriteArrowRows(t *testing.T) {
	h := New(t)
	c, _, tables := writeTables(t, h)
	raw, err := bqstorage.NewBigQueryWriteClient(h.Context(), storageWriteOptions(h)...)
	if err != nil {
		t.Fatalf("NewBigQueryWriteClient: %v", err)
	}
	defer raw.Close()
	schema, first := arrowMessagesOf(t, arrowWriteRecord(t, []int64{1, 2}, map[int64]bool{1: true}))
	_, second := arrowMessagesOf(t, arrowWriteRecord(t, []int64{3}, nil))
	want := []string{fullRowRead, idRowRead("2"), idRowRead("3")}

	send := func(ar storagepb.BigQueryWrite_AppendRowsClient, req *storagepb.AppendRowsRequest) *storagepb.AppendRowsResponse {
		t.Helper()
		if err := ar.Send(req); err != nil {
			t.Fatalf("AppendRows send: %v", err)
		}
		resp, err := ar.Recv()
		if err != nil {
			t.Fatalf("AppendRows: %v", err)
		}
		return resp
	}
	read := func(tbl *bigquery.Table) []string {
		return readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read")
	}

	// The default stream.
	tbl := tables[0]
	ar, err := raw.AppendRows(h.Context())
	if err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	if resp := send(ar, arrowAppend(writeTablePath(tbl)+"/streams/_default", schema, first, nil)); resp.GetError() != nil || resp.GetAppendResult().GetOffset() != nil {
		t.Fatalf("append to the default stream: %v", resp)
	}
	if resp := send(ar, arrowAppend("", nil, second, nil)); resp.GetError() != nil {
		t.Fatalf("the second append: %v", resp)
	}
	_ = ar.CloseSend()
	if got := read(tbl); !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read of the default stream's rows:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// COMMITTED, PENDING and BUFFERED streams of the other table, in turn.
	tbl = tables[1]
	var all []string
	for _, typ := range []storagepb.WriteStream_Type{storagepb.WriteStream_COMMITTED, storagepb.WriteStream_PENDING, storagepb.WriteStream_BUFFERED} {
		ws, err := raw.CreateWriteStream(h.Context(), &storagepb.CreateWriteStreamRequest{Parent: writeTablePath(tbl),
			WriteStream: &storagepb.WriteStream{Type: typ}})
		if err != nil {
			t.Fatalf("CreateWriteStream %s: %v", typ, err)
		}
		ar, err := raw.AppendRows(h.Context())
		if err != nil {
			t.Fatalf("AppendRows: %v", err)
		}
		if resp := send(ar, arrowAppend(ws.GetName(), schema, first, wrapperspb.Int64(0))); resp.GetError() != nil || resp.GetAppendResult().GetOffset().GetValue() != 0 {
			t.Fatalf("%s: append: %v", typ, resp)
		}
		if resp := send(ar, arrowAppend("", nil, second, wrapperspb.Int64(2))); resp.GetError() != nil || resp.GetAppendResult().GetOffset().GetValue() != 2 {
			t.Fatalf("%s: the second append: %v", typ, resp)
		}
		if resp := send(ar, arrowAppend("", nil, second, wrapperspb.Int64(2))); codes.Code(resp.GetError().GetCode()) != codes.AlreadyExists {
			t.Errorf("%s: an offset already written: %v, want ALREADY_EXISTS", typ, resp)
		}
		_ = ar.CloseSend()
		switch typ {
		case storagepb.WriteStream_PENDING:
			if got := read(tbl); !reflect.DeepEqual(got, all) {
				t.Errorf("PENDING rows before the commit: %q", got)
			}
			if f, err := raw.FinalizeWriteStream(h.Context(), &storagepb.FinalizeWriteStreamRequest{Name: ws.GetName()}); err != nil || f.GetRowCount() != 3 {
				t.Fatalf("FinalizeWriteStream: %v, %v", f, err)
			}
			resp, err := raw.BatchCommitWriteStreams(h.Context(), &storagepb.BatchCommitWriteStreamsRequest{Parent: writeTablePath(tbl), WriteStreams: []string{ws.GetName()}})
			if err != nil || len(resp.GetStreamErrors()) != 0 {
				t.Fatalf("BatchCommitWriteStreams: %v, %v", resp, err)
			}
		case storagepb.WriteStream_BUFFERED:
			if got := read(tbl); !reflect.DeepEqual(got, all) {
				t.Errorf("BUFFERED rows before the flush: %q", got)
			}
			if _, err := raw.FlushRows(h.Context(), &storagepb.FlushRowsRequest{WriteStream: ws.GetName(), Offset: wrapperspb.Int64(2)}); err != nil {
				t.Fatalf("FlushRows: %v", err)
			}
		}
		all = append(all, want...)
		sort.Strings(all)
		if got := read(tbl); !reflect.DeepEqual(got, all) {
			t.Errorf("Table.Read after the %s stream:\n%s\nwant\n%s", typ, strings.Join(got, "\n"), strings.Join(all, "\n"))
		}
	}
}

// TestBigQueryStorageWriteArrowRefusesSchemas: an Arrow writer schema the
// table does not take (a field it has no column for, a TIMESTAMP in
// nanoseconds, a list for a column that is not REPEATED) fails its append
// with INVALID_ARGUMENT, and nothing is written.
//
// covers: bigquery.tabledata.list
func TestBigQueryStorageWriteArrowRefusesSchemas(t *testing.T) {
	h := New(t)
	c, _, tables := writeTables(t, h)
	tbl := tables[0]
	raw, err := bqstorage.NewBigQueryWriteClient(h.Context(), storageWriteOptions(h)...)
	if err != nil {
		t.Fatalf("NewBigQueryWriteClient: %v", err)
	}
	defer raw.Close()
	for name, f := range map[string]arrow.Field{
		"a field the table has no column for": {Name: "nope", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		"a TIMESTAMP in nanoseconds":          {Name: "ts", Type: arrow.FixedWidthTypes.Timestamp_ns, Nullable: true},
		"a list for a column not REPEATED":    {Name: "s", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
	} {
		b := array.NewRecordBuilder(memory.DefaultAllocator, arrow.NewSchema([]arrow.Field{{Name: "i", Type: arrow.PrimitiveTypes.Int64}, f}, nil))
		b.Field(0).(*array.Int64Builder).Append(1)
		b.Field(1).AppendNull()
		schema, batch := arrowMessagesOf(t, b.NewRecord())
		ar, err := raw.AppendRows(h.Context())
		if err != nil {
			t.Fatalf("AppendRows: %v", err)
		}
		if err := ar.Send(arrowAppend(writeTablePath(tbl)+"/streams/_default", schema, batch, nil)); err != nil {
			t.Fatalf("send: %v", err)
		}
		resp, err := ar.Recv()
		if err != nil || codes.Code(resp.GetError().GetCode()) != codes.InvalidArgument {
			t.Errorf("%s: %v, %v; want INVALID_ARGUMENT", name, resp, err)
		}
		_ = ar.CloseSend()
	}
	if got := readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read"); len(got) != 0 {
		t.Errorf("rows written: %q", got)
	}
}
