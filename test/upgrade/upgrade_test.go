//go:build upgrade

package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/iterator"
)

// OlderRelease is the release whose state this one must load. It is the
// latest tag; move it forward with each release (docs/install.md,
// "Upgrading").
const OlderRelease = "v0.1.0"

// services is every service both releases serve.
const services = "storage,pubsub,tasks,run,secretmanager,firestore,datastore,bigtable,spanner,cloudsql,bigquery,memorystore,cloudsql-mysql,scheduler,logging,kms"

// keptByOlderRelease are the services whose state OlderRelease keeps
// across a stop in persistent mode. The others ran in memory in that
// release (Pub/Sub, Firestore, Bigtable, Spanner, BigQuery, Logging), so
// there is nothing of theirs to upgrade.
var keptByOlderRelease = map[string]bool{
	"storage": true, "tasks": true, "secretmanager": true, "kms": true, "scheduler": true, "projects": true,
	"datastore": true, "cloudsql": true, "cloudsql-mysql": true, "memorystore": true,
}

// inOlderArchive are the services OlderRelease's `state save` captures.
var inOlderArchive = map[string]bool{
	"storage": true, "tasks": true, "secretmanager": true, "projects": true, "cloudsql": true,
}

// ports gives both releases the same fixed ports, so one env reaches both.
func ports(base int) []string {
	var out []string
	for i, p := range []string{"control", "console", "ingress", "metadata", "storage", "pubsub", "tasks", "run",
		"secrets", "resourcemanager", "scheduler", "logging", "firestore", "datastore", "bigtable", "spanner",
		"bigquery", "bigquery-storage", "memorystore", "cloudsql-mysql", "kms"} {
		out = append(out, "--port-"+p, fmt.Sprint(base+i))
	}
	return out
}

// TestUpgradeFromOlderRelease builds OlderRelease, seeds a persistent
// instance with it and stops it, then brings the same state directory up
// with this release and reads everything back; then it loads the older
// release's `state save` archive into this release and reads that back.
//
// It needs Docker and kind, and takes about half an hour:
//
//	CLOUDBURROW_UPGRADE_BIN=$PWD/bin/cloudburrow go test -tags upgrade -timeout 90m ./test/upgrade/
//
// CLOUDBURROW_UPGRADE_PORT_BASE picks the ports (default 29300).
func TestUpgradeFromOlderRelease(t *testing.T) {
	newBin := os.Getenv("CLOUDBURROW_UPGRADE_BIN")
	if newBin == "" {
		t.Skip("set CLOUDBURROW_UPGRADE_BIN to this release's binary")
	}
	base := 29300
	if v := os.Getenv("CLOUDBURROW_UPGRADE_PORT_BASE"); v != "" {
		fmt.Sscan(v, &base)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Minute)
	defer cancel()
	tmp := t.TempDir()
	oldBin := buildRelease(t, OlderRelease, tmp)
	stateDir := filepath.Join(tmp, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "upgrade-" + strings.ReplaceAll(strings.TrimPrefix(OlderRelease, "v"), ".", "-")
	common := append([]string{"--name", name, "--state-dir", stateDir, "--services", services}, ports(base)...)
	t.Cleanup(func() {
		_ = exec.Command(newBin, "stop", "--name", name, "--state-dir", stateDir).Run()
		_ = exec.Command(newBin, "delete", "--name", name, "--state-dir", stateDir).Run()
	})

	up(t, oldBin, append([]string{"--mode", "persistent"}, common...))
	old := connect(t, oldBin, name, stateDir, common)
	seeded := seed(ctx, t, old)
	archive := filepath.Join(tmp, "older.tar.gz")
	cli(t, oldBin, append([]string{"state", "save", archive}, common...)...)
	// This release refuses to start over the buckets of the server the
	// older one ran (#780), and does not yet migrate them (#1606): they are
	// in the archive, and go from the volume before the upgrade.
	clearOlderStorage(ctx, t, old)
	cli(t, oldBin, "stop", "--name", name, "--state-dir", stateDir)

	// The same state directory under this release. Its other surfaces
	// (this release serves more) are moved clear of the older ones.
	current := append([]string{"--port-base", fmt.Sprint(base + 100)}, common...)
	up(t, newBin, append([]string{"--mode", "persistent"}, current...))
	cur := connect(t, newBin, name, stateDir, current)
	fromDir := expected(seeded, keptByOlderRelease)
	delete(fromDir, "storage") // #1606
	report(t, "state directory", verify(ctx, t, cur, fromDir))

	// The older archive, loaded into this release.
	cli(t, newBin, append([]string{"state", "load", archive}, current...)...)
	report(t, "state load", verify(ctx, t, cur, expected(seeded, inOlderArchive)))
}

// clearOlderStorage deletes the seeded bucket through the older release.
func clearOlderStorage(ctx context.Context, t *testing.T, in instance) {
	t.Helper()
	c := in.storage(ctx, t)
	defer c.Close()
	b := c.Bucket(bucketName)
	it := b.Objects(ctx, nil)
	for {
		o, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Object(o.Name).Delete(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Delete(ctx); err != nil {
		t.Fatal(err)
	}
}

// expected is the services that were seeded and that kept their state.
func expected(seeded map[string]error, kept map[string]bool) map[string]bool {
	out := map[string]bool{}
	for s, err := range seeded {
		if err == nil && kept[s] {
			out[s] = true
		}
	}
	return out
}

func report(t *testing.T, what string, rs map[string]error) {
	t.Helper()
	for _, s := range sortedKeys(rs) {
		if rs[s] != nil {
			t.Errorf("%s: %s: %v", what, s, rs[s])
		}
	}
}

// buildRelease builds the CLI at tag into dir, from a worktree of this
// repository.
func buildRelease(t *testing.T, tag, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "src-"+tag)
	if out, err := exec.Command("git", "worktree", "add", "--detach", src, tag).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add %s: %v\n%s", tag, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("git", "worktree", "remove", "--force", src).Run() })
	bin := filepath.Join(dir, "cloudburrow-"+tag)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags",
		"-X github.com/cloudburrow/cloudburrow/internal/version.Version="+tag, "-o", bin, "./cmd/cloudburrow")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", tag, err, out)
	}
	return bin
}

func cli(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), args[0], err, redact(out))
	}
}

func up(t *testing.T, bin string, args []string) {
	t.Helper()
	cli(t, bin, append([]string{"up", "--detach", "--detach-timeout", "20m"}, args...)...)
}

var pgHost = regexp.MustCompile(`cloudsql host (127\.0\.0\.1:\d+)`)

// connect reads the running instance's endpoints.
func connect(t *testing.T, bin, name, stateDir string, common []string) instance {
	t.Helper()
	in := instance{env: readEnv(t, bin, common...), project: name}
	if log, err := os.ReadFile(filepath.Join(stateDir, name, "up.log")); err == nil {
		if m := pgHost.FindAllSubmatch(log, -1); len(m) > 0 {
			in.pgAddr = string(m[len(m)-1][1])
		}
	}
	if p := in.env["GOOGLE_CLOUD_PROJECT"]; p != "" {
		in.project = p
	}
	return in
}

var tokenish = regexp.MustCompile(`(?i)((token|password)[^=:]*[=:]\s*['"]?)[A-Za-z0-9._~+/=-]+`)

func redact(b []byte) string { return tokenish.ReplaceAllString(string(b), "$1<redacted>") }

// TestSeedRunning and TestVerifyRunning seed, and read back, an instance
// that is already running, for checking an upgrade by hand. They take the
// binary and the flags the instance was started with:
//
//	CLOUDBURROW_UPGRADE_RUNNING="bin|--name x --state-dir y ..." go test -tags upgrade -run TestSeedRunning ./test/upgrade/
//
// CLOUDBURROW_UPGRADE_EXPECT is "dir" or "archive", the set TestVerifyRunning checks.
func TestSeedRunning(t *testing.T) {
	in := running(t)
	for s, err := range seed(context.Background(), t, in) {
		if err != nil {
			t.Logf("not seeded: %s: %v", s, err)
		}
	}
}

func TestVerifyRunning(t *testing.T) {
	in := running(t)
	want := map[string]bool{}
	for s := range keptByOlderRelease {
		want[s] = true
	}
	if os.Getenv("CLOUDBURROW_UPGRADE_EXPECT") == "archive" {
		want = inOlderArchive
	}
	report(t, "verify", verify(context.Background(), t, in, want))
}

func running(t *testing.T) instance {
	t.Helper()
	spec := os.Getenv("CLOUDBURROW_UPGRADE_RUNNING")
	if spec == "" {
		t.Skip("set CLOUDBURROW_UPGRADE_RUNNING to bin|flags")
	}
	bin, flags, _ := strings.Cut(spec, "|")
	args := strings.Fields(flags)
	var name, stateDir string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--name":
			name = args[i+1]
		case "--state-dir":
			stateDir = args[i+1]
		}
	}
	return connect(t, bin, name, stateDir, args)
}
