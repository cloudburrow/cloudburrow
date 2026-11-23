//go:build compat

package compat

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/alloydb/apiv1/alloydbpb"
)

// EnvAlloyDBTLS points at the AlloyDB Admin API over TLS, which the Java
// and Python connectors need (they open only TLS channels); EnvAlloyDBCA
// names the file holding its certificate authority's certificate.
const (
	EnvAlloyDBTLS = "CLOUDBURROW_TEST_ALLOYDB_TLS"
	EnvAlloyDBCA  = "CLOUDBURROW_TEST_ALLOYDB_CA"
)

// TestAlloyDBConnectorsSendRealSQL (#2191): Google's own AlloyDB clients,
// each at a pinned release, write and read rows on an instance's primary,
// with no connection attempt off the machine:
//
//   - the Python connector (google-cloud-alloydb-connector 1.15.0, pg8000)
//     and the Java connector (alloydb-jdbc-connector 1.4.1, PostgreSQL
//     JDBC), through the admin API over TLS, and the connectors' listener
//     at port 5433 of the address GetConnectionInfo reports;
//   - the AlloyDB Auth Proxy (v1.18.0, its SHA-256 checked), through the
//     plaintext admin API, with pg8000 at its local port.
//
// They run in a container on an internal Docker network, with no route
// out, as TestServedClientsSendNothingOffTheMachine's clients do: the sink
// counts every attempt to leave, and a control attempt proves the count.
// The Go connector then reads the three clients' rows back.
func TestAlloyDBConnectorsSendRealSQL(t *testing.T) {
	h := New(t)
	if runtime.GOOS == "windows" {
		t.Skip("the no-egress container needs Linux containers")
	}
	tlsAddr := h.Endpoint(EnvAlloyDBTLS)
	ca := strings.TrimSpace(os.Getenv(EnvAlloyDBCA))
	if ca == "" {
		t.Skipf("%s is not set", EnvAlloyDBCA)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	api := h.Endpoint(EnvAlloyDB)
	c := alloyDBClients(t, h)["grpc"]
	ctx := alloyDBWait(t)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	cluster, primary := alloyDBCluster(ctx, t, h, c, "us-central1", "conn", nil)
	info, err := c.GetConnectionInfo(ctx, &alloydbpb.GetConnectionInfoRequest{Parent: primary.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(info.GetIpAddress())
	if ip == nil || ip.To4() == nil || !ip.IsLoopback() {
		t.Fatalf("the primary's address is %q: the container reaches only IPv4 loopback addresses "+
			"(start the instance without CLOUDBURROW_ALLOYDB_INSTANCE_ADDRESSES=::1)", info.GetIpAddress())
	}
	connector := net.JoinHostPort(ip.String(), "5433")
	if l := os.Getenv(EnvAlloyDBConnector); l != "" {
		connector = l
	}

	image, dir := alloyDBConnectorsImage(ctx, t)
	pem, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pem, 0o644); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	network, relay := "cb-adbconn-"+suffix, "cb-adbconn-relay-"+suffix
	if b, err := exec.CommandContext(ctx, "docker", "network", "create", "--internal", network).CombinedOutput(); err != nil {
		t.Fatalf("create the internal network: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	args := []string{"run", "-d", "--name", relay, "-v", dir + ":/egress:ro", "--entrypoint", "/egress/sink"}
	hostIP := "127.0.0.1"
	if runtime.GOOS == "linux" {
		hostIP = bridgeGateway(ctx, t)
		args = append(args, "--add-host", "host.docker.internal:"+hostIP)
	}
	// Each container address is served at the relay on the port of the
	// host forwarder to the instance's address.
	targets := map[string]string{api: api, tlsAddr: tlsAddr, connector: net.JoinHostPort(ip.String(), "5433")}
	var forwards []string
	args = append(args, image)
	for host, inContainer := range targets {
		_, apiPort, _ := net.SplitHostPort(inContainer)
		if inContainer == api || inContainer == tlsAddr {
			inContainer = "127.0.0.1:" + apiPort
		}
		p := hostForwarder(t, hostIP, host)
		args = append(args, "-forward", "0.0.0.0:"+p+"=host.docker.internal:"+p)
		forwards = append(forwards, inContainer+"="+p)
	}
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start the relay: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", relay).Run() })
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", "--alias", "relay", network, relay).CombinedOutput(); err != nil {
		t.Fatalf("connect the relay: %v\n%s", err, b)
	}

	_, apiPort, _ := net.SplitHostPort(api)
	_, tlsPort, _ := net.SplitHostPort(tlsAddr)
	for _, client := range []string{"go", "python", "java", "proxy"} {
		t.Run(client, func(t *testing.T) {
			out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network, "--cap-add", "NET_ADMIN",
				"-v", dir+":/egress:ro",
				"-e", "EGRESS_FORWARDS="+strings.Join(forwards, " "),
				"-e", "ADB_INSTANCE="+primary.GetName(),
				"-e", "ADB_API=127.0.0.1:"+apiPort,
				"-e", "ADB_TLS=127.0.0.1:"+tlsPort,
				"-e", "ADB_TOKEN=cloudburrow",
				"-e", "ADB_PASSWORD=burrow-pw",
				"--entrypoint", "bash", image, "/egress/run.sh", "self", client).CombinedOutput()
			if err != nil || !strings.Contains(string(out), "DONE") {
				t.Fatalf("the %s probe: %v\n%s", client, err, out)
			}
			control, ok := false, false
			var attempts []string
			for _, l := range strings.Split(string(out), "\n") {
				switch {
				case l == "EGRESS tcp 192.0.2.1:443":
					control = true
				case strings.HasPrefix(l, "EGRESS "):
					attempts = append(attempts, strings.TrimPrefix(l, "EGRESS "))
				case l == "OK "+client+" 1":
					ok = true
				}
			}
			if !control {
				t.Fatalf("the control connection to 192.0.2.1 was not counted, so the count proves nothing:\n%s", out)
			}
			if !ok {
				t.Errorf("the %s client did not write and read its row", client)
			}
			var monitoring []string
			for _, a := range attempts {
				switch {
				case client == "proxy" && (strings.HasPrefix(a, "dns monitoring.googleapis.com ") || a == "tcp 198.18.0.1:443"):
					// The Auth Proxy's alloydbconn opens a Cloud Monitoring
					// gRPC client even with --disable-built-in-telemetry,
					// and the proxy has no option that stops it
					// (docs/alloydb.md).
					monitoring = append(monitoring, a)
				default:
					t.Errorf("the %s client tried to leave the machine (stopped in the container): %s", client, a)
				}
			}
			if len(monitoring) > 0 {
				t.Logf("the Auth Proxy's Cloud Monitoring client tried monitoring.googleapis.com, stopped in the container: %v", monitoring)
			}
			if t.Failed() {
				t.Logf("the container's output:\n%s", out)
			}
		})
	}

	conn := alloyDBConn(ctx, t, alloyDBDialer(t, h), primary.GetName(), "postgres", "burrow-pw")
	var clients string
	if err := conn.QueryRow(ctx, "SELECT string_agg(client, ',' ORDER BY client) FROM connector_probe").Scan(&clients); err != nil {
		t.Fatal(err)
	}
	if clients != "go,java,proxy,python" {
		t.Errorf("rows on %s: %q, want go,java,proxy,python", cluster, clients)
	}
}

// alloyDBConnectorsImage builds the connectors' image
// (testdata/alloydbconnectors, tagged by its Dockerfile's and pom's hash)
// and a directory holding the sink, built for the daemon's architecture,
// and the probes, to mount at /egress.
func alloyDBConnectorsImage(ctx context.Context, t *testing.T) (string, string) {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("testdata", "alloydbconnectors"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	for _, f := range []string{"Dockerfile", "pom.xml"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		sum.Write(b)
	}
	image := fmt.Sprintf("cloudburrow-compat-alloydbconnectors:%x", sum.Sum(nil))[:len("cloudburrow-compat-alloydbconnectors:")+12]
	if exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() != nil {
		if b, err := exec.CommandContext(ctx, "docker", "build", "-t", image, src).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", image, err, b)
		}
	}
	arch, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	dir := t.TempDir()
	for out, pkg := range map[string]string{"sink": "./testdata/egress/sink", "goprobe": "./testdata/alloydbconnectors/goprobe"} {
		build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, out), pkg)
		build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0")
		if b, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
	}
	for _, f := range []string{"run.sh", "probe_connector.py", "probe_proxy.py", "Probe.java"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return image, dir
}
