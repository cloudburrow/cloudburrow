package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// fakeRuntime answers the docker and kubectl calls clusterHost makes.
type fakeRuntime struct {
	desktopIP string // what host.docker.internal resolves to in the node; "" on Linux
	gateway   string
	info      string // `docker info` JSON; "" fails the call
	applied   string
	calls     []string
}

// Run answers the docker calls.
func (f *fakeRuntime) Run(_ context.Context, name string, args ...string) (string, error) {
	return f.answer("", name, args)
}

// kubeVia is the fake's kubectl, which clusterHost runs through internal/k8s.
type kubeVia struct{ f *fakeRuntime }

func (k kubeVia) Run(_ context.Context, stdin string, args ...string) (string, error) {
	return k.f.answer(stdin, "kubectl", args)
}

// use makes f h's docker and kubectl.
func (f *fakeRuntime) use(h *clusterHost) { h.docker, h.kube = f, kubeVia{f} }

func (f *fakeRuntime) answer(stdin, name string, args []string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case strings.Contains(call, "getent ahostsv4 host.docker.internal"):
		if f.desktopIP == "" {
			return "", errors.New("exit status 2")
		}
		return f.desktopIP + "     STREAM host.docker.internal\n", nil
	case strings.HasPrefix(call, "docker info "):
		if f.info == "" {
			return "", errors.New("exit status 1")
		}
		return f.info, nil
	case strings.Contains(call, "network inspect kind"):
		return f.gateway + " fc00:f853:ccd:e793::1 \n", nil
	case strings.HasPrefix(call, "kubectl ") && strings.Contains(call, "apply -f -"):
		f.applied = stdin
		return "", nil
	}
	return "", errors.New("unexpected " + call)
}

func hostCfg() config.Config {
	var c config.Config
	c.Name, c.Cluster.Namespace = "demo", "cloudburrow"
	return c
}

// On Docker Desktop the node resolves host.docker.internal to the host, whose
// loopback listeners pods already reach (#553): the Service points there and
// nothing more is bound. Admin is never published (ADR-0004).
func TestClusterHostOnDockerDesktop(t *testing.T) {
	f := &fakeRuntime{desktopIP: "192.168.65.254"}
	h := newClusterHost(hostCfg(), io.Discard, func() map[string]string {
		return map[string]string{"secretmanager": "127.0.0.1:9006", "tasks": "127.0.0.1:9003", "metadata": "127.0.0.1:9005"}
	})
	f.use(h)
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	if got := h.InCluster("secretmanager"); got != "cloudburrow-host.cloudburrow.svc.cluster.local:9006" {
		t.Errorf("InCluster(secretmanager) = %q", got)
	}
	if h.InCluster("control") != "" {
		t.Error("control was published")
	}
	for _, want := range []string{"kind: Service", "name: cloudburrow-host", "kind: EndpointSlice", "kubernetes.io/service-name: cloudburrow-host",
		`addresses: ["192.168.65.254"]`, "name: secretmanager\n    port: 9006", "name: tasks\n    port: 9003", "name: metadata\n    port: 9005"} {
		if !strings.Contains(f.applied, want) {
			t.Errorf("manifest lacks %q:\n%s", want, f.applied)
		}
	}
	if strings.Contains(f.applied, "control") || strings.Contains(f.applied, "admin") {
		t.Errorf("the admin port reached the manifest:\n%s", f.applied)
	}
	if len(h.relays) != 0 {
		t.Errorf("%d relays on Docker Desktop; want none", len(h.relays))
	}
	// The Service is applied through internal/k8s with the instance's
	// kubeconfig and the arguments it always had (#599).
	want := "kubectl --kubeconfig " + hostCfg().KubeconfigPath() + " apply -f -"
	if last := f.calls[len(f.calls)-1]; last != want {
		t.Errorf("applied with %q, want %q", last, want)
	}
}

// On Docker Engine the host is the kind gateway: a relay there forwards to
// the service's loopback listener, and the Service points at the gateway.
// 127.0.0.2 stands in for the gateway, so the relay is really bound and used.
func TestClusterHostOnDockerEngineRelays(t *testing.T) {
	if ln, err := net.Listen("tcp", "127.0.0.2:0"); err != nil {
		t.Skip("127.0.0.2 is not bindable here (it is on Linux)")
	} else {
		ln.Close()
	}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret manager")) }))
	defer svc.Close()
	addr := strings.TrimPrefix(svc.URL, "http://")
	f := &fakeRuntime{gateway: "127.0.0.2"}
	h := newClusterHost(hostCfg(), io.Discard, func() map[string]string { return map[string]string{"secretmanager": addr} })
	f.use(h)
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	if !strings.Contains(f.applied, `addresses: ["127.0.0.2"]`) {
		t.Errorf("the EndpointSlice does not point at the gateway:\n%s", f.applied)
	}
	_, port, _ := net.SplitHostPort(addr)
	resp, err := http.Get("http://127.0.0.2:" + port + "/")
	if err != nil {
		t.Fatalf("through the gateway relay: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "secret manager" {
		t.Errorf("relayed answer %q", b)
	}
	h.Stop(context.Background())
	if _, err := net.Dial("tcp", "127.0.0.2:"+port); err == nil {
		t.Error("the relay still listens after Stop")
	}
}

// With no way to find the host the start fails with the reason, rather than
// publishing a Service that points nowhere.
func TestClusterHostWithoutAGatewayFails(t *testing.T) {
	f := &fakeRuntime{gateway: "fc00::1"}
	h := newClusterHost(hostCfg(), io.Discard, func() map[string]string { return map[string]string{"tasks": "127.0.0.1:9003"} })
	f.use(h)
	if err := h.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "IPv4 gateway") {
		t.Errorf("Start = %v; want an error naming the missing gateway", err)
	}
	if f.applied != "" {
		t.Error("a Service was applied with no address to point at")
	}
}

// An engine whose kind gateway is not an address on this host fails before
// anything is bound or applied, naming the engine and the way round it,
// rather than with a bare listen error (#712). The fixtures are the
// constructed `docker info` of each engine in internal/doctor/testdata.
func TestClusterHostGatewayNotOnThisHostNamesTheEngine(t *testing.T) {
	notHere := func(string, string) (net.Listener, error) {
		return nil, errors.New("listen tcp 172.18.0.1:0: bind: cannot assign requested address")
	}
	for _, c := range []struct {
		fixture, goos string
		want          []string
	}{
		{"rootless-docker-linux.json", "linux", []string{"rootless Docker", "rootless daemon's network namespace", "--services"}},
		{"colima-macos.json", "darwin", []string{"colima (in a VM)", "use Docker Desktop", "--services"}},
		{"podman-machine-macos.json", "darwin", []string{"Podman (in a VM, rootless)", "Podman is unsupported", "--services"}},
		{"", "linux", []string{"not identified", "--services"}},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			f := &fakeRuntime{gateway: "172.18.0.1"}
			if c.fixture != "" {
				b, err := os.ReadFile(filepath.Join("..", "..", "internal", "doctor", "testdata", "dockerinfo", c.fixture))
				if err != nil {
					t.Fatal(err)
				}
				f.info = string(b)
			}
			h := newClusterHost(hostCfg(), io.Discard, func() map[string]string { return map[string]string{"tasks": "127.0.0.1:9003"} })
			f.use(h)
			h.listen, h.goos = notHere, c.goos
			err := h.Start(context.Background())
			if err == nil {
				t.Fatal("Start succeeded with a gateway that cannot be bound")
			}
			for _, w := range append([]string{"172.18.0.1", "not an address on this host", "cannot assign requested address"}, c.want...) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
			if f.applied != "" || len(h.relays) != 0 {
				t.Error("a Service was applied or a relay bound for a gateway pods cannot use")
			}
		})
	}
}

// Docker Desktop never gets as far as the gateway: host.docker.internal
// resolves, so nothing is bound and the probe is not asked.
func TestClusterHostOnDockerDesktopDoesNotProbeTheGateway(t *testing.T) {
	f := &fakeRuntime{desktopIP: "192.168.65.254"}
	h := newClusterHost(hostCfg(), io.Discard, func() map[string]string { return map[string]string{"tasks": "127.0.0.1:9003"} })
	f.use(h)
	h.listen = func(string, string) (net.Listener, error) {
		t.Error("the gateway was probed on Docker Desktop")
		return nil, errors.New("unexpected")
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
}

// A routed Service (#881) gets an EndpointSlice beside cloudburrow-host's:
// each of its ports, by name, at the published service's port on the same
// address, labelled so an unrouted run can remove it. A port whose service
// is not published fails Start rather than leaving the Service without it.
func TestClusterHostRoutesTheBigQueryService(t *testing.T) {
	f := &fakeRuntime{desktopIP: "192.168.65.254"}
	var out strings.Builder
	h := newClusterHost(hostCfg(), &out, func() map[string]string {
		return map[string]string{"bigquery": "127.0.0.1:9050", "bigquery-storage": "127.0.0.1:9060", "tasks": "127.0.0.1:9003"}
	})
	f.use(h)
	h.route("bigquery", map[string]string{"api": "bigquery", "storage-read": "bigquery-storage"})
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: EndpointSlice\nmetadata:\n  name: bigquery-routed\n  namespace: cloudburrow\n",
		"    kubernetes.io/service-name: bigquery\n",
		"    cloudburrow.dev/routes: bigquery\n",
		"  - addresses: [\"192.168.65.254\"]\nports:\n  - name: api\n    port: 9050\n    protocol: TCP\n  - name: storage-read\n    port: 9060\n",
	} {
		if !strings.Contains(f.applied, want) {
			t.Errorf("the applied manifest lacks %q:\n%s", want, f.applied)
		}
	}
	if !strings.Contains(out.String(), "the bigquery Service is routed here too") {
		t.Errorf("Start did not announce the route: %q", out.String())
	}

	f2 := &fakeRuntime{desktopIP: "192.168.65.254"}
	h2 := newClusterHost(hostCfg(), io.Discard, func() map[string]string { return map[string]string{"bigquery": "127.0.0.1:9050"} })
	f2.use(h2)
	h2.route("bigquery", map[string]string{"api": "bigquery", "storage-read": "bigquery-storage"})
	if err := h2.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "bigquery-storage is not published") {
		t.Errorf("Start with the Storage Read tunnel unpublished = %v", err)
	}
	if f2.applied != "" {
		t.Errorf("a failed route still applied:\n%s", f2.applied)
	}
}
