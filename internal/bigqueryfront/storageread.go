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
// be of another table. ReadRows checks again, since another dataset may
// have made a table of the ID after the session. A stream the front did
// not see made (a session made before the front last started) is
// UNIMPLEMENTED too, as the front cannot tell its table. The Storage
// Write API writes by the whole name (the emulator's AddTableData,
// read in its source) and is passed through.

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
	// shared reports whether a dataset other than dataset has a table of
	// the ID table in project, or whether that cannot be told.
	shared func(ctx context.Context, project, dataset, table string) bool

	mu      sync.Mutex
	streams map[string]readTable
	order   []string
}

// readTable is the table a read stream reads.
type readTable struct{ project, dataset, table string }

// ServeStorageRead serves the Storage Read front on l until ctx ends:
// every call goes to the emulator's gRPC port at upstream (host:port),
// and the checks read the emulator's REST API through rest.
func ServeStorageRead(ctx context.Context, l net.Listener, upstream string, rest http.Handler) error {
	s, err := newStorageRead(upstream, rest)
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

func newStorageRead(upstream string, rest http.Handler) (*storageRead, error) {
	conn, err := grpc.NewClient(upstream,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32), grpc.MaxCallSendMsgSize(math.MaxInt32)))
	if err != nil {
		return nil, fmt.Errorf("the emulator's Storage Read API at %s: %w", upstream, err)
	}
	return &storageRead{upstream: conn, streams: map[string]readTable{},
		shared: func(ctx context.Context, project, dataset, table string) bool {
			r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://bigquery/", nil)
			if err != nil {
				return true
			}
			f := front{next: rest, base: "/bigquery/v2/projects/" + url.PathEscape(project)}
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
		if err := s.checkStream(ctx, req.GetReadStream()); err != nil {
			return err
		}
		cs, err := open()
		if err != nil {
			return err
		}
		if err := sendAndClose(cs, in); err != nil {
			return err
		}
		return relay(ss, cs, nil)
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

// createSession serves CreateReadSession: refused when the table's ID is
// another dataset's too, else sent, and the answer's streams remembered.
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
	if ok {
		if err := s.checkTable(ctx, t); err != nil {
			return err
		}
	}
	cs, err := open()
	if err != nil {
		return err
	}
	if err := sendAndClose(cs, in); err != nil {
		return err
	}
	return relay(ss, cs, func(fr rawFrame) {
		var sess storagepb.ReadSession
		if !ok || proto.Unmarshal(fr, &sess) != nil {
			return
		}
		for _, st := range sess.GetStreams() {
			s.remember(st.GetName(), t)
		}
	})
}

// checkTable refuses a read of t when its ID is another dataset's too.
func (s *storageRead) checkTable(ctx context.Context, t readTable) error {
	ctx, cancel := context.WithTimeout(ctx, storageReadTimeout)
	defer cancel()
	if !s.shared(ctx, t.project, t.dataset, t.table) {
		return nil
	}
	return status.Errorf(codes.Unimplemented, "Not implemented here: a Storage Read API session of %s.%s, "+
		"whose table ID another dataset of the project has too (or CloudBurrow could not tell). BigQuery reads the "+
		"table the session names, but the emulator behind CloudBurrow reads a session's rows by the bare table ID, "+
		"which it resolves to the first table of that ID made in any dataset, so it would stream another table's rows "+
		"(#1032). Nothing was read. Read the table through tabledata.list (the client library's default read) or a "+
		"query of %s.%s, which CloudBurrow reads by the whole name.", t.dataset, t.table, t.dataset, t.table)
}

// checkStream refuses ReadRows of a stream whose table's ID is now another
// dataset's too, or of one the front did not see made.
func (s *storageRead) checkStream(ctx context.Context, name string) error {
	s.mu.Lock()
	t, ok := s.streams[name]
	s.mu.Unlock()
	if !ok {
		return status.Errorf(codes.Unimplemented, "Not implemented here: ReadRows of the stream %q, which "+
			"CloudBurrow did not see made, so it cannot tell which table it reads: the emulator behind it reads a "+
			"stream's rows by the bare table ID, which may be another dataset's table (#1032). Nothing was read. "+
			"Create the read session again.", name)
	}
	return s.checkTable(ctx, t)
}

func (s *storageRead) remember(stream string, t readTable) {
	if stream == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.streams[stream]; !ok {
		s.order = append(s.order, stream)
	}
	s.streams[stream] = t
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
// message (seen by see, when not nil) and its trailer, and returns its
// status, nil for OK.
func relay(ss grpc.ServerStream, cs grpc.ClientStream, see func(rawFrame)) error {
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
			see(fr)
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
