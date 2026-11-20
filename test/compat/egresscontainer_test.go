//go:build compat

package compat

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestServedClientsSendNothingOffTheMachine runs a served Cloud KMS client
// in Go, Node and Python, over gRPC and REST, each with the ADC fixture
// `cloudburrow env` exports as its only credential, and counts every
// connection attempt it makes to a non-loopback address (#1981).
//
// The count is taken at the network layer, never by a proxy (gaxios on
// Node's fetch ignores HTTPS_PROXY): each client runs in a container on an
// internal Docker network, which has no route out, and its nat table
// redirects every TCP connection and UDP datagram to a non-loopback address,
// and every DNS query, to a sink that logs it and answers nothing real
// (testdata/egress). A relay container, the only one that can reach the
// host, forwards the instance's KMS and token ports; the sink serves them
// on the container's loopback, where the fixture names them. A control
// connection to 192.0.2.1 must be counted first, or a zero proves nothing.
//
// The instance listens on the host's loopback, which a container reaches
// only on Docker Desktop (host.docker.internal). On Linux the relay reaches
// the host at its bridge network's gateway, where nothing of the instance
// listens, so this test forwards each port from that gateway to the
// loopback for its own duration (hostForwarder); on CI every client got EOF
// or a timeout without it.
//
// Go and Node must make no attempt. Python's google-auth starts a Regional
// Access Boundary lookup at iamcredentials.googleapis.com for any
// service-account credential in the default universe, and no fixture or
// variable turns it off without breaking every Node or Go client
// (docs/security.md, "Nothing reaches Google"): while those lookups are the
// only attempts the Python subtest skips naming #1981, and any other
// attempt fails it. Nothing is sent off the machine either way.
func TestServedClientsSendNothingOffTheMachine(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvKMS)
	fixture := credentialsFixture(t)
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker is not available")
	}
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var adc struct {
		ProjectID string `json:"project_id"`
		TokenURI  string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &adc); err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	tok, err := url.Parse(adc.TokenURI)
	if err != nil || adc.ProjectID == "" {
		t.Fatalf("the fixture has no usable token_uri or project_id (%v)", err)
	}
	h.requireLocal(tok.Host)
	_, kmsPort, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	image, dir := egressImage(ctx, t)
	if err := os.WriteFile(filepath.Join(dir, "adc.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	network, relay := "cb-egress-"+suffix, "cb-egress-relay-"+suffix
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
	args = append(args, image,
		"-forward", "0.0.0.0:"+kmsPort+"=host.docker.internal:"+hostForwarder(t, hostIP, "127.0.0.1:"+kmsPort),
		"-forward", "0.0.0.0:"+tok.Port()+"=host.docker.internal:"+hostForwarder(t, hostIP, tok.Host))
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start the relay: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", relay).Run() })
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", "--alias", "relay", network, relay).CombinedOutput(); err != nil {
		t.Fatalf("connect the relay: %v\n%s", err, b)
	}

	run := func(t *testing.T, client string) (ok, failed, attempts []string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network, "--cap-add", "NET_ADMIN",
			"-v", dir+":/egress:ro",
			"-e", "EGRESS_KMS=127.0.0.1:"+kmsPort,
			"-e", "EGRESS_PARENT=projects/"+adc.ProjectID+"/locations/global",
			"-e", "EGRESS_PORTS="+kmsPort+" "+tok.Port(),
			"-e", "GOOGLE_APPLICATION_CREDENTIALS=/egress/adc.json",
			"--entrypoint", "bash", image, "/egress/run.sh", "self", client).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "DONE") {
			t.Fatalf("the %s probe: %v\n%s", client, err, out)
		}
		control := false
		for _, l := range strings.Split(string(out), "\n") {
			switch {
			case l == "OK self":
			case strings.HasPrefix(l, "OK "):
				ok = append(ok, l)
			case strings.HasPrefix(l, "FAIL "):
				failed = append(failed, l)
			case l == "EGRESS tcp 192.0.2.1:443":
				control = true
			case strings.HasPrefix(l, "EGRESS "):
				attempts = append(attempts, strings.TrimPrefix(l, "EGRESS "))
			}
		}
		if !control {
			t.Fatalf("the control connection to 192.0.2.1 was not counted, so the count proves nothing:\n%s", out)
		}
		if len(failed) > 0 || len(ok) != 2 {
			t.Errorf("served %s clients: %d worked, want 2 (gRPC and REST); failures: %v\n%s", client, len(ok), failed, out)
		}
		return ok, failed, attempts
	}

	for _, client := range []string{"go", "node"} {
		t.Run(client, func(t *testing.T) {
			ok, _, attempts := run(t, client)
			if len(attempts) > 0 {
				t.Errorf("served %s clients tried to leave the machine %d times (stopped in the container): %v", client, len(attempts), attempts)
			}
			t.Logf("%v; no connection attempt off the machine", ok)
		})
	}
	t.Run("python", func(t *testing.T) {
		ok, _, attempts := run(t, "python")
		lookups := 0
		for _, a := range attempts {
			switch {
			case strings.HasPrefix(a, "dns iamcredentials.googleapis.com "), a == "tcp 198.18.0.1:443":
				lookups++
			default:
				t.Errorf("a served Python client tried to leave the machine (stopped in the container): %s", a)
			}
		}
		if lookups > 0 && !t.Failed() {
			t.Skipf("#1981: %v, but google-auth's Regional Access Boundary lookup tried iamcredentials.googleapis.com "+
				"(%d DNS queries and connections, all stopped in the container): %v", ok, lookups, attempts)
		}
		t.Logf("%v; no connection attempt off the machine", ok)
	})
}

// bridgeGateway is the host's address on Docker's default bridge network,
// where the relay starts.
func bridgeGateway(ctx context.Context, t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", "bridge", "--format",
		"{{range .IPAM.Config}}{{.Gateway}} {{end}}").Output()
	if err != nil {
		t.Fatalf("docker network inspect bridge: %v", err)
	}
	for _, f := range strings.Fields(string(out)) {
		if ip := net.ParseIP(f); ip != nil && ip.To4() != nil {
			return f
		}
	}
	t.Fatalf("the bridge network has no IPv4 gateway: %q", out)
	return ""
}

// hostForwarder listens on ip, at a port the system picks, and forwards each
// connection to target, an address of the instance on the host's loopback,
// until the test ends. It returns the port.
func hostForwarder(t *testing.T, ip, target string) string {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatalf("listen on %s for the relay: %v", ip, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				u, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer u.Close()
				done := make(chan struct{}, 2)
				pipe := func(dst, src net.Conn) {
					_, _ = io.Copy(dst, src)
					if tc, ok := dst.(*net.TCPConn); ok {
						_ = tc.CloseWrite()
					}
					done <- struct{}{}
				}
				go pipe(u, c)
				go pipe(c, u)
				<-done
				<-done
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// egressImage builds the no-egress container's image (testdata/egress's
// Dockerfile, tagged by its hash so a change rebuilds it) and a directory
// holding the sink, the Go probe built for the daemon's architecture, and
// the scripts, to mount at /egress.
func egressImage(ctx context.Context, t *testing.T) (string, string) {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("testdata", "egress"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(src, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	image := fmt.Sprintf("cloudburrow-compat-egress:%x", sha256.Sum256(dockerfile))[:len("cloudburrow-compat-egress:")+12]
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
	for _, pkg := range []string{"sink", "goprobe"} {
		build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, pkg), "./"+filepath.ToSlash(filepath.Join("testdata", "egress", pkg)))
		build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0")
		if b, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
	}
	for _, f := range []string{"run.sh", "probe.py", "probe.js"} {
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
