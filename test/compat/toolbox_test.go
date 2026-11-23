//go:build compat

package compat

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	"testing"
	"time"
)

// The tool box (#2174): every gcloud, gsutil, bq, docker-credential-gcloud,
// Terraform and OpenTofu command of this package runs in a Docker container
// on an internal Docker network (`docker network create --internal`), which
// has no route out. Proxy variables are not an egress block (on 2026-11-20 a
// Node client ignored HTTPS_PROXY and reached Google), and a subprocess, a
// provider plugin, a metadata probe or a DNS lookup that ignores them would
// have left the machine; in the box there is nowhere for it to go.
//
//   - The box reaches the instance only through a relay container, the one
//     container on both the internal network and Docker's bridge. The box's
//     nat table redirects each TCP connection to 127.0.0.0/8, where the
//     harness's variables and gcloud-setup's configuration name the
//     instance, to boxd (testdata/toolbox/boxd), which sends it through the
//     relay to a listener of this process on the host (boxMux), with the
//     box's token and the port; the listener connects to that port on the
//     host's loopback and nowhere else.
//   - DNS is a second layer: every query goes to boxd's resolver, which
//     answers REFUSED for *.googleapis.com, *.google.com,
//     metadata.google.internal and Google's other domains, and NXDOMAIN for
//     any other name outside the machine.
//   - The filter table rejects every packet that leaves the box by anything
//     but its loopback, except boxd's own connections to the relay.
//   - The proxy guards (egressGuard, noGoogleEgress) stay, as a third layer
//     that fails a test on any request a tool tries to send off the machine.
//
// gcloud is the harness's own (EnvGcloud, or gcloud on PATH), mounted into
// the box at its host path (on macOS, an APFS clone of it under the
// temporary directory, which Docker Desktop shares) and run by the image's
// Python. Terraform and OpenTofu are the image's, at CI's pins, with the
// providers in a filesystem mirror made when the image was built: the box
// has no network to fetch them. testdata/toolbox/Dockerfile.
//
// It fails closed. From TestMain on, the first directory on PATH holds a
// stub under each tool's name that refuses to run; toolBox replaces them
// with boxexec (testdata/toolbox/boxexec), which runs the tool in the box
// and nowhere else. Without Docker, or without an internal network, every
// test that needs a tool skips with the reason; none falls back to the
// host. The box is started once, by the first test that needs it, and
// removed when the run ends.

// boxTools are the names on the guarded PATH: gcloud's tools, Terraform's,
// and gke-gcloud-auth-plugin, which a kubeconfig gcloud wrote may name (the
// box does not provide it, so it only ever refuses).
var boxTools = []string{"gcloud", "gsutil", "bq", "docker-credential-gcloud", "gke-gcloud-auth-plugin", "terraform", "tofu"}

// boxGcloudTools are the boxTools that come from gcloud's SDK.
var boxGcloudTools = []string{"gcloud", "gsutil", "bq", "docker-credential-gcloud"}

var (
	// hostPATH is PATH as the run began, before the guard: where the
	// harness looks for the host's tools, to decide whether a test runs.
	hostPATH string
	// toolDir is the guarded directory first on PATH.
	toolDir string

	boxOnce sync.Once
	theBox  *toolBoxState
	boxSkip string // why tests that need the box skip
	boxErr  error  // why they fail
)

type toolBoxState struct {
	network, relay, container string
	image                     string
	sdk                       string // the SDK root mounted in the box, "" without gcloud
	mounts                    []string
	cleanup                   []func()
	mux                       net.Listener
}

const boxStubScript = `#!/bin/sh
echo "$(basename "$0") (compat): gcloud and Terraform run only in the tool box (test/compat/toolbox_test.go, #2174), never on the host; the test must get the tool from gcloudBinary, realTool, terraformTool or tofuBinary" >&2
exit 125
`

// installToolGuard puts the stubs first on PATH. TestMain calls it before
// any test runs.
func installToolGuard() error {
	hostPATH = os.Getenv("PATH")
	dir, err := os.MkdirTemp("", "cb-toolbox-path-")
	if err != nil {
		return err
	}
	for _, name := range boxTools {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(boxStubScript), 0o755); err != nil {
			return err
		}
	}
	toolDir = dir
	return os.Setenv("PATH", dir+string(os.PathListSeparator)+hostPATH)
}

// stopToolBox removes the box, its relay and network, and the guard.
// TestMain calls it after the run.
func stopToolBox() {
	if theBox != nil {
		for i := len(theBox.cleanup) - 1; i >= 0; i-- {
			theBox.cleanup[i]()
		}
	}
	if toolDir != "" {
		_ = os.RemoveAll(toolDir)
	}
}

// hostLookPath is exec.LookPath on the PATH the run began with.
func hostLookPath(name string) (string, error) {
	if strings.Contains(name, "/") {
		return exec.LookPath(name)
	}
	for _, d := range filepath.SplitList(hostPATH) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found on PATH", name)
}

// hostGcloud is the host's gcloud the harness runs (EnvGcloud when set,
// else gcloud on PATH), resolved through symlinks, and its SDK root.
func hostGcloud() (bin, sdk string, err error) {
	name := "gcloud"
	if v := os.Getenv(EnvGcloud); v != "" {
		name = v
	}
	p, err := hostLookPath(name)
	if err != nil {
		return "", "", err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", "", err
	}
	if abs, err := filepath.Abs(real); err == nil {
		real = abs
	}
	return p, filepath.Dir(filepath.Dir(real)), nil
}

// toolBox is the running box, started on first use. It skips the test when
// Docker or an internal network is unavailable, and fails it when the box
// could not be set up.
func toolBox(t *testing.T) *toolBoxState {
	t.Helper()
	boxOnce.Do(func() {
		theBox, boxSkip, boxErr = startToolBox()
		if boxErr != nil && theBox != nil {
			stopToolBox()
			theBox = nil
		}
	})
	switch {
	case boxSkip != "":
		t.Skipf("%s: gcloud and Terraform run only in the tool box, never on the host (#2174)", boxSkip)
	case boxErr != nil:
		t.Fatalf("the tool box (#2174): %v", boxErr)
	}
	return theBox
}

// terraformTool is the name a Terraform or OpenTofu test runs (directly or
// through `cloudburrow terraform --binary`): the box's binary, on the
// guarded PATH. The test skips, as it did before the box, when the host has
// no such tool: CI installs them in the shards whose tests need them.
func terraformTool(t *testing.T, name string) string {
	t.Helper()
	host := name
	if v := os.Getenv(EnvTofu); name == "tofu" && v != "" {
		host = v
	}
	if _, err := hostLookPath(host); err != nil {
		t.Skipf("%s is not on PATH", host)
	}
	toolBox(t)
	return name
}

func boxDocker(ctx context.Context, args ...string) (string, error) {
	b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

func startToolBox() (box *toolBoxState, skip string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if toolDir == "" {
		return nil, "", errors.New("the PATH guard is not installed (TestMain calls installToolGuard)")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return nil, "docker is not on PATH", nil
	}
	if out, err := exec.CommandContext(ctx, docker, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return nil, "Docker is not available (" + strings.TrimSpace(string(out)) + ")", nil
	}
	box = &toolBoxState{}
	defer func() {
		if err != nil || skip != "" {
			for i := len(box.cleanup) - 1; i >= 0; i-- {
				box.cleanup[i]()
			}
			box = nil
		}
	}()

	image, err := toolBoxImage(ctx)
	if err != nil {
		return box, "", err
	}
	box.image = image

	// boxd for the daemon's architecture, and run.sh, mounted at /box.
	arch, err := exec.CommandContext(ctx, docker, "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return box, "", fmt.Errorf("docker version: %v", err)
	}
	boxDir, err := os.MkdirTemp("", "cb-toolbox-")
	if err != nil {
		return box, "", err
	}
	box.cleanup = append(box.cleanup, func() { _ = os.RemoveAll(boxDir) })
	build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(boxDir, "boxd"), "./testdata/toolbox/boxd")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		return box, "", fmt.Errorf("build boxd: %v\n%s", err, b)
	}
	runSh, err := os.ReadFile(filepath.Join("testdata", "toolbox", "run.sh"))
	if err != nil {
		return box, "", err
	}
	if err := os.WriteFile(filepath.Join(boxDir, "run.sh"), runSh, 0o755); err != nil {
		return box, "", err
	}
	exe := filepath.Join(boxDir, "boxexec")
	build = exec.CommandContext(ctx, "go", "build", "-o", exe, "./testdata/toolbox/boxexec")
	if b, err := build.CombinedOutput(); err != nil {
		return box, "", fmt.Errorf("build boxexec: %v\n%s", err, b)
	}

	// What the box mounts, each at its host path: the temporary directory
	// (the tests' modules, gcloud configurations, kubeconfigs and files),
	// the repository (testdata), the instance's directory (the ADC fixture
	// gcloud-setup names, the Terraform intercept's CA) and gcloud's SDK.
	tools := map[string]string{"terraform": "/usr/local/bin/terraform", "tofu": "/usr/local/bin/tofu"}
	if _, sdk, err := hostGcloud(); err == nil {
		if runtime.GOOS == "darwin" {
			// Docker Desktop shares the temporary directory, not
			// Homebrew's: an APFS clone there costs no space.
			clone, err := os.MkdirTemp("", "cb-toolbox-sdk-")
			if err != nil {
				return box, "", err
			}
			box.cleanup = append(box.cleanup, func() { _ = os.RemoveAll(clone) })
			dst := filepath.Join(clone, "google-cloud-sdk")
			if b, err := exec.CommandContext(ctx, "cp", "-cR", sdk, dst).CombinedOutput(); err != nil {
				return box, "", fmt.Errorf("clone gcloud's SDK %s: %v\n%s", sdk, err, b)
			}
			sdk = dst
		}
		box.sdk = sdk
		for _, name := range boxGcloudTools {
			if _, err := os.Stat(filepath.Join(sdk, "bin", name)); err == nil {
				tools[name] = filepath.Join(sdk, "bin", name)
			}
		}
	}
	var ro, rw []string
	rw = append(rw, os.TempDir())
	if repo, err := filepath.Abs(filepath.Join("..", "..")); err == nil {
		ro = append(ro, repo)
	}
	if c := os.Getenv("CLOUDBURROW_TEST_CREDENTIALS"); c != "" {
		ro = append(ro, filepath.Dir(c))
	}
	args := cliArgs()
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--state-dir="); ok {
			ro = append(ro, v)
		} else if a == "--state-dir" && i+1 < len(args) {
			ro = append(ro, args[i+1])
		}
	}
	if box.sdk != "" {
		ro = append(ro, box.sdk)
	}
	var mountArgs []string
	seen := map[string]bool{}
	addMount := func(p, mode string) {
		p, err := filepath.Abs(p)
		if err != nil || seen[p] {
			return
		}
		if _, err := os.Stat(p); err != nil {
			return
		}
		seen[p] = true
		box.mounts = append(box.mounts, p)
		mountArgs = append(mountArgs, "-v", p+":"+p+":"+mode)
	}
	for _, p := range rw {
		addMount(p, "rw")
		if r, err := filepath.EvalSymlinks(p); err == nil {
			addMount(r, "rw")
		}
	}
	for _, p := range ro {
		addMount(p, "ro")
		if r, err := filepath.EvalSymlinks(p); err == nil {
			addMount(r, "ro")
		}
	}

	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return box, "", err
	}
	token := hex.EncodeToString(tok)

	// The host's listener for the relay. Docker Desktop's
	// host.docker.internal reaches the host's loopback; on Linux the relay
	// reaches the host at its bridge network's gateway.
	hostIP := "127.0.0.1"
	var relayArgs []string
	if runtime.GOOS == "linux" {
		gw, err := boxBridgeGateway(ctx)
		if err != nil {
			return box, "", err
		}
		hostIP = gw
		relayArgs = append(relayArgs, "--add-host", "host.docker.internal:"+gw)
	}
	mux, err := net.Listen("tcp", net.JoinHostPort(hostIP, "0"))
	if err != nil {
		return box, "", fmt.Errorf("listen on %s for the relay: %v", hostIP, err)
	}
	box.mux = mux
	box.cleanup = append(box.cleanup, func() { _ = mux.Close() })
	go boxMux(mux, token)
	_, muxPort, _ := net.SplitHostPort(mux.Addr().String())

	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()%1e6)
	box.network = "cb-toolbox-" + suffix
	// The label names this process, so a run killed before TestMain's
	// cleanup (a go test timeout) leaves boxes its owner can find.
	label := "cloudburrow.compat.toolbox=" + strconv.Itoa(os.Getpid())
	if out, err := boxDocker(ctx, "network", "create", "--internal", "--label", label, box.network); err != nil {
		return box, "Docker could not create an --internal network (" + strings.TrimSpace(out) + ")", nil
	}
	box.cleanup = append(box.cleanup, func() { _ = exec.Command("docker", "network", "rm", box.network).Run() })
	if err := checkInternalNetwork(ctx, box.network); err != nil {
		return box, "", err
	}

	box.relay = "cb-toolbox-relay-" + suffix
	relayRun := append([]string{"run", "-d", "--name", box.relay, "--label", label, "-v", boxDir + ":/box:ro", "--entrypoint", "/box/boxd"}, relayArgs...)
	relayRun = append(relayRun, image, "relay", "-listen", ":7000", "-target", "host.docker.internal:"+muxPort)
	if _, err := boxDocker(ctx, relayRun...); err != nil {
		return box, "", fmt.Errorf("start the relay: %v", err)
	}
	box.cleanup = append(box.cleanup, func() { _ = exec.Command("docker", "rm", "-f", "-v", box.relay).Run() })
	if _, err := boxDocker(ctx, "network", "connect", "--alias", "relay", box.network, box.relay); err != nil {
		return box, "", fmt.Errorf("connect the relay: %v", err)
	}

	box.container = "cb-toolbox-" + suffix
	run := []string{"run", "-d", "--name", box.container, "--label", label, "--network", box.network, "--cap-add", "NET_ADMIN",
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=1",
		"-e", "BOX_TOKEN=" + token, "-e", "BOX_UID=" + strconv.Itoa(os.Getuid()), "-e", "BOX_GID=" + strconv.Itoa(os.Getgid()),
		"-v", boxDir + ":/box:ro"}
	run = append(run, mountArgs...)
	run = append(run, "--entrypoint", "/bin/bash", image, "/box/run.sh")
	if _, err := boxDocker(ctx, run...); err != nil {
		return box, "", fmt.Errorf("start the box: %v", err)
	}
	box.cleanup = append(box.cleanup, func() { _ = exec.Command("docker", "rm", "-f", "-v", box.container).Run() })
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", box.container).CombinedOutput()
		if strings.Contains(string(logs), "FAIL") {
			return box, "", fmt.Errorf("the box did not set up its network:\n%s", logs)
		}
		if strings.Contains(string(logs), "\nready") || strings.HasPrefix(string(logs), "ready") {
			break
		}
		if time.Now().After(deadline) {
			return box, "", fmt.Errorf("the box did not start:\n%s", logs)
		}
	}
	// The box is on the internal network alone.
	nets, err := exec.CommandContext(ctx, "docker", "inspect", "--format",
		"{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", box.container).Output()
	if err != nil || strings.TrimSpace(string(nets)) != box.network {
		return box, "", fmt.Errorf("the box is on networks %q, want %s alone (%v)", strings.TrimSpace(string(nets)), box.network, err)
	}

	// boxexec replaces the stubs.
	conf := map[string]any{
		"docker": docker, "container": box.container, "user": fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"mounts": box.mounts, "tools": tools,
		"path": boxPath(box.sdk),
		"env": []string{"HOME=/home/box", "TF_CLI_CONFIG_FILE=/opt/box/terraformrc", "CHECKPOINT_DISABLE=1",
			"PLUGIN_UNIX_SOCKET_DIR=/run/box/plugins", "BOX_TOKEN="},
	}
	if runtime.GOOS != "linux" {
		// A macOS SDK's bundled Python is a macOS binary: the image's
		// runs it. On Linux gcloud picks its own, as on the host.
		conf["env"] = append(conf["env"].([]string), "CLOUDSDK_PYTHON=/usr/bin/python3")
	}
	raw, _ := json.MarshalIndent(conf, "", "  ")
	if err := os.WriteFile(filepath.Join(toolDir, "box.json"), raw, 0o644); err != nil {
		return box, "", err
	}
	if err := copyFile(exe, filepath.Join(toolDir, "boxexec"), 0o755); err != nil {
		return box, "", err
	}
	for name := range tools {
		p := filepath.Join(toolDir, name)
		tmp := p + ".new"
		_ = os.Remove(tmp)
		if err := os.Symlink("boxexec", tmp); err != nil {
			return box, "", err
		}
		if err := os.Rename(tmp, p); err != nil {
			return box, "", err
		}
	}
	return box, "", nil
}

// boxPath is PATH in the box.
func boxPath(sdk string) string {
	p := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if sdk != "" {
		p = filepath.Join(sdk, "bin") + ":" + p
	}
	return p
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}

// checkInternalNetwork fails unless Docker reports the network internal.
func checkInternalNetwork(ctx context.Context, network string) error {
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", "--format", "{{.Internal}}", network).Output()
	if err != nil {
		return fmt.Errorf("docker network inspect %s: %v", network, err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("network %s is not --internal (Internal=%s)", network, strings.TrimSpace(string(out)))
	}
	return nil
}

// toolBoxImage builds the box's image, tagged by the hash of its build
// context, so a change rebuilds it.
func toolBoxImage(ctx context.Context) (string, error) {
	src, err := filepath.Abs(filepath.Join("testdata", "toolbox"))
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, f := range []string{"Dockerfile", "terraformrc"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d\n", f, len(b))
		h.Write(b)
	}
	image := fmt.Sprintf("cloudburrow-compat-toolbox:%x", h.Sum(nil))[:len("cloudburrow-compat-toolbox:")+12]
	if exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() == nil {
		return image, nil
	}
	// Only the two files the image copies are its context.
	tmp, err := os.MkdirTemp("", "cb-toolbox-build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	for _, f := range []string{"Dockerfile", "terraformrc"} {
		if err := copyFile(filepath.Join(src, f), filepath.Join(tmp, f), 0o644); err != nil {
			return "", err
		}
	}
	if b, err := exec.CommandContext(ctx, "docker", "build", "-t", image, tmp).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build %s: %v\n%s", image, err, lastLines(string(b), 40))
	}
	return image, nil
}

// boxBridgeGateway is the host's address on Docker's default bridge
// network, where the relay starts.
func boxBridgeGateway(ctx context.Context) (string, error) {
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

// boxMux serves the relay: each connection begins with "<token> <port>\n",
// and is then joined to that port on the host's loopback. Anything else is
// closed unanswered, so another container on the bridge cannot use it.
func boxMux(ln net.Listener, token string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			br := bufio.NewReaderSize(c, 128)
			line, err := br.ReadString('\n')
			if err != nil || len(line) > 100 {
				return
			}
			tok, p, ok := strings.Cut(strings.TrimSpace(line), " ")
			port, perr := strconv.Atoi(p)
			if !ok || perr != nil || port < 1 || port > 65535 || subtle.ConstantTimeCompare([]byte(tok), []byte(token)) != 1 {
				return
			}
			_ = c.SetReadDeadline(time.Time{})
			u, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 10*time.Second)
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(u, br)
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

// exec runs argv in the box as the host's user, with exactly env (no
// variable of this process's), and returns its combined output.
func (b *toolBoxState) exec(ctx context.Context, env []string, argv ...string) (string, error) {
	args := []string{"exec", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), b.container, "env", "-i"}
	args = append(args, env...)
	args = append(args, argv...)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}

// TestToolBoxBlocksEgress proves the block the tool box puts gcloud and
// Terraform behind, before anything else trusts it. It checks the box's
// network is --internal first, and only then, inside the box, with no
// proxy variable set, tries to leave the machine:
//   - https://storage.googleapis.com, by name: the resolver refuses the
//     name, and its log says so;
//   - Google's addresses and the metadata server, by address
//     (142.250.0.1:443, 8.8.8.8:53 and 169.254.169.254:80): the network has
//     no route, and the filter table rejects them;
//   - the relay's port directly, as a tool: rejected (only boxd may use it);
//   - gcloud with no endpoint override and a token, so that it sends a
//     request to storage.googleapis.com, and Terraform with no CLI
//     configuration, so that it asks registry.terraform.io for a provider:
//     each fails to connect.
//
// The control: a listener of this test on the host's loopback answers
// through the relay, so the failures are the block and not a broken box.
func TestToolBoxBlocksEgress(t *testing.T) {
	box := toolBox(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// The network must be --internal before any probe runs.
	if err := checkInternalNetwork(ctx, box.network); err != nil {
		t.Fatal(err)
	}
	nets, err := exec.CommandContext(ctx, "docker", "inspect", "--format",
		"{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", box.container).Output()
	if err != nil || strings.TrimSpace(string(nets)) != box.network {
		t.Fatalf("the box is on %q, want %s alone (%v)", nets, box.network, err)
	}
	env := []string{"PATH=" + boxPath(box.sdk), "HOME=/home/box"}

	// The control.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("cloudburrow-box-control\n"))
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ctl := `import socket,sys
s=socket.create_connection(("127.0.0.1",int(sys.argv[1])),timeout=10)
print(s.recv(100).decode().strip())`
	if out, err := box.exec(ctx, env, "python3", "-I", "-c", ctl, port); err != nil || !strings.Contains(out, "cloudburrow-box-control") {
		t.Fatalf("the control: the host's loopback through the relay = %v: %s", err, out)
	}

	probe := `import socket,sys,errno,urllib.request
for a in sys.argv[1:]:
    try:
        if a.startswith("https://"):
            urllib.request.urlopen(a, timeout=10)
        else:
            h,p=a.rsplit(":",1)
            socket.create_connection((h,int(p)),timeout=10).close()
        print("REACHED",a)
    except Exception as e:
        print("BLOCKED",a,type(e).__name__,e)
`
	targets := []string{"https://storage.googleapis.com/storage/v1/b?project=x", "storage.googleapis.com:443",
		"metadata.google.internal:80", "142.250.0.1:443", "8.8.8.8:53", "169.254.169.254:80", "relay:7000"}
	out, err := box.exec(ctx, env, append([]string{"python3", "-I", "-c", probe}, targets...)...)
	if err != nil {
		t.Fatalf("the probe: %v\n%s", err, out)
	}
	for _, a := range targets {
		switch {
		case strings.Contains(out, "REACHED "+a):
			t.Errorf("from the box, %s was reached:\n%s", a, out)
		case !strings.Contains(out, "BLOCKED "+a):
			t.Errorf("the probe did not report %s:\n%s", a, out)
		}
	}
	t.Logf("from the box, with no proxy variable:\n%s", out)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "BLOCKED") && (strings.Contains(line, "timed out") || strings.Contains(line, "timeout")) {
			t.Errorf("an attempt timed out rather than failing at once, so a route may exist: %s", line)
		}
	}
	logs, _ := exec.CommandContext(ctx, "docker", "logs", box.container).CombinedOutput()
	if !strings.Contains(string(logs), "DNS storage.googleapis.com type 1 rcode 5") {
		t.Errorf("the resolver did not refuse storage.googleapis.com:\n%s", lastLines(string(logs), 30))
	}

	t.Run("gcloud", func(t *testing.T) {
		if box.sdk == "" {
			t.Skip("no gcloud on the host")
		}
		cfg := t.TempDir()
		out, err := box.exec(ctx, append(env, "CLOUDSDK_CONFIG="+cfg, "CLOUDSDK_PYTHON=/usr/bin/python3",
			"CLOUDSDK_AUTH_ACCESS_TOKEN=not-a-token", "CLOUDSDK_CORE_DISABLE_PROMPTS=1"),
			filepath.Join(box.sdk, "bin", "gcloud"), "storage", "buckets", "list", "--project", "cb-toolbox-probe")
		if err == nil {
			t.Fatalf("gcloud storage buckets list at Google's endpoint succeeded from the box:\n%s", out)
		}
		t.Logf("gcloud, unconfigured, from the box: %v\n%s", err, lastLines(out, 8))
		logs, _ := exec.CommandContext(ctx, "docker", "logs", box.container).CombinedOutput()
		if !strings.Contains(string(logs), "DNS storage.googleapis.com") {
			t.Errorf("gcloud never looked up storage.googleapis.com, so this proves nothing:\n%s", lastLines(out, 20))
		}
	})
	t.Run("terraform", func(t *testing.T) {
		dir := t.TempDir()
		mod := `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}
`
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(mod), 0o644); err != nil {
			t.Fatal(err)
		}
		// No CLI configuration: Terraform's default, the registry.
		out, err := box.exec(ctx, append(env, "TF_CLI_CONFIG_FILE=/dev/null", "CHECKPOINT_DISABLE=1"),
			"sh", "-c", "cd \"$0\" && terraform init -input=false -no-color", dir)
		if err == nil {
			t.Fatalf("terraform init reached a registry from the box:\n%s", out)
		}
		t.Logf("terraform init, unconfigured, from the box: %v\n%s", err, lastLines(out, 8))
		logs, _ := exec.CommandContext(ctx, "docker", "logs", box.container).CombinedOutput()
		if !strings.Contains(string(logs), "DNS registry.terraform.io") {
			t.Errorf("terraform never looked up registry.terraform.io, so this proves nothing")
		}
	})
}
