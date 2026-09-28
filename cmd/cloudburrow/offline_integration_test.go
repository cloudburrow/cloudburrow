//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestPrefetchThenUpOfflineWithNoEgress is #604's acceptance run: prefetch,
// then `up --offline` for the default services plus Bigtable into a cluster
// whose node has no route out, which must reach ready.
//
// The node's egress is cut with iptables inside the node, before anything
// but kind's own pods runs, rather than by a Docker network without egress:
// kind's node entrypoint needs the network's gateway, and on a network
// created with --internal (which has none) the node exits at start (kind
// v0.33.0, measured). So the cluster is created first, with the name and
// kind configuration `up` would use, its egress cut, and `up --offline`
// then finds it running. Everything the node runs after that must come from
// the cache: a pull would be refused.
//
// What this does not cut is the Docker daemon's own network, so it proves
// nothing about the daemon; the unit tests prove `up --offline` never asks
// it to pull (TestUpOfflineNeverPullsOrDownloads).
//
// It downloads about 2 GiB (the console terminal's image is about 1 GB of
// it, #824) and takes minutes, so it runs only when asked:
//
//	CLOUDBURROW_TEST_OFFLINE=1 go test -tags=integration -run TestPrefetchThenUpOffline ./cmd/cloudburrow/
func TestPrefetchThenUpOfflineWithNoEgress(t *testing.T) {
	if os.Getenv("CLOUDBURROW_TEST_OFFLINE") == "" {
		t.Skip("downloads about 2 GiB; set CLOUDBURROW_TEST_OFFLINE=1 to run")
	}
	bin := filepath.Join(t.TempDir(), "cloudburrow")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	suffix := time.Now().UnixNano() % 1e6
	name := fmt.Sprintf("off-%d", suffix)
	base := 21000 + int(time.Now().UnixNano()%200)*100
	flags := []string{"--name", name, "--state-dir", t.TempDir(), "--port-base", strconv.Itoa(base),
		"--services", "storage,pubsub,tasks,run,secretmanager,bigtable", "--mode", "ephemeral"}
	t.Cleanup(func() {
		_ = exec.Command(bin, append([]string{"stop"}, flags...)...).Run()
		_ = exec.Command("kind", "delete", "cluster", "--name", "cloudburrow-"+name).Run()
	})
	run := func(timeout time.Duration, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, append(args, flags...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	out := run(30*time.Minute, "prefetch")
	t.Logf("prefetch:\n%s", out)

	// The cluster `up` would create, created now so its egress can be cut
	// before anything of CloudBurrow's runs in it.
	cfg, err := config.Load(config.Options{Args: flags})
	if err != nil {
		t.Fatal(err)
	}
	c, err := newCluster(cfg)
	if err != nil {
		t.Fatal(err)
	}
	kindConfig, err := cluster.WriteConfig(cfg.InstanceDir(), ingressMappings(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := c.Create(ctx, kindConfig); err != nil {
		t.Fatalf("create the cluster: %v", err)
	}
	node := "cloudburrow-" + name + "-control-plane"
	cutEgress(t, node)

	out = run(15*time.Minute, "up", "--offline", "--detach", "--detach-timeout", "12m")
	t.Logf("up --offline:\n%s", out)

	// Ready is the claim; that nothing was pulled is why it holds. The
	// kubelet records a Pulling event for every image it fetches.
	events, err := exec.Command("kubectl", "--kubeconfig", cfg.KubeconfigPath(), "get", "events", "-A",
		"--field-selector", "reason=Pulling", "-o", "custom-columns=NS:.metadata.namespace,MSG:.message", "--no-headers").CombinedOutput()
	if err != nil {
		t.Fatalf("read events: %v\n%s", err, events)
	}
	if s := strings.TrimSpace(string(events)); s != "" {
		t.Errorf("the kubelet pulled images, which the cache should have supplied:\n%s", s)
	}
	status, err := exec.Command(bin, append([]string{"status"}, flags...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	if !strings.Contains(string(status), "bigtable") {
		t.Errorf("status does not report bigtable:\n%s", status)
	}
}

// cutEgress rejects every packet the node, or a pod on it, sends anywhere
// but loopback, link-local and the private ranges the cluster, the kind
// network and Docker's DNS live in, and checks that a registry is then
// unreachable from the node.
func cutEgress(t *testing.T, node string) {
	t.Helper()
	script := `set -e
for ipt in iptables ip6tables; do $ipt -N CB-NOEGRESS; done
for d in 127.0.0.0/8 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16; do iptables -A CB-NOEGRESS -d $d -j RETURN; done
for d in ::1/128 fc00::/7 fe80::/10; do ip6tables -A CB-NOEGRESS -d $d -j RETURN; done
for ipt in iptables ip6tables; do
  $ipt -A CB-NOEGRESS -j REJECT
  $ipt -I OUTPUT 1 -j CB-NOEGRESS
  $ipt -I FORWARD 1 -j CB-NOEGRESS
done`
	if out, err := exec.Command("docker", "exec", node, "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("cut the node's egress: %v\n%s", err, out)
	}
	for _, url := range []string{"https://registry-1.docker.io/v2/", "https://gcr.io/v2/", "https://ghcr.io/v2/", "https://github.com/"} {
		if out, err := exec.Command("docker", "exec", node, "curl", "-sS", "--max-time", "10", "-o", "/dev/null", url).CombinedOutput(); err == nil {
			t.Fatalf("the node still reaches %s after its egress was cut: %s", url, out)
		}
	}
}
