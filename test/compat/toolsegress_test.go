//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestToolsContainerBlocksGoogleAtTheNetworkLayer is the tools container's
// self-test (#2174). On a stack of its own, so the escapes it makes on
// purpose fail no other test, it checks that:
//
//   - Docker reports the network as --internal;
//   - a bare container on it, with no nat table and no resolver, cannot
//     reach a documentation address or the metadata server, and resolves
//     storage.googleapis.com to nothing: the block is the network's own;
//   - in the tools container, with no proxy variable set, curl to
//     https://storage.googleapis.com, http://169.254.169.254 and
//     http://metadata.google.internal, and gcloud with no endpoint override,
//     all fail, and each attempt is logged as an escape, the one way the
//     harness fails a test;
//   - and the relay still reaches the host's loopback, where the instance
//     listens.
//
// Nothing leaves the machine: the DNS answer is 198.18.0.1, routed nowhere,
// and every connection off the loopback ends at the sink.
func TestToolsContainerBlocksGoogleAtTheNetworkLayer(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage) // the storage shard, which runs gcloud's and Terraform's tests
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	work := t.TempDir()
	s, err := startTools(ctx, []string{work})
	if err != nil {
		if toolsRequired() || !errors.Is(err, errNoDocker) {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	defer s.stop()

	if err := checkInternal(ctx, s.network); err != nil {
		t.Fatal(err)
	}

	t.Run("the network itself has no route out", func(t *testing.T) {
		out, _ := exec.CommandContext(ctx, "docker", "run", "--rm", "--platform", "linux/amd64", "--network", s.network,
			"--dns", "127.0.0.1", "--entrypoint", "bash", s.image, "-c", `
for a in 192.0.2.1/443 169.254.169.254/80; do
  if timeout 10 bash -c "exec 3<>/dev/tcp/$a" 2>/dev/null; then echo "REACHED $a"; else echo "BLOCKED $a"; fi
done
if timeout 10 getent hosts storage.googleapis.com; then echo "RESOLVED"; else echo "UNRESOLVED"; fi`).CombinedOutput()
		got := string(out)
		for _, want := range []string{"BLOCKED 192.0.2.1/443", "BLOCKED 169.254.169.254/80", "UNRESOLVED"} {
			if !strings.Contains(got, want) {
				t.Errorf("a bare container on the internal network: want %q:\n%s", want, got)
			}
		}
	})

	// exec runs a command in the tools container as the test's uid, with
	// nothing but a home: no proxy variable, no configuration.
	run := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "-u", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			s.container, "env", "-i", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/home/tools",
			"CLOUDSDK_CONFIG=" + filepath.Join(work, "gcloud"), "CLOUDSDK_CORE_DISABLE_PROMPTS=1"}, args...)...)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	escaped := func(t *testing.T, before int, want ...string) {
		t.Helper()
		lines := s.escapesSince(before)
		for _, w := range want {
			found := false
			for _, l := range lines {
				if strings.HasPrefix(l, "EGRESS "+w) {
					found = true
				}
			}
			if !found {
				t.Errorf("no escape %q was logged; the log has %v", w, lines)
			}
		}
		t.Logf("stopped and reported: %v", lines)
	}

	for _, c := range []struct{ name, url, dns, tcp string }{
		{"storage.googleapis.com", "https://storage.googleapis.com/storage/v1/b?project=p", "dns storage.googleapis.com ", "tcp 198.18.0.1:443"},
		{"the metadata server's address", "http://169.254.169.254/computeMetadata/v1/", "", "tcp 169.254.169.254:80"},
		{"metadata.google.internal", "http://metadata.google.internal/computeMetadata/v1/", "dns metadata.google.internal ", "tcp 198.18.0.1:80"},
	} {
		t.Run("curl "+c.name, func(t *testing.T) {
			before := s.escapeCount()
			out, err := run(t, "curl", "-sS", "-m", "15", "-H", "Metadata-Flavor: Google", c.url)
			if err == nil {
				t.Fatalf("curl %s succeeded in the tools container:\n%s", c.url, out)
			}
			want := []string{c.tcp}
			if c.dns != "" {
				want = append(want, c.dns)
			}
			escaped(t, before, want...)
		})
	}

	t.Run("gcloud with no endpoint override", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(work, "token"), []byte("not-a-token\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := s.escapeCount()
		out, err := run(t, "timeout", "180", "gcloud", "storage", "buckets", "list", "--project", "p",
			"--access-token-file", filepath.Join(work, "token"), "--verbosity", "error")
		if err == nil {
			t.Fatalf("gcloud reached a bucket list with no override:\n%s", out)
		}
		escaped(t, before, "dns storage.googleapis.com ", "tcp 198.18.0.1:443")
	})

	t.Run("the relay reaches the host's loopback", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("local " + r.URL.Path))
		}))
		defer srv.Close()
		before := s.escapeCount()
		out, err := run(t, "curl", "-sS", "-m", "15", srv.URL+"/ok")
		if err != nil || out != "local /ok" {
			t.Fatalf("curl %s/ok from the tools container = %q, %v; want the host's loopback server", srv.URL, out, err)
		}
		if l := s.escapesSince(before); len(l) > 0 {
			t.Errorf("a loopback request was logged as an escape: %v", l)
		}
	})
}
