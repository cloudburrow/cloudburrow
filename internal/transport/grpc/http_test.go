package grpc

import (
	"context"
	"io"
	"net/http"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// With a JSON API added, one port serves both: a gRPC client reaches the
// gRPC service and a plain HTTP client reaches the handler (#366).
func TestServeHTTPSharesThePortWithGRPC(t *testing.T) {
	ctx := context.Background()
	s := New("127.0.0.1:0")
	s.ServeHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "json") }))
	if err := s.Register(func(g *grpc.Server) { healthpb.RegisterHealthServer(g, health.NewServer()) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Stop(ctx)

	conn, err := grpc.NewClient(s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("gRPC health check = %v, %v", resp, err)
	}
	resp, err := http.Get("http://" + s.Addr() + "/v2/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "json" {
		t.Errorf("HTTP reached %q, want the JSON handler", b)
	}
}
