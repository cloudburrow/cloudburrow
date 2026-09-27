package localhost

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRewrite(t *testing.T) {
	for addr, want := range map[string]string{
		"hello.default.cloudburrow.localhost:9080": "127.0.0.1:9080",
		"HELLO.Default.CloudBurrow.LOCALHOST:80":   "127.0.0.1:80",
		"foo.localhost.:8080":                      "127.0.0.1:8080",
		"foo.localhost:443":                        "127.0.0.1:443",
		// Left to the resolver.
		"localhost:9080":                "localhost:9080",
		".localhost:9080":               ".localhost:9080",
		"localhost.example:9080":        "localhost.example:9080",
		"notlocalhost:9080":             "notlocalhost:9080",
		"example.com:443":               "example.com:443",
		"127.0.0.1:9080":                "127.0.0.1:9080",
		"[::1]:9080":                    "[::1]:9080",
		"no-port.cloudburrow.localhost": "no-port.cloudburrow.localhost",
	} {
		if got := Rewrite(addr); got != want {
			t.Errorf("Rewrite(%q) = %q, want %q", addr, got, want)
		}
	}
}

// TestTransportDialsLocalhostNamesOnLoopback: a request to a `.localhost`
// name reaches a server on 127.0.0.1 with its Host header intact, which is
// what the gateway routes by.
func TestTransportDialsLocalhostNamesOnLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Host)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	host := "hello.default.cloudburrow.localhost:" + port
	resp, err := (&http.Client{Transport: Transport()}).Get("http://" + host + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != host {
		t.Errorf("the server saw Host %q, want %q", body, host)
	}
}
