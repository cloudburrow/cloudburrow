package main

import (
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestPinLocalAIPortHoldsThePortUntilTheEndpointServes: the port chosen
// before anything starts stays bound, so nothing else can take it before
// the local AI endpoint does (main run 45023269046: "bind local AI address
// 127.0.0.1:41367: bind: address already in use"), and the endpoint then
// serves that very listener.
func TestPinLocalAIPortHoldsThePortUntilTheEndpointServes(t *testing.T) {
	cfg := config.Config{BindAddress: "127.0.0.1"}
	if err := pinLocalAIPort(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoints.LocalAI == 0 {
		t.Fatal("no port chosen")
	}
	addr := net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.LocalAI))
	if l, err := net.Listen("tcp", addr); err == nil {
		_ = l.Close()
		t.Fatalf("%s was free for another process to bind before the endpoint started", addr)
	}
	srv := newLocalAIServer(addr, nil)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start on the held port: %v", err)
	}
	defer func() { _ = srv.Stop(context.Background()) }()
	if srv.Addr() != addr {
		t.Errorf("serving %s, want %s", srv.Addr(), addr)
	}
	if takeHeldPort(addr) != nil {
		t.Error("the held listener was handed out twice")
	}
}

// TestPinKMSPortHoldsThePort: Cloud KMS's early port is held the same way.
func TestPinKMSPortHoldsThePort(t *testing.T) {
	cfg := config.Config{BindAddress: "127.0.0.1"}
	if err := pinKMSPort(&cfg); err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.KMS))
	if l, err := net.Listen("tcp", addr); err == nil {
		_ = l.Close()
		t.Fatalf("%s was free for another process to bind before Cloud KMS started", addr)
	}
	l := takeHeldPort(addr)
	if l == nil {
		t.Fatal("no listener held")
	}
	_ = l.Close()
}
