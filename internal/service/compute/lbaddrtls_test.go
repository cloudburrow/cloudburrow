package compute

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGroupsNetSec adds address groups and backend authentication
// configs to fakeNetSec.
type fakeGroupsNetSec struct {
	fakeNetSec
	gmu    sync.Mutex
	groups map[string][]string
	auth   map[string]BackendAuthConfig
}

func (f *fakeGroupsNetSec) AddressGroupItems(name string) ([]string, bool) {
	f.gmu.Lock()
	defer f.gmu.Unlock()
	it, ok := f.groups[name]
	return it, ok
}

func (f *fakeGroupsNetSec) BackendAuthConfig(name string) (BackendAuthConfig, bool) {
	f.gmu.Lock()
	defer f.gmu.Unlock()
	c, ok := f.auth[name]
	return c, ok
}

func (f *fakeGroupsNetSec) set(name string, items ...string) {
	f.gmu.Lock()
	f.groups[name] = items
	f.gmu.Unlock()
}

func TestCloudArmorAddressGroups(t *testing.T) {
	ns := &fakeGroupsNetSec{groups: map[string][]string{}}
	ag := "projects/p/locations/global/addressGroups/bad"
	ns.set(ag, "10.9.0.0/16")
	s, _, base := originFixture(t, Options{NetSec: ns}, http.HandlerFunc(okHandler))
	rule := obj{"priority": 100, "action": "deny(403)", "match": obj{"expr": obj{"expression": "evaluateAddressGroup('bad', origin.ip)"}}}
	pol := obj{"name": "ag", "rules": []any{rule}}
	if _, err := vmCall(t, s, "POST", "global/securityPolicies", jsonBody(pol)); ErrorStatus(err) != 400 || !strings.Contains(err.Error(), "Cloud Armor Enterprise") {
		t.Fatalf("a standard-tier project: %v", err)
	}
	if str(lbCall(t, s, "GET", "", nil), "cloudArmorTier") != "CA_STANDARD" {
		t.Fatal("the default tier is not CA_STANDARD")
	}
	if _, err := vmCall(t, s, "POST", "setCloudArmorTier", obj{"cloudArmorTier": "CA_GOLD"}); ErrorStatus(err) != 400 {
		t.Fatalf("a bad tier: %v", err)
	}
	lbCall(t, s, "POST", "setCloudArmorTier", obj{"cloudArmorTier": "CA_ENTERPRISE_PAYGO"})
	if str(lbCall(t, s, "GET", "", nil), "cloudArmorTier") != "CA_ENTERPRISE_PAYGO" {
		t.Fatal("setCloudArmorTier did not stick")
	}
	for name, e := range map[string]string{
		"missing group":         "evaluateAddressGroup('nope', origin.ip)",
		"another project":       "evaluateAddressGroup('projects/q/locations/global/addressGroups/bad', origin.ip)",
		"no organization group": "evaluateOrganizationAddressGroup('bad', origin.ip)",
	} {
		b := obj{"name": "x", "rules": []any{obj{"priority": 1, "action": "deny(403)", "match": obj{"expr": obj{"expression": e}}}}}
		if _, err := vmCall(t, s, "POST", "global/securityPolicies", jsonBody(b)); ErrorStatus(err) != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := vmCall(t, s, "POST", "regions/us-central1/securityPolicies", jsonBody(pol)); ErrorStatus(err) != 400 {
		t.Errorf("a regional policy: %v", err)
	}
	lbCall(t, s, "POST", "global/securityPolicies", pol)
	lbCall(t, s, "POST", "global/backendServices/app/setSecurityPolicy", obj{"securityPolicy": "global/securityPolicies/ag"})
	if code, _, _ := get(t, http.DefaultClient, base+"/"); code != 200 {
		t.Fatalf("not in the group: %d", code)
	}
	ns.set(ag, "10.9.0.0/16", "127.0.0.1/32")
	if code, _, _ := get(t, http.DefaultClient, base+"/"); code != 403 {
		t.Fatalf("in the group: %d", code)
	}
	refs := s.AddressGroupReferences(ag)
	if len(refs) != 1 || refs[0].RulePriority != 100 || !strings.HasSuffix(refs[0].SecurityPolicy, "/projects/p/global/securityPolicies/ag") {
		t.Fatalf("references: %+v", refs)
	}
	// An exclusion, then the user IP from a header.
	patchRule := func(e string) {
		t.Helper()
		if _, err := s.Call("POST", "p", "global/securityPolicies/ag/patchRule", map[string][]string{"priority": {"100"}},
			jsonBody(obj{"match": obj{"expr": obj{"expression": e}}})); err != nil {
			t.Fatal(err)
		}
	}
	patchRule("evaluateAddressGroup('bad', origin.ip, ['127.0.0.0/8'])")
	if code, _, _ := get(t, http.DefaultClient, base+"/"); code != 200 {
		t.Fatalf("excluded: %d", code)
	}
	lbCall(t, s, "PATCH", "global/securityPolicies/ag", obj{"advancedOptionsConfig": obj{"userIpRequestHeaders": []any{"X-User-IP"}}})
	patchRule("evaluateAddressGroup('projects/p/locations/global/addressGroups/bad', origin.user_ip)")
	req, _ := http.NewRequest("GET", base+"/", nil)
	req.Header.Set("X-User-IP", "10.9.1.2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("user_ip in the group: %v %v", resp, err)
	}
	resp.Body.Close()
	req.Header.Set("X-User-IP", "192.0.2.1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("user_ip outside the group: %v %v", resp, err)
	}
	resp.Body.Close()
}

// tlsOrigin is an HTTPS backend with a certificate from ca for names; it
// asks for a client certificate and answers its common name.
func tlsOrigin(t *testing.T, ca testCA, names ...string) *httptest.Server {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &k.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		fmt.Fprintf(w, "sni=%s client=%s", r.TLS.ServerName, cn)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}}, ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestBackendAuthenticatedTLSAndMTLS(t *testing.T) {
	ca, other := newTestCA(t, "backend-root"), newTestCA(t, "other-root")
	roots, otherRoots := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(ca.cert)
	otherRoots.AddCert(other.cert)
	client := ca.client(t, x509.ExtKeyUsageClientAuth)
	cfgName := "projects/p/locations/global/backendAuthenticationConfigs/bac"
	ns := &fakeGroupsNetSec{groups: map[string][]string{}, auth: map[string]BackendAuthConfig{
		cfgName: {TrustConfig: "projects/p/locations/global/trustConfigs/tc", TrustFound: true, Roots: roots, ClientCert: &client, Version: "1"},
	}}
	s, _, _ := originFixture(t, Options{NetSec: ns}, http.HandlerFunc(okHandler))
	backend := tlsOrigin(t, ca, "backend.example")
	host, port, _ := net.SplitHostPort(backend.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	z := "zones/us-central1-a/"
	lbCall(t, s, "POST", z+"networkEndpointGroups", obj{"name": "tlsorigin", "networkEndpointType": "NON_GCP_PRIVATE_IP_PORT",
		"network": "global/networks/default", "defaultPort": p})
	lbCall(t, s, "POST", z+"networkEndpointGroups/tlsorigin/attachNetworkEndpoints", obj{"networkEndpoints": []any{obj{"ipAddress": host, "port": p}}})
	lbCall(t, s, "POST", "global/healthChecks", obj{"name": "tcp", "type": "TCP", "tcpHealthCheck": obj{"portSpecification": "USE_SERVING_PORT"},
		"checkIntervalSec": 1, "timeoutSec": 1, "healthyThreshold": 1})
	bs := obj{"name": "tls", "protocol": "HTTPS", "loadBalancingScheme": "EXTERNAL_MANAGED", "healthChecks": []any{"global/healthChecks/tcp"},
		"backends": []any{obj{"group": z + "networkEndpointGroups/tlsorigin", "balancingMode": "RATE", "maxRatePerEndpoint": 100}}}
	lbCall(t, s, "POST", "global/backendServices", bs)
	lbCall(t, s, "POST", "global/urlMaps", obj{"name": "tls", "defaultService": "global/backendServices/tls"})
	lbCall(t, s, "POST", "global/targetHttpProxies", obj{"name": "tls", "urlMap": "global/urlMaps/tls"})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "tls", "target": "global/targetHttpProxies/tls", "portRange": "80",
		"loadBalancingScheme": "EXTERNAL_MANAGED"})
	base := "http://" + str(lbCall(t, s, "GET", "global/forwardingRules/tls", nil), "cloudburrowAddress")
	var code int
	var body string
	waitFor(t, "the HTTPS backend", func() bool {
		code, body, _ = get(t, http.DefaultClient, base+"/")
		return code == 200
	})
	if body != "sni= client=" {
		t.Fatalf("without tlsSettings: %q", body)
	}
	patch := func(ts obj) {
		t.Helper()
		lbCall(t, s, "PATCH", "global/backendServices/tls", obj{"tlsSettings": ts})
	}
	ref := "//networksecurity.googleapis.com/" + cfgName
	patch(obj{"sni": "backend.example", "authenticationConfig": ref})
	if got := str(objOf(lbCall(t, s, "GET", "global/backendServices/tls", nil)["tlsSettings"]), "authenticationConfig"); got != ref {
		t.Fatalf("tlsSettings.authenticationConfig = %q", got)
	}
	if code, body, _ = get(t, http.DefaultClient, base+"/"); code != 200 || body != "sni=backend.example client=client" {
		t.Fatalf("backend mTLS: %d %q", code, body)
	}
	patch(obj{"sni": "other.example", "authenticationConfig": ref})
	if code, body, _ = get(t, http.DefaultClient, base+"/"); code != 502 {
		t.Fatalf("an SNI the certificate does not name: %d %q", code, body)
	}
	patch(obj{"sni": "other.example", "authenticationConfig": ref, "subjectAltNames": []any{obj{"dnsName": "backend.example"}}})
	if code, body, _ = get(t, http.DefaultClient, base+"/"); code != 200 || body != "sni=other.example client=client" {
		t.Fatalf("subjectAltNames over the SNI: %d %q", code, body)
	}
	ns.gmu.Lock()
	ns.auth[cfgName] = BackendAuthConfig{TrustConfig: "projects/p/locations/global/trustConfigs/tc", TrustFound: true, Roots: otherRoots, Version: "2"}
	ns.gmu.Unlock()
	if code, body, _ = get(t, http.DefaultClient, base+"/"); code != 502 || !strings.Contains(body, "ssl_certificate_verification_failed") {
		t.Fatalf("an untrusted backend: %d %q", code, body)
	}
	for name, ts := range map[string]obj{
		"six SANs": {"subjectAltNames": []any{obj{"dnsName": "a"}, obj{"dnsName": "b"}, obj{"dnsName": "c"}, obj{"dnsName": "d"},
			obj{"dnsName": "e"}, obj{"dnsName": "f"}}},
		"both SAN kinds":    {"subjectAltNames": []any{obj{"dnsName": "a", "uniformResourceIdentifier": "spiffe://x"}}},
		"another location":  {"authenticationConfig": "//networksecurity.googleapis.com/projects/p/locations/us-east1/backendAuthenticationConfigs/bac"},
		"not a config name": {"authenticationConfig": "projects/p/locations/global/serverTlsPolicies/x"},
	} {
		if _, err := vmCall(t, s, "PATCH", "global/backendServices/tls", jsonBody(obj{"tlsSettings": ts})); ErrorStatus(err) != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := vmCall(t, s, "PATCH", "global/backendServices/app", jsonBody(obj{"tlsSettings": obj{"sni": "x"}})); ErrorStatus(err) != 400 {
		t.Errorf("tlsSettings on an HTTP backend service: %v", err)
	}
}

func TestVerifyBackendCert(t *testing.T) {
	ca := newTestCA(t, "root")
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	srv := tlsOrigin(t, ca, "b.example")
	raw := srv.TLS.Certificates[0].Certificate
	now := time.Now()
	cfg := &BackendAuthConfig{TrustConfig: "tc", TrustFound: true, Roots: roots}
	if err := verifyBackendCert(cfg, "b.example", nil, raw, now); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := verifyBackendCert(cfg, "", []obj{{"dnsName": "b.example"}}, raw, now); err != nil {
		t.Errorf("SAN: %v", err)
	}
	if err := verifyBackendCert(cfg, "", nil, nil, now); err != errServerCertNotProvided {
		t.Errorf("no certificate: %v", err)
	}
	if err := verifyBackendCert(&BackendAuthConfig{TrustConfig: "tc"}, "", nil, raw, now); err != errServerCertTrustNotFound {
		t.Errorf("missing trust config: %v", err)
	}
	if err := verifyBackendCert(&BackendAuthConfig{PublicRoots: true}, "", nil, raw, now); err != errServerCertNotValidated {
		t.Errorf("public roots without an SNI: %v", err)
	}
	leaf, _ := x509.ParseCertificate(raw[0])
	if err := verifyBackendCert(&BackendAuthConfig{TrustConfig: "tc", TrustFound: true, Allowlisted: []*x509.Certificate{leaf}}, "", nil, raw, now); err != nil {
		t.Errorf("allowlisted: %v", err)
	}
	if err := verifyBackendCert(cfg, "", nil, raw, now.Add(48*time.Hour)); err != errServerCertVerify {
		t.Errorf("expired: %v", err)
	}
	cl := ca.client(t, x509.ExtKeyUsageClientAuth)
	if err := verifyBackendCert(cfg, "", nil, cl.Certificate, now); err != errServerCertEKU {
		t.Errorf("a clientAuth-only leaf: %v", err)
	}
}
