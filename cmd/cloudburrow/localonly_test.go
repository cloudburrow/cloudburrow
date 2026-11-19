package main

import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"testing"

	"cloud.google.com/go/bigtable"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
)

// The switches that keep the console's client libraries off Google's hosts
// (#1825); TestNoGoogleEgress (integration) proves them on a running
// instance. Each is a default of the library that reaches Google whatever
// endpoint the client is given.
func TestConsoleBigtableConfigIsLocalOnly(t *testing.T) {
	if _, ok := consoleBigtableConfig.MetricsProvider.(bigtable.NoopMetricsProvider); !ok {
		t.Errorf("MetricsProvider = %T, want NoopMetricsProvider: the built-in exporter sends to Cloud Monitoring", consoleBigtableConfig.MetricsProvider)
	}
	if !consoleBigtableConfig.DisableDirectAccess {
		t.Error("DisableDirectAccess is false: the Direct Access check probes the GCE metadata server")
	}
}

func TestSpannerLocalOnlySetsTheEmulatorHost(t *testing.T) {
	t.Setenv("SPANNER_EMULATOR_HOST", "")
	spannerLocalOnly("127.0.0.1:9010")
	if got := os.Getenv("SPANNER_EMULATOR_HOST"); got != "127.0.0.1:9010" {
		t.Errorf("SPANNER_EMULATOR_HOST = %q, want the endpoint: unset, every Spanner client probes the metadata server", got)
	}
	// One the process already has is kept.
	spannerLocalOnly("127.0.0.1:1")
	if got := os.Getenv("SPANNER_EMULATOR_HOST"); got != "127.0.0.1:9010" {
		t.Errorf("SPANNER_EMULATOR_HOST = %q, want the one already set", got)
	}
}

// countingListener counts the connections it accepts.
type countingListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

// TestConsoleBigtableClientHasNoChannelPool (#1828): a console data client
// runs on the one connection the console dials, never on a channel pool of
// the library's. The pool is what runs the Direct Access check, which
// CBT_ENABLE_DIRECTPATH in the environment turns on whatever the client's
// configuration says, and which asks the GCE metadata server. (The variable
// is not set here: were the fix undone, this test would make that probe.
// TestNoGoogleEgress sets it, under the egress guard.)
func TestConsoleBigtableClientHasNoChannelPool(t *testing.T) {
	gs := grpc.NewServer()
	btpb.RegisterBigtableServer(gs, &sessionlessBigtable{})
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &countingListener{Listener: inner}
	go func() { _ = gs.Serve(l) }()
	t.Cleanup(gs.Stop)

	ctx := context.Background()
	c, err := newConsoleBigtableClient(ctx, l.Addr().String(), "demo", "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open("events").ReadRow(ctx, "r1"); err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := l.accepted.Load(); n != 1 {
		t.Errorf("the client made %d connections, want 1: more is the library's channel pool, whose Direct Access check CBT_ENABLE_DIRECTPATH turns on", n)
	}
}
