package main

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

func notifyFixture(t *testing.T, port int) *notifyService {
	t.Helper()
	cfg := config.Default()
	cfg.BindAddress = "127.0.0.1"
	cfg.Endpoints.Storage = port
	cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub}
	n := newNotifyService(cfg, nil)
	if n == nil {
		t.Fatal("storage and pubsub are both enabled, so the service must exist")
	}
	return n
}

// TestStorageFrontAdvertisesTheAddressItServes.
//
// The storage backend is told which Host its clients will send, and matches it
// against the download path the official client reads objects through. That
// value was built from the configured port, so with --port-storage 0 the
// backend expected 127.0.0.1:0 while the handler served some other port — and
// every object read through the SDK 404'd, on every main CI run for two days,
// while uploads kept working. The address advertised must be the one bound.
func TestStorageFrontAdvertisesTheAddressItServes(t *testing.T) {
	n := notifyFixture(t, 0)
	defer n.releaseUnstarted()

	addr, err := n.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(addr, ":0") {
		t.Fatalf("Listen returned %q: the configured port, not the bound one", addr)
	}
	// The advertised address is actually being listened on.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("nothing is listening on the advertised address %s: %v", addr, err)
	}
	_ = conn.Close()
	if got := n.Addr(); got != addr {
		t.Errorf("Addr() = %q, Listen returned %q: two answers to one question", got, addr)
	}
	// Idempotent, so Start reuses the listener rather than binding a second port
	// the backend was never told about.
	again, err := n.Listen(context.Background())
	if err != nil || again != addr {
		t.Errorf("a second Listen = %q, %v; want the same %q", again, err, addr)
	}
}

// TestAnUnstartedStorageFrontReleasesItsPort.
//
// `up` binds before the coordinator runs, and a failure in between means Start
// never runs. Without the release the port stays bound for the life of the
// process.
func TestAnUnstartedStorageFrontReleasesItsPort(t *testing.T) {
	n := notifyFixture(t, 0)
	addr, err := n.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n.releaseUnstarted()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port %s is still held after release: %v", addr, err)
	}
	_ = ln.Close()
	if n.Addr() != "" {
		t.Error("Addr() still reports a released listener")
	}
}

// TestAStartedStorageFrontKeepsItsPort.
//
// The release must not touch a listener Start owns, or `up`'s deferred release
// would close the live storage endpoint when it returns.
func TestAStartedStorageFrontKeepsItsPort(t *testing.T) {
	n := notifyFixture(t, 0)
	addr, err := n.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.ln.Close() }()
	n.mu.Lock()
	n.started = true
	n.mu.Unlock()

	n.releaseUnstarted()
	if n.Addr() != addr {
		t.Fatalf("release closed a listener Start owns: Addr() = %q", n.Addr())
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("the started endpoint stopped accepting: %v", err)
	}
	_ = conn.Close()
}
