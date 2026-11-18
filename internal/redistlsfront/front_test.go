package redistlsfront

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/frontready"
)

func TestRelayPort(t *testing.T) {
	for server, want := range map[string]int{"redis-a": RelayBase, "redis-a-n0": RelayBase + 1, "redis-a-n5": RelayBase + 6} {
		if got, ok := RelayPort(server); !ok || got != want {
			t.Errorf("RelayPort(%q) = %d, %v, want %d", server, got, ok, want)
		}
	}
	if _, ok := RelayPort("redis-a-n64"); ok {
		t.Error("RelayPort of a node past RelayNodes is ok")
	}
	if Base("redis-a-n3") != "redis-a" || Base("redis-a") != "redis-a" {
		t.Error("Base")
	}
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, pool
}

// TestFront: a TLS client trusting the CA reaches the upstream through
// the front, a plain one does not, and the readiness path answers in
// plain HTTP by the upstream's state.
func TestFront(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	cert, pool := selfSigned(t)
	f := &Front{Upstream: up.Addr().String(), Server: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, Roots: pool}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go f.Serve(ln)

	tc, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tc.Write([]byte("PING\r\n"))
	line, err := bufio.NewReader(tc).ReadString('\n')
	if err != nil || line != "PING\r\n" {
		t.Errorf("through the front: %q, %v", line, err)
	}
	tc.Close()

	if _, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{MinVersion: tls.VersionTLS12}); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("a client without the CA: %v", err)
	}
	pc, _ := net.Dial("tcp", ln.Addr().String())
	_, _ = pc.Write([]byte("*1\r\n$4\r\nPING\r\n"))
	_ = pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	if b, _ := io.ReadAll(pc); strings.Contains(string(b), "PING") {
		t.Errorf("a plain client reached the upstream: %q", b)
	}
	pc.Close()

	res, err := http.Get("http://" + ln.Addr().String() + frontready.Path)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("readiness: %v, %v", res, err)
	}
	res.Body.Close()
	up.Close()
	res, err = http.Get("http://" + ln.Addr().String() + frontready.Path)
	if err != nil || res.StatusCode != 503 {
		t.Errorf("readiness with the upstream down: %v, %v", res, err)
	}

	// A relay forwards plain TCP over TLS to a front, verifying the CA.
	rl, _ := net.Listen("tcp", "127.0.0.1:0")
	defer rl.Close()
	up2, _ := net.Listen("tcp", "127.0.0.1:0")
	defer up2.Close()
	go func() {
		c, err := up2.Accept()
		if err == nil {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}
	}()
	f.Upstream = up2.Addr().String()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f.PeerPort, _ = strconv.Atoi(port)
	go f.ServeRelay(rl, "127.0.0.1")
	rc, err := net.Dial("tcp", rl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, _ = rc.Write([]byte("SYNC\r\n"))
	_ = rc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(rc).ReadString('\n'); err != nil || line != "SYNC\r\n" {
		t.Errorf("through the relay: %q, %v", line, err)
	}
}
