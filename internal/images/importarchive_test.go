package images

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// nodeRunner stands in for docker and kind around one node: docker cp
// keeps the copied bytes (short by `lose` of them), stat reports their
// size, ctr's import records the image, and the CRI lists it once it has
// been asked `criLag` times after the import.
type nodeRunner struct {
	mu       sync.Mutex
	calls    []string
	lose     int
	criLag   int
	imported bool
	asked    int
	size     int
}

func (n *nodeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := name + " " + strings.Join(args, " ")
	n.calls = append(n.calls, c)
	switch {
	case strings.HasPrefix(c, "kind get nodes"):
		return "node-1\n", nil
	case strings.HasPrefix(c, "docker cp "):
		b, err := os.ReadFile(args[1])
		if err != nil {
			return "", err
		}
		n.size = len(b) - n.lose
		return "", nil
	case strings.Contains(c, " stat -c %s "):
		return strconv.Itoa(n.size), nil
	case strings.Contains(c, " images import "):
		n.imported = true
		return "unpacking ref (sha256:abc)...done\n", nil
	case strings.Contains(c, " images ls -q"):
		if n.imported {
			return "gcr.io/x/img@sha256:1\n", nil
		}
		return "", nil
	case strings.Contains(c, " crictl inspecti "):
		if !n.imported {
			return "", os.ErrNotExist
		}
		if n.asked < n.criLag {
			n.asked++
			return "", os.ErrNotExist
		}
		return "{}", nil
	}
	return "", nil
}

func (n *nodeRunner) ran(prefix string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	k := 0
	for _, c := range n.calls {
		if strings.HasPrefix(c, prefix) {
			k++
		}
	}
	return k
}

func archiveFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "img.tar")
	if err := os.WriteFile(p, []byte("an image archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const testRef = "gcr.io/x/img@sha256:1"

// The archive is copied into the node and imported from that file, never
// streamed through docker exec's stdin, and the file is removed after.
func TestImportArchiveCopiesIntoTheNode(t *testing.T) {
	r := &nodeRunner{}
	if err := (&Loader{ClusterName: "c", Runner: r}).ImportArchive(context.Background(), archiveFile(t), testRef); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.calls, "\n")
	for _, want := range []string{
		"docker cp ", "node-1:/var/cloudburrow-import-",
		"ctr --namespace=k8s.io images import --snapshotter=overlayfs /var/cloudburrow-import-",
		"docker exec node-1 rm -f /var/cloudburrow-import-",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, " -i ") || strings.Contains(joined, "import --snapshotter=overlayfs -\n") {
		t.Errorf("streamed the archive through docker exec:\n%s", joined)
	}
}

// A copy short of the archive's size is an error before ctr reads it.
func TestImportArchiveRefusesAShortCopy(t *testing.T) {
	r := &nodeRunner{lose: 3}
	err := (&Loader{ClusterName: "c", Runner: r}).ImportArchive(context.Background(), archiveFile(t), testRef)
	if err == nil || !strings.Contains(err.Error(), "holds 13 bytes of its 16") {
		t.Fatalf("err = %v, want the short copy named", err)
	}
	if r.ran("docker exec --privileged node-1 ctr") != 0 {
		t.Errorf("imported a short copy:\n%s", strings.Join(r.calls, "\n"))
	}
	if r.ran("docker exec node-1 rm -f /var/cloudburrow-import-") != 1 {
		t.Errorf("left the short copy in the node:\n%s", strings.Join(r.calls, "\n"))
	}
}

// The CRI lists an image from containerd's events, after ctr's import
// returns: the import waits for it.
func TestImportArchiveWaitsForTheCRI(t *testing.T) {
	r := &nodeRunner{criLag: 3}
	if err := (&Loader{ClusterName: "c", Runner: r}).ImportArchive(context.Background(), archiveFile(t), testRef); err != nil {
		t.Fatal(err)
	}
}

// An image the CRI never lists is an error that says whether ctr has it
// and what its import printed.
func TestImportArchiveReportsAnImageTheCRILacks(t *testing.T) {
	old := criSync
	criSync = 600 * time.Millisecond
	t.Cleanup(func() { criSync = old })
	r := &nodeRunner{criLag: 1 << 30}
	err := (&Loader{ClusterName: "c", Runner: r}).ImportArchive(context.Background(), archiveFile(t), testRef)
	if err == nil {
		t.Fatal("no error for an image the node's runtime lacks")
	}
	for _, want := range []string{"does not have " + testRef, "ctr lists it", "unpacking ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q in it", err, want)
		}
	}
}
