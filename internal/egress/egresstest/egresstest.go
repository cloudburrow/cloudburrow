// Package egresstest is the loopback-only sink a dispatcher's egress test
// sends through (#2015), the in-process form of test/compat's egress
// container (testdata/egress): every dial to an address that is not
// loopback is counted and refused before any packet is sent, and no name is
// ever resolved, so a zero count means nothing left the machine.
package egresstest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Control is the address the control attempt dials (TEST-NET-1, RFC 5737).
const Control = "192.0.2.1:443"

// Sink counts dial attempts.
type Sink struct {
	mu       sync.Mutex
	attempts []string
	local    []string
}

// New is a sink whose control attempt has been counted: a sink that does
// not count it would prove nothing, and the test fails.
func New(t testing.TB) *Sink {
	t.Helper()
	s := &Sink{}
	resp, err := (&http.Client{Transport: s.Transport()}).Get("https://" + Control + "/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the control request was answered")
	}
	if got := s.Attempts(); len(got) != 1 || got[0] != "tcp "+Control {
		t.Fatalf("the control attempt was not counted (%v), so the count proves nothing", got)
	}
	s.mu.Lock()
	s.attempts = nil
	s.mu.Unlock()
	return s
}

// Transport sends through the sink: no proxy, loopback only.
func (s *Sink) Transport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: s.DialContext, ForceAttemptHTTP2: false,
		TLSHandshakeTimeout: 5 * time.Second}
}

// DialContext dials a loopback address, and counts and refuses any other,
// a name included, without resolving it.
func (s *Sink) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		s.mu.Lock()
		s.attempts = append(s.attempts, network+" "+addr)
		s.mu.Unlock()
		return nil, fmt.Errorf("egresstest: refused a dial off the machine to %s", addr)
	}
	s.mu.Lock()
	s.local = append(s.local, network+" "+addr)
	s.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, network, net.JoinHostPort(host, port))
}

// Attempts are the dials off the machine counted so far.
func (s *Sink) Attempts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.attempts...)
}

// Local are the loopback dials made so far.
func (s *Sink) Local() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.local...)
}

// RequireNone fails t when any dial off the machine was attempted.
func (s *Sink) RequireNone(t testing.TB) {
	t.Helper()
	if a := s.Attempts(); len(a) > 0 {
		t.Errorf("%d connection attempts off the machine (stopped by the sink): %v", len(a), a)
	}
}

// Recorder is a local server standing in for a served API; it records the
// requests it gets.
type Recorder struct {
	mu   sync.Mutex
	reqs []string
}

// Handler answers 200 and records "METHOD PATH host=HOST".
func (r *Recorder) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.reqs = append(r.reqs, req.Method+" "+req.URL.Path+" host="+req.Host)
		r.mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	})
}

// Requests are those recorded.
func (r *Recorder) Requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reqs...)
}
