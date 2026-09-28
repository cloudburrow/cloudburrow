package bigqueryfront

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
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

// fakeTables is the REST front the Write front writes through: tables.get
// of the tables it has, and tabledata.insertAll, which keeps the rows.
type fakeTables struct {
	mu      sync.Mutex
	schemas map[string]string // "ds.t" -> the "fields" JSON
	rows    map[string][]map[string]any
	inserts int
}

func (f *fakeTables) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(r.URL.Path, "/")
	// /bigquery/v2/projects/p-1/datasets/ds/tables/t[/insertAll]
	if len(parts) < 9 || parts[4] != "p-1" {
		writeError(w, 404, "notFound", "Not found: "+r.URL.Path)
		return
	}
	name := parts[6] + "." + parts[8]
	schema, ok := f.schemas[name]
	if !ok {
		writeError(w, 404, "notFound", "Not found: Table "+name)
		return
	}
	if r.Method == http.MethodGet && len(parts) == 9 {
		writeJSON(w, 200, map[string]any{"schema": map[string]any{"fields": json.RawMessage(schema)}})
		return
	}
	if r.Method == http.MethodPost && len(parts) == 10 && parts[9] == "insertAll" {
		var req struct {
			Rows []struct {
				JSON map[string]any `json:"json"`
			} `json:"rows"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			writeError(w, 400, "invalid", err.Error())
			return
		}
		f.inserts++
		for _, row := range req.Rows {
			f.rows[name] = append(f.rows[name], row.JSON)
		}
		writeJSON(w, 200, map[string]any{"kind": "bigquery#tableDataInsertAllResponse"})
		return
	}
	writeError(w, 400, "invalid", "unexpected "+r.Method+" "+r.URL.Path)
}

func (f *fakeTables) ids(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.rows[name] {
		s, _ := r["i"].(string)
		out = append(out, s)
	}
	return out
}

const writeTestFields = `[{"name":"i","type":"INTEGER","mode":"REQUIRED"},{"name":"s","type":"STRING"},` +
	`{"name":"r","type":"INTEGER","mode":"REPEATED"},` +
	`{"name":"rec","type":"RECORD","fields":[{"name":"ts","type":"TIMESTAMP"},{"name":"d","type":"DATE"}]}]`

// writeTestDescriptor is a writer schema of writeTestFields, as the Go
// client's NormalizeDescriptor writes one: proto2, the RECORD nested.
func writeTestDescriptor() *descriptorpb.DescriptorProto {
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	return &descriptorpb.DescriptorProto{
		Name: proto.String("root"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("i"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				Label: descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum()},
			{Name: proto.String("s"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: opt},
			{Name: proto.String("r"), Number: proto.Int32(3), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()},
			{Name: proto.String("rec"), Number: proto.Int32(4), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String("root_rec"), Label: opt},
		},
		NestedType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("root_rec"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ts"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(), Label: opt},
				{Name: proto.String("d"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(), Label: opt},
			},
		}},
	}
}

// storageWriteFront serves the Write front with fake tables ds.t and
// ds.u; it returns a client and the fake.
func storageWriteFront(t *testing.T, ctx context.Context) (storagepb.BigQueryWriteClient, *fakeTables) {
	t.Helper()
	tables := &fakeTables{schemas: map[string]string{"ds.t": writeTestFields, "ds.u": writeTestFields}, rows: map[string][]map[string]any{}}
	c, _ := serveWriteFront(t, ctx, tables, nil)
	return c, tables
}

// serveWriteFront serves the Write front over tables with options wo; it
// returns a client and a function that stops the front, as a restart of
// its container does.
func serveWriteFront(t *testing.T, ctx context.Context, tables *fakeTables, wo *writeOptions) (storagepb.BigQueryWriteClient, func()) {
	t.Helper()
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer() // the emulator: nothing registered, so any call that reaches it is UNIMPLEMENTED
	go func() { _ = gs.Serve(up) }()
	t.Cleanup(gs.Stop)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	go func() { served <- serveStorageRead(ctx, l, up.Addr().String(), &fakeRows{}, nil, tables, wo) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-served; err != nil {
				t.Errorf("serveStorageRead: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return storagepb.NewBigQueryWriteClient(conn), stop
}

func testRow(t *testing.T, i int64, s *string) []byte {
	t.Helper()
	md, err := writerDescriptor(writeTestDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	msg := dynamicpb.NewMessage(md)
	msg.Set(md.Fields().ByName("i"), protoreflect.ValueOfInt64(i))
	if s != nil {
		msg.Set(md.Fields().ByName("s"), protoreflect.ValueOfString(*s))
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// appendConnection is one AppendRows connection: send sends a request and
// returns its answer, or the connection's end.
type appendConnection struct {
	t  *testing.T
	ar storagepb.BigQueryWrite_AppendRowsClient
}

func (a appendConnection) send(req *storagepb.AppendRowsRequest) (*storagepb.AppendRowsResponse, error) {
	if err := a.ar.Send(req); err != nil && err != io.EOF {
		a.t.Fatalf("send: %v", err)
	}
	return a.ar.Recv()
}

func openAppend(t *testing.T, ctx context.Context, c storagepb.BigQueryWriteClient) appendConnection {
	t.Helper()
	ar, err := c.AppendRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.CloseSend() })
	return appendConnection{t: t, ar: ar}
}

func rowsReq(stream string, schema bool, offset *wrapperspb.Int64Value, rows ...[]byte) *storagepb.AppendRowsRequest {
	pd := &storagepb.AppendRowsRequest_ProtoData{Rows: &storagepb.ProtoRows{SerializedRows: rows}}
	if schema {
		pd.WriterSchema = &storagepb.ProtoSchema{ProtoDescriptor: writeTestDescriptor()}
	}
	return &storagepb.AppendRowsRequest{WriteStream: stream, Offset: offset, Rows: &storagepb.AppendRowsRequest_ProtoRows{ProtoRows: pd}}
}

func respCode(resp *storagepb.AppendRowsResponse, err error) codes.Code {
	if err != nil {
		return status.Code(err)
	}
	if e := resp.GetError(); e != nil {
		return codes.Code(e.GetCode())
	}
	return codes.OK
}

// TestStorageWriteStreamsKeepTheirRows: the semantics storagewrite.go
// lists, against a fake REST front: every connection's later requests
// write to its own stream; offsets; the default stream; PENDING rows at
// commit only, atomically and once; BUFFERED rows at flush, once each.
func TestStorageWriteStreamsKeepTheirRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, tables := storageWriteFront(t, ctx)
	create := func(table string, typ storagepb.WriteStream_Type) *storagepb.WriteStream {
		ws, err := c.CreateWriteStream(ctx, &storagepb.CreateWriteStreamRequest{Parent: "projects/p-1/datasets/ds/tables/" + table,
			WriteStream: &storagepb.WriteStream{Type: typ}})
		if err != nil {
			t.Fatalf("CreateWriteStream %s %s: %v", table, typ, err)
		}
		return ws
	}

	// Two COMMITTED streams, one per table, on two connections, whose
	// later requests name no stream.
	st, su := create("t", storagepb.WriteStream_COMMITTED), create("u", storagepb.WriteStream_COMMITTED)
	if len(st.GetTableSchema().GetFields()) != 4 || st.GetCommitTime() == nil {
		t.Errorf("CreateWriteStream: %v", st)
	}
	ct, cu := openAppend(t, ctx, c), openAppend(t, ctx, c)
	for i, step := range []struct {
		conn   appendConnection
		stream string
		first  bool
		id     int64
		offset int64
	}{
		{ct, st.GetName(), true, 1, 0}, {cu, su.GetName(), true, 11, 0}, {ct, "", false, 2, 1}, {cu, "", false, 12, 1},
	} {
		resp, err := step.conn.send(rowsReq(step.stream, step.first, wrapperspb.Int64(step.offset), testRow(t, step.id, nil)))
		if err != nil || resp.GetAppendResult().GetOffset().GetValue() != step.offset || resp.GetError() != nil {
			t.Fatalf("append %d: %v, %v", i, resp, err)
		}
	}
	if got := tables.ids("ds.t"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("ds.t: %v", got)
	}
	if got := tables.ids("ds.u"); !reflect.DeepEqual(got, []string{"11", "12"}) {
		t.Errorf("ds.u: %v", got)
	}
	for off, want := range map[int64]codes.Code{0: codes.AlreadyExists, 1: codes.AlreadyExists, 5: codes.OutOfRange} {
		if got := respCode(ct.send(rowsReq("", false, wrapperspb.Int64(off), testRow(t, 9, nil)))); got != want {
			t.Errorf("append at offset %d of 2: %s, want %s", off, got, want)
		}
	}
	if f, err := c.FinalizeWriteStream(ctx, &storagepb.FinalizeWriteStreamRequest{Name: st.GetName()}); err != nil || f.GetRowCount() != 2 {
		t.Errorf("Finalize: %v, %v", f, err)
	}
	if got := respCode(ct.send(rowsReq("", false, nil, testRow(t, 9, nil)))); got != codes.InvalidArgument {
		t.Errorf("append after Finalize: %s", got)
	}

	// The default stream: no offset in its results, none allowed.
	def := "projects/p-1/datasets/ds/tables/t/streams/_default"
	cd := openAppend(t, ctx, c)
	resp, err := cd.send(rowsReq(def, true, nil, testRow(t, 3, nil)))
	if err != nil || resp.GetError() != nil || resp.GetAppendResult() == nil || resp.GetAppendResult().GetOffset() != nil {
		t.Errorf("default stream append: %v, %v", resp, err)
	}
	if got := respCode(cd.send(rowsReq("", false, wrapperspb.Int64(1), testRow(t, 9, nil)))); got != codes.InvalidArgument {
		t.Errorf("default stream append with an offset: %s", got)
	}
	if _, err := c.FinalizeWriteStream(ctx, &storagepb.FinalizeWriteStreamRequest{Name: def}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Finalize of the default stream: %v", err)
	}
	if ws, err := c.GetWriteStream(ctx, &storagepb.GetWriteStreamRequest{Name: def}); err != nil || ws.GetType() != storagepb.WriteStream_COMMITTED ||
		ws.GetTableSchema() != nil {
		t.Errorf("GetWriteStream of the default stream (BASIC): %v, %v", ws, err)
	}
	if ws, err := c.GetWriteStream(ctx, &storagepb.GetWriteStreamRequest{Name: def, View: storagepb.WriteStreamView_FULL}); err != nil ||
		len(ws.GetTableSchema().GetFields()) != 4 {
		t.Errorf("GetWriteStream of the default stream (FULL): %v, %v", ws, err)
	}

	// PENDING: nothing until the commit; a commit with a stream not
	// finalized commits none; then once.
	p1, p2 := create("u", storagepb.WriteStream_PENDING), create("u", storagepb.WriteStream_PENDING)
	for i, p := range []*storagepb.WriteStream{p1, p2} {
		conn := openAppend(t, ctx, c)
		if got := respCode(conn.send(rowsReq(p.GetName(), true, nil, testRow(t, int64(21+i), nil)))); got != codes.OK {
			t.Fatalf("pending append: %s", got)
		}
	}
	commit := func() *storagepb.BatchCommitWriteStreamsResponse {
		r, err := c.BatchCommitWriteStreams(ctx, &storagepb.BatchCommitWriteStreamsRequest{Parent: "projects/p-1/datasets/ds/tables/u",
			WriteStreams: []string{p1.GetName(), p2.GetName()}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if _, err := c.FinalizeWriteStream(ctx, &storagepb.FinalizeWriteStreamRequest{Name: p1.GetName()}); err != nil {
		t.Fatal(err)
	}
	if r := commit(); len(r.GetStreamErrors()) != 1 || r.GetStreamErrors()[0].GetEntity() != p2.GetName() || r.GetCommitTime() != nil {
		t.Errorf("commit with p2 not finalized: %v", r)
	}
	if got := tables.ids("ds.u"); len(got) != 2 {
		t.Errorf("rows after a refused commit: %v", got)
	}
	if _, err := c.FinalizeWriteStream(ctx, &storagepb.FinalizeWriteStreamRequest{Name: p2.GetName()}); err != nil {
		t.Fatal(err)
	}
	if r := commit(); len(r.GetStreamErrors()) != 0 || r.GetCommitTime() == nil {
		t.Errorf("commit: %v", r)
	}
	if r := commit(); len(r.GetStreamErrors()) != 2 || r.GetStreamErrors()[0].GetCode() != storagepb.StorageError_STREAM_ALREADY_COMMITTED {
		t.Errorf("second commit: %v", r)
	}
	if got := tables.ids("ds.u"); !reflect.DeepEqual(got, []string{"11", "12", "21", "22"}) {
		t.Errorf("ds.u after the commit: %v", got)
	}
	if r, _ := c.BatchCommitWriteStreams(ctx, &storagepb.BatchCommitWriteStreamsRequest{Parent: "projects/p-1/datasets/ds/tables/u",
		WriteStreams: []string{su.GetName()}}); len(r.GetStreamErrors()) != 1 || r.GetStreamErrors()[0].GetCode() != storagepb.StorageError_INVALID_STREAM_TYPE {
		t.Errorf("commit of a COMMITTED stream: %v", r)
	}

	// BUFFERED: rows up to each flush, once each.
	b := create("t", storagepb.WriteStream_BUFFERED)
	cb := openAppend(t, ctx, c)
	if got := respCode(cb.send(rowsReq(b.GetName(), true, nil, testRow(t, 31, nil), testRow(t, 32, nil), testRow(t, 33, nil)))); got != codes.OK {
		t.Fatalf("buffered append: %s", got)
	}
	for _, step := range []struct {
		off  int64
		code codes.Code
		want []string
	}{
		{0, codes.OK, []string{"1", "2", "3", "31"}},
		{0, codes.OK, []string{"1", "2", "3", "31"}},
		{2, codes.OK, []string{"1", "2", "3", "31", "32", "33"}},
		{3, codes.OutOfRange, []string{"1", "2", "3", "31", "32", "33"}},
	} {
		r, err := c.FlushRows(ctx, &storagepb.FlushRowsRequest{WriteStream: b.GetName(), Offset: wrapperspb.Int64(step.off)})
		if status.Code(err) != step.code || err == nil && r.GetOffset() != step.off {
			t.Errorf("FlushRows(%d): %v, %v", step.off, r, err)
		}
		if got := tables.ids("ds.t"); !reflect.DeepEqual(got, step.want) {
			t.Errorf("ds.t after FlushRows(%d): %v", step.off, got)
		}
	}
	if _, err := c.FlushRows(ctx, &storagepb.FlushRowsRequest{WriteStream: st.GetName(), Offset: wrapperspb.Int64(0)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("FlushRows of a COMMITTED stream: %v", err)
	}
}

// TestStorageWriteRefusesMalformedRequests: what the emulator panicked on,
// and the rest storage.proto requires, is refused before any row is
// written; a bad row fails its whole append with row_errors.
func TestStorageWriteRefusesMalformedRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, tables := storageWriteFront(t, ctx)
	ws, err := c.CreateWriteStream(ctx, &storagepb.CreateWriteStreamRequest{Parent: "projects/p-1/datasets/ds/tables/t",
		WriteStream: &storagepb.WriteStream{Type: storagepb.WriteStream_COMMITTED}})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		req  *storagepb.AppendRowsRequest
		code codes.Code
	}{
		"no write_stream":    {rowsReq("", true, nil, testRow(t, 1, nil)), codes.InvalidArgument},
		"no writer_schema":   {rowsReq(ws.GetName(), false, nil, testRow(t, 1, nil)), codes.InvalidArgument},
		"no rows":            {&storagepb.AppendRowsRequest{WriteStream: ws.GetName()}, codes.InvalidArgument},
		"arrow rows":         {&storagepb.AppendRowsRequest{WriteStream: ws.GetName(), Rows: &storagepb.AppendRowsRequest_ArrowRows{}}, codes.Unimplemented},
		"a stream not made":  {rowsReq("projects/p-1/datasets/ds/tables/t/streams/x", true, nil, testRow(t, 1, nil)), codes.NotFound},
		"a table not there":  {rowsReq("projects/p-1/datasets/ds/tables/gone/streams/_default", true, nil, testRow(t, 1, nil)), codes.NotFound},
		"a malformed stream": {rowsReq("projects/p-1/streams/x", true, nil, testRow(t, 1, nil)), codes.InvalidArgument},
	} {
		if got := respCode(openAppend(t, ctx, c).send(tc.req)); got != tc.code {
			t.Errorf("%s: %s, want %s", name, got, tc.code)
		}
	}

	conn := openAppend(t, ctx, c)
	bad := []byte{0xff, 0xff} // not a message of the schema
	resp, err := conn.send(rowsReq(ws.GetName(), true, nil, testRow(t, 1, nil), bad))
	if respCode(resp, err) != codes.InvalidArgument || len(resp.GetRowErrors()) != 1 || resp.GetRowErrors()[0].GetIndex() != 1 {
		t.Errorf("a row that does not decode: %v, %v", resp, err)
	}
	if got := tables.ids("ds.t"); len(got) != 0 {
		t.Errorf("rows written by a failed append: %v", got)
	}
	// The connection goes on after an append fails.
	if got := respCode(conn.send(rowsReq("", false, nil, testRow(t, 2, nil)))); got != codes.OK {
		t.Errorf("the append after a failed one: %s", got)
	}
	for _, mvi := range []*storagepb.AppendRowsRequest{
		{DefaultMissingValueInterpretation: storagepb.AppendRowsRequest_DEFAULT_VALUE},
		{MissingValueInterpretations: map[string]storagepb.AppendRowsRequest_MissingValueInterpretation{"s": storagepb.AppendRowsRequest_DEFAULT_VALUE}},
	} {
		req := rowsReq("", false, nil, testRow(t, 3, nil))
		req.DefaultMissingValueInterpretation, req.MissingValueInterpretations = mvi.DefaultMissingValueInterpretation, mvi.MissingValueInterpretations
		if got := respCode(conn.send(req)); got != codes.Unimplemented {
			t.Errorf("DEFAULT_VALUE: %s, want UNIMPLEMENTED", got)
		}
	}

	for name, req := range map[string]*storagepb.CreateWriteStreamRequest{
		"no type":    {Parent: "projects/p-1/datasets/ds/tables/t", WriteStream: &storagepb.WriteStream{}},
		"no stream":  {Parent: "projects/p-1/datasets/ds/tables/t"},
		"bad parent": {Parent: "projects/p-1/datasets/ds", WriteStream: &storagepb.WriteStream{Type: storagepb.WriteStream_PENDING}},
	} {
		if _, err := c.CreateWriteStream(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateWriteStream, %s: %v", name, err)
		}
	}
	if _, err := c.CreateWriteStream(ctx, &storagepb.CreateWriteStreamRequest{Parent: "projects/p-1/datasets/ds/tables/gone",
		WriteStream: &storagepb.WriteStream{Type: storagepb.WriteStream_PENDING}}); status.Code(err) != codes.NotFound {
		t.Errorf("CreateWriteStream of a table not there: %v", err)
	}
	if _, err := c.FlushRows(ctx, &storagepb.FlushRowsRequest{WriteStream: ws.GetName()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("FlushRows with no offset: %v", err)
	}
	if _, err := c.BatchCommitWriteStreams(ctx, &storagepb.BatchCommitWriteStreamsRequest{Parent: "projects/p-1/datasets/ds/tables/t"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("BatchCommitWriteStreams of no streams: %v", err)
	}
	if _, err := c.GetWriteStream(ctx, &storagepb.GetWriteStreamRequest{Name: "projects/p-1/datasets/ds/tables/t/streams/x"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetWriteStream of a stream not made: %v", err)
	}
}

// testMessage builds the message descriptor of a writer schema.
func testMessage(t *testing.T, dp *descriptorpb.DescriptorProto) protoreflect.MessageDescriptor {
	t.Helper()
	md, err := writerDescriptor(dp)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// TestStorageWriteValueConversions: each column type's protocol buffer
// types (supported-data-types), written as insertAll's JSON.
func TestStorageWriteValueConversions(t *testing.T) {
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	scalar := func(typ descriptorpb.FieldDescriptorProto_Type) protoreflect.FieldDescriptor {
		md := testMessage(t, &descriptorpb.DescriptorProto{Name: proto.String("m"), Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("v"), Number: proto.Int32(1), Type: typ.Enum(), Label: opt}}})
		return md.Fields().Get(0)
	}
	enumMD := testMessage(t, &descriptorpb.DescriptorProto{Name: proto.String("m"),
		Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("v"), Number: proto.Int32(1),
			Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String("m_E.K"), Label: opt}},
		NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("m_E"), EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("K"), Value: []*descriptorpb.EnumValueDescriptorProto{{Name: proto.String("ZERO"), Number: proto.Int32(0)},
				{Name: proto.String("TWO"), Number: proto.Int32(2)}}}}}}})
	enumFD := enumMD.Fields().Get(0)
	type T = descriptorpb.FieldDescriptorProto_Type
	I64, I32, U32, U64, STR, BYT, DBL, FLT, BOOL := T(3), T(5), T(13), T(4), T(9), T(12), T(1), T(2), T(8)
	for _, tc := range []struct {
		col  string
		fd   protoreflect.FieldDescriptor
		v    protoreflect.Value
		want any
		bad  bool
	}{
		{"INTEGER", scalar(I64), protoreflect.ValueOfInt64(-7), "-7", false},
		{"INTEGER", scalar(U32), protoreflect.ValueOfUint32(7), "7", false},
		{"INTEGER", enumFD, protoreflect.ValueOfEnum(2), "2", false},
		{"STRING", enumFD, protoreflect.ValueOfEnum(2), "TWO", false},
		{"STRING", scalar(STR), protoreflect.ValueOfString("x"), "x", false},
		{"BOOLEAN", scalar(BOOL), protoreflect.ValueOfBool(true), true, false},
		{"BOOLEAN", scalar(I32), protoreflect.ValueOfInt32(0), false, false},
		{"BOOLEAN", scalar(U64), protoreflect.ValueOfUint64(3), true, false},
		{"BYTES", scalar(BYT), protoreflect.ValueOfBytes([]byte("ab")), "YWI=", false},
		{"BYTES", scalar(STR), protoreflect.ValueOfString("ab"), "YWI=", false},
		{"FLOAT", scalar(DBL), protoreflect.ValueOfFloat64(1.5), json.Number("1.5"), false},
		{"FLOAT", scalar(FLT), protoreflect.ValueOfFloat32(0.1), json.Number("0.1"), false},
		{"FLOAT", scalar(DBL), protoreflect.ValueOfFloat64(math.Inf(-1)), "-Infinity", false},
		{"FLOAT", scalar(DBL), protoreflect.ValueOfFloat64(math.NaN()), "NaN", false},
		{"NUMERIC", scalar(BYT), protoreflect.ValueOfBytes([]byte{0x14, 0x1a, 0x99, 0xbe, 0x1c}), "123.456789012", false},
		{"NUMERIC", scalar(BYT), protoreflect.ValueOfBytes([]byte{0x00, 0xd1, 0x97, 0xa6, 0xff}), "-1.5", false},
		{"NUMERIC", scalar(BYT), protoreflect.ValueOfBytes(nil), "0", false},
		{"BIGNUMERIC", scalar(I64), protoreflect.ValueOfInt64(-3), "-3", false},
		{"NUMERIC", scalar(STR), protoreflect.ValueOfString("1.25"), "1.25", false},
		{"NUMERIC", scalar(DBL), protoreflect.ValueOfFloat64(0.5), "0.5", false},
		{"NUMERIC", scalar(DBL), protoreflect.ValueOfFloat64(math.NaN()), nil, true},
		{"DATE", scalar(I32), protoreflect.ValueOfInt32(19724), "2024-01-02", false},
		{"DATE", scalar(I32), protoreflect.ValueOfInt32(-719162), "0001-01-01", false},
		{"DATE", scalar(I32), protoreflect.ValueOfInt32(2932897), nil, true},
		{"DATE", scalar(STR), protoreflect.ValueOfString("2024-01-02"), "2024-01-02", false},
		{"TIME", scalar(I64), protoreflect.ValueOfInt64(3<<32 | 4<<26 | 5<<20 | 500000), "03:04:05.500000", false},
		{"TIME", scalar(I64), protoreflect.ValueOfInt64(24 << 32), nil, true},
		{"DATETIME", scalar(I64), protoreflect.ValueOfInt64(2024<<46 | 1<<42 | 2<<37 | 3<<32 | 4<<26 | 5<<20 | 123456),
			"2024-01-02 03:04:05.123456", false},
		{"DATETIME", scalar(I64), protoreflect.ValueOfInt64(2024<<46 | 2<<42 | 30<<37), nil, true},
		{"DATETIME", scalar(STR), protoreflect.ValueOfString("2024-01-02T03:04:05"), "2024-01-02T03:04:05", false},
		{"TIMESTAMP", scalar(I64), protoreflect.ValueOfInt64(1704164645123456), "2024-01-02 03:04:05.123456 UTC", false},
		{"TIMESTAMP", scalar(I64), protoreflect.ValueOfInt64(-500000), "1969-12-31 23:59:59.500000 UTC", false},
		{"TIMESTAMP", scalar(I64), protoreflect.ValueOfInt64(math.MaxInt64), nil, true},
		{"JSON", scalar(STR), protoreflect.ValueOfString(`{"k":1}`), `{"k":1}`, false},
		{"GEOGRAPHY", scalar(STR), protoreflect.ValueOfString("POINT(1 2)"), "POINT(1 2)", false},
	} {
		got, err := scalarValue(tc.col, tc.fd, tc.v)
		if tc.bad {
			if err == nil {
				t.Errorf("%s from %s %v: %v, want an error", tc.col, tc.fd.Kind(), tc.v, got)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s from %s %v: %#v, %v; want %#v", tc.col, tc.fd.Kind(), tc.v, got, err, tc.want)
		}
	}
}

// TestStorageWriteSchemaMatching: a writer schema's fields against the
// table's columns; presence as proto2's.
func TestStorageWriteSchemaMatching(t *testing.T) {
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	fields := []field{{Name: "a", Type: "INT64"}, {Name: "r", Type: "INTEGER", Mode: "REPEATED"}, {Name: "ts", Type: "TIMESTAMP"},
		{Name: "g", Type: "RANGE"}, {Name: "col-1", Type: "STRING"}}
	one := func(f *descriptorpb.FieldDescriptorProto, nested ...*descriptorpb.DescriptorProto) *descriptorpb.DescriptorProto {
		f.Number = proto.Int32(1)
		return &descriptorpb.DescriptorProto{Name: proto.String("m"), Field: []*descriptorpb.FieldDescriptorProto{f}, NestedType: nested}
	}
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	colName := &descriptorpb.FieldOptions{}
	proto.SetExtension(colName, storagepb.E_ColumnName, "col-1")
	for name, tc := range map[string]struct {
		dp   *descriptorpb.DescriptorProto
		code codes.Code
	}{
		"a column":             {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("A"), Type: i64, Label: opt}), codes.OK},
		"an extra field":       {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("z"), Type: i64, Label: opt}), codes.InvalidArgument},
		"repeated to scalar":   {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("a"), Type: i64, Label: rep}), codes.InvalidArgument},
		"scalar to repeated":   {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("r"), Type: i64, Label: opt}), codes.InvalidArgument},
		"fixed64 to INTEGER":   {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("a"), Type: descriptorpb.FieldDescriptorProto_TYPE_FIXED64.Enum(), Label: opt}), codes.InvalidArgument},
		"string to TIMESTAMP":  {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("ts"), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: opt}), codes.InvalidArgument},
		"a RANGE":              {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("g"), Type: i64, Label: opt}), codes.Unimplemented},
		"column_name":          {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("col_1"), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: opt, Options: colName}), codes.OK},
		"a wrapper to INTEGER": {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("a"), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String("google_protobuf_Int64Value"), Label: opt}, &descriptorpb.DescriptorProto{Name: proto.String("google_protobuf_Int64Value"), Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("value"), Number: proto.Int32(1), Type: i64, Label: opt}}}), codes.OK},
		"a message to INTEGER": {one(&descriptorpb.FieldDescriptorProto{Name: proto.String("a"), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String("x"), Label: opt}, &descriptorpb.DescriptorProto{Name: proto.String("x")}), codes.InvalidArgument},
	} {
		md, err := writerDescriptor(tc.dp)
		if err == nil {
			_, err = newRowConv(md, fields, "")
		}
		if status.Code(err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}

	// Presence: a field with no value is NULL, with a default its default;
	// a repeated one with none an empty array; a wrapper's value unwrapped.
	dp := &descriptorpb.DescriptorProto{Name: proto.String("m"), Field: []*descriptorpb.FieldDescriptorProto{
		{Name: proto.String("a"), Number: proto.Int32(1), Type: i64, Label: opt, DefaultValue: proto.String("4")},
		{Name: proto.String("r"), Number: proto.Int32(2), Type: i64, Label: rep},
		{Name: proto.String("ts"), Number: proto.Int32(3), Type: i64, Label: opt},
		{Name: proto.String("col_1"), Number: proto.Int32(4), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: proto.String("google_protobuf_StringValue"), Label: opt, Options: colName},
	}, NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("google_protobuf_StringValue"), Field: []*descriptorpb.FieldDescriptorProto{
		{Name: proto.String("value"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: opt}}}}}
	md := testMessage(t, dp)
	rc, err := newRowConv(md, fields, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := rc.row(nil)
	if err != nil || !reflect.DeepEqual(got, map[string]any{"a": "4", "r": []any{}, "ts": nil, "col-1": nil}) {
		t.Errorf("an empty row: %v, %v", got, err)
	}
	msg := dynamicpb.NewMessage(md)
	w := dynamicpb.NewMessage(md.Fields().Get(3).Message())
	w.Set(w.Descriptor().Fields().Get(0), protoreflect.ValueOfString("v"))
	msg.Set(md.Fields().Get(3), protoreflect.ValueOfMessage(w))
	msg.Set(md.Fields().Get(0), protoreflect.ValueOfInt64(0))
	b, _ := proto.Marshal(msg)
	if got, err := rc.row(b); err != nil || !reflect.DeepEqual(got, map[string]any{"a": "0", "r": []any{}, "ts": nil, "col-1": "v"}) {
		t.Errorf("a row with a zero and a wrapper: %v, %v", got, err)
	}
}
