package bigtablefront

import (
	"context"
	"errors"
	"io"
	"regexp"
	"sync"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// The session data plane (#1523): GetClientConfiguration, OpenTable,
// OpenAuthorizedView and OpenMaterializedView. Google documents each as
// "only intended to be used by the official Cloud Bigtable client libraries
// to implement the Bigtable Session based protocol" (google/bigtable/v2/
// bigtable.proto). The Go client (cloud.google.com/go/bigtable, since
// v1.50) builds a session client whenever it dials an endpoint itself (not
// through BIGTABLE_EMULATOR_HOST, which hands it a dialled connection),
// polls GetClientConfiguration, and sends the share of its ReadRow and
// Apply calls the configuration's session_load names through sessions
// instead of ReadRows and MutateRow.
//
// The front answers a configuration with session_load 1, so such a client
// sends every point read and write through a session, and serves the
// sessions: the OpenSession handshake, the session parameters (the
// heartbeat interval), heartbeats while a vRPC runs, and each vRPC (V1:
// one at a time per session, ReadRow and MutateRow) as the classic call it
// is, run by the front as a call of its own: so an authorized view's
// subset and a table's change log apply as they do to ReadRows and
// MutateRow. A materialized view's sessions open, but their ReadRow is
// refused UNIMPLEMENTED as ReadRows naming a materialized view is (its
// rows are read with SQL, sqlviews.go), and on UNIMPLEMENTED the client
// reads it the classic way.

const (
	getClientConfiguration = data + "GetClientConfiguration"
	openTable              = data + "OpenTable"
	openAuthorizedView     = data + "OpenAuthorizedView"
	openMaterializedView   = data + "OpenMaterializedView"
)

// sessionKeepAlive is the longest the front lets a session go without a
// message while a vRPC runs: it sends a heartbeat at half of it.
const sessionKeepAlive = 2 * time.Second

var (
	sessionInstanceRe = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+$`)
	sessionTableRe    = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+/tables/[^/]+$`)
	sessionAuthViewRe = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+/tables/[^/]+/authorizedViews/[^/]+$`)
	sessionMatViewRe  = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+/materializedViews/[^/]+$`)
)

// clientConfiguration is the configuration the front gives every client:
// every point call through a session, a small pool (one server to talk
// to), and a poll every five minutes, as Google's default does.
func clientConfiguration() *btpb.ClientConfiguration {
	return &btpb.ClientConfiguration{
		SessionConfiguration: &btpb.SessionClientConfiguration{
			SessionLoad: 1,
			ChannelConfiguration: &btpb.SessionClientConfiguration_ChannelPoolConfiguration{
				MinServerCount: 1, MaxServerCount: 1, PerServerSessionCount: 10,
				Mode: &btpb.SessionClientConfiguration_ChannelPoolConfiguration_CloudPathOnly_{
					CloudPathOnly: &btpb.SessionClientConfiguration_ChannelPoolConfiguration_CloudPathOnly{}},
			},
			SessionPoolConfiguration: &btpb.SessionClientConfiguration_SessionPoolConfiguration{
				Headroom: 0.5, MinSessionCount: 1, MaxSessionCount: 10,
				NewSessionQueueLength: 10, NewSessionCreationBudget: 10,
				NewSessionCreationPenalty:          durationpb.New(time.Second),
				ConsecutiveSessionFailureThreshold: 10,
				LoadBalancingOptions: &btpb.LoadBalancingOptions{LoadBalancingStrategy: &btpb.LoadBalancingOptions_LeastInFlight_{
					LeastInFlight: &btpb.LoadBalancingOptions_LeastInFlight{}}},
			},
		},
		Polling: &btpb.ClientConfiguration_PollingConfiguration_{PollingConfiguration: &btpb.ClientConfiguration_PollingConfiguration{
			PollingInterval:  durationpb.New(5 * time.Minute),
			ValidityDuration: durationpb.New(30 * time.Minute),
			MaxRpcRetryCount: 5,
		}},
	}
}

// sessionMethods are the session data plane's methods.
func (f *Front) sessionMethods() (map[string]method, map[string]func(grpc.ServerStream) error) {
	config := func(_ context.Context, req *btpb.GetClientConfigurationRequest) (proto.Message, error) {
		if !sessionInstanceRe.MatchString(req.GetInstanceName()) {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid instance_name %q: want projects/<project>/instances/<instance>.", req.GetInstanceName())
		}
		return clientConfiguration(), nil
	}
	served := map[string]method{
		getClientConfiguration: unary(func() *btpb.GetClientConfigurationRequest { return &btpb.GetClientConfigurationRequest{} }, config),
	}
	streams := map[string]func(grpc.ServerStream) error{
		openTable:            func(ss grpc.ServerStream) error { return f.serveSession(ss, tableScope) },
		openAuthorizedView:   func(ss grpc.ServerStream) error { return f.serveSession(ss, authViewScope) },
		openMaterializedView: func(ss grpc.ServerStream) error { return f.serveSession(ss, matViewScope) },
	}
	return served, streams
}

type sessionScope int

const (
	tableScope sessionScope = iota
	authViewScope
	matViewScope
)

// session is one open session: what it names and may do.
type session struct {
	scope       sessionScope
	name        string
	appProfile  string
	read, write bool
}

// openSession reads the OpenSession payload of scope.
func openSession(scope sessionScope, payload []byte) (*session, proto.Message, error) {
	bad := func(format string, args ...any) (*session, proto.Message, error) {
		return nil, nil, status.Errorf(codes.InvalidArgument, format, args...)
	}
	switch scope {
	case tableScope:
		var req btpb.OpenTableRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return bad("cannot decode the OpenTableRequest: %v", err)
		}
		if !sessionTableRe.MatchString(req.GetTableName()) {
			return bad("Invalid table_name %q: want projects/<project>/instances/<instance>/tables/<table>.", req.GetTableName())
		}
		p := req.GetPermission()
		return &session{scope: scope, name: req.GetTableName(), appProfile: req.GetAppProfileId(),
			read:  p == btpb.OpenTableRequest_PERMISSION_READ || p == btpb.OpenTableRequest_PERMISSION_READ_WRITE,
			write: p == btpb.OpenTableRequest_PERMISSION_WRITE || p == btpb.OpenTableRequest_PERMISSION_READ_WRITE}, &btpb.OpenTableResponse{}, nil
	case authViewScope:
		var req btpb.OpenAuthorizedViewRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return bad("cannot decode the OpenAuthorizedViewRequest: %v", err)
		}
		if !sessionAuthViewRe.MatchString(req.GetAuthorizedViewName()) {
			return bad("Invalid authorized_view_name %q: want projects/<project>/instances/<instance>/tables/<table>/authorizedViews/<authorized_view>.", req.GetAuthorizedViewName())
		}
		p := req.GetPermission()
		return &session{scope: scope, name: req.GetAuthorizedViewName(), appProfile: req.GetAppProfileId(),
			read:  p == btpb.OpenAuthorizedViewRequest_PERMISSION_READ || p == btpb.OpenAuthorizedViewRequest_PERMISSION_READ_WRITE,
			write: p == btpb.OpenAuthorizedViewRequest_PERMISSION_WRITE || p == btpb.OpenAuthorizedViewRequest_PERMISSION_READ_WRITE}, &btpb.OpenAuthorizedViewResponse{}, nil
	default:
		var req btpb.OpenMaterializedViewRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return bad("cannot decode the OpenMaterializedViewRequest: %v", err)
		}
		if !sessionMatViewRe.MatchString(req.GetMaterializedViewName()) {
			return bad("Invalid materialized_view_name %q: want projects/<project>/instances/<instance>/materializedViews/<materialized_view>.", req.GetMaterializedViewName())
		}
		return &session{scope: scope, name: req.GetMaterializedViewName(), appProfile: req.GetAppProfileId(),
			read: req.GetPermission() == btpb.OpenMaterializedViewRequest_PERMISSION_READ}, &btpb.OpenMaterializedViewResponse{}, nil
	}
}

// serveSession serves one session stream.
func (f *Front) serveSession(ss grpc.ServerStream, scope sessionScope) error {
	var first btpb.SessionRequest
	if err := ss.RecvMsg(&first); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	open := first.GetOpenSession()
	if open == nil {
		return status.Error(codes.InvalidArgument, "the first message of a session must be an OpenSessionRequest")
	}
	sess, resp, err := openSession(scope, open.GetPayload())
	if err != nil {
		return err
	}
	if md, ok := metadata.FromIncomingContext(ss.Context()); ok {
		f.sawIf(f.seen.seeMetadata(md))
	}
	payload, err := proto.Marshal(resp)
	if err != nil {
		return status.Errorf(codes.Internal, "encoding the open response: %v", err)
	}
	var sendMu sync.Mutex
	send := func(r *btpb.SessionResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return ss.SendMsg(r)
	}
	if err := send(&btpb.SessionResponse{Payload: &btpb.SessionResponse_OpenSession{OpenSession: &btpb.OpenSessionResponse{Payload: payload}}}); err != nil {
		return err
	}
	if err := send(&btpb.SessionResponse{Payload: &btpb.SessionResponse_SessionParameters{
		SessionParameters: &btpb.SessionParametersResponse{KeepAlive: durationpb.New(sessionKeepAlive)}}}); err != nil {
		return err
	}
	for {
		var req btpb.SessionRequest
		if err := ss.RecvMsg(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch p := req.GetPayload().(type) {
		case *btpb.SessionRequest_CloseSession:
			return nil
		case *btpb.SessionRequest_OpenSession:
			return status.Error(codes.InvalidArgument, "a session is opened once")
		case *btpb.SessionRequest_VirtualRpc:
			if err := f.serveVRPC(ss.Context(), sess, p.VirtualRpc, send); err != nil {
				return err
			}
		}
	}
}

// serveVRPC runs one vRPC, sending heartbeats while it runs, and answers
// its result or its error. Only a failure to send ends the session.
func (f *Front) serveVRPC(ctx context.Context, sess *session, v *btpb.VirtualRpcRequest, send func(*btpb.SessionResponse) error) error {
	if d := v.GetDeadline(); d != nil && d.AsDuration() > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.AsDuration())
		defer cancel()
	}
	done := make(chan struct{})
	beats := make(chan error, 1)
	go func() {
		t := time.NewTicker(sessionKeepAlive / 2)
		defer t.Stop()
		for {
			select {
			case <-done:
				beats <- nil
				return
			case <-t.C:
				if err := send(&btpb.SessionResponse{Payload: &btpb.SessionResponse_Heartbeat{Heartbeat: &btpb.HeartbeatResponse{}}}); err != nil {
					beats <- err
					return
				}
			}
		}
	}()
	payload, err := f.runVRPC(ctx, sess, v.GetPayload())
	close(done)
	if err := <-beats; err != nil {
		return err
	}
	if err != nil {
		st := status.Convert(err)
		return send(&btpb.SessionResponse{Payload: &btpb.SessionResponse_Error{Error: &btpb.ErrorResponse{RpcId: v.GetRpcId(), Status: st.Proto()}}})
	}
	return send(&btpb.SessionResponse{Payload: &btpb.SessionResponse_VirtualRpc{VirtualRpc: &btpb.VirtualRpcResponse{RpcId: v.GetRpcId(), Payload: payload}}})
}

// runVRPC decodes a vRPC's payload for the session's scope, runs it and
// encodes its answer.
func (f *Front) runVRPC(ctx context.Context, sess *session, payload []byte) ([]byte, error) {
	var read *btpb.SessionReadRowRequest
	var mutate *btpb.SessionMutateRowRequest
	switch sess.scope {
	case tableScope:
		var req btpb.TableRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "cannot decode the TableRequest: %v", err)
		}
		read, mutate = req.GetReadRow(), req.GetMutateRow()
	case authViewScope:
		var req btpb.AuthorizedViewRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "cannot decode the AuthorizedViewRequest: %v", err)
		}
		read, mutate = req.GetReadRow(), req.GetMutateRow()
	default:
		var req btpb.MaterializedViewRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "cannot decode the MaterializedViewRequest: %v", err)
		}
		if req.GetReadRow() != nil {
			return nil, status.Error(codes.Unimplemented, "ReadRow of a materialized view is not served here: a materialized view is not stored rows, its rows are read with SQL (ExecuteQuery), as for ReadRows")
		}
	}
	var out proto.Message
	switch {
	case read != nil:
		if !sess.read {
			return nil, status.Errorf(codes.PermissionDenied, "the session on %s was not opened to read", sess.name)
		}
		row, err := f.readRow(ctx, sess, read)
		if err != nil {
			return nil, err
		}
		rr := &btpb.SessionReadRowResponse{Row: row}
		if sess.scope == tableScope {
			out = &btpb.TableResponse{Payload: &btpb.TableResponse_ReadRow{ReadRow: rr}}
		} else {
			out = &btpb.AuthorizedViewResponse{Payload: &btpb.AuthorizedViewResponse_ReadRow{ReadRow: rr}}
		}
	case mutate != nil:
		if !sess.write {
			return nil, status.Errorf(codes.PermissionDenied, "the session on %s was not opened to write", sess.name)
		}
		if err := f.mutateRow(ctx, sess, mutate); err != nil {
			return nil, err
		}
		mr := &btpb.SessionMutateRowResponse{}
		if sess.scope == tableScope {
			out = &btpb.TableResponse{Payload: &btpb.TableResponse_MutateRow{MutateRow: mr}}
		} else {
			out = &btpb.AuthorizedViewResponse{Payload: &btpb.AuthorizedViewResponse_MutateRow{MutateRow: mr}}
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "the vRPC names no operation")
	}
	b, err := proto.Marshal(out)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding the vRPC response: %v", err)
	}
	return b, nil
}

// readRow is ReadRows of the one key, run through the front.
func (f *Front) readRow(ctx context.Context, sess *session, r *btpb.SessionReadRowRequest) (*btpb.Row, error) {
	if len(r.GetKey()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ReadRow requires a row key")
	}
	req := &btpb.ReadRowsRequest{AppProfileId: sess.appProfile, Rows: &btpb.RowSet{RowKeys: [][]byte{r.GetKey()}}, Filter: r.GetFilter(), RowsLimit: 1}
	if sess.scope == authViewScope {
		req.AuthorizedViewName = sess.name
	} else {
		req.TableName = sess.name
	}
	in, err := proto.Marshal(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding ReadRows: %v", err)
	}
	frames, err := f.innerCall(ctx, data+"ReadRows", in)
	if err != nil {
		return nil, err
	}
	var rows rowAssembler
	for _, fr := range frames {
		var resp btpb.ReadRowsResponse
		if err := proto.Unmarshal(fr, &resp); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding ReadRows: %v", err)
		}
		for _, ch := range resp.GetChunks() {
			rows.add(ch)
		}
	}
	if len(rows.done) == 0 {
		return nil, nil
	}
	return rows.done[0], nil
}

// mutateRow is MutateRow, run through the front.
func (f *Front) mutateRow(ctx context.Context, sess *session, r *btpb.SessionMutateRowRequest) error {
	req := &btpb.MutateRowRequest{AppProfileId: sess.appProfile, RowKey: r.GetKey(), Mutations: r.GetMutations()}
	if sess.scope == authViewScope {
		req.AuthorizedViewName = sess.name
	} else {
		req.TableName = sess.name
	}
	in, err := proto.Marshal(req)
	if err != nil {
		return status.Errorf(codes.Internal, "encoding MutateRow: %v", err)
	}
	_, err = f.innerCall(ctx, data+"MutateRow", in)
	return err
}

// innerCall runs one call of the data API through the front's own handler, as
// if a client had made it, and returns the messages it answered.
func (f *Front) innerCall(ctx context.Context, method string, in []byte) ([]frame, error) {
	s := &innerStream{ctx: grpc.NewContextWithServerTransportStream(ctx, innerMethod(method)), in: in}
	if err := f.handle(nil, s); err != nil {
		return nil, err
	}
	return s.out, nil
}

// innerMethod names the method of an inner call.
type innerMethod string

func (m innerMethod) Method() string             { return string(m) }
func (innerMethod) SetHeader(metadata.MD) error  { return nil }
func (innerMethod) SendHeader(metadata.MD) error { return nil }
func (innerMethod) SetTrailer(metadata.MD) error { return nil }

// innerStream is the server stream of an inner call: one request in,
// every response kept.
type innerStream struct {
	ctx  context.Context
	in   []byte
	sent bool
	out  []frame
}

func (s *innerStream) SetHeader(metadata.MD) error  { return nil }
func (s *innerStream) SendHeader(metadata.MD) error { return nil }
func (s *innerStream) SetTrailer(metadata.MD)       {}
func (s *innerStream) Context() context.Context     { return s.ctx }

func (s *innerStream) SendMsg(m any) error {
	b, err := rawCodec{}.Marshal(m)
	if err != nil {
		return err
	}
	s.out = append(s.out, append(frame(nil), b...))
	return nil
}

func (s *innerStream) RecvMsg(m any) error {
	if s.sent {
		return io.EOF
	}
	s.sent = true
	return rawCodec{}.Unmarshal(s.in, m)
}

// rowAssembler puts ReadRows chunks together into rows.
type rowAssembler struct {
	done []*btpb.Row
	cur  *btpb.Row
	fam  *btpb.Family
	col  *btpb.Column
	cell *btpb.Cell
}

func (a *rowAssembler) add(ch *btpb.ReadRowsResponse_CellChunk) {
	if ch.GetResetRow() {
		a.cur, a.fam, a.col, a.cell = nil, nil, nil, nil
		return
	}
	if a.cell == nil {
		if a.cur == nil || len(ch.GetRowKey()) > 0 && string(ch.GetRowKey()) != string(a.cur.GetKey()) {
			a.cur, a.fam, a.col = &btpb.Row{Key: append([]byte(nil), ch.GetRowKey()...)}, nil, nil
		}
		if fn := ch.GetFamilyName(); fn != nil && (a.fam == nil || a.fam.Name != fn.GetValue()) {
			a.fam = &btpb.Family{Name: fn.GetValue()}
			a.cur.Families = append(a.cur.Families, a.fam)
			a.col = nil
		}
		if q := ch.GetQualifier(); q != nil && (a.col == nil || string(a.col.Qualifier) != string(q.GetValue())) {
			a.col = &btpb.Column{Qualifier: append([]byte(nil), q.GetValue()...)}
			a.fam.Columns = append(a.fam.Columns, a.col)
		}
		a.cell = &btpb.Cell{TimestampMicros: ch.GetTimestampMicros(), Labels: ch.GetLabels()}
	}
	a.cell.Value = append(a.cell.Value, ch.GetValue()...)
	if ch.GetValueSize() == 0 {
		if a.col != nil {
			a.col.Cells = append(a.col.Cells, a.cell)
		}
		a.cell = nil
	}
	if ch.GetCommitRow() {
		a.done = append(a.done, a.cur)
		a.cur, a.fam, a.col = nil, nil, nil
	}
}
