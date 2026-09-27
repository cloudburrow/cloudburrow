package hostguard

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/hostrelay"
)

// Every way a legitimate caller names a CloudBurrow listener, and the names
// a rebinding page would arrive with (#676).
func TestAllowed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		host  string
		extra []string
		want  bool
		why   string
	}{
		{"", nil, true, "no Host: HTTP/1.0 and non-browser clients"},
		{"127.0.0.1:9090", nil, true, "the default bind, as the SDKs, gcloud and Terraform are configured"},
		{"127.0.0.1", nil, true, "no port"},
		{"[::1]:9090", nil, true, "IPv6 loopback"},
		{"[::1]", nil, true, "IPv6 loopback without a port"},
		{"localhost:9090", nil, true, "localhost"},
		{"LocalHost.:9090", nil, true, "case and a root dot do not matter"},
		{"my-bucket.storage.localhost:9001", nil, true, "a virtual-hosted bucket through the storage tunnel"},
		{"0.0.0.0:9002", nil, true, "--allow-remote with the wildcard bind"},
		{"192.168.1.20:9002", nil, true, "--allow-remote with a LAN bind address"},
		{"172.18.0.1:9003", nil, true, "the kind network gateway, where the Linux relay listens (#575)"},
		{"10.244.0.7:4443", nil, true, "a kubelet probe to a pod IP"},
		{"[fe80::1%en0]:9002", nil, true, "a scoped IPv6 literal"},
		{"host.docker.internal:9003", nil, true, "Docker Desktop's name for the host"},
		{"cloudburrow-host.cloudburrow.svc.cluster.local:9003", nil, true, "the in-cluster name (#575)"},
		{"cloudburrow-host.team-a.svc.cluster.local:9003", nil, true, "another namespace"},
		{"cloudburrow-host.cloudburrow.svc:9003", nil, true, "the search-path form"},
		{"cloudburrow-host:9003", nil, true, "the bare Service name"},

		{"attacker.example:9090", nil, false, "a rebound attacker domain"},
		{"attacker.example", nil, false, "without a port"},
		{"localhost.attacker.example:9090", nil, false, "localhost as a label of another domain"},
		{"127.0.0.1.attacker.example:9090", nil, false, "an address as a label of another domain"},
		{"127.0.0.1.sslip.io:9090", nil, false, "a public wildcard-DNS name for loopback"},
		{"cloudburrow-host.dev:9003", nil, false, "<service>.<namespace>, which can be a registrable domain"},
		{"cloudburrow-host.example.com:9003", nil, false, "cloudburrow-host under a public domain"},
		{"cloudburrow-host.x.svc.cluster.local.attacker.example", nil, false, "the in-cluster name as a prefix"},
		{"host.docker.internal.attacker.example", nil, false, "host.docker.internal as a prefix"},
		{"storage.cb.svc.cluster.local:4443", nil, false, "a name this listener did not add"},
		{"2130706433:9090", nil, false, "a decimal address is not a literal Go or a browser sends"},

		{"storage.cb.svc.cluster.local:4443", []string{"storage.cb.svc.cluster.local"}, true, "an exact extra name"},
		{"b.storage.cb.svc.cluster.local:4443", []string{"*.storage.cb.svc.cluster.local"}, true, "under a wildcard extra"},
		{"storage.cb.svc.cluster.local:4443", []string{"*.storage.cb.svc.cluster.local"}, false, "a wildcard does not match its own base"},
		{"evilstorage.cb.svc.cluster.local", []string{"*.storage.cb.svc.cluster.local"}, false, "a wildcard matches whole labels"},
		{"metadata.google.internal", []string{"Metadata.Google.Internal"}, true, "extra names are case-insensitive"},
	} {
		if got := Allowed(c.host, c.extra...); got != c.want {
			t.Errorf("Allowed(%q, %q) = %v, want %v: %s", c.host, c.extra, got, c.want, c.why)
		}
	}
}

func serveGuarded(t *testing.T, extra ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "served")
	}), extra...))
	t.Cleanup(srv.Close)
	return srv
}

func getHost(t *testing.T, url, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A refusal is 421 and names the host, so a caller that was refused can see
// why rather than debugging its own client.
func TestWrapRefusesAForeignHostByName(t *testing.T) {
	t.Parallel()
	srv := serveGuarded(t)
	code, body := getHost(t, srv.URL+"/v1/projects/p/secrets", "attacker.example:9004")
	if code != http.StatusMisdirectedRequest {
		t.Errorf("status = %d, want 421", code)
	}
	for _, want := range []string{`"attacker.example:9004"`, "DNS-rebinding", "127.0.0.1"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %q does not mention %s", body, want)
		}
	}
	if code, body := getHost(t, srv.URL, "localhost:9004"); code != http.StatusOK || body != "served" {
		t.Errorf("localhost = %d %q, want 200 served", code, body)
	}
}

// Cleartext HTTP/2 never comes from a browser, so it is not checked: a gRPC
// client dialed at any name keeps working on a port that shares gRPC with
// a JSON API. HTTP/2 over TLS, which a browser does speak, is checked.
func TestWrapLeavesCleartextHTTP2Alone(t *testing.T) {
	t.Parallel()
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, c := range []struct {
		name  string
		major int
		tls   bool
		want  int
	}{
		{"h2c, as gRPC with insecure credentials", 2, false, http.StatusNoContent},
		{"HTTP/1.1, as a browser", 1, false, http.StatusMisdirectedRequest},
		{"h2 over TLS, as a browser could", 2, true, http.StatusMisdirectedRequest},
	} {
		r := httptest.NewRequest(http.MethodPost, "/google.pubsub.v1.Publisher/Publish", nil)
		r.Host, r.ProtoMajor = "some-name-a-client-dialed:9003", c.major
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
	}
}

// On Docker Engine a pod reaches the CLI through a relay on the kind
// network gateway (#575). The relay copies bytes, so the Host the service
// sees is whatever the pod used: the cluster-host name the pod is given,
// or the gateway address itself. Both are served; a foreign name through
// the same relay is not.
func TestARelayedPodRequestIsServed(t *testing.T) {
	t.Parallel()
	srv := serveGuarded(t)
	relay := &hostrelay.Relay{Listen: "127.0.0.1:0", Target: srv.Listener.Addr().String()}
	if err := relay.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	_, port, _ := net.SplitHostPort(relay.Addr())
	url := "http://" + relay.Addr() + "/"
	for host, want := range map[string]int{
		"cloudburrow-host.cloudburrow.svc.cluster.local:" + port: http.StatusOK,
		"172.18.0.1:" + port:       http.StatusOK,
		relay.Addr():               http.StatusOK,
		"attacker.example:" + port: http.StatusMisdirectedRequest,
	} {
		if code, _ := getHost(t, url, host); code != want {
			t.Errorf("through the relay with Host %s = %d, want %d", host, code, want)
		}
	}
}
