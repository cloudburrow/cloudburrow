package callfront

import (
	"context"
	"net"
	"net/http"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/grpchttp"
	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// grpcIntercept answers every call with a gRPC server of its own, served
// through ServeHTTP, as the Firestore Admin API, the query fronts and the
// Spanner front do.
type grpcIntercept struct{ g *grpc.Server }

func (i grpcIntercept) Claims(*http.Request) bool                        { return true }
func (i grpcIntercept) ServeHTTP(w http.ResponseWriter, r *http.Request) { i.g.ServeHTTP(w, r) }
func (i grpcIntercept) Stop(ctx context.Context) error                   { return grpchttp.Stop(ctx, i.g) }

// TestServeAllWaitsForInterceptsCallEnd: once ctx ends, the front returns
// only after every call an intercept answered has ended for the
// intercept's stats handlers (#1804). Its gRPC server ends a call on a
// goroutine of its own, after ServeHTTP has returned, so the HTTP server's
// Shutdown alone does not wait for it.
func TestServeAllWaitsForInterceptsCallEnd(t *testing.T) {
	var h grpchttptest.SlowEnd
	g := grpchttp.NewServer(h.Option())
	healthpb.RegisterHealthServer(g, health.NewServer())
	icpts := Intercepts{grpcIntercept{g}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := Server(HandlerWith("127.0.0.1:1", &calllog.Ring{}, t.Logf, icpts), t.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serveAll(ctx, []*http.Server{srv}, []net.Listener{l}, icpts) }()
	if err := grpchttptest.Call(context.Background(), l.Addr().String(), "/grpc.health.v1.Health/Check"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if !h.Ended() {
		t.Fatalf("the front returned before the call's stats End (%d calls)", h.Calls())
	}
}
