package hostrelay

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A relayed connection reaches the target and carries its answer back, and
// Close ends live connections rather than leaving them open.
func TestRelayCarriesAndCloses(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello " + r.URL.Path))
	}))
	defer target.Close()
	r := &Relay{Listen: "127.0.0.1:0", Target: strings.TrimPrefix(target.URL, "http://")}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + r.Addr() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	resp.Body.Close()
	if got := string(b[:n]); got != "hello /x" {
		t.Errorf("through the relay: %q", got)
	}

	// A connection held open is ended by Close.
	c, err := net.Dial("tcp", r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung on an open connection")
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := bufio.NewReader(c).ReadByte(); err == nil {
		t.Error("the held connection is still open after Close")
	}
	if _, err := net.DialTimeout("tcp", r.Addr(), 200*time.Millisecond); err == nil && r.Addr() != "" {
		t.Error("the relay still accepts after Close")
	}
}

// A target that refuses ends the relayed connection rather than holding it.
func TestRelayToADeadTargetEndsTheConnection(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	r := &Relay{Listen: "127.0.0.1:0", Target: dead}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c, err := net.Dial("tcp", r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Error("a connection to a dead target stayed open")
	}
}
