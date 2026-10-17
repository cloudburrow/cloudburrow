package hostguard

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The cross-site check (#1586): what a page on another site can send to a
// loopback listener, and what the clients CloudBurrow serves send.
func TestWrapRefusesCrossSiteRequests(t *testing.T) {
	// Not parallel: AllowOrigins is process-wide.
	if err := AllowOrigins([]string{"https://app.example:8443/"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = AllowOrigins(nil) })

	ran := 0
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran++ }))
	for _, c := range []struct {
		method, host, origin, fetchSite string
		want                            bool
		why                             string
	}{
		{"POST", "127.0.0.1:9002", "", "", true, "an SDK, gcloud, Terraform or curl: no Origin"},
		{"POST", "127.0.0.1:9002", "http://localhost:3000", "cross-site", true, "a local web app under development"},
		{"POST", "127.0.0.1:9002", "http://127.0.0.1:5173", "same-site", true, "a loopback address origin"},
		{"POST", "127.0.0.1:9002", "http://[::1]:8080", "cross-site", true, "IPv6 loopback origin"},
		{"POST", "127.0.0.1:9090", "http://127.0.0.1:9090", "same-origin", true, "the listener's own page"},
		{"POST", "192.168.1.20:9090", "http://192.168.1.20:9090", "same-origin", true, "its own page under --allow-remote"},
		{"POST", "127.0.0.1:9002", "https://app.example:8443", "cross-site", true, "an origin named with --cors-allow-origin"},
		{"GET", "127.0.0.1:9002", "", "cross-site", true, "a no-cors GET (an image): the page cannot read it"},

		{"POST", "127.0.0.1:9002", "https://evil.example", "cross-site", false, "a simple cross-site POST (CSRF)"},
		{"OPTIONS", "127.0.0.1:9002", "https://evil.example", "cross-site", false, "a preflight from another site"},
		{"GET", "127.0.0.1:9002", "https://evil.example", "cross-site", false, "a CORS read from another site"},
		{"POST", "127.0.0.1:9002", "null", "cross-site", false, "a sandboxed frame or file: page"},
		{"POST", "127.0.0.1:9002", "https://app.example", "cross-site", false, "an allowed host on another port"},
		{"POST", "192.168.1.20:9090", "http://192.168.1.21:9090", "cross-site", false, "another machine's page"},
		{"POST", "127.0.0.1:9002", "", "cross-site", false, "an unsafe method with fetch metadata but no Origin"},
		{"DELETE", "127.0.0.1:9002", "", "same-site", false, "same, from a sibling site"},
	} {
		ran = 0
		req := httptest.NewRequest(c.method, "http://"+c.host+"/v1/projects/p/topics/t", strings.NewReader("{}"))
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", c.fetchSite)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := ran == 1; got != c.want {
			t.Errorf("%s (%s %s Origin %q): served=%v, want %v; status %d", c.why, c.method, c.host, c.origin, got, c.want, rec.Code)
			continue
		}
		if !c.want {
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: status %d, want 403", c.why, rec.Code)
			}
			if v := rec.Header().Get("Access-Control-Allow-Origin"); v != "" {
				t.Errorf("%s: a refusal carried Access-Control-Allow-Origin %q", c.why, v)
			}
			if !strings.Contains(rec.Body.String(), "--cors-allow-origin") {
				t.Errorf("%s: the refusal does not say how to allow an origin: %q", c.why, rec.Body.String())
			}
		}
	}
}

// The listener's own origin is its scheme and Host: an https page on the
// same host and port is not the http listener's own.
func TestOwnOriginNeedsTheScheme(t *testing.T) {
	t.Parallel()
	if OriginAllowed("https://192.168.1.20:9090", "http", "192.168.1.20:9090") {
		t.Error("an https origin passed as an http listener's own")
	}
	if !OriginAllowed("https://192.168.1.20:9090", "https", "192.168.1.20:9090") {
		t.Error("an https listener's own origin was refused")
	}
	req := httptest.NewRequest("POST", "https://192.168.1.20:9090/x", nil)
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Origin", "https://192.168.1.20:9090")
	if !CrossSiteAllowed(req) {
		t.Error("CrossSiteAllowed refused a TLS listener's own origin")
	}
}

// WrapHost is for a listener that checks Origin itself (storage, #677):
// it still refuses a foreign Host, and leaves Origin alone.
func TestWrapHostLeavesOriginToTheListener(t *testing.T) {
	t.Parallel()
	ran := false
	h := WrapHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran = true }))
	req := httptest.NewRequest("POST", "http://127.0.0.1:4443/upload", nil)
	req.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !ran {
		t.Error("WrapHost checked Origin")
	}
	req = httptest.NewRequest("POST", "http://attacker.example:4443/upload", nil)
	rec := httptest.NewRecorder()
	ran = false
	h.ServeHTTP(rec, req)
	if ran || rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("WrapHost served a foreign Host: ran=%v status %d", ran, rec.Code)
	}
}

func TestAllowOriginsRefusesAWildcard(t *testing.T) {
	t.Parallel()
	if err := validateOrigins([]string{"https://*.example"}); err == nil {
		t.Error("a wildcard origin was accepted")
	}
}

// WrapService (#1587) is Wrap for a front in a pod: it also answers to the
// names pods dial its Service by, in any namespace, and to no other
// Service's, and it still checks Origin.
func TestWrapServiceAnswersToItsServiceName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		service, host, origin string
		want                  int
	}{
		{"bigquery", "bigquery.cloudburrow.svc.cluster.local:9050", "", http.StatusOK},
		{"bigquery", "BigQuery.team-a.svc:9050", "", http.StatusOK},
		{"bigquery", "bigquery:9050", "", http.StatusOK},
		{"bigquery", "127.0.0.1:9050", "", http.StatusOK},
		{"bigquery", "bigquery.cloudburrow:9050", "", http.StatusMisdirectedRequest},
		{"bigquery", "bigquery.attacker.example:9050", "", http.StatusMisdirectedRequest},
		{"bigquery", "pubsub.cloudburrow.svc.cluster.local:9050", "", http.StatusMisdirectedRequest},
		{"bigquery", "bigquery.cloudburrow.svc.cluster.local:9050", "https://evil.example", http.StatusForbidden},
		{"", ".x.svc:9050", "", http.StatusMisdirectedRequest},
		{"", "bigquery.cloudburrow.svc.cluster.local:9050", "", http.StatusMisdirectedRequest},
	} {
		h := WrapService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), c.service)
		req := httptest.NewRequest("GET", "http://"+c.host+"/x", nil)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("WrapService(%q): Host %s, Origin %q: status %d, want %d", c.service, c.host, c.origin, rec.Code, c.want)
		}
	}
}
