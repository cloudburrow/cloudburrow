//go:build integration

package filestoreserver

// Runs a real NFS server in Docker on the kind network and mounts it from
// a client container there (make test-integration). It needs Docker and
// the kind network, which any CloudBurrow instance makes; it removes what
// it made.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
)

func TestServerSnapshotsBacksUpAndRestores(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "network", "inspect", Network).Run(); err != nil {
		t.Skip("no kind network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	inst := fmt.Sprintf("it%d", time.Now().UnixNano()%1e6)
	d := &Docker{Instance: inst, Dir: t.TempDir(), Logf: t.Logf}
	t.Cleanup(func() {
		if err := d.Stop(context.Background(), true); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	key := "fs-0a1b2c"
	ip, err := d.Ensure(ctx, key, "", filestore.Spec{NFSv41: true, Shares: []filestore.Share{{Name: "vol1"}}})
	if err != nil {
		t.Fatal(err)
	}
	client := func(script string) string {
		t.Helper()
		img, _ := d.image(ctx)
		out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--privileged", "--network", Network,
			"--entrypoint", "sh", img, "-c", "set -e; mkdir -p /m; mount -t nfs -o vers=4.1 "+ip+":/vol1 /m; "+script+"; umount /m").CombinedOutput()
		if err != nil {
			t.Fatalf("client: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	client("echo one > /m/f")
	if err := d.Snapshot(ctx, key, "vol1", "snap-01"); err != nil {
		t.Fatal(err)
	}
	client("echo two > /m/f")
	if n, err := d.Backup(ctx, key, "vol1", "backup-01", nil); err != nil || n == 0 {
		t.Fatalf("backup: %d, %v", n, err)
	}
	if err := d.Revert(ctx, key, "vol1", "snap-01"); err != nil {
		t.Fatal(err)
	}
	if got := client("cat /m/f"); got != "one" {
		t.Fatalf("after revert: %q", got)
	}
	if err := d.Restore(ctx, key, "vol1", "backup-01", nil); err != nil {
		t.Fatal(err)
	}
	if got := client("cat /m/f"); got != "two" {
		t.Fatalf("after restore: %q", got)
	}
	// A new configuration keeps the files and the address.
	ip2, err := d.Ensure(ctx, key, ip, filestore.Spec{Shares: []filestore.Share{{Name: "vol1"}}})
	if err != nil || ip2 != ip {
		t.Fatalf("reconfigure: %s, %v", ip2, err)
	}
	if u, err := d.Usage(ctx, key, "vol1"); err != nil || u == 0 {
		t.Fatalf("usage: %d, %v", u, err)
	}
}
