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

// The JSON API on a shared port (Cloud KMS, Cloud Tasks, Cloud Scheduler,
// Cloud Logging and Cloud Run all serve one here) refuses a rebound
// attacker domain (#676); a gRPC client dialed under any name is still
// served, because cleartext HTTP/2 never comes from a browser.
func TestSharedPortRefusesForeignHostsOnJSONOnly(t *testing.T) {
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

	conn, err := grpc.NewClient(s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithAuthority("attacker.example:9003"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Errorf("gRPC with :authority attacker.example:9003 = %v, want served", err)
	}

	for host, want := range map[string]int{
		"attacker.example:9003": http.StatusMisdirectedRequest,
		"127.0.0.1:9003":        http.StatusOK,
		"cloudburrow-host.cloudburrow.svc.cluster.local:9003": http.StatusOK,
		"172.18.0.1:9003": http.StatusOK,
	} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/v1/projects/p/locations/l/keyRings", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("JSON with Host %s = %d, want %d", host, resp.StatusCode, want)
		}
	}
}
