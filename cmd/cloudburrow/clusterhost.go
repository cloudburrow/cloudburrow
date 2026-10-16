package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/hostrelay"
	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// ClusterHostService is the cluster name pods use for the services this
// process serves (#575). Selector-less: its EndpointSlice points at the
// developer's machine as the cluster sees it.
// hostguard accepts it as a Host on every listener (#676).
const ClusterHostService = hostguard.ClusterHostService

// dockerRunner runs a command and returns its stdout; images.ExecRunner is
// the real one.
type dockerRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// clusterHost publishes the CLI-hosted service APIs and the metadata
// server under one cluster-resolvable name, so a Cloud Run container can
// read a secret, enqueue a task or ask the metadata server for a token.
//
// Before, those ran only on loopback and `up` printed their host address
// as the in-cluster one, which a pod cannot use. The admin/control port is
// never published here (ADR-0004): only service APIs, which are
// unauthenticated by design, and the metadata server.
//
// How the machine is reached depends on the runtime. Docker Desktop
// forwards host.docker.internal to the host's loopback, so the existing
// listeners already serve pods and nothing more is bound. Docker Engine on
// Linux reaches the host only at the kind network's gateway address, so a
// relay listens there for each service and forwards to its loopback
// listener. Either way the Service's EndpointSlice carries that address.
type clusterHost struct {
	cfg config.Config
	// docker runs the docker CLI; kube runs kubectl, through internal/k8s.
	docker   dockerRunner
	kube     k8s.Invoker
	out      io.Writer
	services func() map[string]string // name -> bound loopback address

	// listen and goos are the relay's bind probe and the client's OS, for
	// naming the engine when the gateway is not an address here (#712).
	listen func(network, address string) (net.Listener, error)
	goos   string

	mu      sync.Mutex
	address string
	mode    string
	ports   map[string]int
	relays  []*hostrelay.Relay
	// clusterIP is the Service's, which a Cloud SQL Auth Proxy dials (#1346).
	clusterIP string
}

// clusterServicePort is the Service port of a published service whose
// clients dial a fixed port, where it differs from the bound one: the
// Cloud SQL connectors always dial 3307 (#1346), and the pods' connector
// listener is on a port of its own.
var clusterServicePort = map[string]int{"cloudsql-connector": sqladmin.ConnectorPort}

func servicePort(name string, port int) int {
	if p, ok := clusterServicePort[name]; ok {
		return p
	}
	return port
}

func newClusterHost(cfg config.Config, out io.Writer, services func() map[string]string) *clusterHost {
	return &clusterHost{cfg: cfg, docker: images.ExecRunner{}, kube: kubeInvoker, out: out, services: services,
		listen: net.Listen, goos: runtime.GOOS}
}

func (h *clusterHost) Name() string { return "cluster-host" }

// InCluster is the address a pod uses for a service, or "" when the
// service is not published.
func (h *clusterHost) InCluster(service string) string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	port, ok := h.ports[service]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", ClusterHostService, h.cfg.Cluster.Namespace, servicePort(service, port))
}

// ClusterIP is the cloudburrow-host Service's cluster IP, or "" before
// Start or when it could not be read.
func (h *clusterHost) ClusterIP() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clusterIP
}

// Address is the address pods reach this machine at, or "" before Start.
func (h *clusterHost) Address() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.address
}

// InClusterAll is every published service's in-cluster address.
func (h *clusterHost) InClusterAll() map[string]string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	names := make([]string, 0, len(h.ports))
	for n := range h.ports {
		names = append(names, n)
	}
	h.mu.Unlock()
	out := map[string]string{}
	for _, n := range names {
		out[n] = h.InCluster(n)
	}
	return out
}

// discover returns the address pods reach this machine at, and whether the
// CLI must listen there itself.
func (h *clusterHost) discover(ctx context.Context) (address string, relay bool, err error) {
	node := h.cfg.ClusterName() + "-control-plane"
	if out, err := h.docker.Run(ctx, "docker", "exec", node, "getent", "ahostsv4", "host.docker.internal"); err == nil {
		if f := strings.Fields(out); len(f) > 0 && net.ParseIP(f[0]) != nil {
			return f[0], false, nil
		}
	}
	out, err := h.docker.Run(ctx, "docker", "network", "inspect", "kind",
		"--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return "", false, fmt.Errorf("find the kind network's gateway: %w", err)
	}
	for _, f := range strings.Fields(out) {
		if ip := net.ParseIP(f); ip != nil && ip.To4() != nil {
			if err := h.gatewayBindable(ctx, f); err != nil {
				return "", false, err
			}
			return f, true, nil
		}
	}
	return "", false, fmt.Errorf("the kind network has no IPv4 gateway (%q)", strings.TrimSpace(out))
}

// gatewayBindable checks that the relay can listen on the kind gateway
// before anything is bound or applied (#712). On a rootless daemon or a VM
// engine other than Docker Desktop the gateway is an address in a namespace
// or VM, not on this host: binding it failed later as a bare listen error,
// so the failure names the engine and the way round it.
func (h *clusterHost) gatewayBindable(ctx context.Context, gateway string) error {
	ln, err := h.listen("tcp", net.JoinHostPort(gateway, "0"))
	if err == nil {
		return ln.Close()
	}
	engine := "not identified (`docker info` gave no answer)"
	remedy := doctor.HostRelayRemedy(doctor.Engine{})
	if out, ierr := h.docker.Run(ctx, "docker", "info", "--format", "{{json .}}"); ierr == nil {
		var info doctor.DockerInfo
		if json.Unmarshal([]byte(out), &info) == nil {
			e := doctor.ClassifyEngine(info, h.goos)
			engine, remedy = e.String(), doctor.HostRelayRemedy(e)
		}
	}
	return fmt.Errorf("pods cannot reach this machine: host.docker.internal does not resolve in the kind node, "+
		"and the kind network's gateway %s is not an address on this host (%v). The Docker engine is %s: %s "+
		"(docs/install.md, Container engines)", gateway, err, engine, remedy)
}

func (h *clusterHost) Start(ctx context.Context) error {
	address, relay, err := h.discover(ctx)
	if err != nil {
		return err
	}
	ports := map[string]int{}
	var relays []*hostrelay.Relay
	stop := func() {
		for _, r := range relays {
			_ = r.Close()
		}
	}
	for name, addr := range h.services() {
		if addr == "" {
			continue
		}
		_, p, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		port, _ := strconv.Atoi(p)
		if port == 0 {
			continue
		}
		if relay {
			r := &hostrelay.Relay{Listen: net.JoinHostPort(address, p), Target: addr}
			if err := r.Start(ctx); err != nil {
				stop()
				return fmt.Errorf("publish %s to the cluster: %w", name, err)
			}
			relays = append(relays, r)
		}
		ports[name] = port
	}
	kube := k8s.NewWith(h.kube, h.cfg.KubeconfigPath(), "", "")
	if err := kube.Apply(ctx, clusterHostManifest(h.cfg.Cluster.Namespace, h.cfg.Name, address, ports), k8s.ApplyOptions{}); err != nil {
		stop()
		return fmt.Errorf("apply the %s Service: %w", ClusterHostService, err)
	}
	// Read back for the Cloud SQL connector's address (#1346); a failure
	// leaves Cloud SQL volumes refused, and nothing else.
	clusterIP := ""
	if _, ok := ports["cloudsql-connector"]; ok {
		if out, err := kube.Do(ctx, "", "get", "service", ClusterHostService, "-n", h.cfg.Cluster.Namespace,
			"-o", "jsonpath={.spec.clusterIP}"); err == nil && net.ParseIP(strings.TrimSpace(out)) != nil {
			clusterIP = strings.TrimSpace(out)
		}
	}
	// Artifact Registry's Docker registry (#1383) is served on the Run
	// port; each node's containerd is pointed at it for every region's
	// {region}-docker.pkg.dev, so a revision pulls a pushed image there and
	// never from Google.
	if p, ok := ports["run"]; ok {
		if err := h.registryMirror(ctx, net.JoinHostPort(address, strconv.Itoa(p))); err != nil {
			fmt.Fprintf(h.out, "  warning: Cloud Run cannot pull Artifact Registry images: %v\n", err)
		}
	}
	mode := "host.docker.internal"
	if relay {
		mode = "a relay on the kind network gateway"
	}
	h.mu.Lock()
	h.address, h.mode, h.ports, h.relays, h.clusterIP = address, mode, ports, relays, clusterIP
	h.mu.Unlock()
	names := make([]string, 0, len(ports))
	for n := range ports {
		names = append(names, n)
	}
	sort.Strings(names)
	// ADR-0004: relaxing loopback for the container network is announced.
	fmt.Fprintf(h.out, "  pods reach %s at %s.%s.svc.cluster.local (%s, via %s); admin is not published\n",
		strings.Join(names, ", "), ClusterHostService, h.cfg.Cluster.Namespace, address, mode)
	return nil
}

func (h *clusterHost) Stop(context.Context) error {
	h.mu.Lock()
	relays := h.relays
	h.relays = nil
	h.mu.Unlock()
	for _, r := range relays {
		_ = r.Close()
	}
	return nil
}

// clusterHostManifest is the selector-less Service and its EndpointSlice.
func clusterHostManifest(namespace, instance, address string, ports map[string]int) string {
	names := make([]string, 0, len(ports))
	for n := range ports {
		names = append(names, n)
	}
	sort.Strings(names)
	var svc, eps strings.Builder
	for _, n := range names {
		fmt.Fprintf(&svc, "    - name: %s\n      port: %d\n      targetPort: %d\n      protocol: TCP\n", n, servicePort(n, ports[n]), ports[n])
		fmt.Fprintf(&eps, "  - name: %s\n    port: %d\n    protocol: TCP\n", n, ports[n])
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %[3]q
spec:
  ports:
%[5]s---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    kubernetes.io/service-name: %[1]s
    endpointslice.kubernetes.io/managed-by: cloudburrow.dev
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %[3]q
addressType: IPv4
endpoints:
  - addresses: [%[4]q]
ports:
%[6]s`, ClusterHostService, namespace, instance, address, svc.String(), eps.String())
}

// registryMirrorHosts is the containerd hosts.toml that sends a pull from
// one {region}-docker.pkg.dev to this instance's registry at addr: as its
// server, so a failed pull never falls back to Google, and as a mirror, so
// containerd names the registry it was asked for in the ns parameter.
func registryMirrorHosts(addr string) string {
	return fmt.Sprintf("server = \"http://%s\"\n\n[host.\"http://%s\"]\n  capabilities = [\"pull\", \"resolve\"]\n", addr, addr)
}

// registryMirror writes registryMirrorHosts into every node's
// /etc/containerd/certs.d, the registry configuration directory containerd
// reads on each pull, for every region Artifact Registry serves.
func (h *clusterHost) registryMirror(ctx context.Context, addr string) error {
	out, err := h.docker.Run(ctx, "kind", "get", "nodes", "--name", h.cfg.ClusterName())
	if err != nil {
		return err
	}
	var script strings.Builder
	script.WriteString("set -e\n")
	for _, r := range locations.Regions {
		dir := "/etc/containerd/certs.d/" + r + "-docker.pkg.dev"
		fmt.Fprintf(&script, "mkdir -p %s && printf '%%s' '%s' > %s/hosts.toml\n", dir, registryMirrorHosts(addr), dir)
	}
	for _, node := range strings.Fields(out) {
		if _, err := h.docker.Run(ctx, "docker", "exec", node, "sh", "-c", script.String()); err != nil {
			return fmt.Errorf("configure %s's registry mirror: %w", node, err)
		}
	}
	return nil
}
