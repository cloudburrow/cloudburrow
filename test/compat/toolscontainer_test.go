//go:build compat

package compat

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The tools container (#2174). Every gcloud, gsutil, bq,
// docker-credential-gcloud, Terraform and OpenTofu command the compat tests
// run, runs in a container on an internal Docker network (`docker network
// create --internal`), which has no route out: an egress block at the
// network layer, which a client that ignores HTTPS_PROXY cannot get round,
// as one did on 2026-11-20. The proxy guards (egressGuard, noGoogleEgress)
// stay, as a second layer.
//
//   - testdata/tools/Dockerfile: Google's cloud-sdk 593.0.0 image, by
//     digest, with Terraform 1.16.6, OpenTofu 1.12.6 and the hashicorp/google
//     and google-beta 8.12.0 providers in a filesystem mirror, each checked
//     against a pinned SHA-256 at build time. init never reaches a registry.
//   - testdata/tools/init.sh: the container's nat table sends every TCP
//     connection to a loopback address to the relay, and so to the same port
//     on the host's loopback, where the instance and the tests' own servers
//     listen; answers every DNS query, *.googleapis.com, *.google.com and
//     metadata.google.internal among them, with 198.18.0.1, routed nowhere;
//     and logs and refuses every other connection and datagram
//     (testdata/egress/sink). Each is an EGRESS line, an escape the test that
//     ran the tool fails on, and the run fails on at the end.
//   - testdata/tools/shim: what PATH names gcloud, terraform and the rest:
//     `docker exec` into the container, with the caller's working directory,
//     environment and uid. Without the container it runs nothing.
//
// The relay is the one container on both the internal network and the
// default bridge. It forwards to toolsMux, a listener of the test process's
// on the host, which dials the host's loopback only, and only for a
// connection that names the run's token.
//
// Fail closed: no Docker, or no internal network, means the tests skip with
// the reason (fail, where CI requires the tools); a tool is never run on the
// host instead.

const toolsImageName = "cloudburrow-compat-tools"

// toolNames are the commands the shim stands in for.
var toolNames = []string{"gcloud", "gsutil", "bq", "docker-credential-gcloud", "terraform", "tofu"}

// toolsShimDir is the directory TestMain put first on PATH, holding the
// shim under each tool's name.
var toolsShimDir string

type toolsStack struct {
	image, network, relay, container string
	dir, logDir                      string
	mux                              net.Listener
	token                            string

	mu      sync.Mutex
	escapes []string // from the mux itself
}

var (
	toolsOnce  sync.Once
	toolsState *toolsStack
	toolsErr   error
)

// setupToolShims builds the shim into a fresh directory and puts it first
// on PATH, before any test runs, so no test can reach a tool on the host:
// until a test starts the container (toolBinary), the shim refuses.
func setupToolShims() error {
	dir, err := os.MkdirTemp("", "cb-tools-shim-")
	if err != nil {
		return err
	}
	bin := filepath.Join(dir, "toolshim")
	build := exec.Command("go", "build", "-o", bin, "./testdata/tools/shim")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build the tools shim: %v\n%s", err, b)
	}
	for _, n := range toolNames {
		if err := os.Symlink("toolshim", filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	toolsShimDir = dir
	return os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// teardownTools stops the container, and fails the run (returns 1) on any
// escape it logged, whichever test's tool made it.
func teardownTools(code int) int {
	if toolsState != nil {
		if lines := toolsState.escapesSince(0); len(lines) > 0 {
			fmt.Fprintf(os.Stderr, "compat: a tool tried to leave the machine; the tools container stopped it at the network layer:\n  %s\n",
				strings.Join(lines, "\n  "))
			code = 1
		}
		toolsState.stop()
	}
	if toolsShimDir != "" {
		_ = os.RemoveAll(toolsShimDir)
	}
	return code
}

// toolsRequired reports whether the run must have the tools container: CI
// names the tools it requires (EnvGcloud, EnvTofu) in the shards that run
// them, so a missing Docker fails there rather than skipping.
func toolsRequired() bool {
	return os.Getenv(EnvGcloud) != "" || os.Getenv(EnvTofu) != ""
}

// toolBinary is the path of the shim for tool, after starting the tools
// container if this is the first test to ask. It skips (or fails, when
// toolsRequired) without Docker, and fails the test, when it ends, on any
// escape logged meanwhile.
func toolBinary(t *testing.T, tool string) string {
	t.Helper()
	s := toolsContainer(t)
	start := s.escapeCount()
	t.Cleanup(func() {
		if lines := s.escapesSince(start); len(lines) > 0 {
			t.Errorf("a tool tried to leave the machine during this test (stopped in the tools container, at the network layer):\n  %s",
				strings.Join(lines, "\n  "))
		}
	})
	return filepath.Join(toolsShimDir, tool)
}

func toolsContainer(t *testing.T) *toolsStack {
	t.Helper()
	if toolsShimDir == "" {
		t.Fatal("TestMain did not set up the tools shim")
	}
	toolsOnce.Do(func() {
		toolsState, toolsErr = startTools(context.Background(), toolsMounts())
		if toolsErr == nil {
			toolsErr = toolsState.writeShimConfig()
		}
	})
	if toolsErr != nil {
		if toolsRequired() || !errors.Is(toolsErr, errNoDocker) {
			t.Fatalf("the tools container (gcloud and Terraform run only there, never on the host): %v", toolsErr)
		}
		t.Skipf("gcloud and Terraform run only in the tools container, on an internal Docker network: %v", toolsErr)
	}
	return toolsState
}

var errNoDocker = errors.New("docker is not available")

// toolsMounts are the host directories the container mounts at the same
// paths: the temporary directory (every test's t.TempDir, gcloud
// configuration and Terraform module), the repository, and the instance's
// state directory, fixture and kubeconfig. Nothing under the developer's
// home is mounted unless it is one of those.
func toolsMounts() []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		p, err := filepath.Abs(p)
		if err != nil {
			return
		}
		out = append(out, filepath.Clean(p))
		if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
			out = append(out, r)
		}
	}
	add(os.TempDir())
	add(filepath.Join("..", ".."))
	args := cliArgs()
	for i, a := range args {
		if (a == "--state-dir" || a == "-state-dir") && i+1 < len(args) {
			add(args[i+1])
		} else if v, ok := strings.CutPrefix(a, "--state-dir="); ok {
			add(v)
		}
	}
	for _, v := range []string{"CLOUDBURROW_TEST_CREDENTIALS", "CLOUDBURROW_TEST_KUBECONFIG"} {
		if p := os.Getenv(v); p != "" {
			add(filepath.Dir(p))
		}
	}
	return out
}

// deniedPaths are the developer's own gcloud configuration and ADC, which
// the shim refuses to pass to a tool.
func deniedPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	p := filepath.Join(home, ".config", "gcloud")
	out := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		out = append(out, r)
	}
	return out
}

// toolsImage builds testdata/tools (tagged by the hash of its files, so a
// change rebuilds it), for linux/amd64, the one platform Google's image is
// published for.
func toolsImage(ctx context.Context) (string, error) {
	src, err := filepath.Abs(filepath.Join("testdata", "tools"))
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, f := range []string{"Dockerfile"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			return "", err
		}
		h.Write(b)
	}
	image := fmt.Sprintf("%s:%x", toolsImageName, h.Sum(nil)[:6])
	if exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() == nil {
		return image, nil
	}
	b, err := exec.CommandContext(ctx, "docker", "build", "--platform", "linux/amd64", "-t", image, "-f",
		filepath.Join(src, "Dockerfile"), src).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build %s: %v\n%s", image, err, lastLines(string(b), 30))
	}
	return image, nil
}

// startTools builds the image, the sink and the entrypoint's directory,
// starts the host end of the relay, makes the internal network, and starts
// the relay and the tools container, with mounts at their own paths.
func startTools(ctx context.Context, mounts []string) (*toolsStack, error) {
	if exec.CommandContext(ctx, "docker", "info").Run() != nil {
		return nil, errNoDocker
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	removeStaleTools(ctx)
	image, err := toolsImage(ctx)
	if err != nil {
		return nil, err
	}
	s := &toolsStack{image: image}
	ok := false
	defer func() {
		if !ok {
			s.stop()
		}
	}()
	if s.dir, err = os.MkdirTemp("", "cb-tools-"); err != nil {
		return nil, err
	}
	s.logDir = filepath.Join(s.dir, "log")
	if err := os.Mkdir(s.logDir, 0o777); err != nil {
		return nil, err
	}
	_ = os.Chmod(s.logDir, 0o777)
	build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(s.dir, "sink"), "./testdata/egress/sink")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build the sink: %v\n%s", err, b)
	}
	initSh, err := os.ReadFile(filepath.Join("testdata", "tools", "init.sh"))
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(s.dir, "init.sh"), initSh, 0o755); err != nil {
		return nil, err
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	s.token = hex.EncodeToString(tok)

	hostIP := "127.0.0.1"
	if runtime.GOOS == "linux" {
		if hostIP, err = bridgeGatewayIP(ctx); err != nil {
			return nil, err
		}
	}
	if s.mux, err = net.Listen("tcp", net.JoinHostPort(hostIP, "0")); err != nil {
		return nil, fmt.Errorf("listen on %s for the relay: %v", hostIP, err)
	}
	go s.serveMux()
	_, muxPort, _ := net.SplitHostPort(s.mux.Addr().String())

	suffix := fmt.Sprintf("%d-%s", os.Getpid(), s.token[:6])
	network := "cb-tools-" + suffix
	if b, err := exec.CommandContext(ctx, "docker", "network", "create", "--internal",
		"--label", "cloudburrow.compat=tools", "--label", pidLabel, network).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("create the internal network: %v\n%s", err, b)
	}
	s.network = network
	if err := checkInternal(ctx, network); err != nil {
		return nil, err
	}

	relay := "cb-tools-relay-" + suffix
	args := []string{"run", "-d", "--platform", "linux/amd64", "--name", relay, "--label", "cloudburrow.compat=tools", "--label", pidLabel,
		"-v", s.dir + ":/cbtools:ro", "--entrypoint", "/cbtools/sink"}
	if runtime.GOOS == "linux" {
		args = append(args, "--add-host", "host.docker.internal:"+hostIP)
	}
	args = append(args, image, "-forward", "0.0.0.0:7000=host.docker.internal:"+muxPort)
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("start the relay: %v\n%s", err, b)
	}
	s.relay = relay
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", "--alias", "relay", network, relay).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("connect the relay: %v\n%s", err, b)
	}

	container := "cb-tools-" + suffix
	args = []string{"run", "-d", "--platform", "linux/amd64", "--name", container, "--label", "cloudburrow.compat=tools", "--label", pidLabel,
		"--network", network, "--cap-add", "NET_ADMIN", "-e", "CB_TOOLS_TOKEN=" + s.token,
		"-v", s.dir + ":/cbtools:ro", "-v", s.logDir + ":/cblog"}
	tmp := []string{filepath.Clean(os.TempDir())}
	if r, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		tmp = append(tmp, r)
	}
	seen := map[string]bool{}
	for _, m := range mounts {
		if seen[m] {
			continue
		}
		seen[m] = true
		mode := ":ro"
		for _, d := range tmp {
			if within(m, d) {
				mode = "" // the tools write their configuration and state here
			}
		}
		args = append(args, "-v", m+":"+m+mode)
	}
	args = append(args, "--entrypoint", "bash", image, "/cbtools/init.sh")
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("start the tools container: %v\n%s", err, b)
	}
	s.container = container
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := os.Stat(filepath.Join(s.logDir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
			return nil, fmt.Errorf("the tools container did not come up:\n%s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	ok = true
	return s, nil
}

// pidLabel names the test process that made a container or network, so a
// later run removes those a run killed by go test's -timeout (which skips
// TestMain's teardown) left behind, and only those.
var pidLabel = "cloudburrow.compat.pid=" + strconv.Itoa(os.Getpid())

func removeStaleTools(ctx context.Context) {
	for _, kind := range []string{"container", "network"} {
		args := []string{kind, "ls", "--filter", "label=cloudburrow.compat=tools", "--format", `{{.Name}} {{.Label "cloudburrow.compat.pid"}}`}
		if kind == "container" {
			args = []string{"ps", "-a", "--filter", "label=cloudburrow.compat=tools", "--format", `{{.Names}} {{.Label "cloudburrow.compat.pid"}}`}
		}
		out, err := exec.CommandContext(ctx, "docker", args...).Output()
		if err != nil {
			continue
		}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f := strings.Fields(l)
			if len(f) != 2 {
				continue
			}
			pid, err := strconv.Atoi(f[1])
			if err != nil || pid <= 0 || syscall.Kill(pid, 0) != syscall.ESRCH {
				continue // running, or not ours to judge
			}
			if kind == "container" {
				_ = exec.CommandContext(ctx, "docker", "rm", "-f", "-v", f[0]).Run()
			} else {
				_ = exec.CommandContext(ctx, "docker", "network", "rm", f[0]).Run()
			}
		}
	}
}

func within(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// checkInternal fails unless Docker reports the network as internal, with
// no route out, which the block rests on.
func checkInternal(ctx context.Context, network string) error {
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", network, "--format", "{{.Internal}}").Output()
	if err != nil {
		return fmt.Errorf("docker network inspect %s: %v", network, err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("the network %s is not --internal (Internal=%s); no tool runs on it", network, strings.TrimSpace(string(out)))
	}
	return nil
}

// bridgeGatewayIP is the host's address on Docker's default bridge network,
// where the relay starts (Linux: the instance listens on the loopback, which
// a container cannot reach, so the mux listens here).
func bridgeGatewayIP(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", "bridge", "--format",
		"{{range .IPAM.Config}}{{.Gateway}} {{end}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker network inspect bridge: %v", err)
	}
	for _, f := range strings.Fields(string(out)) {
		if ip := net.ParseIP(f); ip != nil && ip.To4() != nil {
			return f, nil
		}
	}
	return "", fmt.Errorf("the bridge network has no IPv4 gateway: %q", out)
}

// serveMux is the relay's host end: each connection begins "CBTOOLS <token>
// <port>", and is carried to that port on the host's loopback. Anything
// else is an escape.
func (s *toolsStack) serveMux() {
	for {
		c, err := s.mux.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			r := bufio.NewReader(c)
			line, err := r.ReadString('\n')
			f := strings.Fields(line)
			port, perr := strconv.Atoi(lastField(f))
			if err != nil || len(f) != 3 || f[0] != "CBTOOLS" || f[1] != s.token || perr != nil || port <= 0 || port > 65535 {
				s.mu.Lock()
				s.escapes = append(s.escapes, fmt.Sprintf("relay: a connection without the run's token, from %s", c.RemoteAddr()))
				s.mu.Unlock()
				return
			}
			_ = c.SetReadDeadline(time.Time{})
			u, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(u, r)
				if tc, ok := u.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				done <- struct{}{}
			}()
			go func() {
				_, _ = io.Copy(c, u)
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				done <- struct{}{}
			}()
			<-done
			<-done
		}()
	}
}

func lastField(f []string) string {
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// escapeLines are the container's EGRESS lines so far, then the mux's.
func (s *toolsStack) escapeLines() []string {
	var out []string
	if b, err := os.ReadFile(filepath.Join(s.logDir, "egress.log")); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
	}
	s.mu.Lock()
	out = append(out, s.escapes...)
	s.mu.Unlock()
	return out
}

func (s *toolsStack) escapeCount() int { return len(s.escapeLines()) }

func (s *toolsStack) escapesSince(n int) []string {
	l := s.escapeLines()
	if n >= len(l) {
		return nil
	}
	return l[n:]
}

func (s *toolsStack) writeShimConfig() error {
	b, err := json.Marshal(map[string]any{
		"container": s.container,
		"user":      fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"mounts":    toolsMounts(),
		"denied":    deniedPaths(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(toolsShimDir, "shim.json"), b, 0o644)
}

// stop removes the containers and the network, by the names this run gave
// them.
func (s *toolsStack) stop() {
	for _, c := range []string{s.container, s.relay} {
		if c != "" {
			_ = exec.Command("docker", "rm", "-f", "-v", c).Run()
		}
	}
	if s.network != "" {
		_ = exec.Command("docker", "network", "rm", s.network).Run()
	}
	if s.mux != nil {
		_ = s.mux.Close()
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}
