// Package bigtablefront is the front the Bigtable pod runs beside Google's
// emulator (#1206), as `cloudburrow-storage bigtable-front`.
//
// The emulator (cbtemulator, bttest) embeds a nil server interface for the
// RPCs it does not implement, so any of them panics it: the pod restarts and
// every table, all in memory, is lost. The front answers those RPCs
// UNIMPLEMENTED itself, as the emulator answers the ones it does refuse
// (authorized views, snapshots), and forwards every other call to the
// emulator unchanged, each message passed through undecoded.
//
// It serves gRPC and, on the same port, the admin REST API (rest.go) and the readiness path the emulator's
// container is probed through (internal/frontready), since the emulator
// listens on the pod's loopback alone (#1114).
package bigtablefront

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/frontready"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

const (
	admin = "/google.bigtable.admin.v2.BigtableTableAdmin/"
	data  = "/google.bigtable.v2.Bigtable/"
)

// Refused are the methods the emulator has no implementation of and would
// panic on, measured one by one (#1206), each with the message the front
// answers it UNIMPLEMENTED with.
var Refused = map[string]string{
	admin + "CreateSchemaBundle":                   schemaBundles,
	admin + "GetSchemaBundle":                      schemaBundles,
	admin + "ListSchemaBundles":                    schemaBundles,
	admin + "UpdateSchemaBundle":                   schemaBundles,
	admin + "DeleteSchemaBundle":                   schemaBundles,
	admin + "UndeleteTable":                        "the emulator does not currently support undeleting tables",
	data + "GenerateInitialChangeStreamPartitions": changeStreams,
	data + "ReadChangeStream":                      changeStreams,
}

// The messages, worded as the emulator's own refusals are ("the emulator
// does not currently support authorized views").
const (
	schemaBundles = "the emulator does not currently support schema bundles"
	changeStreams = "the emulator does not currently support change streams"
)

// Front forwards Bigtable calls to the emulator, refusing the Refused ones.
type Front struct {
	upstream *grpc.ClientConn
	addr     string
	logf     func(format string, args ...any)
	// calls is the call log the CLI reads into its metrics, request log
	// and traces (internal/calllog, #1267).
	calls calllog.Ring
	// instances serves the instance admin API the emulator does not
	// (#1331), and served is its methods.
	instances *instances
	tableIAM  *tableIAM
	// copies is the backups and snapshots, views the authorized views
	// (#1499).
	copies *copies
	views  *authViews
	served map[string]method
}

// New returns a front for the emulator at upstream (host:port). The
// connection is lazy: nothing is dialled until the first call.
func New(upstream string, logf func(format string, args ...any)) (*Front, error) {
	conn, err := grpc.NewClient(upstream,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32), grpc.MaxCallSendMsgSize(math.MaxInt32)))
	if err != nil {
		return nil, fmt.Errorf("the emulator at %s: %w", upstream, err)
	}
	if logf == nil {
		logf = log.Printf
	}
	f := &Front{upstream: conn, addr: upstream, logf: logf}
	f.instances = newInstances(f.dropTables)
	f.served = f.instances.methods()
	f.copies = newCopies(conn, f.instances)
	f.views = &authViews{upstream: conn, byName: map[string]*btapb.AuthorizedView{}}
	f.tableIAM = &tableIAM{upstream: conn, policies: map[string]*iampolicy.Stored{}, views: f.views, copies: f.copies}
	for _, ms := range []map[string]method{f.tableIAM.methods(), f.copies.methods(), f.viewMethods()} {
		for name, m := range ms {
			f.served[name] = m
		}
	}
	return f, nil
}

// viewMethods are the authorized view admin methods, whose operations are
// kept with the instance admin's.
func (f *Front) viewMethods() map[string]method {
	op := f.copies.op
	return map[string]method{
		admin + "CreateAuthorizedView": unary(func() *btapb.CreateAuthorizedViewRequest { return &btapb.CreateAuthorizedViewRequest{} },
			func(ctx context.Context, req *btapb.CreateAuthorizedViewRequest) (proto.Message, error) {
				return f.views.create(ctx, req, op)
			}),
		admin + "GetAuthorizedView":   unary(func() *btapb.GetAuthorizedViewRequest { return &btapb.GetAuthorizedViewRequest{} }, f.views.get),
		admin + "ListAuthorizedViews": unary(func() *btapb.ListAuthorizedViewsRequest { return &btapb.ListAuthorizedViewsRequest{} }, f.views.list),
		admin + "UpdateAuthorizedView": unary(func() *btapb.UpdateAuthorizedViewRequest { return &btapb.UpdateAuthorizedViewRequest{} },
			func(ctx context.Context, req *btapb.UpdateAuthorizedViewRequest) (proto.Message, error) {
				return f.views.update(ctx, req, op)
			}),
		admin + "DeleteAuthorizedView": unary(func() *btapb.DeleteAuthorizedViewRequest { return &btapb.DeleteAuthorizedViewRequest{} }, f.views.delete),
	}
}

// Close releases the connection to the emulator.
func (f *Front) Close() error { return f.upstream.Close() }

// Server returns the gRPC server of the front: every method is forwarded,
// with no size limit of its own, but the Refused ones.
func (f *Front) Server() *grpc.Server {
	return grpc.NewServer(
		grpc.ForceServerCodec(rawCodec{}),
		grpc.UnknownServiceHandler(f.handle),
		grpc.StreamInterceptor(calllog.StreamInterceptor(&f.calls, firstName, streaming)),
		grpc.MaxRecvMsgSize(math.MaxInt32), grpc.MaxSendMsgSize(math.MaxInt32),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
}

// Serve serves the front on l until ctx ends: gRPC through grpc-go's own
// transport, and any other connection (the kubelet's readiness probe) by
// net/http, which answers frontready.Path alone.
func (f *Front) Serve(ctx context.Context, l net.Listener) error {
	grpcSrv := f.Server()
	srv := &http.Server{
		Handler:           frontready.Wrap(f.httpHandler(), f.addr),
		ReadHeaderTimeout: 10 * time.Second,
	}
	grpcL, httpL := pubsubfront.Split(l)
	errc := make(chan error, 2)
	go func() { errc <- grpcSrv.Serve(grpcL) }()
	go func() { errc <- srv.Serve(httpL) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	// Not a graceful stop: an open ReadRows stream could hold it forever.
	_ = srv.Close()
	grpcSrv.Stop()
	_ = l.Close()
	if err == nil || errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// handle refuses a Refused method and forwards any other.
func (f *Front) handle(_ any, ss grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(ss)
	if !ok {
		return status.Error(codes.Internal, "no method on the stream")
	}
	if m, ok := f.served[method]; ok {
		return serveUnary(ss, m)
	}
	if strings.HasPrefix(method, instanceAdmin) || strings.HasPrefix(method, operations) {
		f.logf("bigtable front: refused %s, which neither the emulator nor the front implements", method)
		return status.Errorf(codes.Unimplemented, "the emulator does not currently support %s", strings.TrimPrefix(method[strings.LastIndexByte(method, '/'):], "/"))
	}
	if msg, refused := Refused[method]; refused {
		f.logf("bigtable front: refused %s, which the emulator does not implement", method)
		return status.Error(codes.Unimplemented, msg)
	}
	var first *frame
	if viewMethods[method] {
		// A data call naming an authorized view is served on its
		// table, limited to the view (authviews.go).
		var in frame
		if err := ss.RecvMsg(&in); err != nil {
			return err
		}
		out, err := f.views.rewrite(method, in)
		if err != nil {
			return err
		}
		first = &out
	}
	ctx, cancel := context.WithCancel(ss.Context())
	defer cancel()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = metadata.NewOutgoingContext(ctx, forwardable(md))
	}
	cs, err := f.upstream.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method,
		grpc.ForceCodec(rawCodec{}))
	if err != nil {
		return err
	}
	reqErr := make(chan error, 1)
	go func() {
		if first != nil {
			if err := cs.SendMsg(first); err != nil && !errors.Is(err, io.EOF) {
				reqErr <- err
				return
			}
		}
		reqErr <- pumpRequests(ss, cs)
	}()
	respErr := make(chan error, 1)
	go func() { respErr <- pumpResponses(ss, cs) }()
	for {
		select {
		case err := <-reqErr:
			if err != nil {
				cancel()
				<-respErr
				return err
			}
			reqErr = nil
		case err := <-respErr:
			return err
		}
	}
}

// forwardable is the incoming metadata without the pseudo and transport
// headers the client connection sets for itself.
func forwardable(md metadata.MD) metadata.MD {
	out := metadata.MD{}
	for k, v := range md {
		switch {
		case len(k) > 0 && k[0] == ':', k == "content-type", k == "te", k == "user-agent",
			len(k) > 5 && k[:5] == "grpc-":
			continue
		}
		out[k] = v
	}
	return out
}

// pumpRequests copies the client's requests to the emulator.
func pumpRequests(ss grpc.ServerStream, cs grpc.ClientStream) error {
	for {
		var fr frame
		if err := ss.RecvMsg(&fr); err != nil {
			if errors.Is(err, io.EOF) {
				return cs.CloseSend()
			}
			return err
		}
		if err := cs.SendMsg(&fr); err != nil {
			if errors.Is(err, io.EOF) {
				// The emulator ended the call; its status is
				// pumpResponses'.
				return nil
			}
			return err
		}
	}
}

// pumpResponses copies the emulator's answer to the client: headers, every
// message and the trailer, and returns its status (nil for OK).
func pumpResponses(ss grpc.ServerStream, cs grpc.ClientStream) error {
	sentHeader := false
	for {
		var fr frame
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
		if err := ss.SendMsg(&fr); err != nil {
			return err
		}
	}
}

// frame is one message as it travels: its encoded bytes, never decoded.
type frame []byte

// rawCodec passes frames through unchanged. Its name is "proto", so the
// content type on both sides is what the clients and the emulator expect.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case *frame:
		return *m, nil
	case proto.Message:
		return proto.Marshal(m)
	}
	return nil, fmt.Errorf("bigtablefront: cannot encode %T", v)
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	switch m := v.(type) {
	case *frame:
		*m = append((*m)[:0], data...)
		return nil
	case proto.Message:
		return proto.Unmarshal(data, m)
	}
	return fmt.Errorf("bigtablefront: cannot decode into %T", v)
}

func (rawCodec) Name() string { return "proto" }

// Calls returns the calls numbered after seq, oldest first.
func (f *Front) Calls(after uint64) []calllog.Call { return f.calls.After(after) }

// httpHandler answers calllog.Path and the admin REST API (rest.go, #1440),
// and nothing else, on the connections that are not gRPC.
func (f *Front) httpHandler() http.Handler {
	api := f.restHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == calllog.Path && r.Method == http.MethodGet {
			f.calls.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, restPrefix) {
			api.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

// dropTables deletes every table of the instance in the emulator.
func (f *Front) dropTables(ctx context.Context, instance string) error {
	f.tableIAM.drop(instance)
	f.views.drop(instance)
	f.copies.drop(instance)
	c := btapb.NewBigtableTableAdminClient(f.upstream)
	var token string
	for {
		resp, err := c.ListTables(ctx, &btapb.ListTablesRequest{Parent: instance, View: btapb.Table_NAME_ONLY, PageToken: token})
		if err != nil {
			return err
		}
		for _, t := range resp.GetTables() {
			if _, err := c.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: t.GetName()}); err != nil && status.Code(err) != codes.NotFound {
				return err
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			return nil
		}
	}
}

// firstName reads the table, instance or parent a request names.
func firstName(m any) string {
	if fr, ok := m.(*frame); ok {
		return calllog.FirstName(*fr)
	}
	return ""
}

// streaming reports Bigtable's server-streaming methods.
func streaming(method string) bool {
	switch method {
	case data + "ReadRows", data + "MutateRows", data + "SampleRowKeys", data + "ReadChangeStream",
		data + "GenerateInitialChangeStreamPartitions", data + "ExecuteQuery":
		return true
	}
	return false
}
