package networkservices

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeInspectPolicies struct {
	fakePolicies
	pool string
}

func (f *fakeInspectPolicies) TLSInspectionCAPool(string) (string, bool) { return f.pool, f.pool != "" }

// testCA is a CA pool's CA, signing CSRs as Certificate Authority Service
// would.
type testCA struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	pem   string
	calls atomic.Int32
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{cert: c, key: k, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func (ca *testCA) issue(_ context.Context, pool, csrPEM string, lifetime time.Duration) ([]string, error) {
	if pool != "projects/p/locations/us-central1/caPools/swp" {
		return nil, errors.New("wrong pool " + pool)
	}
	ca.calls.Add(1)
	b, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: csr.Subject, DNSNames: csr.DNSNames,
		IPAddresses: csr.IPAddresses, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(lifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), ca.pem}, nil
}

// TestSecureWebProxyTLSInspection: a CONNECT whose first session match
// asks for inspection is decrypted with a leaf from the CA pool, each
// request inside is matched with its attributes (an applicationMatcher on
// the path denies /secret with 403 inside the tunnel), an allowed one
// reaches the local TLS origin re-encrypted, and a host whose rule does
// not inspect is tunnelled untouched. Every target is on loopback.
func TestSecureWebProxyTLSInspection(t *testing.T) {
	// The origin's certificate, for localhost and 127.0.0.1, from a CA of
	// its own.
	originCA := newTestCA(t, "origin CA")
	ok, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "origin"}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	oder, err := x509.CreateCertificate(rand.Reader, otpl, originCA.cert, &ok.PublicKey, originCA.key)
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "origin "+r.URL.Path)
	}))
	origin.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{oder}, PrivateKey: ok}}}
	origin.StartTLS()
	defer origin.Close()
	ou, _ := url.Parse(origin.URL)
	_, port, _ := net.SplitHostPort(ou.Host)
	ca := newTestCA(t, "swp test CA")
	pols := &fakeInspectPolicies{pool: "projects/p/locations/us-central1/caPools/swp", fakePolicies: fakePolicies{rules: map[string][]GatewayRule{pol: {
		{Name: "deny-secret", Priority: 1, Enabled: true, Allow: false, TLSInspection: true, Session: "host() == 'localhost'",
			Application: "request.path.startsWith('/secret')"},
		{Name: "inspect", Priority: 2, Enabled: true, Allow: true, TLSInspection: true, Session: "host() == 'localhost'"},
		{Name: "plain", Priority: 3, Enabled: true, Allow: true, Session: "host() == '127.0.0.1'"},
	}}}}
	s, addr := newGateway(t, pols)
	s.UseLeafIssuer(ca.issue)
	roots := x509.NewCertPool()
	roots.AddCert(originCA.cert)
	s.UseUpstreamRoots(roots)

	pu, _ := url.Parse("http://" + addr)
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(ca.cert)
	var leafIssuer string
	tr := &http.Transport{Proxy: http.ProxyURL(pu)}
	tr.TLSClientConfig = clientTLS(clientRoots, func(cs []*x509.Certificate) { leafIssuer = cs[0].Issuer.CommonName })
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	get := func(u string) (int, string) {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("https://localhost:" + port + "/hello"); code != 200 || body != "origin /hello" {
		t.Errorf("inspected /hello: %d %q", code, body)
	}
	if leafIssuer != "swp test CA" {
		t.Errorf("the client saw a leaf issued by %q, want the CA pool's CA", leafIssuer)
	}
	if code, body := get("https://localhost:" + port + "/secret/x"); code != 403 || body != "Forbidden\n" {
		t.Errorf("inspected /secret/x: %d %q, want 403", code, body)
	}
	if n := ca.calls.Load(); n != 1 {
		t.Errorf("%d leaves issued for one host, want 1 (cached)", n)
	}

	// 127.0.0.1's rule does not inspect: the origin's own certificate
	// reaches the client.
	plain := &http.Transport{Proxy: http.ProxyURL(pu)}
	var direct string
	plain.TLSClientConfig = clientTLS(roots, func(cs []*x509.Certificate) { direct = cs[0].Issuer.String() })
	resp, err := (&http.Client{Transport: plain, Timeout: 10 * time.Second}).Get("https://127.0.0.1:" + port + "/secret")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(direct, "swp test CA") {
		t.Errorf("uninspected tunnel: %d, issuer %q", resp.StatusCode, direct)
	}

	// An origin whose certificate the CA set does not hold: 502.
	s.UseUpstreamRoots(x509.NewCertPool())
	tr.CloseIdleConnections()
	if code, _ := get("https://localhost:" + port + "/again"); code != 502 {
		t.Errorf("an untrusted origin: %d, want 502", code)
	}

	// Without a CA Service issuer, an inspecting rule refuses the tunnel.
	s.UseLeafIssuer(nil)
	cn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cn.Close() }()
	_, _ = io.WriteString(cn, "CONNECT localhost:"+port+" HTTP/1.1\r\nHost: localhost\r\n\r\n")
	_ = cn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 512)
	n, _ := cn.Read(b)
	if !strings.HasPrefix(string(b[:n]), "HTTP/1.1 403") {
		t.Errorf("no issuer: %q, want 403", b[:n])
	}
}

// clientTLS trusts roots and reports the server's chain.
func clientTLS(roots *x509.CertPool, seen func([]*x509.Certificate)) *tls.Config {
	return &tls.Config{RootCAs: roots, VerifyConnection: func(cs tls.ConnectionState) error {
		seen(cs.PeerCertificates)
		return nil
	}}
}
