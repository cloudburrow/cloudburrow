package grpc

import (
	"context"
	"io"
	"net/http"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// TestStopWaitsForCallEnd: Stop returns only once every call has ended for
// the server's stats handlers, where tracing ends the call's span (#1159,
// #1804), on the shared port's both transports: gRPC alone, and gRPC
// through ServeHTTP beside a JSON API, which ends a call on a goroutine of
// its own after the answer is written.
func TestStopWaitsForCallEnd(t *testing.T) {
	for _, withJSON := range []bool{false, true} {
		ctx := context.Background()
		var h grpchttptest.SlowEnd
		s := New("127.0.0.1:0")
		s.ServerOptions(h.Option())
		if withJSON {
			s.ServeHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "json") }))
		}
		if err := s.Register(func(g *grpc.Server) { healthpb.RegisterHealthServer(g, health.NewServer()) }); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if err := grpchttptest.Call(ctx, s.Addr(), "/grpc.health.v1.Health/Check"); err != nil {
			t.Fatal(err)
		}
		_ = s.Stop(ctx)
		if !h.Ended() {
			t.Errorf("with a JSON API %v: Stop returned before the call's stats End (%d calls)", withJSON, h.Calls())
		}
	}
}
