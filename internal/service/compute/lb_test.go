package compute

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLB reaches each instance's port 80 at its own test server, and
// serves a Cloud Run API and ingress.
type fakeLB struct {
	mu      sync.Mutex
	vms     map[string]string // instance name: address
	ingress string
	run     string
	storage string
}

func (f *fakeLB) InstanceAddr(_ context.Context, vm VMSpec, port int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.vms[vm.Name]; ok && port == 80 {
		return a, nil
	}
	return "", fmt.Errorf("no %s:%d", vm.Name, port)
}
func (f *fakeLB) Ingress() string { return f.ingress }
func (f *fakeLB) LocalAPI(api string) string {
	switch api {
	case "run":
		return f.run
	case "storage":
		return f.storage
	}
	return ""
}

// named answers every request with its name, the path it got and the
// headers the load balancer added.
func named(t *testing.T, name string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		fmt.Fprintf(w, "%s %s %s via=%s xff=%s", name, r.Host, r.URL.Path, r.Header.Get("Via"), r.Header.Get("X-Forwarded-For"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func lbCall(t *testing.T, s *Server, method, path string, body obj) obj {
	t.Helper()
	out, err := vmCall(t, s, method, path, jsonBody(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return out
}

// jsonBody is b as a request's JSON arrives (numbers as float64).
func jsonBody(b obj) obj {
	if b == nil {
		return nil
	}
	raw, _ := json.Marshal(b)
	var out obj
	_ = json.Unmarshal(raw, &out)
	return out
}

func get(t *testing.T, c *http.Client, u string) (int, string, http.Header) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// waitFor retries f until it is true or 20 seconds pass.
func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// lbFixture is an external Application Load Balancer in front of an
// unmanaged instance group of two instances (/ and /web/*) and a
// serverless network endpoint group of a Cloud Run service (/api/*).
func lbFixture(t *testing.T) (*Server, *fakeLB, string) {
	f := &fakeLB{vms: map[string]string{}}
	for _, n := range []string{"web-1", "web-2"} {
		f.vms[n] = named(t, n).Listener.Addr().String()
	}
	ingress := named(t, "ingress")
	f.ingress = ingress.Listener.Addr().String()
	runAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/projects/p/locations/us-central1/services/api" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(obj{"name": "api", "uri": "https://api-abc-uc.a.run.app", "ingress": "INGRESS_TRAFFIC_ALL"})
	}))
	t.Cleanup(runAPI.Close)
	f.run = runAPI.URL
	s, err := New(Options{VMs: fakeVMs{}, LB: f})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	z := "zones/us-central1-a/"
	for _, n := range []string{"web-1", "web-2"} {
		lbCall(t, s, "POST", z+"instances", obj{"name": n, "machineType": z + "machineTypes/e2-small",
			"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{obj{"network": "global/networks/default"}}})
	}
	lbCall(t, s, "POST", "global/firewalls", obj{"name": "allow-hc", "network": "global/networks/default",
		"sourceRanges": []any{"35.191.0.0/16", "130.211.0.0/22"}, "allowed": []any{obj{"IPProtocol": "tcp", "ports": []any{"80"}}}})
	lbCall(t, s, "POST", z+"instanceGroups", obj{"name": "web", "network": "global/networks/default",
		"namedPorts": []any{obj{"name": "http", "port": 80}}})
	lbCall(t, s, "POST", z+"instanceGroups/web/addInstances", obj{"instances": []any{
		obj{"instance": z + "instances/web-1"}, obj{"instance": z + "instances/web-2"}}})
	lbCall(t, s, "POST", "global/healthChecks", obj{"name": "hc", "type": "HTTP",
		"httpHealthCheck": obj{"port": 80, "requestPath": "/healthz"}, "checkIntervalSec": 1, "timeoutSec": 1, "healthyThreshold": 1})
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "web", "protocol": "HTTP", "portName": "http",
		"loadBalancingScheme": "EXTERNAL_MANAGED", "healthChecks": []any{"global/healthChecks/hc"},
		"backends": []any{obj{"group": z + "instanceGroups/web", "balancingMode": "UTILIZATION"}}})
	lbCall(t, s, "POST", "regions/us-central1/networkEndpointGroups", obj{"name": "api", "networkEndpointType": "SERVERLESS",
		"cloudRun": obj{"service": "api"}})
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "api", "loadBalancingScheme": "EXTERNAL_MANAGED",
		"backends": []any{obj{"group": "regions/us-central1/networkEndpointGroups/api"}}})
	lbCall(t, s, "POST", "global/urlMaps", obj{"name": "lb", "defaultService": "global/backendServices/web",
		"hostRules": []any{obj{"hosts": []any{"*"}, "pathMatcher": "paths"}},
		"pathMatchers": []any{obj{"name": "paths", "defaultService": "global/backendServices/web",
			"pathRules": []any{obj{"paths": []any{"/api/*"}, "service": "global/backendServices/api"},
				obj{"paths": []any{"/old"}, "urlRedirect": obj{"pathRedirect": "/new", "redirectResponseCode": "MOVED_PERMANENTLY_DEFAULT"}}}}}})
	lbCall(t, s, "POST", "global/targetHttpProxies", obj{"name": "lb", "urlMap": "global/urlMaps/lb"})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "lb", "target": "global/targetHttpProxies/lb",
		"portRange": "80", "loadBalancingScheme": "EXTERNAL_MANAGED"})
	rule := lbCall(t, s, "GET", "global/forwardingRules/lb", nil)
	addr := str(rule, "cloudburrowAddress")
	if addr == "" || rule["IPAddress"] != "127.0.0.1" || rule["portRange"] != "80-80" {
		t.Fatalf("forwarding rule: %v", rule)
	}
	return s, f, "http://" + addr
}

func TestLBRoutesByPathToAnInstanceGroupAndCloudRun(t *testing.T) {
	s, _, base := lbFixture(t)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	waitFor(t, "both instances healthy", func() bool {
		h := lbCall(t, s, "POST", "global/backendServices/web/getHealth", obj{"group": "zones/us-central1-a/instanceGroups/web"})
		st := listOf(h["healthStatus"])
		return len(st) == 2 && str(objOf(st[0]), "healthState") == "HEALTHY" && str(objOf(st[1]), "healthState") == "HEALTHY"
	})
	seen := map[string]bool{}
	for range 6 {
		code, body, hdr := get(t, c, base+"/web/index.html")
		if code != 200 || !strings.Contains(body, "/web/index.html via=1.1 google") || hdr.Get("Via") != "1.1 google" {
			t.Fatalf("GET /web/index.html: %d %q %v", code, body, hdr)
		}
		seen[strings.Fields(body)[0]] = true
	}
	if !seen["web-1"] || !seen["web-2"] {
		t.Fatalf("round robin reached %v", seen)
	}
	code, body, _ := get(t, c, base+"/api/items")
	if code != 200 || !strings.HasPrefix(body, "ingress api-abc-uc.a.run.app /api/items") {
		t.Fatalf("GET /api/items: %d %q", code, body)
	}
	code, _, hdr := get(t, c, base+"/old?q=1")
	if code != http.StatusMovedPermanently || !strings.HasSuffix(hdr.Get("Location"), "/new?q=1") {
		t.Fatalf("redirect: %d %v", code, hdr)
	}
	// Session affinity by client IP keeps one instance.
	lbCall(t, s, "PATCH", "global/backendServices/web", obj{"sessionAffinity": "CLIENT_IP"})
	seen = map[string]bool{}
	for range 6 {
		_, body, _ := get(t, c, base+"/")
		seen[strings.Fields(body)[0]] = true
	}
	if len(seen) != 1 {
		t.Fatalf("CLIENT_IP affinity reached %v", seen)
	}
	// Draining a backend (capacity scaler 0) leaves no healthy backend: 503.
	bs := lbCall(t, s, "GET", "global/backendServices/web", nil)
	lbCall(t, s, "PATCH", "global/backendServices/web", obj{"fingerprint": bs["fingerprint"], "backends": []any{obj{"group": "zones/us-central1-a/instanceGroups/web",
		"balancingMode": "UTILIZATION", "capacityScaler": 0}}})
	if code, body, _ := get(t, c, base+"/"); code != http.StatusServiceUnavailable {
		t.Fatalf("drained: %d %q", code, body)
	}
}

func TestLBHealthChecksNeedTheProbeRangesAllowed(t *testing.T) {
	s, _, base := lbFixture(t)
	lbCall(t, s, "DELETE", "global/firewalls/allow-hc", nil)
	// The probes fail from the next sync on; the unhealthy threshold
	// (two by default) then marks both instances UNHEALTHY.
	waitFor(t, "both instances UNHEALTHY without the probe ranges allowed", func() bool {
		h := lbCall(t, s, "POST", "global/backendServices/web/getHealth", obj{"group": "zones/us-central1-a/instanceGroups/web"})
		l := listOf(h["healthStatus"])
		for _, e := range l {
			if str(objOf(e), "healthState") != "UNHEALTHY" {
				return false
			}
		}
		return len(l) == 2
	})
	if code, body, _ := get(t, http.DefaultClient, base+"/"); code != http.StatusServiceUnavailable || body != "no healthy upstream" {
		t.Fatalf("no healthy backend: %d %q", code, body)
	}
}

func TestLBRefusesDeletingWhatIsInUse(t *testing.T) {
	s, _, _ := lbFixture(t)
	for _, p := range []string{"global/urlMaps/lb", "global/backendServices/api", "global/healthChecks/hc", "global/targetHttpProxies/lb"} {
		if _, err := vmCall(t, s, "DELETE", p, nil); ErrorStatus(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "is already being used by") {
			t.Fatalf("DELETE %s: %v", p, err)
		}
	}
	addr := str(lbCall(t, s, "GET", "global/forwardingRules/lb", nil), "cloudburrowAddress")
	lbCall(t, s, "DELETE", "global/forwardingRules/lb", nil)
	waitFor(t, "the listener to close", func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
	for _, p := range []string{"global/targetHttpProxies/lb", "global/urlMaps/lb", "global/backendServices/api", "global/backendServices/web", "global/healthChecks/hc"} {
		lbCall(t, s, "DELETE", p, nil)
	}
}

func TestLBTCPProxyAndTargetPool(t *testing.T) {
	s, _, _ := lbFixture(t)
	lbCall(t, s, "POST", "global/backendServices", obj{"name": "tcp", "protocol": "TCP", "portName": "http",
		"loadBalancingScheme": "EXTERNAL_MANAGED", "healthChecks": []any{"global/healthChecks/hc"},
		"backends": []any{obj{"group": "zones/us-central1-a/instanceGroups/web", "balancingMode": "UTILIZATION"}}})
	lbCall(t, s, "POST", "global/targetTcpProxies", obj{"name": "tcp", "service": "global/backendServices/tcp"})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "tcp", "target": "global/targetTcpProxies/tcp",
		"portRange": "110", "loadBalancingScheme": "EXTERNAL_MANAGED"})
	addr := str(lbCall(t, s, "GET", "global/forwardingRules/tcp", nil), "cloudburrowAddress")
	waitFor(t, "the TCP proxy to serve", func() bool {
		// Until a health check passes, the proxy closes the connection.
		resp, err := http.Get("http://" + addr + "/x")
		if err != nil {
			return false
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode == 200 && strings.HasPrefix(string(b), "web-")
	})
	// A target pool keeps the rule's port (80) to its instances.
	r := "regions/us-central1/"
	lbCall(t, s, "POST", r+"targetPools", obj{"name": "pool", "instances": []any{"zones/us-central1-a/instances/web-1"}})
	lbCall(t, s, "POST", r+"forwardingRules", obj{"name": "nlb", "target": r + "targetPools/pool", "portRange": "80", "IPProtocol": "TCP"})
	addr = str(lbCall(t, s, "GET", r+"forwardingRules/nlb", nil), "cloudburrowAddress")
	if code, body, _ := get(t, http.DefaultClient, "http://"+addr+"/p"); code != 200 || !strings.HasPrefix(body, "web-1 ") {
		t.Fatalf("target pool: %d %q", code, body)
	}
	h := lbCall(t, s, "POST", r+"targetPools/pool/getHealth", obj{"instance": "zones/us-central1-a/instances/web-1"})
	if str(objOf(listOf(h["healthStatus"])[0]), "healthState") != "HEALTHY" {
		t.Fatalf("pool health: %v", h)
	}
}

func TestLBHTTPSWithAGoogleManagedCertificate(t *testing.T) {
	s, _, _ := lbFixture(t)
	lbCall(t, s, "POST", "global/sslCertificates", obj{"name": "shop", "type": "MANAGED", "managed": obj{"domains": []any{"shop.example.com"}}})
	if c := lbCall(t, s, "GET", "global/sslCertificates/shop", nil); str(objOf(c["managed"]), "status") != "PROVISIONING" {
		t.Fatalf("a managed certificate no rule serves: %v", c["managed"])
	}
	lbCall(t, s, "POST", "global/targetHttpsProxies", obj{"name": "shop", "urlMap": "global/urlMaps/lb",
		"sslCertificates": []any{"global/sslCertificates/shop"}})
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "shop", "target": "global/targetHttpsProxies/shop",
		"portRange": "443", "loadBalancingScheme": "EXTERNAL_MANAGED"})
	waitFor(t, "the certificate to be provisioned", func() bool {
		c := lbCall(t, s, "GET", "global/sslCertificates/shop", nil)
		return str(objOf(c["managed"]), "status") == "ACTIVE"
	})
	ca, err := s.LBCACertificate()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca))
	addr := str(lbCall(t, s, "GET", "global/forwardingRules/shop", nil), "cloudburrowAddress")
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "shop.example.com"}}}
	waitFor(t, "both instances healthy", func() bool {
		h := lbCall(t, s, "POST", "global/backendServices/web/getHealth", obj{"group": "zones/us-central1-a/instanceGroups/web"})
		return strings.Count(fmt.Sprint(h), "HEALTHY") == 2 && !strings.Contains(fmt.Sprint(h), "UNHEALTHY")
	})
	req, _ := http.NewRequest("GET", "https://"+addr+"/web/a", nil)
	req.Host = "shop.example.com"
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "shop.example.com /web/a") {
		t.Fatalf("HTTPS: %d %q", resp.StatusCode, b)
	}
}
