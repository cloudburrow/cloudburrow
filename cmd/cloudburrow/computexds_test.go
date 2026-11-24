package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// TestComputePortTakesTLSBesidePlaintext (#2160): the Compute Engine
// port's listener hands a TLS handshake to TLS (the xDS API's gRPC over
// HTTP/2, verified against the load balancing CA) and plaintext HTTP to
// the JSON API, on one port.
func TestComputePortTakesTLSBesidePlaintext(t *testing.T) {
	api, err := compute.New(compute.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := api.XDSServerTLS([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	hs := health.NewServer()
	healthpb.RegisterHealthServer(g, hs)
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)
	protos.SetHTTP2(true)
	srv := &http.Server{Protocols: &protos, ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			g.ServeHTTP(w, r)
			return
		}
		_, _ = io.WriteString(w, "json")
	})}
	go func() { _ = srv.Serve(newTLSSniffListener(ln, cfg)) }()
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "json" {
		t.Fatalf("plaintext: %q", b)
	}
	caPEM, err := api.LBCACertificate()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(caPEM))
	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool})))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r, err := healthpb.NewHealthClient(cc).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil || r.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("gRPC over TLS: %v %v", r, err)
	}
}
