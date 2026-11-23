package networkservices

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	nsvpb "cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"google.golang.org/protobuf/types/known/durationpb"
)

type fakePolicies struct {
	rules map[string][]GatewayRule
	lists map[string][]string
}

func (f *fakePolicies) GatewayRules(p string) ([]GatewayRule, bool) { r, ok := f.rules[p]; return r, ok }
func (f *fakePolicies) URLList(n string) ([]string, bool)           { l, ok := f.lists[n]; return l, ok }

// loopbackOnly refuses any address that is not on this machine, so the
// test's proxy can never reach the internet.
func loopbackOnly(ctx context.Context, network, addr string) (net.Conn, error) {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if h == "localhost" {
		addr = net.JoinHostPort("127.0.0.1", addr[strings.LastIndex(addr, ":")+1:])
	} else if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
		return nil, &net.AddrError{Err: "not loopback", Addr: addr}
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

const (
	pol  = "projects/p/locations/us-central1/gatewaySecurityPolicies/pol"
	list = "projects/p/locations/us-central1/urlLists/allowed"
)

func newGateway(t *testing.T, pols Policies) (*Server, string) {
	t.Helper()
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.UsePolicies(pols)
	s.UseTransport(&http.Transport{Proxy: nil, DialContext: loopbackOnly}, loopbackOnly)
	s.Start()
	t.Cleanup(s.Stop)
	_, err = s.CreateGateway(context.Background(), &nsvpb.CreateGatewayRequest{Parent: "projects/p/locations/us-central1", GatewayId: "swp",
		Gateway: &nsvpb.Gateway{Type: nsvpb.Gateway_SECURE_WEB_GATEWAY, Ports: []int32{443}, GatewaySecurityPolicy: pol,
			Network: "projects/p/global/networks/default", Subnetwork: "projects/p/regions/us-central1/subnetworks/default"}})
	if err != nil {
		t.Fatal(err)
	}
	addr := s.GatewayAddress("projects/p/locations/us-central1/gateways/swp", 443)
	if addr == "" {
		t.Fatal("the gateway listens on nothing")
	}
	return s, addr
}

func TestSecureWebProxyRules(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello "+r.URL.Path)
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)
	_, port, _ := net.SplitHostPort(bu.Host)
	pols := &fakePolicies{lists: map[string][]string{list: {"127.0.0.1/allowed/*"}}, rules: map[string][]GatewayRule{pol: {
		{Name: "deny-secret", Priority: 1, Enabled: true, Allow: false, Session: "host() == '127.0.0.1'", Application: "request.path.startsWith('/secret')"},
		{Name: "list", Priority: 5, Enabled: true, Allow: true, Session: "true", Application: "inUrlList(request.url(), '" + list + "')"},
		{Name: "disabled", Priority: 6, Enabled: false, Allow: true, Session: "true"},
		{Name: "tunnel", Priority: 10, Enabled: true, Allow: true, Session: "host() == 'localhost' && destination.port == " + port},
		{Name: "ua", Priority: 20, Enabled: true, Allow: true, Session: "inIpRange(source.ip, '127.0.0.0/8')", Application: "request.useragent() == 'swp-test'"},
	}}}
	_, addr := newGateway(t, pols)
	pu, _ := url.Parse("http://" + addr)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 10 * time.Second}
	get := func(path, ua string) int {
		req, _ := http.NewRequest("GET", backend.URL+path, nil)
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, tc := range []struct {
		path, ua string
		want     int
	}{
		{"/allowed/x", "", 200},
		{"/secret/allowed/x", "", 403},
		{"/other", "", 403},
		{"/other", "swp-test", 200},
		{"/secret", "swp-test", 403},
	} {
		if got := get(tc.path, tc.ua); got != tc.want {
			t.Errorf("GET %s (ua %q): %d, want %d", tc.path, tc.ua, got, tc.want)
		}
	}
	// CONNECT: the tunnel rule matches localhost on the backend's port;
	// 127.0.0.1 matches no session-only rule, as a rule with an
	// applicationMatcher never matches a tunnel.
	connect := func(target string) string {
		cn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cn.Close() }()
		_, _ = io.WriteString(cn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
		_ = cn.SetReadDeadline(time.Now().Add(5 * time.Second))
		b := make([]byte, 4096)
		n, _ := cn.Read(b)
		first := string(b[:n])
		if !strings.HasPrefix(first, "HTTP/1.1 200") {
			return first
		}
		_, _ = io.WriteString(cn, "GET /through HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
		all, _ := io.ReadAll(cn)
		return first + string(all)
	}
	if got := connect("localhost:" + port); !strings.Contains(got, "hello /through") {
		t.Errorf("CONNECT localhost: %q", got)
	}
	if got := connect("127.0.0.1:" + port); !strings.HasPrefix(got, "HTTP/1.1 403") {
		t.Errorf("CONNECT 127.0.0.1: %q, want 403", got)
	}
}

func TestSecureWebProxyNeverDialsGoogle(t *testing.T) {
	pols := &fakePolicies{rules: map[string][]GatewayRule{pol: {{Name: "all", Priority: 1, Enabled: true, Allow: true, Session: "true"}}}}
	s, addr := newGateway(t, pols)
	dialed := false
	s.UseTransport(nil, func(ctx context.Context, n, a string) (net.Conn, error) { dialed = true; return loopbackOnly(ctx, n, a) })
	cn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cn.Close() }()
	_, _ = io.WriteString(cn, "CONNECT storage.googleapis.com:443 HTTP/1.1\r\nHost: storage.googleapis.com:443\r\n\r\n")
	b := make([]byte, 512)
	_ = cn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _ := cn.Read(b)
	if !strings.HasPrefix(string(b[:n]), "HTTP/1.1 502") || dialed {
		t.Errorf("CONNECT to Google: %q, dialed %v; want 502 without a dial", b[:n], dialed)
	}
}

func TestSecureWebProxyDefaultDeny(t *testing.T) {
	_, addr := newGateway(t, &fakePolicies{rules: map[string][]GatewayRule{pol: nil}})
	pu, _ := url.Parse("http://" + addr)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := c.Get("http://127.0.0.1:9/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("no rule: %d, want 403", resp.StatusCode)
	}
}

func TestURLListMatching(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		yes, no []string
	}{
		{"*.example.com", []string{"server.example.com/", "sample.server.example.com/path/", "server.example.com"}, []string{"example.com/", "example.com/path", "myexample.com/", "example.com"}},
		{"example.com", []string{"example.com/", "example.com/path/subpath", "example.com"}, []string{"server.example.com/", "example.com.bad.com/"}},
		{"*example.com/path/*", []string{"example.com/path/subpath", "sample.server.example.com/path/", "example.com"}, []string{"example.com/path", "example.com/"}},
		{"example.com/", []string{"example.com/", "example.com"}, []string{"server.example.com/", "example.com/path"}},
	} {
		for _, v := range tc.yes {
			if !urlListMatches(v, []string{tc.pattern}) {
				t.Errorf("%s does not match %s", v, tc.pattern)
			}
		}
		for _, v := range tc.no {
			if urlListMatches(v, []string{tc.pattern}) {
				t.Errorf("%s matches %s", v, tc.pattern)
			}
		}
	}
	for e, ok := range map[string]bool{"example.com": true, "*.example.com": true, "example.com/a/*": true, "localhost": false, "a b.com": false} {
		if ValidURLListEntry(e) != ok {
			t.Errorf("ValidURLListEntry(%q) = %v", e, !ok)
		}
	}
}

func TestMatchers(t *testing.T) {
	for e, app := range map[string]bool{
		"host() == 'a.com' && source.matchTag('tagValues/1')":       false,
		"source.matchServiceAccount('x@p.iam.gserviceaccount.com')": false,
		"inIpRange(source.ip, '10.0.0.0/8') && destination.port == 443": false,
		"request.method == 'GET' && request.url().startsWith('a.com/')": true,
		"request.headers['x'] == 'y' && request.host() == 'a.com'":      true,
	} {
		if err := CheckMatcher(e, app); err != nil {
			t.Errorf("%s: %v", e, err)
		}
	}
	if CheckMatcher("request.method == 'GET'", false) == nil {
		t.Error("a sessionMatcher reading request compiled")
	}
	if CheckMatcher("1 + 1", false) == nil {
		t.Error("a non-boolean matcher compiled")
	}
}

func TestAuthzExtensionValidation(t *testing.T) {
	s, _ := New(Options{})
	ctx := context.Background()
	parent := "projects/p/locations/us-central1"
	ext := &nsvpb.AuthzExtension{Service: "https://www.googleapis.com/compute/v1/projects/p/regions/us-central1/backendServices/authz",
		Authority: "authz.example.com", LoadBalancingScheme: nsvpb.LoadBalancingScheme_INTERNAL_MANAGED, Timeout: durationpb.New(5 * time.Millisecond)}
	if _, err := s.CreateAuthzExtension(ctx, &nsvpb.CreateAuthzExtensionRequest{Parent: parent, AuthzExtensionId: "a", AuthzExtension: ext}); err == nil {
		t.Error("a 5 ms timeout was accepted")
	}
	ext.Timeout = durationpb.New(100 * time.Millisecond)
	ext.ForwardHeaders = []string{"Authorization"}
	if _, err := s.CreateAuthzExtension(ctx, &nsvpb.CreateAuthzExtensionRequest{Parent: parent, AuthzExtensionId: "a", AuthzExtension: ext}); err != nil {
		t.Fatal(err)
	}
	info, ok := s.AuthzExtension(parent + "/authzExtensions/a")
	if !ok || info.Service != "projects/p/regions/us-central1/backendServices/authz" || info.WireFormat != "EXT_PROC_GRPC" ||
		info.Timeout != 100*time.Millisecond || info.ForwardHeaders[0] != "Authorization" {
		t.Errorf("AuthzExtension: %+v %v", info, ok)
	}
}
