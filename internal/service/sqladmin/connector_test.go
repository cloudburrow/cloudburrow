package sqladmin

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"golang.org/x/oauth2"
)

// echoEngine is the fake engine with its address moved to an echo server.
type echoEngine struct {
	*fake
	addr string
}

func (e echoEngine) Addr() string { return e.addr }

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// TestConnectorDialsThroughTheListener drives the official Go connector
// (cloud.google.com/go/cloudsqlconn) end to end: connectSettings,
// generateEphemeralCert, the TLS 1.3 handshake with the ephemeral client
// certificate, and the forwarded connection to the engine.
func TestConnectorDialsThroughTheListener(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer("127.0.0.1:0", echoEngine{newFake(), echoServer(t)})
	if err := srv.EnableConnector("127.0.0.1:0", dir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(ctx)
	connAddr := srv.conn.ln.Addr().String()

	d, err := cloudsqlconn.NewDialer(ctx,
		cloudsqlconn.WithAdminAPIEndpoint("http://"+srv.Addr()+"/"),
		cloudsqlconn.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "unused", Expiry: time.Now().Add(time.Hour)})),
		// The connector dials port 3307; this test's listener is elsewhere.
		cloudsqlconn.WithDialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !strings.HasSuffix(addr, ":3307") {
				t.Errorf("connector dialed %s, want port 3307", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, connAddr)
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, err := d.Dial(ctx, "proj:us-central1:pg")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("SELECT 1\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "SELECT 1\n" {
		t.Fatalf("echo through the connector: %q, %v", line, err)
	}

	// The CA is kept, so a restart presents the same one.
	if _, err := os.Stat(filepath.Join(dir, "cloudsql-pg-server-ca.json")); err != nil {
		t.Fatal(err)
	}
	again := NewServer("", newFake())
	if err := again.EnableConnector("127.0.0.1:0", dir); err != nil {
		t.Fatal(err)
	}
	if !again.conn.cas["pg"].cert.Equal(srv.conn.cas["pg"].cert) {
		t.Error("the server CA changed after a restart")
	}
}

// TestConnectorRefusesIAMAuthAndBadKeys: an access_token is IAM database
// authentication, which is 501; a key that is not PEM is 400.
func TestConnectorRefusesIAMAuthAndBadKeys(t *testing.T) {
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]int{
		`{"public_key":"x","access_token":"t"}`: http.StatusNotImplemented,
		`{"public_key":"not pem"}`:              http.StatusBadRequest,
	} {
		req, _ := http.NewRequest(http.MethodPost, "/v1/projects/p/instances/pg:generateEphemeralCert", strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", body, rec.Code, want)
		}
	}
	// No listener bound: the connector methods say so rather than hand out
	// an address nothing answers on.
	srv.conn.unavailable = "port in use"
	req, _ := http.NewRequest(http.MethodGet, "/v1/projects/p/instances/pg/connectSettings", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("connectSettings without a listener: %d, want 503", rec.Code)
	}
}

func TestParseDNSName(t *testing.T) {
	p, i, ok := parseDNSName(dnsName("my-proj", "postgres") + ".")
	if !ok || p != "my-proj" || i != "postgres" {
		t.Errorf("parseDNSName: %q %q %v", p, i, ok)
	}
	if _, _, ok := parseDNSName("postgres.example.com"); ok {
		t.Error("parseDNSName accepted a foreign name")
	}
}
