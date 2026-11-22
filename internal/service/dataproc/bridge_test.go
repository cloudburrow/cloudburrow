package dataproc

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"golang.org/x/net/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/emptypb"
)

// echoBackend is a stand-in Spark Connect server: each call answers its
// requests' bytes back, tagged with the method.
func echoBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		for {
			m := &emptypb.Empty{}
			if err := stream.RecvMsg(m); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
			}
			out := &emptypb.Empty{}
			out.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(m.ProtoReflect().GetUnknown(), 99, protowire.BytesType), method))
			if err := stream.SendMsg(out); err != nil {
				return err
			}
		}
	}))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func connectRequest(session string) *emptypb.Empty {
	m := &emptypb.Empty{}
	m.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), session))
	return m
}

func callThrough(ctx context.Context, conn *grpc.ClientConn, session string) (string, error) {
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/spark.connect.SparkConnectService/ExecutePlan")
	if err != nil {
		return "", err
	}
	if err := cs.SendMsg(connectRequest(session)); err != nil {
		return "", err
	}
	_ = cs.CloseSend()
	m := &emptypb.Empty{}
	if err := cs.RecvMsg(m); err != nil {
		return "", err
	}
	return string(m.ProtoReflect().GetUnknown()), nil
}

// wsDial is Google's client's side of the bridge: the HTTP/2 connection's
// bytes as hex text messages on a WebSocket.
type wsClient struct {
	ws  *websocket.Conn
	buf []byte
}

func (c *wsClient) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		var msg string
		if err := websocket.Message.Receive(c.ws, &msg); err != nil {
			return 0, err
		}
		b, err := hex.DecodeString(msg)
		if err != nil {
			return 0, err
		}
		c.buf = b
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *wsClient) Write(p []byte) (int, error) {
	return len(p), websocket.Message.Send(c.ws, hex.EncodeToString(p))
}

func (c *wsClient) Close() error                       { return c.ws.Close() }
func (c *wsClient) LocalAddr() net.Addr                { return bridgeAddr{} }
func (c *wsClient) RemoteAddr() net.Addr               { return bridgeAddr{} }
func (c *wsClient) SetDeadline(t time.Time) error      { return c.ws.SetDeadline(t) }
func (c *wsClient) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsClient) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

// TestSparkConnectBridge: a Spark Connect call reaches the ACTIVE session
// whose UUID is its session_id, on the API's gRPC port and through the
// TCP-over-WebSocket bridge, and is refused for an unknown UUID.
func TestSparkConnectBridge(t *testing.T) {
	s, err := NewServer(Options{Engine: &fakeEngine{addr: echoBackend(t)}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	op, err := s.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: "bridge-1", Session: &dataprocpb.Session{
		SessionConfig: &dataprocpb.Session_SparkConnectSession{SparkConnectSession: &dataprocpb.SparkConnectConfig{}}}})
	if err != nil {
		t.Fatal(err)
	}
	ops := &operations{s: s}
	wait(t, func() bool { o, _ := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name}); return o.GetDone() })
	x, err := s.GetSession(ctx, &dataprocpb.GetSessionRequest{Name: parent + "/sessions/bridge-1"})
	if err != nil || x.State != dataprocpb.Session_ACTIVE {
		t.Fatalf("session: %v %v", x, err)
	}

	// The API's own gRPC port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer(s.ProxyOption())
	s.Register(g)
	go func() { _ = g.Serve(ln) }()
	defer g.Stop()
	conn, err := grpc.NewClient("passthrough:///"+ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got, err := callThrough(ctx, conn, x.Uuid); err != nil || !strings.Contains(got, x.Uuid) || !strings.Contains(got, "ExecutePlan") {
		t.Errorf("through the gRPC port: %q %v", got, err)
	}
	if _, err := callThrough(ctx, conn, "no-such-uuid"); status.Code(err) != codes.NotFound {
		t.Errorf("an unknown session_id: %v", err)
	}

	// The WebSocket bridge.
	hs := httptest.NewServer(s.Bridge(http.NotFoundHandler()))
	defer hs.Close()
	wsURL := "ws://" + strings.TrimPrefix(hs.URL, "http://") + BridgePath
	wconn, err := grpc.NewClient("passthrough:///bridge", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			cfg, err := websocket.NewConfig(wsURL, "http://localhost/")
			if err != nil {
				return nil, err
			}
			cfg.Header = http.Header{"Authorization": {"Bearer local"}}
			ws, err := websocket.DialConfig(cfg)
			if err != nil {
				return nil, err
			}
			return &wsClient{ws: ws}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer wconn.Close()
	for i := 0; i < 3; i++ {
		if got, err := callThrough(ctx, wconn, x.Uuid); err != nil || !strings.Contains(got, x.Uuid) {
			t.Fatalf("through the bridge, call %d: %q %v", i, got, err)
		}
	}
}
