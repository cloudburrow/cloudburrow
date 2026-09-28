package storageserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
)

// The server started with the arguments its Deployment gets for `up
// --cors-allow-origin` answers that origin and loopback ones, and refuses
// any other (#677).
func TestStorageServerCORSAllowOrigin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	b := components.BuiltinStorageBackend("cb", "unused", false, false, nil, []string{"https://app.test:8443", "https://b.test"})
	if got := strings.Join(b.Args, " "); !strings.Contains(got, "--cors-allow-origin https://app.test:8443 --cors-allow-origin https://b.test") {
		t.Fatalf("Deployment args = %s", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, append(b.Args, "--listen", addr), io.Discard, io.Discard) }()
	defer func() {
		http.DefaultClient.CloseIdleConnections()
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	base := "http://" + addr
	for i := 0; ; i++ {
		resp, err := http.Get(base + "/storage/v1/b?project=p")
		if err == nil {
			resp.Body.Close()
			break
		}
		if i == 100 {
			t.Fatalf("the server did not start: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for origin, want := range map[string]int{
		"https://app.test:8443": 200, "https://b.test": 200, "http://localhost:5173": 200,
		"https://evil.example": 403, "https://app.test": 403,
	} {
		req, _ := http.NewRequest("GET", base+"/storage/v1/b?project=p", nil)
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		allow := resp.Header.Get("Access-Control-Allow-Origin")
		if resp.StatusCode != want || (want == 200) != (allow == origin) {
			t.Errorf("Origin %s = %d, Allow-Origin %q; want %d", origin, resp.StatusCode, allow, want)
		}
	}
	if err := Run(context.Background(), []string{"--cors-allow-origin", "https://*.example"}, io.Discard, io.Discard); err != ErrUsage {
		t.Errorf("a wildcard origin = %v, want a usage error", err)
	}
}
