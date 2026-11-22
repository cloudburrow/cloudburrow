package compute

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNetSec serves one server TLS policy and denies requests whose path
// starts with /deny.
type fakeNetSec struct {
	mu  sync.Mutex
	pol ServerTLSPolicy
}

func (f *fakeNetSec) PolicyExists(name string) bool {
	return strings.HasPrefix(name, "projects/p/locations/global/")
}
func (f *fakeNetSec) ServerTLS(string) (ServerTLSPolicy, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pol, true
}
func (f *fakeNetSec) Authorize(r AuthzRequest) bool { return !strings.HasPrefix(r.Path, "/deny") }

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, cn string) testCA {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: cn}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return testCA{c, k}
}

func (ca testCA) client(t *testing.T, eku x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse("spiffe://example.org/ns/client")
	tpl := &x509.Certificate{SerialNumber: big.NewInt(0x2a), Subject: pkix.Name{CommonName: "client"}, URIs: []*url.URL{u}, DNSNames: []string{"client.example"},
		ExtKeyUsage: []x509.ExtKeyUsage{eku}, KeyUsage: x509.KeyUsageDigitalSignature, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &k.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k, Leaf: leaf}
}

func TestFrontendMutualTLSAndAuthorization(t *testing.T) {
	ca, other := newTestCA(t, "root"), newTestCA(t, "other")
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	ns := &fakeNetSec{pol: ServerTLSPolicy{Mode: "REJECT_INVALID", TrustConfig: "projects/p/locations/global/trustConfigs/tc", TrustFound: true, Roots: roots}}
	cm := fakeCertMaps{primary: selfSigned(t, "primary.example"), www: selfSigned(t, "www.example")}
	origin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "present=%s verified=%s error=%s spiffe=%s leaf=%s", r.Header.Get("X-Present"), r.Header.Get("X-Verified"),
			r.Header.Get("X-Error"), r.Header.Get("X-Spiffe"), r.Header.Get("X-Leaf"))
	})
	s, _, _ := originFixture(t, Options{CertMaps: cm, NetSec: ns}, origin)
	lbCall(t, s, "PATCH", "global/backendServices/app", obj{"customRequestHeaders": []any{"X-Present:{client_cert_present}",
		"X-Verified:{client_cert_chain_verified}", "X-Error:{client_cert_error}", "X-Spiffe:{client_cert_spiffe_id}", "X-Leaf:{client_cert_leaf}"}})
	if _, err := vmCall(t, s, "POST", "global/targetHttpsProxies", obj{"name": "bad", "urlMap": "global/urlMaps/lb",
		"certificateMap":  "//certificatemanager.googleapis.com/projects/p/locations/global/certificateMaps/m",
		"serverTlsPolicy": "//networksecurity.googleapis.com/projects/p/locations/us-east1/serverTlsPolicies/x"}); ErrorStatus(err) != 400 {
		t.Fatalf("a policy in another location: %v", err)
	}
	lbCall(t, s, "POST", "global/targetHttpsProxies", obj{"name": "mtls", "urlMap": "global/urlMaps/lb",
		"certificateMap":  "//certificatemanager.googleapis.com/projects/p/locations/global/certificateMaps/m",
		"serverTlsPolicy": "projects/p/locations/global/serverTlsPolicies/stp"})
	p := lbCall(t, s, "GET", "global/targetHttpsProxies/mtls", nil)
	if str(p, "serverTlsPolicy") != "//networksecurity.googleapis.com/projects/p/locations/global/serverTlsPolicies/stp" {
		t.Fatalf("proxy: %v", p)
	}
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "mtls", "target": "global/targetHttpsProxies/mtls",
		"portRange": "443", "loadBalancingScheme": "EXTERNAL_MANAGED"})
	addr := str(lbCall(t, s, "GET", "global/forwardingRules/mtls", nil), "cloudburrowAddress")
	get := func(cert *tls.Certificate, path string) (int, string, error) {
		cfg := &tls.Config{ServerName: "primary.example", InsecureSkipVerify: true} //nolint:gosec // the test's server certificate is self-signed
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
		resp, err := c.Get("https://" + addr + path)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}
	good := ca.client(t, x509.ExtKeyUsageClientAuth)
	var code int
	var body string
	var err error
	waitFor(t, "the mTLS listener", func() bool {
		code, body, err = get(&good, "/")
		return err == nil
	})
	if code != 200 || !strings.Contains(body, "present=true verified=true error= spiffe=spiffe://example.org/ns/client leaf=:"+base64.StdEncoding.EncodeToString(good.Leaf.Raw)+":") {
		t.Fatalf("a valid client: %d %q", code, body)
	}
	if _, _, err := get(nil, "/"); err == nil {
		t.Error("REJECT_INVALID: a client without a certificate got through")
	}
	bad := other.client(t, x509.ExtKeyUsageClientAuth)
	if _, _, err := get(&bad, "/"); err == nil {
		t.Error("REJECT_INVALID: an untrusted certificate got through")
	}
	server := ca.client(t, x509.ExtKeyUsageServerAuth)
	if _, _, err := get(&server, "/"); err == nil {
		t.Error("REJECT_INVALID: a certificate without clientAuth got through")
	}
	if code, _, err := get(&good, "/deny/x"); err != nil || code != 403 {
		t.Errorf("an authorization policy's denial: %d %v", code, err)
	}
	// ALLOW_INVALID_OR_MISSING_CLIENT_CERT lets them through, and tells
	// the backend.
	ns.mu.Lock()
	ns.pol.Mode = "ALLOW_INVALID_OR_MISSING_CLIENT_CERT"
	ns.mu.Unlock()
	if code, body, err := get(nil, "/"); err != nil || code != 200 || !strings.Contains(body, "present=false verified=false error=client_cert_not_provided") {
		t.Errorf("no certificate, permissive: %d %q %v", code, body, err)
	}
	if code, body, err := get(&bad, "/"); err != nil || code != 200 || !strings.Contains(body, "present=true verified=false error=client_cert_validation_failed spiffe=spiffe://example.org/ns/client leaf=") ||
		strings.Contains(body, "leaf=:") {
		t.Errorf("an untrusted certificate, permissive: %d %q %v", code, body, err)
	}
	if code, body, err := get(&server, "/"); err != nil || !strings.Contains(body, "error=client_cert_chain_invalid_eku") {
		t.Errorf("no clientAuth, permissive: %d %q %v", code, body, err)
	}
	// An allowlisted certificate is always valid.
	ns.mu.Lock()
	ns.pol.Mode, ns.pol.Allowlisted = "REJECT_INVALID", []*x509.Certificate{bad.Leaf}
	ns.mu.Unlock()
	if code, body, err := get(&bad, "/"); err != nil || code != 200 || !strings.Contains(body, "verified=true") {
		t.Errorf("an allowlisted certificate: %d %q %v", code, body, err)
	}
}

func TestBackendClientTLSPolicyOnlyForTrafficDirector(t *testing.T) {
	s, _, _ := originFixture(t, Options{NetSec: &fakeNetSec{}}, http.HandlerFunc(okHandler))
	_, err := vmCall(t, s, "PATCH", "global/backendServices/app", obj{"securitySettings": obj{
		"clientTlsPolicy": "//networksecurity.googleapis.com/projects/p/locations/global/clientTlsPolicies/c"}})
	if ErrorStatus(err) != 400 {
		t.Fatalf("clientTlsPolicy on EXTERNAL_MANAGED: %v", err)
	}
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "td", "protocol": "HTTP2", "loadBalancingScheme": "INTERNAL_SELF_MANAGED",
		"securitySettings": obj{"clientTlsPolicy": "projects/p/locations/global/clientTlsPolicies/c", "subjectAltNames": []any{"spiffe://x"}}})
	b := lbCall(t, s, "GET", "global/backendServices/td", nil)
	if str(objOf(b["securitySettings"]), "clientTlsPolicy") != "//networksecurity.googleapis.com/projects/p/locations/global/clientTlsPolicies/c" {
		t.Fatalf("backend service: %v", b)
	}
}
