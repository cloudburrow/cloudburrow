//go:build compat

package compat

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestEnvResolvesNoGoogleName (#2038) records every DNS query gcloud and
// each served SDK's auth library make under `cloudburrow env`, and under
// gcloud-setup's configuration, and fails on any query for a Google name.
//
// The record is taken at the network layer, never through a proxy: each
// client runs in testdata/egress's no-egress container (an internal Docker
// network, no route out), whose nat table sends every DNS query to the
// sink's resolver, which logs the name, and every connection off the
// machine to the sink, which logs it and closes it. A relay, the only
// container that can reach the host, forwards the instance's ports that
// the exported variables name; the sink serves them on the container's
// loopback, where the variables name them. A control lookup of a Google
// name must be logged first, or an empty log proves nothing.
//
// Before #2038 every gcloud command looked up metadata.google.internal:
// each request's User-Agent asks gce_cache whether gcloud runs on GCE
// unless metrics/environment is set (metrics.py), whatever
// core/check_gce_metadata says, and gce_read.py reads the metadata server
// at GCE_METADATA_ROOT, whose default is that name. `env` now exports
// CLOUDSDK_METRICS_ENVIRONMENT, CLOUDSDK_CORE_CHECK_GCE_METADATA=false and
// GCE_METADATA_ROOT and GCE_METADATA_IP at the instance; gcloud-setup's
// configuration sets the two properties.
func TestEnvResolvesNoGoogleName(t *testing.T) {
	h := New(t)
	// gcloud storage ls and gcloud pubsub topics list are the commands
	// that send requests.
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvPubSub)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker is not available")
	}
	vars := envVarsFromCLI(t)
	for _, k := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "GCE_METADATA_HOST", "GCE_METADATA_ROOT", "GCE_METADATA_IP",
		"CLOUDSDK_METRICS_ENVIRONMENT", "CLOUDSDK_CORE_CHECK_GCE_METADATA", "CLOUDSDK_AUTH_TOKEN_HOST", "CLOUDSDK_AUTH_DISABLE_CREDENTIALS"} {
		if vars[k] == "" {
			t.Fatalf("cloudburrow env exported no %s", k)
		}
	}
	for _, k := range []string{"GCE_METADATA_HOST", "GCE_METADATA_ROOT", "GCE_METADATA_IP", "CLOUDSDK_AUTH_TOKEN_HOST"} {
		h.requireLocal(vars[k])
	}
	fixture := vars["GOOGLE_APPLICATION_CREDENTIALS"]
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("the exported fixture: %v", err)
	}

	// gcloud-setup's configuration, as it writes it, in a directory of its
	// own that the container mounts at the same path.
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	conf, err := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name))
	if err != nil {
		t.Fatal(err)
	}
	mounts := []string{"-v", fixture + ":" + fixture + ":ro"}
	if m := regexp.MustCompile(`(?m)^credential_file_override = (.+)$`).FindStringSubmatch(string(conf)); m != nil && m[1] != fixture {
		mounts = append(mounts, "-v", m[1]+":"+m[1]+":ro")
	}
	for _, want := range []string{"check_gce_metadata = false", "[metrics]\nenvironment = "} {
		if !strings.Contains(string(conf), want) {
			t.Fatalf("gcloud-setup's configuration does not set %q:\n%s", want, conf)
		}
	}

	// Every instance port the variables or the configuration name.
	portRE := regexp.MustCompile(`127\.0\.0\.1:([0-9]+)`)
	portSet := map[string]bool{}
	var envFile strings.Builder
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := vars[k]
		// What a client reads; no admin credential goes into the container.
		if strings.Contains(k, "TOKEN") && !strings.HasSuffix(k, "TOKEN_HOST") {
			continue
		}
		fmt.Fprintf(&envFile, "%s=%s\n", k, v)
		for _, m := range portRE.FindAllStringSubmatch(v, -1) {
			portSet[m[1]] = true
		}
	}
	for _, m := range portRE.FindAllStringSubmatch(string(conf), -1) {
		portSet[m[1]] = true
	}
	ports := make([]string, 0, len(portSet))
	for p := range portSet {
		ports = append(ports, p)
	}
	sort.Strings(ports)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	base, egressDir := egressImage(ctx, t)
	image := envDNSImage(ctx, t, base)
	dir := t.TempDir()
	for _, f := range []string{"sink"} {
		b, err := os.ReadFile(filepath.Join(egressDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	envDNSFiles(ctx, t, dir)
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(envFile.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	network, relay := "cb-envdns-"+suffix, "cb-envdns-relay-"+suffix
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
	args = append(args, image)
	for _, p := range ports {
		args = append(args, "-forward", "0.0.0.0:"+p+"=host.docker.internal:"+hostForwarder(t, hostIP, "127.0.0.1:"+p))
	}
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start the relay: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", relay).Run() })
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", "--alias", "relay", network, relay).CombinedOutput(); err != nil {
		t.Fatalf("connect the relay: %v\n%s", err, b)
	}

	// run runs one client in the container and returns its OK lines and
	// every name looked up or connection attempted off the machine.
	run := func(t *testing.T, client string, withEnv bool) (ok []string, attempts []string, out string) {
		t.Helper()
		a := []string{"run", "--rm", "--network", network, "--cap-add", "NET_ADMIN",
			"-v", dir + ":/egress:ro", "-e", "EGRESS_PORTS=" + strings.Join(ports, " ")}
		a = append(a, mounts...)
		if withEnv {
			a = append(a, "--env-file", filepath.Join(dir, "env"))
		} else {
			// A copy, so the gcloud the container runs as root writes
			// nothing into the test's directory.
			cp := t.TempDir()
			if b, err := exec.Command("cp", "-R", gdir+"/.", cp).CombinedOutput(); err != nil {
				t.Fatalf("copy the configuration: %v\n%s", err, b)
			}
			a = append(a, "-v", cp+":/gcloud", "-e", "EGRESS_GCLOUD_CONFIG=/gcloud", "-e", "EGRESS_GCLOUD_NAME="+name)
		}
		a = append(a, "--entrypoint", "bash", image, "/egress/run.sh", "self", client)
		b, err := exec.CommandContext(ctx, "docker", a...).CombinedOutput()
		out = string(b)
		if err != nil || !strings.Contains(out, "DONE") {
			t.Fatalf("the %s probe: %v\n%s", client, err, out)
		}
		control := false
		for _, l := range strings.Split(out, "\n") {
			switch {
			case l == "OK self":
			case strings.HasPrefix(l, "OK "):
				ok = append(ok, strings.TrimPrefix(l, "OK "))
			case strings.HasPrefix(l, "EGRESS dns control.googleapis.com "):
				control = true
			case strings.HasPrefix(l, "EGRESS "):
				attempts = append(attempts, strings.TrimPrefix(l, "EGRESS "))
			}
		}
		if !control {
			t.Fatalf("the control lookup of control.googleapis.com was not logged, so the log proves nothing:\n%s", out)
		}
		return ok, attempts, out
	}

	// gcloud storage ls must list something to succeed.
	bucket := h.Project() + "-envdns"
	sc := storageClient(t, h)
	if err := sc.Bucket(bucket).Create(h.Context(), vars["CLOUDSDK_CORE_PROJECT"], nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(h.Context()) })

	// Without #2038's variables gcloud must still be seen looking up
	// metadata.google.internal, or the record could not catch it.
	t.Run("control: the variables before #2038", func(t *testing.T) {
		_, attempts, out := run(t, "gcloud-before", true)
		if !slices.ContainsFunc(attempts, func(a string) bool { return strings.HasPrefix(a, "dns metadata.google.internal") }) {
			t.Errorf("gcloud without #2038's variables did not look up metadata.google.internal, so the record proves nothing:\n%s", out)
		}
	})

	for _, c := range []struct {
		client  string
		withEnv bool
		want    []string // OK lines that must appear
		// known is the one Google name a library is known to look up
		// here, which no variable stops, and issue its issue: the subtest
		// skips, naming it, when that name (and connections to the
		// sink's answer) are the only attempts, and fails on any other.
		known, issue string
	}{
		{client: "gcloud-env", withEnv: true, want: []string{"gcloud version", "gcloud config-list", "gcloud auth-list", "gcloud storage-ls", "gcloud pubsub-topics"}},
		{client: "gcloud-setup", want: []string{"gcloud version", "gcloud storage-ls", "gcloud pubsub-topics"}},
		{client: "go", withEnv: true, want: []string{"go oauth2-adc", "go auth-adc", "go metadata"}},
		{client: "python", withEnv: true, want: []string{"python adc", "python metadata"}},
		{client: "node", withEnv: true, want: []string{"node adc", "node metadata"}},
		{client: "java", withEnv: true, want: []string{"java adc", "java metadata"}},
		// The fixture as gcloud's credential: gcloud's bundled google-auth
		// would look up its Regional Access Boundary at
		// iamcredentials.googleapis.com, a host no property moves, but
		// `env` exports CLOUDSDK_AUTH_DISABLE_CREDENTIALS, as gcloud-setup
		// writes auth/disable_credentials, so no credential is loaded
		// (#2040).
		{client: "gcloud-fixture", withEnv: true, want: []string{"gcloud storage-ls"}},
		// gtoken's hard-coded token URL: a program's own getAccessToken()
		// for a service-account key. No variable, fixture field or
		// universe moves it (#2040); another fixture type would lose the
		// key, and signing would go to iamcredentials.googleapis.com.
		{client: "node-token", withEnv: true, known: "oauth2.googleapis.com", issue: "#2040"},
	} {
		t.Run(c.client, func(t *testing.T) {
			ok, attempts, out := run(t, c.client, c.withEnv)
			for _, w := range c.want {
				if !slices.Contains(ok, w) {
					t.Errorf("%s did not succeed:\n%s", w, out)
				}
			}
			knownSeen := 0
			for _, a := range attempts {
				f := strings.Fields(a)
				switch {
				case c.known != "" && len(f) >= 2 && f[0] == "dns" && strings.TrimSuffix(f[1], ".") == c.known,
					c.known != "" && a == "tcp 198.18.0.1:443":
					knownSeen++
				case len(f) >= 2 && f[0] == "dns" && !isGoogleName(f[1]):
					// A local name (a *.localhost override, say): answered
					// in the container, and not Google's.
				case len(f) >= 2 && f[0] == "dns":
					t.Errorf("%s looked up a Google name (answered in the container, never sent): %s", c.client, a)
				default:
					t.Errorf("%s tried to leave the machine (stopped in the container): %s", c.client, a)
				}
			}
			if t.Failed() {
				return
			}
			if knownSeen > 0 {
				t.Skipf("%s: %v; %s still looked up %s (%d lookups and connections, all stopped in the container)",
					c.issue, ok, c.client, c.known, knownSeen)
			}
			t.Logf("%v; no Google name looked up, no connection off the machine", ok)
		})
	}
}

// isGoogleName reports whether a DNS name is one of Google's.
func isGoogleName(name string) bool {
	n := strings.TrimSuffix(strings.ToLower(name), ".")
	if strings.Contains(n, "google") {
		return true
	}
	for _, s := range []string{"gcr.io", "pkg.dev", "run.app", "appspot.com", "cloudfunctions.net", "gstatic.com",
		"gvt1.com", "sourcemanager.dev", "firebaseio.com", "firebaseapp.com", "web.app", "1e100.net"} {
		if n == s || strings.HasSuffix(n, "."+s) {
			return true
		}
	}
	return false
}

// envDNSImage builds testdata/envdns's image on base (egressImage's),
// tagged by the hash of the base and its files so a change rebuilds it.
func envDNSImage(ctx context.Context, t *testing.T, base string) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("testdata", "envdns"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	sum.Write([]byte(base))
	for _, f := range []string{"Dockerfile", "pom.xml", "Probe.java"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		sum.Write(b)
	}
	image := fmt.Sprintf("cloudburrow-compat-envdns:%x", sum.Sum(nil))[:len("cloudburrow-compat-envdns:")+12]
	if exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() != nil {
		if b, err := exec.CommandContext(ctx, "docker", "build", "--build-arg", "BASE="+base, "-t", image, src).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", image, err, b)
		}
	}
	return image
}

// envDNSFiles puts testdata/envdns's Go probe, built for the daemon's
// architecture, and its scripts in dir, to mount at /egress.
func envDNSFiles(ctx context.Context, t *testing.T, dir string) {
	t.Helper()
	arch, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "goprobe"), "./testdata/envdns/goprobe")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the Go probe: %v\n%s", err, b)
	}
	for _, f := range []string{"run.sh", "probe.py", "probe.js"} {
		b, err := os.ReadFile(filepath.Join("testdata", "envdns", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}
