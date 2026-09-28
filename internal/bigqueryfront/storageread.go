package bigqueryfront

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The Storage Read API of a table whose ID another dataset has (#1032).
//
// The emulator's Storage Read API reads a session's rows with
// "SELECT <columns> FROM `<tableId>`" (server/storage_handler.go,
// buildQuery), the bare table ID, which its engine resolves to the first
// table of that ID made in any dataset (qualify.go). Measured against the
// pinned image with the official Storage Read client
// (cloud.google.com/go/bigquery/storage/apiv1), with m1.t (s STRING, three
// rows) made first and m2.t (a INT64, two rows) after: a read session of
// m1.t streamed its three rows, and one of m2.t failed in ReadRows,
// "strconv.ParseInt: parsing \"one\": invalid syntax", reading m1.t's rows
// into m2.t's Arrow schema; with both tables of one schema the other
// dataset's rows would be streamed as the table's. The session's schema is
// the right table's: the emulator reads it by project, dataset and table.
//
// So the front serves the Storage Read port too (the Service's 9060; the
// emulator's is the pod's own), passing every call through as raw frames,
// except that CreateReadSession of a table whose ID another dataset has
// (sharedID), and ReadRows of a stream of one, are UNIMPLEMENTED: the
// front does not write Arrow or Avro rows itself, and the emulator's would
// be of another table. Since #1046 such a session is not refused: the
// emulator reads the table through a view of the front's whose ID is its
// own (storagealias.go). ReadRows checks again, since another dataset may
// have made a table of the ID after the session, and then reads through
// the view too. A stream the front did not see made (a session made
// before the front last started) is UNIMPLEMENTED, as the front cannot
// tell its table. The Storage Write API writes by the whole name (the
// emulator's AddTableData, read in its source) and is passed through.

const (
	readService       = "/google.cloud.bigquery.storage.v1.BigQueryRead/"
	createReadSession = readService + "CreateReadSession"
	readRows          = readService + "ReadRows"
	// maxStreams bounds the streams the front remembers; the oldest half
	// is forgotten when it is reached.
	maxStreams = 100000
)

// storageRead is the Storage Read front.
type storageRead struct {
	upstream *grpc.ClientConn
	// rest is the emulator's REST API.
	rest http.Handler
	// shared reports whether a dataset other than dataset has a table of
	// the ID table in project, or whether that cannot be told.
	shared func(ctx context.Context, project, dataset, table string) bool
	// aliasMu is held while a view of a table is looked up or made
	// (storagealias.go).
	aliasMu sync.Mutex

	mu      sync.Mutex
	streams map[string]*readStream
	order   []string
}

// readTable is the table a read stream reads.
type readTable struct{ project, dataset, table string }

// readStream is what the front knows of a read stream it saw made.
type readStream struct {
	// table is the client's table.
	table readTable
	// aliased: the stream reads the table through the front's view of it
	// (storagealias.go).
	aliased bool
	// avro: its session's format is Avro.
	avro bool
	// req is the client's CreateReadSession of a stream that reads the
	// table itself, to make it again for the view when another dataset
	// has made a table of its ID since (#1046).
	req *storagepb.CreateReadSessionRequest
	// via is then the view's stream its rows are read from, once made.
	via string
}

// ServeStorageRead serves the Storage Read front on l until ctx ends:
// every call goes to the emulator's gRPC port at upstream (host:port),
// and the checks read the emulator's REST API through rest.
func ServeStorageRead(ctx context.Context, l net.Listener, upstream string, rest http.Handler) error {
	return serveStorageRead(ctx, l, upstream, rest, nil)
}

// serveStorageRead is ServeStorageRead, with the table IDs the REST front
// keeps (tableIDs, #1063), or nil to keep none.
func serveStorageRead(ctx context.Context, l net.Listener, upstream string, rest http.Handler, ids *tableIDs) error {
	s, err := newStorageRead(upstream, rest, ids)
	if err != nil {
		return err
	}
	defer s.upstream.Close()
	srv := s.server()
	go func() {
		<-ctx.Done()
		srv.Stop()
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

func newStorageRead(upstream string, rest http.Handler, ids *tableIDs) (*storageRead, error) {
	conn, err := grpc.NewClient(upstream,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32), grpc.MaxCallSendMsgSize(math.MaxInt32)))
	if err != nil {
		return nil, fmt.Errorf("the emulator's Storage Read API at %s: %w", upstream, err)
	}
	return &storageRead{upstream: conn, rest: rest, streams: map[string]*readStream{},
		shared: func(ctx context.Context, project, dataset, table string) bool {
			r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://bigquery/", nil)
			if err != nil {
				return true
			}
			f := front{next: rest, base: "/bigquery/v2/projects/" + url.PathEscape(project), ids: ids}
			return f.sharedID(r, dataset, table)
		}}, nil
}

func (s *storageRead) server() *grpc.Server {
	return grpc.NewServer(
		grpc.ForceServerCodec(frameCodec{}),
		grpc.UnknownServiceHandler(s.handle),
		grpc.MaxRecvMsgSize(math.MaxInt32), grpc.MaxSendMsgSize(math.MaxInt32),
	)
}

// handle forwards one call of any method, after the checks above.
func (s *storageRead) handle(_ any, ss grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(ss)
	if !ok {
		return status.Error(codes.Internal, "no method on the stream")
	}
	ctx, cancel := context.WithCancel(ss.Context())
	defer cancel()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = metadata.NewOutgoingContext(ctx, outgoing(md))
	}
	open := func() (grpc.ClientStream, error) {
		return s.upstream.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method,
			grpc.ForceCodec(frameCodec{}))
	}
	switch method {
	case createReadSession:
		return s.createSession(ctx, ss, open)
	case readRows:
		var in rawFrame
		if err := ss.RecvMsg(&in); err != nil {
			return err
		}
		var req storagepb.ReadRowsRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode ReadRowsRequest: %v", err)
		}
		st, err := s.streamOf(ctx, req.GetReadStream())
		if err != nil {
			return err
		}
		if st.via != "" {
			req.ReadStream = st.via
			if in, err = proto.Marshal(&req); err != nil {
				return status.Errorf(codes.Internal, "encode ReadRowsRequest: %v", err)
			}
		}
		see := bigQueryArrowRows // arrowframes.go
		if st.avro {
			see = func(fr rawFrame) rawFrame { return fr }
			if st.aliased || st.via != "" {
				see = func(fr rawFrame) rawFrame { return clientRows(fr, st.table) }
			}
		}
		cs, err := open()
		if err != nil {
			return err
		}
		if err := sendAndClose(cs, in); err != nil {
			return err
		}
		return relay(ss, cs, see)
	}
	cs, err := open()
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() {
		for {
			var fr rawFrame
			if err := ss.RecvMsg(&fr); err != nil {
				if errors.Is(err, io.EOF) {
					errc <- cs.CloseSend()
				} else {
					errc <- err
				}
				return
			}
			if err := cs.SendMsg(&fr); err != nil {
				errc <- nil // the answer's status says why
				return
			}
		}
	}()
	respErr := make(chan error, 1)
	go func() { respErr <- relay(ss, cs, nil) }()
	for {
		select {
		case err := <-errc:
			if err != nil {
				cancel()
				<-respErr
				return err
			}
			errc = nil
		case err := <-respErr:
			return err
		}
	}
}

// createSession serves CreateReadSession: of a table whose ID another
// dataset has too, sent for the front's view of it (storagealias.go), else
// sent as it is; the answer's streams are remembered.
func (s *storageRead) createSession(ctx context.Context, ss grpc.ServerStream, open func() (grpc.ClientStream, error)) error {
	var in rawFrame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	var req storagepb.CreateReadSessionRequest
	if err := proto.Unmarshal(in, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "decode CreateReadSessionRequest: %v", err)
	}
	t, ok := parseReadTable(req.GetReadSession().GetTable())
	avro := req.GetReadSession().GetDataFormat() == storagepb.DataFormat_AVRO
	aliased := false
	if ok && s.isShared(ctx, t) {
		alias, found, err := s.aliasOf(ctx, t)
		if err != nil {
			return err
		}
		if found {
			if in, err = aliasRequest(&req, alias); err != nil {
				return err
			}
			aliased = true
		}
	}
	cs, err := open()
	if err != nil {
		return err
	}
	if err := sendAndClose(cs, in); err != nil {
		return err
	}
	return relay(ss, cs, func(fr rawFrame) rawFrame {
		fr = bigQueryArrowSession(fr) // arrowframes.go
		if !ok {
			return fr
		}
		var sess *storagepb.ReadSession
		if aliased {
			fr, sess = clientSession(fr, t)
		} else if sess = new(storagepb.ReadSession); proto.Unmarshal(fr, sess) != nil {
			return fr
		}
		for _, st := range sess.GetStreams() {
			rs := &readStream{table: t, aliased: aliased, avro: avro}
			if !aliased {
				rs.req = proto.Clone(&req).(*storagepb.CreateReadSessionRequest)
			}
			s.remember(st.GetName(), rs)
		}
		return fr
	})
}

// isShared reports whether t's ID is another dataset's too, or whether
// that cannot be told.
func (s *storageRead) isShared(ctx context.Context, t readTable) bool {
	ctx, cancel := context.WithTimeout(ctx, storageReadTimeout)
	defer cancel()
	return s.shared(ctx, t.project, t.dataset, t.table)
}

// streamOf returns what the front knows of the stream name for ReadRows:
// refused when the front did not see it made; of a stream that reads its
// table itself whose ID another dataset has since made a table of, made
// again for the front's view of the table (storagealias.go, via).
func (s *storageRead) streamOf(ctx context.Context, name string) (readStream, error) {
	s.mu.Lock()
	st, ok := s.streams[name]
	var cp readStream
	if ok {
		cp = *st
	}
	s.mu.Unlock()
	if !ok {
		return readStream{}, status.Errorf(codes.Unimplemented, "Not implemented here: ReadRows of the stream %q, which "+
			"CloudBurrow did not see made, so it cannot tell which table it reads: the emulator behind it reads a "+
			"stream's rows by the bare table ID, which may be another dataset's table (#1032). Nothing was read. "+
			"Create the read session again.", name)
	}
	if cp.aliased || cp.req == nil || !s.isShared(ctx, cp.table) {
		return cp, nil
	}
	alias, found, err := s.aliasOf(ctx, cp.table)
	if err != nil {
		return readStream{}, err
	}
	if !found {
		// The table is gone: the emulator's own answer (its rows, if
		// any, are those of the ID's other table, so they are not read).
		return readStream{}, status.Errorf(codes.NotFound, "Not found: Table %s:%s.%s", cp.table.project, cp.table.dataset, cp.table.table)
	}
	fr, err := aliasRequest(cp.req, alias)
	if err != nil {
		return readStream{}, err
	}
	sess, err := s.createAliasSession(ctx, fr)
	if err != nil {
		return readStream{}, err
	}
	if len(sess.GetStreams()) == 0 {
		return readStream{}, status.Errorf(codes.Internal, "the emulator's session of the view of %s.%s has no streams", cp.table.dataset, cp.table.table)
	}
	s.mu.Lock()
	if st, ok := s.streams[name]; ok {
		st.via = sess.Streams[0].GetName()
	}
	s.mu.Unlock()
	cp.via = sess.Streams[0].GetName()
	return cp, nil
}

// createAliasSession sends the emulator the CreateReadSession fr of the
// front's view of a table, and returns its session.
func (s *storageRead) createAliasSession(ctx context.Context, fr rawFrame) (*storagepb.ReadSession, error) {
	cs, err := s.upstream.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, createReadSession,
		grpc.ForceCodec(frameCodec{}))
	if err != nil {
		return nil, err
	}
	if err := sendAndClose(cs, fr); err != nil {
		return nil, err
	}
	var out rawFrame
	if err := cs.RecvMsg(&out); err != nil {
		return nil, err
	}
	var sess storagepb.ReadSession
	if err := proto.Unmarshal(out, &sess); err != nil {
		return nil, status.Errorf(codes.Internal, "decode the emulator's ReadSession: %v", err)
	}
	var rest rawFrame
	for cs.RecvMsg(&rest) == nil {
	}
	return &sess, nil
}

func (s *storageRead) remember(stream string, st *readStream) {
	if stream == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.streams[stream]; !ok {
		s.order = append(s.order, stream)
	}
	s.streams[stream] = st
	if len(s.order) >= maxStreams {
		drop := s.order[:len(s.order)/2]
		for _, n := range drop {
			delete(s.streams, n)
		}
		s.order = append([]string(nil), s.order[len(drop):]...)
	}
}

// parseReadTable reads projects/{project}/datasets/{dataset}/tables/{table}.
func parseReadTable(path string) (readTable, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "datasets" || parts[4] != "tables" ||
		parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return readTable{}, false
	}
	return readTable{project: parts[1], dataset: parts[3], table: parts[5]}, true
}

// sendAndClose sends the one request of a unary or server-streaming call.
func sendAndClose(cs grpc.ClientStream, in rawFrame) error {
	if err := cs.SendMsg(&in); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return cs.CloseSend()
}

// relay copies the emulator's answer to the client: its header, each
// message (as see returns it, when see is not nil) and its trailer, and
// returns its status, nil for OK.
func relay(ss grpc.ServerStream, cs grpc.ClientStream, see func(rawFrame) rawFrame) error {
	sentHeader := false
	for {
		var fr rawFrame
		err := cs.RecvMsg(&fr)
		if !sentHeader {
			if h, herr := cs.Header(); herr == nil && len(h) > 0 {
				_ = ss.SendHeader(h)
			}
			sentHeader = true
		}
		if err != nil {
			ss.SetTrailer(cs.Trailer())
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if see != nil {
			fr = see(fr)
		}
		if err := ss.SendMsg(&fr); err != nil {
			return err
		}
	}
}

// outgoing is the incoming metadata without the pseudo and transport
// headers the client connection sets itself.
func outgoing(md metadata.MD) metadata.MD {
	out := metadata.MD{}
	for k, v := range md {
		if strings.HasPrefix(k, ":") || strings.HasPrefix(k, "grpc-") || k == "content-type" || k == "te" || k == "user-agent" {
			continue
		}
		out[k] = v
	}
	return out
}

// rawFrame is one message as it travels, never decoded on the way through.
type rawFrame []byte

// frameCodec passes frames through. Its name is "proto", so the content
// type on both sides is what the clients and the emulator expect.
type frameCodec struct{}

func (frameCodec) Marshal(v any) ([]byte, error) {
	if m, ok := v.(*rawFrame); ok {
		return *m, nil
	}
	if m, ok := v.(proto.Message); ok {
		return proto.Marshal(m)
	}
	return nil, fmt.Errorf("bigqueryfront: cannot encode %T", v)
}

func (frameCodec) Unmarshal(data []byte, v any) error {
	if m, ok := v.(*rawFrame); ok {
		*m = append((*m)[:0], data...)
		return nil
	}
	if m, ok := v.(proto.Message); ok {
		return proto.Unmarshal(data, m)
	}
	return fmt.Errorf("bigqueryfront: cannot decode into %T", v)
}

func (frameCodec) Name() string { return "proto" }

// storageReadTimeout bounds a check's reads of the emulator's REST API.
var storageReadTimeout = 30 * time.Second
