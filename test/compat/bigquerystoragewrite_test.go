//go:build compat

package compat

import (
	"math/big"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"cloud.google.com/go/bigquery/storage/managedwriter/adapt"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The Storage Write API (#1102), through the official Go clients: the
// managed writer (cloud.google.com/go/bigquery/storage/managedwriter) and
// the raw BigQueryWriteClient. Every stream's rows are read back with the
// Go client's Table.Read, which reads tabledata.list (#1101 is what lets
// it read a TIMESTAMP in a RECORD or REPEATED column).

// storageWriteDDL is a table of every column type the Storage Write API
// writes here, with REPEATED columns and RECORDs.
const storageWriteDDL = `(i INT64 NOT NULL, f FLOAT64, n NUMERIC, bn BIGNUMERIC, b BOOL, s STRING, y BYTES, d DATE,
 dt DATETIME, tm TIME, ts TIMESTAMP, j JSON, g GEOGRAPHY,
 rec STRUCT<a INT64, ts TIMESTAMP, r ARRAY<STRING>>, r ARRAY<INT64>, rts ARRAY<TIMESTAMP>,
 rr ARRAY<STRUCT<x INT64, y STRING>>)`

// writeTables makes one table of storageWriteDDL in each of two datasets
// of the test's own.
func writeTables(t *testing.T, h *Harness) (*bigquery.Client, string, [2]*bigquery.Table) {
	t.Helper()
	c, project := bigqueryClient(t, h)
	one, two := twoDatasets(t, h, c)
	var tables [2]*bigquery.Table
	for i, ds := range []*bigquery.Dataset{one, two} {
		if err := bqRun(h.Context(), c, "CREATE TABLE "+ds.DatasetID+".w "+storageWriteDDL, false); err != nil {
			t.Fatalf("create %s.w: %v", ds.DatasetID, err)
		}
		tables[i] = ds.Table("w")
	}
	return c, project, tables
}

func storageWriteOptions(h *Harness) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvBigQueryStorage)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

func managedWriter(t *testing.T, h *Harness, project string, opts ...option.ClientOption) *managedwriter.Client {
	t.Helper()
	mw, err := managedwriter.NewClient(h.Context(), project, append(storageWriteOptions(h), opts...)...)
	if err != nil {
		t.Fatalf("managedwriter.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = mw.Close() })
	return mw
}

func writeTablePath(tbl *bigquery.Table) string {
	return managedwriter.TableParentFromParts(tbl.ProjectID, tbl.DatasetID, tbl.TableID)
}

// writerSchema is the table's row message, as the Go client's own
// documentation makes it: the table's schema converted by adapt and
// normalized into one self-contained descriptor.
func writerSchema(t *testing.T, h *Harness, tbl *bigquery.Table) (protoreflect.MessageDescriptor, *descriptorpb.DescriptorProto) {
	t.Helper()
	md, err := tbl.Metadata(h.Context())
	if err != nil {
		t.Fatalf("%s metadata: %v", tbl.TableID, err)
	}
	ss, err := adapt.BQSchemaToStorageTableSchema(md.Schema)
	if err != nil {
		t.Fatal(err)
	}
	d, err := adapt.StorageSchemaToProto2Descriptor(ss, "root")
	if err != nil {
		t.Fatal(err)
	}
	m := d.(protoreflect.MessageDescriptor)
	dp, err := adapt.NormalizeDescriptor(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, dp
}

// decimalBytes is BigDecimalByteStringEncoder's form of the decimal text
// at scale (9 for NUMERIC, 38 for BIGNUMERIC): the unscaled value, two's
// complement, little-endian
// (https://cloud.google.com/bigquery/docs/supported-data-types).
func decimalBytes(t *testing.T, text string, scale int64) []byte {
	t.Helper()
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		t.Fatalf("decimal %q", text)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil)))
	n := new(big.Int).Set(r.Num())
	nbytes := len(n.Bytes()) + 1
	if n.Sign() < 0 {
		n.Add(n, new(big.Int).Lsh(big.NewInt(1), uint(8*nbytes)))
	}
	be := n.FillBytes(make([]byte, nbytes))
	le := make([]byte, len(be))
	for i, c := range be {
		le[len(be)-1-i] = c
	}
	return le
}

// Packed DATETIME and TIME, as CivilTimeEncoder writes them.
func packedTime(h, m, s, us int64) int64 { return h<<32 | m<<26 | s<<20 | us }
func packedDateTime(y, mo, d, h, m, s, us int64) int64 {
	return y<<46 | mo<<42 | d<<37 | packedTime(h, m, s, us)
}

// fullRow is a row with every column set, each value in the protocol
// buffer type the supported-data-types page gives for its column (the
// packed forms for DATETIME and TIME, the encoder's bytes for the
// decimals).
func fullRow(t *testing.T, m protoreflect.MessageDescriptor, i int64) []byte {
	t.Helper()
	msg := dynamicpb.NewMessage(m)
	f := func(name string) protoreflect.FieldDescriptor { return m.Fields().ByName(protoreflect.Name(name)) }
	msg.Set(f("i"), protoreflect.ValueOfInt64(i))
	msg.Set(f("f"), protoreflect.ValueOfFloat64(1.5))
	msg.Set(f("n"), protoreflect.ValueOfBytes(decimalBytes(t, "123.456789012", 9)))
	msg.Set(f("bn"), protoreflect.ValueOfBytes(decimalBytes(t, "-2.5", 38)))
	msg.Set(f("b"), protoreflect.ValueOfBool(true))
	msg.Set(f("s"), protoreflect.ValueOfString("x"))
	msg.Set(f("y"), protoreflect.ValueOfBytes([]byte("ab")))
	msg.Set(f("d"), protoreflect.ValueOfInt32(19724)) // 2024-01-02
	msg.Set(f("dt"), protoreflect.ValueOfInt64(packedDateTime(2024, 1, 2, 3, 4, 5, 123456)))
	msg.Set(f("tm"), protoreflect.ValueOfInt64(packedTime(3, 4, 5, 500000)))
	msg.Set(f("ts"), protoreflect.ValueOfInt64(1704164645123456)) // 2024-01-02 03:04:05.123456 UTC
	msg.Set(f("j"), protoreflect.ValueOfString(`{"k":1}`))
	msg.Set(f("g"), protoreflect.ValueOfString("POINT(1 2)"))
	recF := f("rec")
	rec := dynamicpb.NewMessage(recF.Message())
	rec.Set(recF.Message().Fields().ByName("a"), protoreflect.ValueOfInt64(3))
	rec.Set(recF.Message().Fields().ByName("ts"), protoreflect.ValueOfInt64(1577836800000000)) // 2020-01-01
	rl := rec.Mutable(recF.Message().Fields().ByName("r")).List()
	rl.Append(protoreflect.ValueOfString("p"))
	rl.Append(protoreflect.ValueOfString("q"))
	msg.Set(recF, protoreflect.ValueOfMessage(rec))
	r := msg.Mutable(f("r")).List()
	r.Append(protoreflect.ValueOfInt64(7))
	r.Append(protoreflect.ValueOfInt64(8))
	msg.Mutable(f("rts")).List().Append(protoreflect.ValueOfInt64(1609459201500000)) // 2021-01-01 00:00:01.5
	rrF := f("rr")
	e := dynamicpb.NewMessage(rrF.Message())
	e.Set(rrF.Message().Fields().ByName("x"), protoreflect.ValueOfInt64(1))
	e.Set(rrF.Message().Fields().ByName("y"), protoreflect.ValueOfString("a"))
	msg.Mutable(rrF).List().Append(protoreflect.ValueOfMessage(e))
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// idRow is a row with only i set: every other column NULL, every REPEATED
// one empty.
func idRow(t *testing.T, m protoreflect.MessageDescriptor, i int64) []byte {
	t.Helper()
	msg := dynamicpb.NewMessage(m)
	msg.Set(m.Fields().ByName("i"), protoreflect.ValueOfInt64(i))
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The rows fullRow and idRow write, as readRows prints Table.Read's.
const fullRowRead = `1|1.5|30864197253/250000000|-5/2|true|x|[97 98]|2024-01-02|2024-01-02T03:04:05.123456000|03:04:05.500000000|` +
	`2024-01-02 03:04:05.123456 +0000 UTC|{"k":1}|POINT(1 2)|[3 2020-01-01 00:00:00 +0000 UTC [p q]]|[7 8]|` +
	`[2021-01-01 00:00:01.5 +0000 UTC]|[[1 a]]`

func idRowRead(i string) string {
	return i + strings.Repeat("|<nil>", 13) + "|[]|[]|[]"
}

func appendResult(t *testing.T, h *Harness, ms *managedwriter.ManagedStream, rows [][]byte, opts ...managedwriter.AppendOption) (int64, error) {
	t.Helper()
	r, err := ms.AppendRows(h.Context(), rows, opts...)
	if err != nil {
		return 0, err
	}
	return r.GetResult(h.Context())
}

// TestBigQueryStorageWriteDefaultStreamOfEveryType: the managed writer's
// default stream writes a row of every column type, in the protocol buffer
// types BigQuery documents, and a row of NULLs; Table.Read reads each
// value as it was written. The default stream's results carry no offset
// (managedwriter.NoStreamOffset), as storage.proto says. Measured first
// against the pinned emulator: the NUMERIC was stored as 0, the DATETIME
// as 6483-06-17T05:36:51.664 and the TIME as 03:40:39.224, and each
// result had an offset.
func TestBigQueryStorageWriteDefaultStreamOfEveryType(t *testing.T) {
	h := New(t)
	c, project, tables := writeTables(t, h)
	tbl := tables[0]
	m, dp := writerSchema(t, h, tbl)
	mw := managedWriter(t, h, project)
	ms, err := mw.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(tbl)),
		managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(dp))
	if err != nil {
		t.Fatalf("NewManagedStream: %v", err)
	}
	defer ms.Close()
	for _, rows := range [][][]byte{{fullRow(t, m, 1), idRow(t, m, 2)}, {idRow(t, m, 3)}} {
		off, err := appendResult(t, h, ms, rows)
		if err != nil {
			t.Fatalf("AppendRows: %v", err)
		}
		if off != managedwriter.NoStreamOffset {
			t.Errorf("the default stream's result offset: %d, want none", off)
		}
	}
	got := readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read")
	want := []string{fullRowRead, idRowRead("2"), idRowRead("3")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A BYTES value that is not UTF-8 text is an inherited limitation of
	// streamed rows (#1065): UNIMPLEMENTED, and nothing written.
	msg := dynamicpb.NewMessage(m)
	msg.Set(m.Fields().ByName("i"), protoreflect.ValueOfInt64(4))
	msg.Set(m.Fields().ByName("y"), protoreflect.ValueOfBytes([]byte{0, 0xff}))
	b, _ := proto.Marshal(msg)
	if _, err := appendResult(t, h, ms, [][]byte{b}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a non-UTF-8 BYTES value: %v, want UNIMPLEMENTED (#1065)", err)
	}
	if got := readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read"); len(got) != 3 {
		t.Errorf("rows after the refused append: %d, want 3", len(got))
	}
}

// TestBigQueryStorageWriteCommittedStreamsKeepTheirTables: two COMMITTED
// streams, one of each table, appended in turn on the managed writer's
// connections (each request after a connection's first carries no
// write_stream), and two default streams on one multiplexed connection:
// every row lands in its own stream's table. A result's offset is the
// append's first row; an offset already written is ALREADY_EXISTS, one
// past the end OUT_OF_RANGE; Finalize counts the rows, and an append after
// it fails. Measured first against the pinned emulator: the rows after a
// connection's first request landed in another table.
func TestBigQueryStorageWriteCommittedStreamsKeepTheirTables(t *testing.T) {
	h := New(t)
	c, project, tables := writeTables(t, h)
	m, dp := writerSchema(t, h, tables[0])
	mw := managedWriter(t, h, project)
	var streams [2]*managedwriter.ManagedStream
	for i, tbl := range tables {
		ms, err := mw.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(tbl)),
			managedwriter.WithType(managedwriter.CommittedStream), managedwriter.WithSchemaDescriptor(dp))
		if err != nil {
			t.Fatalf("NewManagedStream: %v", err)
		}
		defer ms.Close()
		streams[i] = ms
	}
	for round := int64(0); round < 3; round++ {
		for i, ms := range streams {
			id := 10*int64(i+1) + round
			off, err := appendResult(t, h, ms, [][]byte{idRow(t, m, id)}, managedwriter.WithOffset(round))
			if err != nil || off != round {
				t.Errorf("append of %d at offset %d: offset %d, %v", id, round, off, err)
			}
		}
	}
	if _, err := appendResult(t, h, streams[0], [][]byte{idRow(t, m, 99)}, managedwriter.WithOffset(1)); status.Code(err) != codes.AlreadyExists {
		t.Errorf("an append at an offset already written: %v, want ALREADY_EXISTS", err)
	}
	if _, err := appendResult(t, h, streams[0], [][]byte{idRow(t, m, 99)}, managedwriter.WithOffset(7)); status.Code(err) != codes.OutOfRange {
		t.Errorf("an append past the end: %v, want OUT_OF_RANGE", err)
	}
	if n, err := streams[0].Finalize(h.Context()); err != nil || n != 3 {
		t.Errorf("Finalize: %d rows, %v; want 3", n, err)
	}
	if _, err := appendResult(t, h, streams[0], [][]byte{idRow(t, m, 99)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an append after Finalize: %v, want INVALID_ARGUMENT", err)
	}

	// Two default streams on one multiplexed connection.
	mx := managedWriter(t, h, project, managedwriter.WithMultiplexing())
	for i, tbl := range tables {
		ms, err := mx.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(tbl)),
			managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(dp))
		if err != nil {
			t.Fatalf("NewManagedStream (multiplexed): %v", err)
		}
		defer ms.Close()
		if _, err := appendResult(t, h, ms, [][]byte{idRow(t, m, 10*int64(i+1)+5)}); err != nil {
			t.Errorf("multiplexed append: %v", err)
		}
	}
	for i, tbl := range tables {
		got := readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read")
		base := 10 * (i + 1)
		var want []string
		for _, d := range []int{0, 1, 2, 5} {
			want = append(want, idRowRead(itoa(base+d)))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s.%s:\n%s\nwant\n%s", tbl.DatasetID, tbl.TableID, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

func itoa(i int) string { return big.NewInt(int64(i)).String() }

// TestBigQueryStorageWritePendingStreamCommitsOnce: a PENDING stream's
// rows are not in the table until BatchCommitWriteStreams; a stream not
// finalized is a stream error, and nothing is committed; the committed
// rows are in the table once, and a second commit is
// STREAM_ALREADY_COMMITTED. Measured first against the pinned emulator:
// a second commit wrote the rows again.
func TestBigQueryStorageWritePendingStreamCommitsOnce(t *testing.T) {
	h := New(t)
	c, project, tables := writeTables(t, h)
	tbl := tables[0]
	m, dp := writerSchema(t, h, tbl)
	mw := managedWriter(t, h, project)
	ms, err := mw.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(tbl)),
		managedwriter.WithType(managedwriter.PendingStream), managedwriter.WithSchemaDescriptor(dp))
	if err != nil {
		t.Fatalf("NewManagedStream: %v", err)
	}
	defer ms.Close()
	if off, err := appendResult(t, h, ms, [][]byte{fullRow(t, m, 1), idRow(t, m, 2)}, managedwriter.WithOffset(0)); err != nil || off != 0 {
		t.Fatalf("append: offset %d, %v", off, err)
	}
	if off, err := appendResult(t, h, ms, [][]byte{idRow(t, m, 3)}); err != nil || off != 2 {
		t.Fatalf("append: offset %d, %v; want 2", off, err)
	}
	read := func() []string {
		return readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read")
	}
	if got := read(); len(got) != 0 {
		t.Errorf("rows before the commit: %q", got)
	}
	commit := func() *storagepb.BatchCommitWriteStreamsResponse {
		resp, err := mw.BatchCommitWriteStreams(h.Context(), &storagepb.BatchCommitWriteStreamsRequest{
			Parent: writeTablePath(tbl), WriteStreams: []string{ms.StreamName()}})
		if err != nil {
			t.Fatalf("BatchCommitWriteStreams: %v", err)
		}
		return resp
	}
	if resp := commit(); len(resp.GetStreamErrors()) != 1 || resp.GetCommitTime() != nil {
		t.Errorf("commit of a stream not finalized: %v, want a stream error", resp)
	}
	if n, err := ms.Finalize(h.Context()); err != nil || n != 3 {
		t.Fatalf("Finalize: %d, %v", n, err)
	}
	if resp := commit(); len(resp.GetStreamErrors()) != 0 || resp.GetCommitTime() == nil {
		t.Fatalf("commit: %v", resp)
	}
	if resp := commit(); len(resp.GetStreamErrors()) != 1 || resp.GetStreamErrors()[0].GetCode() != storagepb.StorageError_STREAM_ALREADY_COMMITTED {
		t.Errorf("a second commit: %v, want STREAM_ALREADY_COMMITTED", resp)
	}
	want := []string{fullRowRead, idRowRead("2"), idRowRead("3")}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	ws, err := mw.GetWriteStream(h.Context(), &storagepb.GetWriteStreamRequest{Name: ms.StreamName()})
	if err != nil || ws.GetCommitTime() == nil || ws.GetType() != storagepb.WriteStream_PENDING {
		t.Errorf("GetWriteStream after the commit: %v, %v", ws, err)
	}
}

// TestBigQueryStorageWriteBufferedStreamFlushesOnce: a BUFFERED stream's
// rows are in the table up to each FlushRows offset, each once. Measured
// first against the pinned emulator: FlushRows past the rows panicked it,
// and every dataset of the instance was gone.
func TestBigQueryStorageWriteBufferedStreamFlushesOnce(t *testing.T) {
	h := New(t)
	c, project, tables := writeTables(t, h)
	tbl := tables[0]
	m, dp := writerSchema(t, h, tbl)
	mw := managedWriter(t, h, project)
	ms, err := mw.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(tbl)),
		managedwriter.WithType(managedwriter.BufferedStream), managedwriter.WithSchemaDescriptor(dp))
	if err != nil {
		t.Fatalf("NewManagedStream: %v", err)
	}
	defer ms.Close()
	if _, err := appendResult(t, h, ms, [][]byte{idRow(t, m, 1), idRow(t, m, 2), idRow(t, m, 3)}); err != nil {
		t.Fatalf("append: %v", err)
	}
	read := func() []string {
		return readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read")
	}
	if got := read(); len(got) != 0 {
		t.Errorf("rows before a flush: %q", got)
	}
	for _, step := range []struct {
		offset int64
		want   []string
	}{
		{0, []string{idRowRead("1")}},
		{0, []string{idRowRead("1")}},
		{2, []string{idRowRead("1"), idRowRead("2"), idRowRead("3")}},
	} {
		if off, err := ms.FlushRows(h.Context(), step.offset); err != nil || off != step.offset {
			t.Fatalf("FlushRows(%d): %d, %v", step.offset, off, err)
		}
		if got := read(); !reflect.DeepEqual(got, step.want) {
			t.Errorf("after FlushRows(%d): %q, want %q", step.offset, got, step.want)
		}
	}
	if _, err := ms.FlushRows(h.Context(), 3); status.Code(err) != codes.OutOfRange {
		t.Errorf("FlushRows past the rows: %v, want OUT_OF_RANGE", err)
	}
}

// TestBigQueryStorageWriteRefusesMalformedRequests: with the raw
// BigQueryWriteClient, each request that panicked the pinned emulator (and
// emptied the instance) when passed to it, measured, is refused by the
// front with INVALID_ARGUMENT, OUT_OF_RANGE or NOT_FOUND, and the
// instance keeps its data: AppendRows naming no stream, or without a
// writer schema, or rows; a FIXED64 field (a kind no INTEGER column takes,
// per the supported-data-types page); FlushRows with no offset or past the
// rows; and CreateReadSession of a table in a project the instance does
// not have.
func TestBigQueryStorageWriteRefusesMalformedRequests(t *testing.T) {
	h := New(t)
	c, project, tables := writeTables(t, h)
	tbl := tables[0]
	m, dp := writerSchema(t, h, tbl)
	raw, err := bqstorage.NewBigQueryWriteClient(h.Context(), storageWriteOptions(h)...)
	if err != nil {
		t.Fatalf("NewBigQueryWriteClient: %v", err)
	}
	defer raw.Close()
	ws, err := raw.CreateWriteStream(h.Context(), &storagepb.CreateWriteStreamRequest{Parent: writeTablePath(tbl),
		WriteStream: &storagepb.WriteStream{Type: storagepb.WriteStream_BUFFERED}})
	if err != nil {
		t.Fatalf("CreateWriteStream: %v", err)
	}
	if len(ws.GetTableSchema().GetFields()) != 17 {
		t.Errorf("CreateWriteStream's table_schema: %v", ws.GetTableSchema())
	}
	rows := &storagepb.ProtoRows{SerializedRows: [][]byte{idRow(t, m, 1)}}
	fixed := &descriptorpb.DescriptorProto{Name: proto.String("r"), Field: []*descriptorpb.FieldDescriptorProto{{
		Name: proto.String("i"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_FIXED64.Enum(),
		Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}}}
	for _, tc := range []struct {
		name string
		req  *storagepb.AppendRowsRequest
		code codes.Code
		// inAnswer: the append is answered with the error; otherwise the
		// connection ends with it.
		inAnswer bool
	}{
		{"no write_stream", &storagepb.AppendRowsRequest{Rows: &storagepb.AppendRowsRequest_ProtoRows{ProtoRows: &storagepb.AppendRowsRequest_ProtoData{
			WriterSchema: &storagepb.ProtoSchema{ProtoDescriptor: dp}, Rows: rows}}}, codes.InvalidArgument, false},
		{"no writer_schema", &storagepb.AppendRowsRequest{WriteStream: ws.GetName(), Rows: &storagepb.AppendRowsRequest_ProtoRows{
			ProtoRows: &storagepb.AppendRowsRequest_ProtoData{Rows: rows}}}, codes.InvalidArgument, false},
		{"no rows", &storagepb.AppendRowsRequest{WriteStream: ws.GetName()}, codes.InvalidArgument, false},
		{"a FIXED64 field", &storagepb.AppendRowsRequest{WriteStream: ws.GetName(), Rows: &storagepb.AppendRowsRequest_ProtoRows{
			ProtoRows: &storagepb.AppendRowsRequest_ProtoData{WriterSchema: &storagepb.ProtoSchema{ProtoDescriptor: fixed}, Rows: rows}}},
			codes.InvalidArgument, true},
	} {
		ar, err := raw.AppendRows(h.Context())
		if err != nil {
			t.Fatalf("AppendRows: %v", err)
		}
		if err := ar.Send(tc.req); err != nil {
			t.Fatalf("%s: send: %v", tc.name, err)
		}
		resp, err := ar.Recv()
		got := status.Code(err)
		if tc.inAnswer && err == nil {
			got = codes.Code(resp.GetError().GetCode())
		}
		if got != tc.code || tc.inAnswer != (err == nil) {
			t.Errorf("%s: %v, %v; want %s", tc.name, resp, err, tc.code)
		}
		_ = ar.CloseSend()
	}
	if _, err := raw.FlushRows(h.Context(), &storagepb.FlushRowsRequest{WriteStream: ws.GetName()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("FlushRows with no offset: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := raw.FlushRows(h.Context(), &storagepb.FlushRowsRequest{WriteStream: ws.GetName(), Offset: wrapperspb.Int64(5)}); status.Code(err) != codes.OutOfRange {
		t.Errorf("FlushRows past the rows: %v, want OUT_OF_RANGE", err)
	}
	if _, err := raw.CreateWriteStream(h.Context(), &storagepb.CreateWriteStreamRequest{Parent: writeTablePath(tbl),
		WriteStream: &storagepb.WriteStream{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateWriteStream of no type: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := raw.GetWriteStream(h.Context(), &storagepb.GetWriteStreamRequest{Name: writeTablePath(tbl) + "/streams/nosuch"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetWriteStream of no stream: %v, want NOT_FOUND", err)
	}
	rc, err := bqstorage.NewBigQueryReadClient(h.Context(), storageWriteOptions(h)...)
	if err != nil {
		t.Fatalf("NewBigQueryReadClient: %v", err)
	}
	defer rc.Close()
	_, err = rc.CreateReadSession(h.Context(), &storagepb.CreateReadSessionRequest{Parent: "projects/" + project,
		ReadSession:    &storagepb.ReadSession{Table: "projects/cb-no-such-project/datasets/d/tables/t", DataFormat: storagepb.DataFormat_ARROW},
		MaxStreamCount: 1})
	if status.Code(err) != codes.NotFound {
		t.Errorf("CreateReadSession in a project the instance does not have: %v, want NOT_FOUND", err)
	}
	// The instance kept its data.
	if _, err := c.Dataset(tbl.DatasetID).Table(tbl.TableID).Metadata(h.Context()); err != nil {
		t.Errorf("the table after the refused requests: %v", err)
	}
}

// TestBigQueryRESTReadOfNestedTimestamps (#1101): the Go client's
// Table.Read (tabledata.list) and a query's rows (jobs.getQueryResults)
// read a TIMESTAMP in a RECORD and in a REPEATED column, with every other
// type at depth, as it was written. Measured first against the pinned
// emulator: Table.Read failed "strconv.ParseInt: parsing \"2020-01-01
// 00:00:00+00\": invalid syntax", the nested TIMESTAMP being the engine's
// text where BigQuery's REST API gives microseconds.
func TestBigQueryRESTReadOfNestedTimestamps(t *testing.T) {
	h := New(t)
	c, _, tbl, _ := storageReadTypesTable(t, h) // bigquerystorageread_test.go
	want := []string{
		`1|1.5|30864197253/250000000|-5/2|true|x|[0 255 97 98]|2024-01-02|2024-01-02T03:04:05.123456000|03:04:05.500000000|` +
			`2024-01-02 03:04:05.123456 +0000 UTC|{"k":1}|[3 2020-01-01 00:00:00 +0000 UTC [p q]]|[7 8]|` +
			`[2021-01-01 00:00:01.5 +0000 UTC]|[[1 a [0.5 -1]] [2 <nil> []]]`,
		`2|-2.25|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|[9]|[]|[]`,
		`3|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|<nil>|[9]|[]|[]`,
	}
	if got := readRows(t, c.Dataset(tbl.DatasetID).Table(tbl.TableID).Read(h.Context()), "Table.Read"); !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	it, err := c.Query("SELECT * FROM " + tbl.DatasetID + "." + tbl.TableID).Read(h.Context())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := readRows(t, it, "the query's rows"); !reflect.DeepEqual(got, want) {
		t.Errorf("the query's rows:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A query's own nested TIMESTAMPs, one before the epoch.
	it, err = c.Query("SELECT STRUCT(TIMESTAMP '2020-01-01 00:00:00.25 UTC' AS ts) AS rec, [TIMESTAMP '1969-12-31 23:59:59.5 UTC'] AS rts").Read(h.Context())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := readRows(t, it, "the literal query"); !reflect.DeepEqual(got, []string{"[2020-01-01 00:00:00.25 +0000 UTC]|[1969-12-31 23:59:59.5 +0000 UTC]"}) {
		t.Errorf("the literal query: %q", got)
	}
}
