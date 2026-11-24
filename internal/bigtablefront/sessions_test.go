package bigtablefront

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"cloud.google.com/go/bigtable/bttest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// serveFront is serve, with the front itself returned.
func serveFront(t *testing.T) (string, *Front) {
	t.Helper()
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	f, err := New(emu.Addr, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return l.Addr().String(), f
}

// classicCalls counts the calls of method naming resource after seq.
func classicCalls(f *Front, seq uint64, method, resource string) int {
	n := 0
	for _, c := range f.Calls(seq) {
		if c.Method == method && c.Resource == resource {
			n++
		}
	}
	return n
}

// lostWakeup reports whether err is the Go client's own session pool
// giving up on a checkout: cloud.google.com/go/bigtable v1.58.0 can miss
// the wake-up of a caller that reaches a cold pool as its first session
// turns Ready, and leave it parked beside the idle session until its
// deadline (#1824; fixed upstream after v1.58.0 by
// googleapis/google-cloud-go#20832). The error is raised before the
// client sends the vRPC, so the front never saw the call.
//
// A warm pool loses wake-ups too (v1.58.0 and v1.58.1): Session.
// routeVRPCFrame hands a vRPC's result to its caller before its
// onSlotDrained hook puts the session back on its AFE's idle queue and
// signals a waiter. A caller's next call can find the queue still empty
// and, before it parks, miss that signal; it then waits beside the idle
// session until its deadline (main CI after Landing 168, a ReadRow
// straight after a ReadRow).
func lostWakeup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "bigtable: no sessions available")
}

// sessionCall runs one call of the session client with a short deadline,
// and once more if the client lost its own wake-up (lostWakeup): by then
// the session it missed is idle on its queue, so the retry's fast path
// takes it. The retry must succeed, so a front that never answers a vRPC
// (its session stays busy, and the retry waits too) still fails.
func sessionCall(t *testing.T, ctx context.Context, what string, call func(context.Context) error) error {
	t.Helper()
	for try := 0; ; try++ {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := call(cctx)
		cancel()
		if lostWakeup(err) && try == 0 {
			t.Logf("%s: the client lost its session wake-up; retrying once: %v", what, err)
			continue
		}
		return err
	}
}

// TestSessionsServeTheGoClient (#1523): the official client, dialling the
// front itself with CBT_FORCE_SESSION=true, polls GetClientConfiguration
// with sessions_required, is told session_load 1, and sends its point
// reads and writes through OpenTable sessions; they read and write the
// table the classic calls do.
//
// Each session call runs through sessionCall: if the client lost its
// own wake-up (lostWakeup), on the cold pool or a warm one, it runs once
// more, and the retry must succeed, so a front that never opens or never
// answers a session still fails here.
func TestSessionsServeTheGoClient(t *testing.T) {
	t.Setenv("CBT_FORCE_SESSION", "true")
	addr, f := serveFront(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "t", "f"); err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClientWithConfig(ctx, "p", "i", bigtable.ClientConfig{MetricsProvider: bigtable.NoopMetricsProvider{}, DisableDirectAccess: true},
		option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	tbl := c.Open("t")
	name := "projects/p/instances/i/tables/t"
	readRow := func(row string, opts ...bigtable.ReadOption) (r bigtable.Row, err error) {
		err = sessionCall(t, ctx, "ReadRow "+row, func(cctx context.Context) error {
			r, err = tbl.ReadRow(cctx, row, opts...)
			return err
		})
		return r, err
	}
	// Until the client's first poll lands its writes are classic.
	for i := 0; ; i++ {
		seq := lastSeq(f)
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte("v1"))
		err := sessionCall(t, ctx, "Apply", func(cctx context.Context) error { return tbl.Apply(cctx, "r1", m) })
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if classicCalls(f, seq, data+"MutateRow", name) == 0 {
			break
		}
		if i == 100 {
			t.Fatal("the client never sent a write through a session")
		}
		time.Sleep(50 * time.Millisecond)
	}
	seq := lastSeq(f)
	r, err := readRow("r1")
	if err != nil || len(r["f"]) != 1 || string(r["f"][0].Value) != "v1" || r["f"][0].Timestamp != 1000 {
		t.Errorf("ReadRow = %v, %v", r, err)
	}
	if r, err := readRow("missing"); err != nil || r != nil {
		t.Errorf("ReadRow of a missing row = %v, %v, want none", r, err)
	}
	m := bigtable.NewMutation()
	m.Set("f", "q2", bigtable.Timestamp(2000), []byte("v2"))
	if err := sessionCall(t, ctx, "Apply", func(cctx context.Context) error { return tbl.Apply(cctx, "r1", m) }); err != nil {
		t.Fatal(err)
	}
	r, err = readRow("r1", bigtable.RowFilter(bigtable.ColumnFilter("q2")))
	if err != nil || len(r["f"]) != 1 || string(r["f"][0].Value) != "v2" {
		t.Errorf("ReadRow with a filter = %v, %v", r, err)
	}
	if n := classicCalls(f, seq, data+"ReadRows", name) + classicCalls(f, seq, data+"MutateRow", name); n != 0 {
		t.Errorf("%d classic calls after session routing, want none", n)
	}
	_ = c.Close()
	// A session is logged as its handler returns, which the client's
	// Close does not wait for (#1575: under GOMAXPROCS=1 it had not yet returned).
	for !openTableLogged(f) {
		if ctx.Err() != nil {
			t.Fatal("no OpenTable session was logged")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSessionsDefaultClientStaysClassic (#1824): a client without
// CBT_FORCE_SESSION is told Google's default session_load 0, so after its
// configuration poll its point reads and writes stay ReadRows and
// MutateRow, and it opens no session.
func TestSessionsDefaultClientStaysClassic(t *testing.T) {
	addr, f := serveFront(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "t", "f"); err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClientWithConfig(ctx, "p", "i", bigtable.ClientConfig{MetricsProvider: bigtable.NoopMetricsProvider{}, DisableDirectAccess: true},
		option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	for !configPolled(f) {
		if ctx.Err() != nil {
			t.Fatal("the client never polled GetClientConfiguration")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The poll's listeners run as its answer lands; leave them a moment.
	time.Sleep(200 * time.Millisecond)
	seq := lastSeq(f)
	tbl := c.Open("t")
	name := "projects/p/instances/i/tables/t"
	const n = 5
	for i := 0; i < n; i++ {
		m := bigtable.NewMutation()
		m.Set("f", "q", bigtable.Timestamp(1000), []byte("v"))
		if err := tbl.Apply(ctx, "r", m); err != nil {
			t.Fatal(err)
		}
		if r, err := tbl.ReadRow(ctx, "r"); err != nil || len(r["f"]) != 1 {
			t.Fatalf("ReadRow = %v, %v", r, err)
		}
	}
	if w, r := classicCalls(f, seq, data+"MutateRow", name), classicCalls(f, seq, data+"ReadRows", name); w != n || r != n {
		t.Errorf("classic calls = %d MutateRow, %d ReadRows, want %d each", w, r, n)
	}
	_ = c.Close()
	// A session is logged as its handler returns, after the client's Close.
	time.Sleep(200 * time.Millisecond)
	if openTableLogged(f) {
		t.Error("a client without CBT_FORCE_SESSION opened a session")
	}
}

func configPolled(f *Front) bool {
	for _, call := range f.Calls(0) {
		if call.Method == getClientConfiguration {
			return true
		}
	}
	return false
}

func openTableLogged(f *Front) bool {
	for _, call := range f.Calls(0) {
		if call.Method == openTable {
			return true
		}
	}
	return false
}

func lastSeq(f *Front) uint64 {
	calls := f.Calls(0)
	if len(calls) == 0 {
		return 0
	}
	return calls[len(calls)-1].Seq
}

// TestSessionsProtocol drives a session by hand: the handshake, the
// session parameters, a vRPC each way, a permission refused, a
// materialized view's read refused UNIMPLEMENTED.
func TestSessionsProtocol(t *testing.T) {
	addr, _ := serveFront(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := dial(t, addr)
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "t", "f"); err != nil {
		t.Fatal(err)
	}
	d := bigtablepb.NewBigtableClient(conn)
	cfg, err := d.GetClientConfiguration(ctx, &bigtablepb.GetClientConfigurationRequest{InstanceName: "projects/p/instances/i"})
	if err != nil || cfg.GetSessionConfiguration().GetSessionLoad() != 0 {
		t.Fatalf("GetClientConfiguration = %v, %v, want session_load 0 (Google's client default)", cfg, err)
	}
	for _, tc := range []struct {
		flags *bigtablepb.FeatureFlags
		want  float32
	}{
		{&bigtablepb.FeatureFlags{SessionsCompatible: true}, 0},
		{&bigtablepb.FeatureFlags{SessionsCompatible: true, SessionsRequired: true}, 1},
	} {
		b, _ := proto.Marshal(tc.flags)
		mctx := metadata.AppendToOutgoingContext(ctx, featureFlagsHeader, base64.URLEncoding.EncodeToString(b))
		cfg, err := d.GetClientConfiguration(mctx, &bigtablepb.GetClientConfigurationRequest{InstanceName: "projects/p/instances/i"})
		if err != nil || cfg.GetSessionConfiguration().GetSessionLoad() != tc.want {
			t.Errorf("GetClientConfiguration with %v = %v, %v, want session_load %v", tc.flags, cfg, err, tc.want)
		}
	}
	if _, err := d.GetClientConfiguration(ctx, &bigtablepb.GetClientConfigurationRequest{InstanceName: "p/i"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetClientConfiguration of a malformed name = %v", err)
	}

	open := func(perm bigtablepb.OpenTableRequest_Permission) bigtablepb.Bigtable_OpenTableClient {
		s, err := d.OpenTable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := proto.Marshal(&bigtablepb.OpenTableRequest{TableName: "projects/p/instances/i/tables/t", Permission: perm})
		if err := s.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_OpenSession{OpenSession: &bigtablepb.OpenSessionRequest{ProtocolVersion: 1, Payload: payload}}}); err != nil {
			t.Fatal(err)
		}
		if r, err := s.Recv(); err != nil || r.GetOpenSession() == nil {
			t.Fatalf("the open = %v, %v", r, err)
		}
		if r, err := s.Recv(); err != nil || r.GetSessionParameters().GetKeepAlive().AsDuration() <= 0 {
			t.Fatalf("the session parameters = %v, %v", r, err)
		}
		return s
	}
	vrpc := func(s bigtablepb.Bigtable_OpenTableClient, id int64, req *bigtablepb.TableRequest) *bigtablepb.SessionResponse {
		payload, _ := proto.Marshal(req)
		if err := s.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_VirtualRpc{VirtualRpc: &bigtablepb.VirtualRpcRequest{RpcId: id, Payload: payload}}}); err != nil {
			t.Fatal(err)
		}
		for {
			r, err := s.Recv()
			if err != nil {
				t.Fatalf("vRPC %d: %v", id, err)
			}
			if r.GetHeartbeat() == nil {
				return r
			}
		}
	}
	w := open(bigtablepb.OpenTableRequest_PERMISSION_WRITE)
	set := &bigtablepb.Mutation{Mutation: &bigtablepb.Mutation_SetCell_{SetCell: &bigtablepb.Mutation_SetCell{FamilyName: "f", ColumnQualifier: []byte("q"), TimestampMicros: 1000, Value: []byte("v")}}}
	if r := vrpc(w, 1, &bigtablepb.TableRequest{Payload: &bigtablepb.TableRequest_MutateRow{MutateRow: &bigtablepb.SessionMutateRowRequest{Key: []byte("k"), Mutations: []*bigtablepb.Mutation{set}}}}); r.GetVirtualRpc().GetRpcId() != 1 {
		t.Errorf("MutateRow vRPC = %v", r)
	}
	if r := vrpc(w, 2, &bigtablepb.TableRequest{Payload: &bigtablepb.TableRequest_ReadRow{ReadRow: &bigtablepb.SessionReadRowRequest{Key: []byte("k")}}}); r.GetError().GetStatus().GetCode() != int32(codes.PermissionDenied) || r.GetError().GetRpcId() != 2 {
		t.Errorf("ReadRow on a write session = %v, want PERMISSION_DENIED", r)
	}
	if err := w.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_CloseSession{CloseSession: &bigtablepb.CloseSessionRequest{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Recv(); err != io.EOF {
		t.Errorf("after CloseSession = %v, want the stream ended", err)
	}

	rd := open(bigtablepb.OpenTableRequest_PERMISSION_READ)
	r := vrpc(rd, 1, &bigtablepb.TableRequest{Payload: &bigtablepb.TableRequest_ReadRow{ReadRow: &bigtablepb.SessionReadRowRequest{Key: []byte("k")}}})
	var tr bigtablepb.TableResponse
	if err := proto.Unmarshal(r.GetVirtualRpc().GetPayload(), &tr); err != nil {
		t.Fatal(err)
	}
	row := tr.GetReadRow().GetRow()
	if string(row.GetKey()) != "k" || len(row.GetFamilies()) != 1 || string(row.GetFamilies()[0].GetColumns()[0].GetCells()[0].GetValue()) != "v" {
		t.Errorf("ReadRow vRPC = %v", row)
	}

	mv, err := d.OpenMaterializedView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := proto.Marshal(&bigtablepb.OpenMaterializedViewRequest{MaterializedViewName: "projects/p/instances/i/materializedViews/mv", Permission: bigtablepb.OpenMaterializedViewRequest_PERMISSION_READ})
	_ = mv.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_OpenSession{OpenSession: &bigtablepb.OpenSessionRequest{Payload: payload}}})
	_, _ = mv.Recv()
	_, _ = mv.Recv()
	payload, _ = proto.Marshal(&bigtablepb.MaterializedViewRequest{Payload: &bigtablepb.MaterializedViewRequest_ReadRow{ReadRow: &bigtablepb.SessionReadRowRequest{Key: []byte("k")}}})
	_ = mv.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_VirtualRpc{VirtualRpc: &bigtablepb.VirtualRpcRequest{RpcId: 1, Payload: payload}}})
	if r, err := mv.Recv(); err != nil || r.GetError().GetStatus().GetCode() != int32(codes.Unimplemented) {
		t.Errorf("ReadRow of a materialized view = %v, %v, want UNIMPLEMENTED", r, err)
	}

	bad, err := d.OpenTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = bad.Send(&bigtablepb.SessionRequest{Payload: &bigtablepb.SessionRequest_VirtualRpc{VirtualRpc: &bigtablepb.VirtualRpcRequest{}}})
	if _, err := bad.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a session that does not open = %v, want InvalidArgument", err)
	}
}

// TestRowAssemblerJoinsSplitValues: a value split over chunks is one cell.
func TestRowAssemblerJoinsSplitValues(t *testing.T) {
	var a rowAssembler
	fam := func(s string) *bigtablepb.ReadRowsResponse_CellChunk {
		return &bigtablepb.ReadRowsResponse_CellChunk{RowKey: []byte("r"), FamilyName: wrap(s), Qualifier: wrapb("q"), TimestampMicros: 1, Value: []byte("ab"), ValueSize: 4}
	}
	a.add(fam("f"))
	a.add(&bigtablepb.ReadRowsResponse_CellChunk{Value: []byte("cd")})
	a.add(&bigtablepb.ReadRowsResponse_CellChunk{Qualifier: wrapb("q2"), TimestampMicros: 2, Value: []byte("x"), RowStatus: &bigtablepb.ReadRowsResponse_CellChunk_CommitRow{CommitRow: true}})
	if len(a.done) != 1 {
		t.Fatalf("rows = %v", a.done)
	}
	cols := a.done[0].GetFamilies()[0].GetColumns()
	if len(cols) != 2 || string(cols[0].GetCells()[0].GetValue()) != "abcd" || string(cols[1].GetCells()[0].GetValue()) != "x" {
		t.Errorf("row = %v", a.done[0])
	}
}

func wrap(s string) *wrapperspb.StringValue { return wrapperspb.String(s) }
func wrapb(s string) *wrapperspb.BytesValue { return wrapperspb.Bytes([]byte(s)) }
