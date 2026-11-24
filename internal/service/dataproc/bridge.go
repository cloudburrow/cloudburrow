package dataproc

// The Spark Connect bridge (#2156): how a client reaches a session's Spark
// Connect server through this API's port.
//
// Spark Connect is gRPC (spark.connect.SparkConnectService), every request
// of which carries the client's session_id as its field 1; Google's client
// (dataproc-spark-connect, Apache-2.0) sets it to the Dataproc session's
// UUID, from the sc://HOST/;session_id=UUID URI it builds of the session's
// "Spark Connect Server" endpoint. The proxy reads a call's first request,
// finds the ACTIVE session of that UUID, and relays the call, both ways
// and unchanged, to the session's server: on this API's gRPC port itself,
// for a plain Spark Connect client (sc://127.0.0.1:PORT/;session_id=UUID),
// and on the WebSocket path below.
//
// That path is what Google's client tunnels through: its local proxy
// opens wss://HOST/tcp-over-websocket-bridge/35218cb7-1201-4940-89e8-48d8f03fed96
// and sends the HTTP/2 connection's bytes as hex text messages, reading the
// server's back the same way (the client's proxy.py, which names the
// protocol inverting-proxy's tcpbridge). Each WebSocket is one HTTP/2
// connection, served by a gRPC server of the proxy alone.

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"golang.org/x/net/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// BridgePath is the TCP-over-WebSocket bridge's path.
const BridgePath = "/tcp-over-websocket-bridge/35218cb7-1201-4940-89e8-48d8f03fed96"

// connectService is Spark Connect's gRPC service.
const connectService = "/spark.connect.SparkConnectService/"

// ProxyOption is the gRPC server option that relays Spark Connect calls
// on the API's own port.
func (s *Server) ProxyOption() grpc.ServerOption { return grpc.UnknownServiceHandler(s.proxy) }

// sessionIDOf is field 1 of a Spark Connect request, its session_id.
func sessionIDOf(b []byte) string {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return ""
		}
		b = b[n:]
		if num == 1 && typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return ""
			}
			return string(v)
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return ""
		}
		b = b[m:]
	}
	return ""
}

// connectFor is the server of the ACTIVE session whose UUID is uuid.
func (s *Server) connectFor(uuid string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, x := range s.sessions {
		if x.GetUuid() != uuid {
			continue
		}
		c := s.connects[name]
		if x.State != dataprocpb.Session_ACTIVE || c == nil {
			return "", "", status.Errorf(codes.FailedPrecondition, "Session %s is %s, not ACTIVE", name, x.State)
		}
		s.activity[name] = s.now()
		return c.Address(), name, nil
	}
	return "", "", status.Errorf(codes.NotFound, "No Dataproc session has the UUID %q: a Spark Connect client's session_id is its session's UUID", uuid)
}

var (
	backendsMu sync.Mutex
	backends   = map[string]*grpc.ClientConn{}
)

func backend(addr string) (*grpc.ClientConn, error) {
	backendsMu.Lock()
	defer backendsMu.Unlock()
	if c := backends[addr]; c != nil {
		return c, nil
	}
	c, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<30), grpc.MaxCallSendMsgSize(1<<30)))
	if err != nil {
		return nil, err
	}
	backends[addr] = c
	return c, nil
}

// proxy relays one Spark Connect call. A request is received as an Empty,
// whose unknown fields keep its bytes as they came, and sent on so.
func (s *Server) proxy(_ any, stream grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok || !strings.HasPrefix(method, connectService) {
		return status.Errorf(codes.Unimplemented, "unknown method %s", method)
	}
	first := &emptypb.Empty{}
	if err := stream.RecvMsg(first); err != nil {
		return err
	}
	raw, _ := proto.MarshalOptions{}.Marshal(first)
	addr, name, err := s.connectFor(sessionIDOf(raw))
	if err != nil {
		return err
	}
	conn, err := backend(addr)
	if err != nil {
		return status.Errorf(codes.Unavailable, "the Spark Connect server of %s: %v", name, err)
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	if md, ok := metadata.FromIncomingContext(stream.Context()); ok {
		out := metadata.MD{}
		for k, v := range md {
			if strings.HasPrefix(k, ":") || k == "content-type" || k == "user-agent" || k == "authorization" || strings.HasPrefix(k, "grpc-") {
				continue
			}
			out[k] = v
		}
		ctx = metadata.NewOutgoingContext(ctx, out)
	}
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method)
	if err != nil {
		return err
	}
	if err := cs.SendMsg(first); err != nil {
		return err
	}
	go func() {
		for {
			m := &emptypb.Empty{}
			if err := stream.RecvMsg(m); err != nil {
				_ = cs.CloseSend()
				return
			}
			if err := cs.SendMsg(m); err != nil {
				return
			}
		}
	}()
	if md, err := cs.Header(); err == nil && len(md) > 0 {
		_ = stream.SendHeader(md)
	}
	for {
		m := &emptypb.Empty{}
		err := cs.RecvMsg(m)
		if err != nil {
			stream.SetTrailer(cs.Trailer())
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		s.touch(name)
		if err := stream.SendMsg(m); err != nil {
			return err
		}
	}
}

func (s *Server) touch(name string) {
	s.mu.Lock()
	if _, ok := s.activity[name]; ok {
		s.activity[name] = s.now()
	}
	s.mu.Unlock()
}

// ---- the WebSocket bridge ----

// Bridge serves BridgePath on next's port: a WebSocket per connection of
// Google's Spark Connect client.
func (s *Server) Bridge(next http.Handler) http.Handler {
	ws := websocket.Server{
		// The client sends no Origin; the port's host check has passed.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler:   s.serveBridge,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == BridgePath {
			ws.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) bridgeServer() *bridgeListener {
	s.bridgeOnce.Do(func() {
		s.bridgeLn = &bridgeListener{conns: make(chan net.Conn), done: make(chan struct{})}
		srv := grpc.NewServer(grpc.UnknownServiceHandler(s.proxy), grpc.MaxRecvMsgSize(1<<30), grpc.MaxSendMsgSize(1<<30))
		s.bridgeSrv = srv
		go func() { _ = srv.Serve(s.bridgeLn) }()
	})
	return s.bridgeLn
}

func (s *Server) serveBridge(ws *websocket.Conn) {
	c := &wsConn{ws: ws, closed: make(chan struct{})}
	ln := s.bridgeServer()
	select {
	case ln.conns <- c:
	case <-ln.done:
		return
	}
	<-c.closed
}

// bridgeListener hands the bridge's connections to its gRPC server.
type bridgeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *bridgeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *bridgeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *bridgeListener) Addr() net.Addr { return bridgeAddr{} }

type bridgeAddr struct{}

func (bridgeAddr) Network() string { return "websocket" }
func (bridgeAddr) String() string  { return "tcp-over-websocket-bridge" }

// wsConn is a WebSocket of hex text messages as a byte stream.
type wsConn struct {
	ws      *websocket.Conn
	buf     []byte
	rmu     sync.Mutex
	wmu     sync.Mutex
	closed  chan struct{}
	closeMu sync.Once
}

func (c *wsConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.buf) == 0 {
		var msg string
		if err := websocket.Message.Receive(c.ws, &msg); err != nil {
			return 0, err
		}
		b, err := hex.DecodeString(strings.TrimSpace(msg))
		if err != nil {
			return 0, errors.New("the bridge's message is not hex")
		}
		c.buf = b
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := websocket.Message.Send(c.ws, hex.EncodeToString(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error {
	c.closeMu.Do(func() { close(c.closed) })
	return c.ws.Close()
}

func (c *wsConn) LocalAddr() net.Addr                { return bridgeAddr{} }
func (c *wsConn) RemoteAddr() net.Addr               { return bridgeAddr{} }
func (c *wsConn) SetDeadline(t time.Time) error      { return c.ws.SetDeadline(t) }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
