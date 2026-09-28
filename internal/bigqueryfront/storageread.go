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

// The Storage Read API (#1032, #1046, #1095, #1098).
//
// The front serves the Storage Read port (the Service's 9060; the
// emulator's is the pod's own). CreateReadSession is sent to the emulator,
// which reads the session's table and schema by project, dataset and
// table; ReadRows the front answers itself (storagerows.go), from a query
// of the table's whole name; every other call (SplitReadStream, and the
// Storage Write API, which writes by the whole name: the emulator's
// AddTableData, read in its source) is passed through as raw frames.
//
// Why the front writes the rows. The emulator's ReadRows (server/
// storage_handler.go, v0.8.1, read in its source) fails in three ways,
// each measured against the pinned image with the official Storage Read
// client (cloud.google.com/go/bigquery/storage/apiv1):
//
//   - It reads a session's rows with "SELECT <columns> FROM `<tableId>`"
//     (buildQuery), the bare table ID, which its engine resolves to the
//     first table of that ID made in any dataset (qualify.go): with m1.t
//     (s STRING) made first and m2.t (a INT64) after, ReadRows of m2.t
//     failed "strconv.ParseInt: parsing \"one\": invalid syntax", reading
//     m1.t's rows (#1032).
//   - Arrow: it appends each element of a REPEATED value to the list's
//     builder as a list of its own (internal/types/types.go,
//     TableCell.AppendValueToARROWBuilder calls ListBuilder.Append for
//     every element), so the column has more rows than the batch, and
//     arrow's RecordBuilder.NewRecordBatch panics. A panic in a gRPC
//     handler ends the process: ReadRows of a table (i INT64, r ARRAY<INT64>)
//     with rows (1, [7,8]) and (2, [9]) killed the emulator ("panic:
//     arrow/array: field 1 has 3 rows. want=2", storage_handler.go:349),
//     the client got UNAVAILABLE "error reading from server: EOF",
//     Kubernetes restarted the container, and every dataset, table and job
//     of the instance was gone, as the emulator keeps them in memory
//     (#1095). A DATETIME column failed too, "invalid timestamp string
//     \"2024-01-02T03:04:05.123456\"" (it parses a DATETIME as a TIMESTAMP).
//   - Avro: it names the schema's record by the namespace
//     "<project>.<dataset>" (types/avro.go, TableToAVRO), and goavro, which
//     it writes the rows with, refuses a name with a hyphen, which every
//     CloudBurrow project ID has: ReadRows failed "Record ought to have
//     valid name: schema name ought to have second and remaining
//     characters contain only [A-Za-z0-9_]: w1095-local" (#1098).
//
// So the emulator's ReadRows is never called, and no request reaches the
// code that panics. The session's schema is the emulator's (Arrow framed as
// BigQuery's, arrowframes.go, with a DATETIME's type BigQuery's,
// storagerows.go; Avro with valid names, avrorows.go), and the rows are
// written in it. A stream the front did not see made (a session
// made before the front last started) is UNIMPLEMENTED, as the front
// cannot tell its table or schema. CreateReadSession without a session, a
// table name or a data format of ARROW or AVRO is INVALID_ARGUMENT before
// the emulator sees it: its handler reads req.ReadSession.Table without
// looking for the session, which would panic too. So is a table the REST
// API does not find (NOT_FOUND): measured, CreateReadSession of a table in
// a project the emulator does not have panicked it too (getTableMetadata
// calls Dataset on the nil project, storage_handler.go:919), and the
// client's retries kept it restarting (#1102).

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
	// rest is the emulator's REST API, which the rows are read through.
	rest http.Handler
	// records are the REST front's job records, so that the front's own
	// queries are left out of jobs.list (jobrecords.go), or nil.
	records *jobRecords
	// write serves the Storage Write API (storagewrite.go).
	write *storageWrite

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
	// restriction is the session's read_options.row_restriction.
	restriction string
	// arrowSchema is the session's Arrow schema (its IPC message), or
	// avroSchema its Avro schema, as the client was given it.
	arrowSchema []byte
	avroSchema  string
}

// ServeStorageRead serves the Storage Read front on l until ctx ends:
// sessions are made by the emulator's gRPC port at upstream (host:port),
// and rows read through its REST API, rest. The Storage Write API is
// UNIMPLEMENTED here; Run serves it too (storagewrite.go).
func ServeStorageRead(ctx context.Context, l net.Listener, upstream string, rest http.Handler) error {
	return serveStorageRead(ctx, l, upstream, rest, nil, nil)
}

// serveStorageRead is ServeStorageRead, with the REST front's job records
// (records), or nil, and the REST front itself (front), which the Storage
// Write API writes through; nil leaves the Write API UNIMPLEMENTED.
func serveStorageRead(ctx context.Context, l net.Listener, upstream string, rest http.Handler, records *jobRecords, front http.Handler) error {
	s, err := newStorageRead(upstream, rest, records)
	if err != nil {
		return err
	}
	defer s.upstream.Close()
	if front != nil {
		s.write = newStorageWrite(front)
	}
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

func newStorageRead(upstream string, rest http.Handler, records *jobRecords) (*storageRead, error) {
	conn, err := grpc.NewClient(upstream,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32), grpc.MaxCallSendMsgSize(math.MaxInt32)))
	if err != nil {
		return nil, fmt.Errorf("the emulator's Storage Read API at %s: %w", upstream, err)
	}
	return &storageRead{upstream: conn, rest: rest, records: records, streams: map[string]*readStream{}}, nil
}

func (s *storageRead) server() *grpc.Server {
	return grpc.NewServer(
		grpc.ForceServerCodec(frameCodec{}),
		grpc.UnknownServiceHandler(s.handle),
		grpc.MaxRecvMsgSize(math.MaxInt32), grpc.MaxSendMsgSize(math.MaxInt32),
	)
}

// handle serves one call of any method (above).
func (s *storageRead) handle(_ any, ss grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(ss)
	if !ok {
		return status.Error(codes.Internal, "no method on the stream")
	}
	if strings.HasPrefix(method, writeService) {
		return s.write.handle(ss, method) // storagewrite.go: never the emulator's
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
		return s.createSession(ss, open)
	case readRows:
		var in rawFrame
		if err := ss.RecvMsg(&in); err != nil {
			return err
		}
		var req storagepb.ReadRowsRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode ReadRowsRequest: %v", err)
		}
		st, err := s.streamOf(req.GetReadStream())
		if err != nil {
			return err
		}
		return s.serveRows(ss.Context(), ss, st, req.GetOffset()) // storagerows.go
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

// createSession serves CreateReadSession: checked (above), sent to the
// emulator as it is, and its answer's schema made BigQuery's; its streams
// are remembered with the session's table and schema.
func (s *storageRead) createSession(ss grpc.ServerStream, open func() (grpc.ClientStream, error)) error {
	var in rawFrame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	var req storagepb.CreateReadSessionRequest
	if err := proto.Unmarshal(in, &req); err != nil {
		return status.Errorf(codes.InvalidArgument, "decode CreateReadSessionRequest: %v", err)
	}
	if req.GetReadSession() == nil {
		return status.Error(codes.InvalidArgument, "read_session is required")
	}
	t, ok := parseReadTable(req.GetReadSession().GetTable())
	if !ok {
		return status.Errorf(codes.InvalidArgument, "read_session.table %q is not projects/{project}/datasets/{dataset}/tables/{table}",
			req.GetReadSession().GetTable())
	}
	switch f := req.GetReadSession().GetDataFormat(); f {
	case storagepb.DataFormat_ARROW, storagepb.DataFormat_AVRO:
	default:
		return status.Errorf(codes.InvalidArgument, "read_session.data_format %s: give ARROW or AVRO", f)
	}
	if err := s.tableExists(ss.Context(), t); err != nil {
		return err
	}
	restriction := req.GetReadSession().GetReadOptions().GetRowRestriction()
	cs, err := open()
	if err != nil {
		return err
	}
	if err := sendAndClose(cs, in); err != nil {
		return err
	}
	return relay(ss, cs, func(fr rawFrame) rawFrame {
		var sess storagepb.ReadSession
		if proto.Unmarshal(fr, &sess) != nil {
			return fr
		}
		rs := &readStream{table: t, restriction: restriction}
		if a := sess.GetArrowSchema(); a != nil {
			a.SerializedSchema = bigQueryArrowSchema(arrowSchemaMessage(a.SerializedSchema)) // arrowframes.go, storagerows.go
			rs.arrowSchema = a.SerializedSchema
		}
		if a := sess.GetAvroSchema(); a != nil {
			a.Schema = bigQueryAvroSchema(a.Schema) // avrorows.go
			rs.avroSchema = a.Schema
		}
		out, err := proto.Marshal(&sess)
		if err != nil {
			return fr
		}
		for _, st := range sess.GetStreams() {
			s.remember(st.GetName(), rs)
		}
		return out
	})
}

// tableExists reads t with tables.get of the REST API: NOT_FOUND, or the
// REST answer's error, when it is not there (above).
func (s *storageRead) tableExists(ctx context.Context, t readTable) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://bigquery/", nil)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	f := front{next: s.rest, base: "/bigquery/v2/projects/" + url.PathEscape(t.project)}
	code, got := f.get(r, "/datasets/"+url.PathEscape(t.dataset)+"/tables/"+url.PathEscape(t.table))
	if code == http.StatusOK || code == 0 {
		return nil
	}
	return restStatus(code, got, "read_session.table "+t.path())
}

// streamOf returns what the front knows of the stream name for ReadRows,
// refused when the front did not see it made.
func (s *storageRead) streamOf(name string) (*readStream, error) {
	s.mu.Lock()
	st, ok := s.streams[name]
	s.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "Not implemented here: ReadRows of the stream %q, which "+
			"CloudBurrow did not see made, so it cannot tell which table it reads or its schema (the session was made "+
			"before the BigQuery front last started). Nothing was read. Create the read session again.", name)
	}
	return st, nil
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

// storageReadTimeout bounds the query a ReadRows reads its rows with.
var storageReadTimeout = 10 * time.Minute
