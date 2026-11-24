package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

type fakeLabs struct {
	addr  string
	label string
}

func (f fakeLabs) ProxyURI(_ context.Context, name string) (string, error) {
	if name != "projects/p/locations/us-central1-a/instances/one" {
		return "", errors.New("instance " + name + " is Stopped")
	}
	return f.label + "." + JupyterProxyDomain + ":9091", nil
}

func (f fakeLabs) Endpoint(label string) (string, string, string, error) {
	if label != f.label {
		return "", "", "", errors.New("no such instance")
	}
	return f.addr, "server-token", "one", nil
}

// TestJupyterLabProxy (#2198): Open JupyterLab answers a one-time link on
// the instance's proxy host; following it sets the host's session cookie;
// with it, every request (a page, an API call, a WebSocket) reaches the
// instance's Jupyter Server with the server's token and the browser's
// Host, the console's cookie kept back; without it, with another host's,
// from another site or another origin, nothing is proxied.
func TestJupyterLabProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/kernels/k/channels" {
			websocket.Handler(func(ws *websocket.Conn) {
				var msg string
				_ = websocket.Message.Receive(ws, &msg)
				_ = websocket.Message.Send(ws, "echo "+msg+" "+ws.Request().Header.Get("Authorization"))
			}).ServeHTTP(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path, "auth": r.Header.Get("Authorization"),
			"host": r.Host, "cookie": r.Header.Get("Cookie")})
	}))
	defer backend.Close()
	label := "0123456789abcdef-dot-us-central1"
	host := label + "." + JupyterProxyDomain + ":9091"
	s := New("127.0.0.1:0", nil)
	s.SetJupyterLabs(fakeLabs{addr: strings.TrimPrefix(backend.URL, "http://"), label: label})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path, hostHeader string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Host = hostHeader
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	// A stopped instance has no link.
	if resp, body := do(http.MethodPost, "/api/workbench/open?name=projects/p/locations/us-central1-a/instances/two", "", nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("open a stopped instance = %d %s", resp.StatusCode, body)
	}
	open := func() string {
		t.Helper()
		resp, body := do(http.MethodPost, "/api/workbench/open?name="+url.QueryEscape("projects/p/locations/us-central1-a/instances/one"), "", nil)
		var got struct{ URL string }
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("open = %d %s", resp.StatusCode, body)
		}
		u, err := url.Parse(got.URL)
		if err != nil || u.Host != host || u.Path != jupyterSessionPath || u.Query().Get("next") != "/lab" {
			t.Fatalf("open's link = %s", got.URL)
		}
		return u.RequestURI()
	}

	// Without the session, nothing is proxied.
	if resp, body := do(http.MethodGet, "/lab", host, nil); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Open JupyterLab from the console") {
		t.Errorf("no session = %d %s", resp.StatusCode, body)
	}
	link := open()
	// The link is the proxy host's alone.
	if resp, _ := do(http.MethodGet, link, "fedcba9876543210-dot-us-central1."+JupyterProxyDomain+":9091", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the link on another host = %d", resp.StatusCode)
	}
	link = open()
	resp, _ := do(http.MethodGet, link, host, map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"})
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/lab" {
		t.Fatalf("session = %d %v", resp.StatusCode, resp.Header)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == jupyterCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Domain != "" {
		t.Fatalf("session cookie = %+v", cookie)
	}
	// A link is used once.
	if resp, _ := do(http.MethodGet, link, host, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a used link = %d", resp.StatusCode)
	}
	withCookie := map[string]string{"Cookie": jupyterCookie + "=" + cookie.Value + "; _xsrf=abc", "Sec-Fetch-Site": "same-origin"}
	resp, body := do(http.MethodGet, "/api/contents?type=directory", host, withCookie)
	var seen map[string]string
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &seen) != nil {
		t.Fatalf("proxied = %d %s", resp.StatusCode, body)
	}
	if seen["path"] != "/api/contents" || seen["auth"] != "token server-token" || seen["host"] != host || seen["cookie"] != "_xsrf=abc" {
		t.Errorf("the server was sent %v", seen)
	}
	// Another proxy host's session, another site's request and another
	// origin are refused.
	if resp, _ := do(http.MethodGet, "/lab", "fedcba9876543210-dot-us-central1."+JupyterProxyDomain+":9091", withCookie); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the session on another host = %d", resp.StatusCode)
	}
	sameSite := map[string]string{"Cookie": withCookie["Cookie"], "Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors"}
	if resp, _ := do(http.MethodPost, "/api/kernels", host, sameSite); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a same-site POST = %d", resp.StatusCode)
	}
	other := map[string]string{"Cookie": withCookie["Cookie"], "Origin": "http://fedcba9876543210-dot-us-central1." + JupyterProxyDomain + ":9091"}
	if resp, _ := do(http.MethodPost, "/api/kernels", host, other); resp.StatusCode != http.StatusForbidden {
		t.Errorf("another origin's POST = %d", resp.StatusCode)
	}

	// A kernel's channels, as a WebSocket through the proxy.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := websocket.NewConfig("ws://"+host+"/api/kernels/k/channels", "http://"+host)
	cfg.Header = http.Header{"Cookie": {jupyterCookie + "=" + cookie.Value}}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		t.Fatalf("websocket through the proxy: %v", err)
	}
	defer ws.Close()
	if err := websocket.Message.Send(ws, "hi"); err != nil {
		t.Fatal(err)
	}
	var echo string
	if err := websocket.Message.Receive(ws, &echo); err != nil || echo != "echo hi token server-token" {
		t.Errorf("websocket echo = %q, %v", echo, err)
	}

	// The console's own API is not served on a proxy host, and the
	// console's page is not either.
	if resp, _ := do(http.MethodGet, "/api/status", host, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the console API on a proxy host = %d", resp.StatusCode)
	}
}

func TestJupyterLabel(t *testing.T) {
	for host, want := range map[string]string{
		"abc-dot-us-central1.notebooks.localhost:9091": "abc-dot-us-central1",
		"ABC-dot-us-central1.Notebooks.localhost":      "abc-dot-us-central1",
		"abc.notebooks.localhost:9091":                 "",
		"abc-dot-x.other.localhost:9091":               "",
		"a.b-dot-x.notebooks.localhost":                "",
		"127.0.0.1:9091":                               "",
		"localhost:9091":                               "",
	} {
		if got := jupyterLabel(host); got != want {
			t.Errorf("jupyterLabel(%q) = %q, want %q", host, got, want)
		}
	}
}
