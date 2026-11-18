package grpchttp_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp"
	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// serve serves g through ServeHTTP over h2c, as the servers that use this
// package do, and returns its address and its HTTP server.
func serve(t *testing.T, g *grpc.Server) (string, *http.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: h2c.NewHandler(g, &http2.Server{}), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	return ln.Addr().String(), hs
}

// Stop returns only once the call's stats handler has seen it end: a
// server's ServeHTTP returns once the answer is written, and its handler
// goroutine ends the call after that (#1804).
func TestStopWaitsForCallEnd(t *testing.T) {
	ctx := context.Background()
	var h grpchttptest.SlowEnd
	g := grpchttp.NewServer(h.Option())
	healthpb.RegisterHealthServer(g, health.NewServer())
	addr, hs := serve(t, g)
	if err := grpchttptest.Call(ctx, addr, "/grpc.health.v1.Health/Check"); err != nil {
		t.Fatal(err)
	}
	_ = hs.Shutdown(ctx)
	if err := grpchttp.Stop(ctx, g); err != nil {
		t.Fatal(err)
	}
	if !h.Ended() {
		t.Fatalf("Stop returned before the call's stats End (%d calls)", h.Calls())
	}
}

// Stop is bounded by its context: a handler that never returns does not
// hold it.
func TestStopEndsWithContext(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	g := grpchttp.NewServer(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
		close(started)
		<-release
		return nil
	}))
	addr, hs := serve(t, g)
	defer hs.Close()
	go func() { _ = grpchttptest.Call(context.Background(), addr, "/any.Service/Method") }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := grpchttp.Stop(ctx, g); err == nil {
		t.Fatal("Stop = nil with a handler still running, want the context's error")
	}
}
