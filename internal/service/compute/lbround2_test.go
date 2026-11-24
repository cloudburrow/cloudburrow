package compute

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/xds"
)

// originFixture is an external Application Load Balancer whose backend
// service "app" is a hybrid network endpoint group of one origin on this
// machine; it returns the server, the origin's hit counter, and the
// rule's base URL.
func originFixture(t *testing.T, opts Options, origin http.Handler) (*Server, *atomic.Int64, string) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			hits.Add(1)
		}
		origin.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	if opts.VMs == nil {
		opts.VMs = fakeVMs{}
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	z := "zones/us-central1-a/"
	lbCall(t, s, "POST", z+"networkEndpointGroups", obj{"name": "origin", "networkEndpointType": "NON_GCP_PRIVATE_IP_PORT",
		"network": "global/networks/default", "defaultPort": 80})
	p, _ := strconv.Atoi(port)
	lbCall(t, s, "POST", z+"networkEndpointGroups/origin/attachNetworkEndpoints", obj{"networkEndpoints": []any{
		obj{"ipAddress": host, "port": p}}})
	lbCall(t, s, "POST", "global/healthChecks", obj{"name": "hc", "type": "HTTP",
		"httpHealthCheck": obj{"portSpecification": "USE_SERVING_PORT", "requestPath": "/healthz"}, "checkIntervalSec": 1, "timeoutSec": 1, "healthyThreshold": 1})
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "app", "protocol": "HTTP", "loadBalancingScheme": "EXTERNAL_MANAGED",
		"healthChecks": []any{"global/healthChecks/hc"},
		"backends":     []any{obj{"group": z + "networkEndpointGroups/origin", "balancingMode": "RATE", "maxRatePerEndpoint": 100}}})
	lbCall(t, s, "POST", "global/urlMaps", obj{"name": "lb", "defaultService": "global/backendServices/app"})
	lbCall(t, s, "POST", "global/targetHttpProxies", obj{"name": "lb", "urlMap": "global/urlMaps/lb"})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "lb", "target": "global/targetHttpProxies/lb",
		"portRange": "80", "loadBalancingScheme": "EXTERNAL_MANAGED"})
	rule := lbCall(t, s, "GET", "global/forwardingRules/lb", nil)
	base := "http://" + str(rule, "cloudburrowAddress")
	waitFor(t, "the origin to answer through the load balancer", func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return s, &hits, base
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "origin %s %s", r.URL.Path, r.Header.Get("X-Armor"))
}

func TestCloudArmorRulesAreEnforcedOnRequests(t *testing.T) {
	s, _, base := originFixture(t, Options{}, http.HandlerFunc(okHandler))
	g := "global/securityPolicies"
	lbCall(t, s, "POST", g, obj{"name": "armor", "rules": []any{
		obj{"priority": 100, "action": "deny(403)", "match": obj{"expr": obj{"expression": "request.path.startsWith('/admin')"}}},
		obj{"priority": 200, "action": "deny(404)", "match": obj{"expr": obj{
			"expression": "has(request.headers['x-block']) && request.headers['x-block'].lower() == 'yes'"}}},
		obj{"priority": 300, "action": "deny(403)", "match": obj{"expr": obj{
			"expression": "evaluatePreconfiguredWaf('sqli-v33-stable', {'sensitivity': 1})"}}},
		obj{"priority": 400, "action": "throttle", "match": obj{"expr": obj{"expression": "request.path == '/limited'"}},
			"rateLimitOptions": obj{"conformAction": "allow", "exceedAction": "deny(429)", "enforceOnKey": "IP",
				"rateLimitThreshold": obj{"count": 2, "intervalSec": 60}}},
		obj{"priority": 500, "action": "deny(502)", "preview": true, "match": obj{"versionedExpr": "SRC_IPS_V1",
			"config": obj{"srcIpRanges": []any{"*"}}}},
		obj{"priority": 600, "action": "allow", "match": obj{"expr": obj{"expression": "inIpRange(origin.ip, '127.0.0.0/8')"}},
			"headerAction": obj{"requestHeadersToAdds": []any{obj{"headerName": "X-Armor", "headerValue": "local"}}}},
	}})
	p := lbCall(t, s, "GET", g+"/armor", nil)
	rules := listOf(p["rules"])
	if len(rules) != 7 || rulePriority(objOf(rules[6])) != defaultRulePriority || str(p, "type") != "CLOUD_ARMOR" {
		t.Fatalf("policy: %v", p)
	}
	// Before the policy is attached, everything passes.
	if code, _, _ := get(t, http.DefaultClient, base+"/admin"); code != 200 {
		t.Fatalf("unattached: %d", code)
	}
	lbCall(t, s, "POST", "global/backendServices/app/setSecurityPolicy", obj{"securityPolicy": "global/securityPolicies/armor"})
	if code, body, _ := get(t, http.DefaultClient, base+"/admin/x"); code != 403 || !strings.Contains(body, "403 Forbidden") {
		t.Fatalf("/admin: %d %q", code, body)
	}
	req, _ := http.NewRequest("GET", base+"/", nil)
	req.Header.Set("X-Block", "YES")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 404 {
		t.Fatalf("x-block: %v %v", resp, err)
	}
	resp.Body.Close()
	if code, _, _ := get(t, http.DefaultClient, base+"/search?q="+url.QueryEscape("1' or '1'='1")); code != 403 {
		t.Fatalf("SQL injection: %d", code)
	}
	if code, body, _ := get(t, http.DefaultClient, base+"/hello"); code != 200 || body != "origin /hello local" {
		t.Fatalf("allowed with the header: %d %q", code, body)
	}
	var codes []int
	for range 4 {
		code, _, _ := get(t, http.DefaultClient, base+"/limited")
		codes = append(codes, code)
	}
	if fmt.Sprint(codes) != "[200 200 429 429]" {
		t.Fatalf("throttle: %v", codes)
	}
	// Rules: getRule, patchRule, removeRule; the default rule stays.
	r, err := s.Call("GET", "p", g+"/armor/getRule", map[string][]string{"priority": {"100"}}, nil)
	if err != nil || str(r, "action") != "deny(403)" {
		t.Fatalf("getRule: %v %v", r, err)
	}
	if _, err := s.Call("POST", "p", g+"/armor/patchRule", map[string][]string{"priority": {"100"}}, obj{"action": "allow"}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := get(t, http.DefaultClient, base+"/admin/x"); code != 200 {
		t.Fatalf("after patchRule: %d", code)
	}
	if _, err := s.Call("POST", "p", g+"/armor/removeRule", map[string][]string{"priority": {strconv.Itoa(defaultRulePriority)}}, nil); ErrorStatus(err) != 400 {
		t.Fatalf("removing the default rule: %v", err)
	}
	if _, err := s.Call("POST", "p", g+"/armor/removeRule", map[string][]string{"priority": {"200"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("POST", "p", g+"/armor/addRule", nil, jsonBody(obj{"priority": 300, "action": "allow",
		"match": obj{"versionedExpr": "SRC_IPS_V1", "config": obj{"srcIpRanges": []any{"*"}}}})); ErrorStatus(err) != 400 {
		t.Fatalf("duplicate priority: %v", err)
	}
	// A policy in use is not deleted.
	if _, err := vmCall(t, s, "DELETE", g+"/armor", nil); ErrorStatus(err) != 400 {
		t.Fatalf("deleting a policy in use: %v", err)
	}
	// The default rule's action decides the rest.
	if _, err := s.Call("POST", "p", g+"/armor/patchRule", map[string][]string{"priority": {strconv.Itoa(defaultRulePriority)}},
		obj{"action": "deny(404)"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call("POST", "p", g+"/armor/removeRule", map[string][]string{"priority": {"600"}}, nil); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := get(t, http.DefaultClient, base+"/anything"); code != 404 {
		t.Fatalf("default deny: %d", code)
	}
	lbCall(t, s, "POST", "global/backendServices/app/setSecurityPolicy", obj{"securityPolicy": ""})
	if code, _, _ := get(t, http.DefaultClient, base+"/anything"); code != 200 {
		t.Fatalf("detached: %d", code)
	}
}

func TestCloudArmorValidation(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	g := "global/securityPolicies"
	bad := []obj{
		{"name": "a", "rules": []any{obj{"priority": 1, "action": "deny(401)", "match": obj{"versionedExpr": "SRC_IPS_V1", "config": obj{"srcIpRanges": []any{"*"}}}}}},
		{"name": "a", "rules": []any{obj{"priority": 1, "action": "throttle", "match": obj{"versionedExpr": "SRC_IPS_V1", "config": obj{"srcIpRanges": []any{"*"}}},
			"rateLimitOptions": obj{"conformAction": "allow", "exceedAction": "deny(429)", "rateLimitThreshold": obj{"count": 5, "intervalSec": 7}}}}},
		{"name": "a", "rules": []any{obj{"priority": 1, "action": "allow", "match": obj{"expr": obj{"expression": "request.path.("}}}}},
		{"name": "a", "rules": []any{obj{"priority": 1, "action": "allow", "match": obj{"expr": obj{"expression": "evaluatePreconfiguredWaf('nope-stable')"}}}}},
		{"name": "a", "type": "CLOUD_ARMOR_EDGE", "rules": []any{obj{"priority": 1, "action": "redirect", "redirectOptions": obj{"type": "EXTERNAL_302", "target": "https://x"},
			"match": obj{"versionedExpr": "SRC_IPS_V1", "config": obj{"srcIpRanges": []any{"*"}}}}}},
		{"name": "a", "type": "CLOUD_ARMOR_EDGE", "rules": []any{obj{"priority": 1, "action": "allow", "match": obj{"expr": obj{"expression": "request.body.contains('x')"}}}}},
		{"name": "a", "rules": []any{obj{"priority": 1, "action": "allow", "match": obj{"versionedExpr": "SRC_IPS_V1", "config": obj{"srcIpRanges": []any{
			"1.1.1.1", "1.1.1.2", "1.1.1.3", "1.1.1.4", "1.1.1.5", "1.1.1.6", "1.1.1.7", "1.1.1.8", "1.1.1.9", "1.1.1.10", "1.1.1.11"}}}}}},
	}
	for i, b := range bad {
		if _, err := vmCall(t, s, "POST", g, b); ErrorStatus(err) != 400 {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if _, err := vmCall(t, s, "POST", g, obj{"name": "a", "rules": []any{obj{"priority": 1, "action": "allow",
		"match": obj{"expr": obj{"expression": "evaluateThreatIntelligence('iplist-known-malicious-ips')"}}}}}); ErrorStatus(err) != 501 {
		t.Errorf("threat intelligence: %v", err)
	}
	sets := lbCall(t, s, "GET", g+"/listPreconfiguredExpressionSets", nil)
	raw, _ := json.Marshal(sets)
	if !strings.Contains(string(raw), "owasp-crs-v030301-id942100-sqli") || !strings.Contains(string(raw), `"xss-v33-stable"`) {
		t.Fatalf("expression sets: %s", raw)
	}
	// An edge policy cannot be a backend policy.
	lbCall(t, s, "POST", g, obj{"name": "edge", "type": "CLOUD_ARMOR_EDGE"})
	lbCall(t, s, "POST", "global/backendBuckets", obj{"name": "b", "bucketName": "bkt"})
	if _, err := vmCall(t, s, "POST", "global/backendBuckets/b/setEdgeSecurityPolicy", obj{"securityPolicy": "global/securityPolicies/edge"}); err != nil {
		t.Fatal(err)
	}
	bb := lbCall(t, s, "GET", "global/backendBuckets/b", nil)
	if !strings.HasSuffix(str(bb, "edgeSecurityPolicy"), "global/securityPolicies/edge") {
		t.Fatalf("bucket: %v", bb)
	}
}

func TestCloudCDNCachesThroughTheProxy(t *testing.T) {
	var body atomic.Int64
	s, hits, base := originFixture(t, Options{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=600")
		w.Header().Set("Cache-Tag", "pics")
		fmt.Fprintf(w, "v%d %s", body.Add(1), r.URL.Path)
	}))
	lbCall(t, s, "PATCH", "global/backendServices/app", obj{"enableCDN": true,
		"customResponseHeaders": []any{"X-Cache-Status: {cdn_cache_status}"},
		"cdnPolicy":             obj{"cacheMode": "CACHE_ALL_STATIC", "defaultTtl": 3600, "maxTtl": 86400, "clientTtl": 300}})
	bs := lbCall(t, s, "GET", "global/backendServices/app", nil)
	if bs["enableCDN"] != true || str(objOf(bs["cdnPolicy"]), "cacheMode") != "CACHE_ALL_STATIC" {
		t.Fatalf("backend service: %v", bs)
	}
	hits.Store(0)
	code, b1, h1 := get(t, http.DefaultClient, base+"/img.png")
	code2, b2, h2 := get(t, http.DefaultClient, base+"/img.png")
	if code != 200 || code2 != 200 || h1.Get("X-Cache-Status") != "miss" || h2.Get("X-Cache-Status") != "hit" || b1 != b2 || hits.Load() != 1 {
		t.Fatalf("miss then hit: %q %v / %q %v, origin hits %d", b1, h1, b2, h2, hits.Load())
	}
	if h2.Get("Age") == "" || !strings.Contains(h2.Get("Cache-Control"), "max-age=300") {
		t.Fatalf("hit headers: %v", h2)
	}
	// A bypass header and a POST go to the origin.
	req, _ := http.NewRequest("POST", base+"/img.png", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.Header.Get("X-Cache-Status") != "uncacheable" {
		t.Fatalf("POST: %v", resp.Header)
	}
	// Invalidation by path empties the entry.
	lbCall(t, s, "POST", "global/urlMaps/lb/invalidateCache", obj{"path": "/img*"})
	_, b3, h3 := get(t, http.DefaultClient, base+"/img.png")
	if h3.Get("X-Cache-Status") != "miss" || b3 == b1 {
		t.Fatalf("after invalidation: %q %v", b3, h3)
	}
	lbCall(t, s, "POST", "global/urlMaps/lb/invalidateCache", obj{"cacheTags": []any{"pics"}})
	if _, _, h := get(t, http.DefaultClient, base+"/img.png"); h.Get("X-Cache-Status") != "miss" {
		t.Fatalf("after tag invalidation: %v", h)
	}
	// Signed URL keys: then only signed requests are served.
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	kv := base64.URLEncoding.EncodeToString(key)
	lbCall(t, s, "POST", "global/backendServices/app/addSignedUrlKey", obj{"keyName": "k1", "keyValue": kv})
	bs = lbCall(t, s, "GET", "global/backendServices/app", nil)
	if fmt.Sprint(objOf(bs["cdnPolicy"])["signedUrlKeyNames"]) != "[k1]" {
		t.Fatalf("key names: %v", bs["cdnPolicy"])
	}
	if code, _, _ := get(t, http.DefaultClient, base+"/img.png"); code != 403 {
		t.Fatalf("unsigned: %d", code)
	}
	u := base + "/img.png?Expires=" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + "&KeyName=k1"
	mac := hmac.New(sha1.New, key)
	mac.Write([]byte(u))
	signed := u + "&Signature=" + base64.URLEncoding.EncodeToString(mac.Sum(nil))
	if code, _, _ := get(t, http.DefaultClient, signed); code != 200 {
		t.Fatalf("signed: %d", code)
	}
	if code, _, _ := get(t, http.DefaultClient, signed+"x"); code != 403 {
		t.Fatalf("bad signature: %d", code)
	}
	if _, err := s.Call("POST", "p", "global/backendServices/app/deleteSignedUrlKey", map[string][]string{"keyName": {"k1"}}, nil); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := get(t, http.DefaultClient, base+"/img.png"); code != 200 {
		t.Fatalf("after the key is deleted: %d", code)
	}
	// Validation.
	if _, err := vmCall(t, s, "PATCH", "global/backendServices/app", obj{"cdnPolicy": obj{"cacheMode": "USE_ORIGIN_HEADERS", "defaultTtl": 10}}); ErrorStatus(err) != 400 {
		t.Fatalf("USE_ORIGIN_HEADERS with a TTL: %v", err)
	}
}

// fakeIAP verifies tokens "tok:EMAIL:AUD" and allows the emails in allow.
type fakeIAP struct{ allow map[string]bool }

func (f fakeIAP) VerifyIDToken(tok string) (string, string, bool) {
	p := strings.Split(tok, ":")
	if len(p) != 3 || p[0] != "tok" {
		return "", "", false
	}
	return p[1], p[2], true
}
func (f fakeIAP) Allowed(_ context.Context, res, email string, _ IAPRequest) bool {
	return res == "projects/p/iap_web/compute/services/app" && f.allow[email]
}
func (f fakeIAP) Settings(string) map[string]any { return nil }
func (f fakeIAP) ProjectNumber(p string) string  { return "123456789" }

func TestIAPRefusesWithoutATokenAndSignsHeaders(t *testing.T) {
	var seen atomic.Value
	s, _, base := originFixture(t, Options{IAP: fakeIAP{allow: map[string]bool{"dev@example.com": true}}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen.Store(r.Header.Clone())
			_, _ = io.WriteString(w, "ok")
		}))
	lbCall(t, s, "PATCH", "global/backendServices/app", obj{"iap": obj{"enabled": true, "oauth2ClientId": "client-1", "oauth2ClientSecret": "s3cret"}})
	bs := lbCall(t, s, "GET", "global/backendServices/app", nil)
	if iap := objOf(bs["iap"]); iap["oauth2ClientSecret"] != nil || str(iap, "oauth2ClientSecretSha256") == "" {
		t.Fatalf("iap: %v", iap)
	}
	try := func(auth string) (int, string) {
		req, _ := http.NewRequest("GET", base+"/", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		req.Header.Set("X-Goog-Authenticated-User-Email", "forged")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := try(""); code != 401 || body != "Invalid IAP credentials: empty token" {
		t.Fatalf("no token: %d %q", code, body)
	}
	if code, _ := try("garbage"); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	if code, _ := try("tok:dev@example.com:other-client"); code != 401 {
		t.Fatalf("wrong audience: %d", code)
	}
	if code, _ := try("tok:eve@example.com:client-1"); code != 403 {
		t.Fatalf("no access: %d", code)
	}
	if code, body := try("tok:dev@example.com:client-1"); code != 200 || body != "ok" {
		t.Fatalf("allowed: %d %q", code, body)
	}
	h := seen.Load().(http.Header)
	if h.Get("X-Goog-Authenticated-User-Email") != "accounts.google.com:dev@example.com" || !strings.HasPrefix(h.Get("X-Goog-Authenticated-User-Id"), "accounts.google.com:") {
		t.Fatalf("identity headers: %v", h)
	}
	// The signed header verifies with the served key set.
	jwt := h.Get("X-Goog-Iap-Jwt-Assertion")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", IAPKeysPath, nil))
	var jwks struct {
		Keys []struct{ X, Y, Kid string } `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jwks); err != nil || len(jwks.Keys) != 1 {
		t.Fatalf("keys: %s", rec.Body.String())
	}
	parts := strings.Split(jwt, ".")
	xb, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].X)
	yb, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].Y)
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(parts) != 3 || !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatalf("the assertion does not verify: %s", jwt)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	if claims["aud"] != "/projects/123456789/global/backendServices/"+fmt.Sprint(bs["id"]) || claims["iss"] != "https://cloud.google.com/iap" ||
		claims["email"] != "dev@example.com" {
		t.Fatalf("claims: %v", claims)
	}
	// IAP and Cloud CDN exclude each other.
	if _, err := vmCall(t, s, "PATCH", "global/backendServices/app", obj{"enableCDN": true}); ErrorStatus(err) != 400 {
		t.Fatalf("IAP with CDN: %v", err)
	}
}

// fakeCertMaps serves one map whose PRIMARY entry is a self-signed
// certificate for primary.example and whose www.example entry is another.
type fakeCertMaps struct{ primary, www tls.Certificate }

func (f fakeCertMaps) MapExists(name string) bool {
	return name == "projects/p/locations/global/certificateMaps/m"
}
func (f fakeCertMaps) MapCertificates(name, sni string) []tls.Certificate {
	if sni == "www.example" {
		return []tls.Certificate{f.www}
	}
	return []tls.Certificate{f.primary}
}
func (f fakeCertMaps) Certificates([]string) []tls.Certificate { return nil }

func selfSigned(t *testing.T, name string) tls.Certificate {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k, Leaf: leaf}
}

func TestCertificateMapsServeTheHandshake(t *testing.T) {
	cm := fakeCertMaps{primary: selfSigned(t, "primary.example"), www: selfSigned(t, "www.example")}
	s, _, _ := originFixture(t, Options{CertMaps: cm}, http.HandlerFunc(okHandler))
	if _, err := vmCall(t, s, "POST", "global/targetHttpsProxies", obj{"name": "tls", "urlMap": "global/urlMaps/lb",
		"certificateMap": "//certificatemanager.googleapis.com/projects/p/locations/global/certificateMaps/nope"}); ErrorStatus(err) != 404 {
		t.Fatalf("a missing map: %v", err)
	}
	lbCall(t, s, "POST", "global/targetHttpsProxies", obj{"name": "tls", "urlMap": "global/urlMaps/lb",
		"certificateMap": "//certificatemanager.googleapis.com/projects/p/locations/global/certificateMaps/m"})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "tls", "target": "global/targetHttpsProxies/tls",
		"portRange": "443", "loadBalancingScheme": "EXTERNAL_MANAGED"})
	addr := str(lbCall(t, s, "GET", "global/forwardingRules/tls", nil), "cloudburrowAddress")
	for _, sni := range []string{"www.example", "primary.example"} {
		var c *tls.Conn
		var err error
		waitFor(t, "the TLS listener", func() bool {
			c, err = tls.Dial("tcp", addr, &tls.Config{ServerName: sni, InsecureSkipVerify: true}) //nolint:gosec // the test checks the certificate itself
			return err == nil
		})
		got := c.ConnectionState().PeerCertificates[0].Subject.CommonName
		c.Close()
		if got != sni {
			t.Fatalf("SNI %s got %s", sni, got)
		}
	}
	p := lbCall(t, s, "GET", "global/targetHttpsProxies/tls", nil)
	if str(p, "certificateMap") != "//certificatemanager.googleapis.com/projects/p/locations/global/certificateMaps/m" {
		t.Fatalf("proxy: %v", p)
	}
}

// udpLB reaches every instance's UDP port at one local echo server.
type udpLB struct {
	fakeLB
	echo string
}

func (u *udpLB) InstanceUDP(ctx context.Context, vm VMSpec, port int) (DatagramConn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "udp", u.echo)
	if err != nil {
		return nil, err
	}
	return udpConn{c}, nil
}

func TestUDPPassthroughLoadBalancerCarriesDatagrams(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, a, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(append([]byte("echo:"), buf[:n]...), a)
		}
	}()
	f := &udpLB{fakeLB: fakeLB{vms: map[string]string{}}, echo: pc.LocalAddr().String()}
	s, err := New(Options{VMs: fakeVMs{}, LB: f})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	z := "zones/us-central1-a/"
	lbCall(t, s, "POST", z+"instances", obj{"name": "dns-1", "machineType": z + "machineTypes/e2-small",
		"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{obj{"network": "global/networks/default"}}})
	lbCall(t, s, "POST", "regions/us-central1/targetPools", obj{"name": "dns", "instances": []any{z + "instances/dns-1"}})
	lbCall(t, s, "POST", "regions/us-central1/forwardingRules", obj{"name": "dns", "IPProtocol": "UDP", "portRange": "53",
		"target": "regions/us-central1/targetPools/dns"})
	rule := lbCall(t, s, "GET", "regions/us-central1/forwardingRules/dns", nil)
	addr := str(rule, "cloudburrowAddress")
	if addr == "" {
		t.Fatalf("rule: %v", rule)
	}
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 100)
	waitFor(t, "an echo through the UDP load balancer", func() bool {
		_, _ = c.Write([]byte("ping"))
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := c.Read(buf)
		return err == nil && string(buf[:n]) == "echo:ping"
	})
	// A network edge security policy on the pool drops the flows it denies.
	lbCall(t, s, "POST", "regions/us-central1/securityPolicies", obj{"name": "net", "type": "CLOUD_ARMOR_NETWORK", "rules": []any{
		obj{"priority": 10, "action": "deny", "networkMatch": obj{"srcIpRanges": []any{"127.0.0.0/8"}, "ipProtocols": []any{"UDP"}}}}})
	lbCall(t, s, "POST", "regions/us-central1/targetPools/dns/setSecurityPolicy", obj{"securityPolicy": "regions/us-central1/securityPolicies/net"})
	c2, _ := net.Dial("udp", addr)
	defer c2.Close()
	_, _ = c2.Write([]byte("ping"))
	_ = c2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := c2.Read(buf); err == nil {
		t.Fatal("a denied flow was carried")
	}
}

func TestTargetGrpcProxies(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	lbCall(t, s, "POST", "global/healthChecks", obj{"name": "hc", "type": "GRPC", "grpcHealthCheck": obj{"port": 50051}})
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "grpc", "protocol": "GRPC", "loadBalancingScheme": "INTERNAL_SELF_MANAGED",
		"healthChecks": []any{"global/healthChecks/hc"}})
	lbCall(t, s, "POST", "global/urlMaps", obj{"name": "grpc", "defaultService": "global/backendServices/grpc"})
	lbCall(t, s, "POST", "global/targetGrpcProxies", obj{"name": "grpc", "urlMap": "global/urlMaps/grpc", "validateForProxyless": true})
	p := lbCall(t, s, "GET", "global/targetGrpcProxies/grpc", nil)
	if p["validateForProxyless"] != true || p["kind"] != "compute#targetGrpcProxy" {
		t.Fatalf("proxy: %v", p)
	}
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "grpc", "target": "global/targetGrpcProxies/grpc", "IPAddress": "0.0.0.0",
		"portRange": "50051", "loadBalancingScheme": "INTERNAL_SELF_MANAGED", "network": "global/networks/default"})
	if _, err := vmCall(t, s, "POST", "global/forwardingRules", obj{"name": "bad", "target": "global/targetGrpcProxies/grpc",
		"portRange": "50052", "loadBalancingScheme": "EXTERNAL_MANAGED"}); ErrorStatus(err) != 400 {
		t.Fatalf("an external rule: %v", err)
	}
}

func TestProxylessGRPCThroughXDS(t *testing.T) {
	// The backend: a gRPC health server on this machine, a hybrid
	// endpoint of an INTERNAL_SELF_MANAGED GRPC backend service.
	gs := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, hs)
	gl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(gl) }()
	t.Cleanup(gs.Stop)
	gport := gl.Addr().(*net.TCPAddr).Port
	s, err := New(Options{VMs: fakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	z := "zones/us-central1-a/"
	lbCall(t, s, "POST", z+"networkEndpointGroups", obj{"name": "grpc", "networkEndpointType": "NON_GCP_PRIVATE_IP_PORT",
		"network": "global/networks/default", "defaultPort": gport})
	lbCall(t, s, "POST", z+"networkEndpointGroups/grpc/attachNetworkEndpoints", obj{"networkEndpoints": []any{
		obj{"ipAddress": "127.0.0.1", "port": gport}}})
	lbCall(t, s, "POST", "global/healthChecks", obj{"name": "ghc", "type": "GRPC", "grpcHealthCheck": obj{"portSpecification": "USE_SERVING_PORT"},
		"checkIntervalSec": 1, "timeoutSec": 1, "healthyThreshold": 1})
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "grpc", "protocol": "GRPC", "loadBalancingScheme": "INTERNAL_SELF_MANAGED",
		"healthChecks": []any{"global/healthChecks/ghc"},
		"backends":     []any{obj{"group": z + "networkEndpointGroups/grpc", "balancingMode": "RATE", "maxRatePerEndpoint": 100}}})
	lbCall(t, s, "POST", "global/urlMaps", obj{"name": "grpc", "defaultService": "global/backendServices/grpc",
		"hostRules":    []any{obj{"hosts": []any{"health.example"}, "pathMatcher": "pm"}},
		"pathMatchers": []any{obj{"name": "pm", "defaultService": "global/backendServices/grpc"}}})
	lbCall(t, s, "POST", "global/targetGrpcProxies", obj{"name": "grpc", "urlMap": "global/urlMaps/grpc", "validateForProxyless": true})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "grpc", "target": "global/targetGrpcProxies/grpc",
		"portRange": "8000", "loadBalancingScheme": "INTERNAL_SELF_MANAGED", "network": "global/networks/default"})
	// The xDS server, as the Compute Engine port serves it: HTTP/2
	// without TLS.
	xl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true)
	xs := &http.Server{Handler: s.XDSHandler(), Protocols: &protos}
	go func() { _ = xs.Serve(xl) }()
	t.Cleanup(func() { _ = xs.Close() })
	rb, err := xds.NewXDSResolverWithConfigForTesting([]byte(XDSBootstrap(xl.Addr().String(), "p", "default")))
	if err != nil {
		t.Fatal(err)
	}
	cc, err := grpc.NewClient("xds:///health.example:8000", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithResolvers(rb))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	var resp *healthpb.HealthCheckResponse
	defer func() { t.Logf("last error: %v", err) }()
	waitFor(t, "a proxyless gRPC call through xDS", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resp, err = healthpb.NewHealthClient(cc).Check(ctx, &healthpb.HealthCheckRequest{})
		return err == nil
	})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Logf("last error: %v", err)
		t.Fatalf("Check: %v", resp)
	}
	// A host no host rule takes: the default service still serves it,
	// as the URL map's defaultService.
	cc2, _ := grpc.NewClient("xds:///other.example:8000", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithResolvers(rb))
	defer cc2.Close()
	waitFor(t, "the default service", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := healthpb.NewHealthClient(cc2).Check(ctx, &healthpb.HealthCheckRequest{})
		return err == nil
	})
}
