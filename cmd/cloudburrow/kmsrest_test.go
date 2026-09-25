package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The KMS port serves JSON beside gRPC: an HTTP/2 prior-knowledge JSON
// request reaches the JSON router, not the gRPC server, and is observed for
// /admin/events (#414).
func TestKMSPortServesJSONBesideGRPC(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceKMS}
	cfg.Endpoints.KMS = 0
	var mu sync.Mutex
	var seen []rest.Request
	svc := &kmsService{cfg: cfg, db: store.NewMemory(), requests: func(r rest.Request) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
	}}
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop(ctx)
	h2c := &http.Client{Timeout: 5 * time.Second, Transport: &http2.Transport{AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, n, a string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, a)
		}}}
	for name, cl := range map[string]*http.Client{"HTTP/1.1": http.DefaultClient, "HTTP/2 prior knowledge": h2c} {
		resp, err := cl.Post("http://"+svc.Addr()+"/v1/projects/p/locations/global/keyRings/r/cryptoKeys/k:encrypt", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d %s, want a 501 JSON answer from the router", name, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0].Status != http.StatusNotImplemented {
		t.Errorf("observed %+v, want both JSON requests with status 501", seen)
	}
}
