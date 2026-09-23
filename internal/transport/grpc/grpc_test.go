package grpc

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// registerHealth gives the server a real service so it has something to serve.
func registerHealth(t *testing.T, s *Server) {
	t.Helper()
	if err := s.Register(func(g *grpc.Server) {
		healthpb.RegisterHealthServer(g, health.NewServer())
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

// A server with no services would accept connections and answer everything
// Unimplemented, which looks like a broken deployment rather than an
// unconfigured one.
func TestStartRequiresAtLeastOneService(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0")
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start() = nil with no services registered")
	}
}

// gRPC does not permit registration on a serving server; allowing it would
// silently drop the service.
func TestRegisterAfterStartIsRefused(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0")
	registerHealth(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	err := s.Register(func(*grpc.Server) {})
	if err == nil {
		t.Error("Register() succeeded after Start; the service would have been dropped")
	}
}

func TestServesAndReportsAddress(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0")
	registerHealth(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	addr := s.Addr()
	if addr == "" {
		t.Fatal("Addr() is empty after Start")
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING", resp.Status)
	}
}

// An unknown method must map to Unimplemented, not to a fabricated response.
func TestUnknownMethodIsUnimplemented(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0")
	registerHealth(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	conn, err := grpc.NewClient(s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = conn.Invoke(ctx, "/cloudburrow.NoSuchService/NoSuchMethod", &healthpb.HealthCheckRequest{}, &healthpb.HealthCheckResponse{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("unknown method = %v, want Unimplemented", status.Code(err))
	}
}

// An unmapped error reaching a client as Unknown tells the caller nothing
// about whether to retry, so the interceptor must map it.
func TestErrorInterceptorMapsInternalErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"not found", apierror.NotFound("gone"), codes.NotFound},
		{"invalid", apierror.InvalidArgument("bad"), codes.InvalidArgument},
		{"already exists", apierror.AlreadyExists("dup"), codes.AlreadyExists},
		{"plain error becomes internal", errors.New("boom"), codes.Internal},
		{"existing status passes through", status.Error(codes.PermissionDenied, "no"), codes.PermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := errorInterceptor(context.Background(), nil, nil,
				func(context.Context, any) (any, error) { return nil, tt.err })
			if got := status.Code(err); got != tt.want {
				t.Errorf("code = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestErrorInterceptorPassesSuccessThrough(t *testing.T) {
	t.Parallel()
	resp, err := errorInterceptor(context.Background(), nil, nil,
		func(context.Context, any) (any, error) { return "ok", nil })
	if err != nil || resp != "ok" {
		t.Errorf("interceptor altered a successful call: %v, %v", resp, err)
	}
}

// An occupied port must fail with an actionable error.
func TestStartOnOccupiedPort(t *testing.T) {
	t.Parallel()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	s := New(blocker.Addr().String())
	registerHealth(t, s)
	err = s.Start(context.Background())
	if err == nil {
		t.Fatal("Start() = nil on an occupied port")
	}
	if !strings.Contains(err.Error(), blocker.Addr().String()) {
		t.Errorf("error should name the address, got: %v", err)
	}
}

// Stop must stop serving.
//
// This asserts that the address no longer answers, rather than trying to
// rebind it. Rebinding races with every other parallel test in this package:
// once Stop frees the port, another test's OS-assigned port can be the same
// one, and the rebind then fails for a reason that has nothing to do with
// Stop. That is exactly how this test flaked in CI.
func TestStopStopsServing(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0")
	registerHealth(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	addr := s.Addr()

	// It answers before Stop, so the assertion afterwards means something.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("server did not answer before Stop: %v", err)
	}
	_ = conn.Close()

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A fresh connection must fail to complete an RPC.
	after, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return // dial setup refused outright, which is also a stopped server
	}
	defer after.Close()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if _, err := healthpb.NewHealthClient(after).Check(stopCtx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Error("server still answering after Stop")
	}
}

func TestStopBeforeStartIsSafe(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0")
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop() before Start = %v, want nil", err)
	}
}
