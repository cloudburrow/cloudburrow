package storageserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/pstest"
)

// recordingDNS stands in for every DNS server: it records the name each
// query asks for and answers none.
type recordingDNS struct {
	mu    sync.Mutex
	names []string
}

func (d *recordingDNS) dial(context.Context, string, string) (net.Conn, error) {
	return &dnsConn{d: d}, nil
}

type dnsConn struct {
	net.Conn
	d *recordingDNS
}

// Write reads the query's QNAME: labels after the 12-byte header (and the
// 2-byte length a TCP query carries).
func (c *dnsConn) Write(b []byte) (int, error) {
	for _, off := range []int{12, 14} {
		if name := qname(b, off); name != "" {
			c.d.mu.Lock()
			c.d.names = append(c.d.names, name)
			c.d.mu.Unlock()
			break
		}
	}
	return len(b), nil
}

func (c *dnsConn) Read([]byte) (int, error)         { return 0, errors.New("no DNS here") }
func (c *dnsConn) Close() error                     { return nil }
func (c *dnsConn) SetDeadline(time.Time) error      { return nil }
func (c *dnsConn) SetReadDeadline(time.Time) error  { return nil }
func (c *dnsConn) SetWriteDeadline(time.Time) error { return nil }
func (c *dnsConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *dnsConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }

var label = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func qname(b []byte, off int) string {
	var parts []string
	for off < len(b) {
		n := int(b[off])
		if n == 0 {
			return strings.Join(parts, ".")
		}
		if off+1+n > len(b) || !label.Match(b[off+1:off+1+n]) {
			return ""
		}
		parts = append(parts, string(b[off+1:off+1+n]))
		off += 1 + n
	}
	return ""
}

// The builtin server reaches nothing but the Pub/Sub emulator it is given
// (#514): through a whole workload (buckets, objects, a resumable upload,
// XML, a signed URL, notifications and lifecycle) it resolves no name at
// all, so nothing dials *.googleapis.com. The Deployment's NetworkPolicy
// declares the same, where a CNI enforces it.
func TestStorageNoEgress(t *testing.T) {
	dns := &recordingDNS{}
	saved := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: dns.dial}
	t.Cleanup(func() { net.DefaultResolver = saved })
	// Control: the guard sees a lookup, so an empty record means none.
	_, _ = net.DefaultResolver.LookupHost(context.Background(), "storage.googleapis.com")
	dns.mu.Lock()
	seen := strings.Join(dns.names, ",")
	dns.names = nil
	dns.mu.Unlock()
	if !strings.Contains(seen, "storage.googleapis.com") {
		t.Fatalf("the guard missed a lookup of storage.googleapis.com (saw %q); the test would prove nothing", seen)
	}

	ps := pstest.NewServer()
	defer ps.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- Run(ctx, []string{"--listen", addr, "--pubsub-emulator", ps.Addr}, &out, io.Discard) }()
	base := "http://" + addr
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(base + "/storage/v1/b?project=p"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	do := func(method, path, body string, hdr map[string]string) *http.Response {
		t.Helper()
		u := path
		if strings.HasPrefix(path, "/") {
			u = base + path
		}
		req, _ := http.NewRequest(method, u, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	do("POST", "/storage/v1/b?project=p", `{"name":"egress","versioning":{"enabled":true},"lifecycle":{"rule":[{"action":{"type":"Delete"},"condition":{"age":0}}]}}`, nil)
	do("POST", "/storage/v1/b/egress/notificationConfigs", `{"topic":"projects/p/topics/t"}`, nil)
	do("POST", "/upload/storage/v1/b/egress/o?uploadType=media&name=a", "x", nil)
	do("PATCH", "/storage/v1/b/egress/o/a", `{"metadata":{"k":"v"}}`, nil)
	r := do("POST", "/upload/storage/v1/b/egress/o?uploadType=resumable&name=r", "{}", nil)
	do("PUT", r.Header.Get("Location"), "0123456789", map[string]string{"Content-Range": "bytes 0-9/10"})
	do("PUT", "/egress/x.txt", "xml", nil)
	do("GET", "/egress/x.txt?X-Goog-Algorithm=GOOG4-HMAC-SHA256&X-Goog-Credential=GOOGX/20260926/auto/storage/goog4_request&X-Goog-Date=20260926T000000Z&X-Goog-Expires=60&X-Goog-SignedHeaders=host&X-Goog-Signature=00", "", nil)
	do("POST", "/_cloudburrow/lifecycle", "", nil)
	do("DELETE", "/storage/v1/b/egress/o/a", "", nil)
	time.Sleep(300 * time.Millisecond) // let the dispatcher publish
	// A connection the client dialled but never used is StateNew to the
	// server, which Shutdown counts as idle only after 5 s, its whole
	// budget: close them, or a slow (-race) run times out on shutdown.
	http.DefaultClient.CloseIdleConnections()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	dns.mu.Lock()
	defer dns.mu.Unlock()
	if len(dns.names) != 0 {
		t.Errorf("the server resolved %v; it must reach nothing but the Pub/Sub emulator it was given", dns.names)
	}
	if !strings.Contains(out.String(), "listening on") {
		t.Errorf("the server did not start: %s", out.String())
	}
}
