package hostmount

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalComputeBindsThePath(t *testing.T) {
	SetRemote(nil)
	dir := t.TempDir()
	m, err := Bind(context.Background(), dir, "/model", ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.Args(), " "); got != "-v "+dir+":/model:ro" {
		t.Fatalf("args %q", got)
	}
	w, _ := Bind(context.Background(), dir, "/out", ReadWrite)
	if got := strings.Join(w.Args(), " "); got != "-v "+dir+":/out" {
		t.Fatalf("args %q", got)
	}
	if err := w.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Release()
}

func TestVolumeNames(t *testing.T) {
	a := volumeName("lab", "/x", false, false)
	if a != volumeName("lab", "/x", false, false) || a == volumeName("lab", "/x", true, false) || a == volumeName("other", "/x", false, false) {
		t.Fatal("a path's volume is not one per instance, path and writability")
	}
	if !strings.HasPrefix(a, "cb-mount-lab-") || volumeName("lab", "/x", false, true) == volumeName("lab", "/x", false, true) {
		t.Fatalf("names %s", a)
	}
}

func TestWithinRefusesEscapes(t *testing.T) {
	for _, n := range []string{"../x", "/../../etc/passwd", "."} {
		if rel, _, ok := within("/root", n); ok && strings.HasPrefix(rel, "..") {
			t.Fatalf("%q escapes as %q", n, rel)
		}
	}
	if rel, dst, ok := within("/root", "a/../b"); !ok || rel != "b" || dst != "/root/b" {
		t.Fatalf("%v %v %v", rel, dst, ok)
	}
}

// dockerWith skips unless Docker answers and holds image, which the test
// never pulls.
func dockerWith(t *testing.T, image string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("%s is not on this Docker; the test does not pull it", image)
	}
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestVolumeCarriesThePath runs the remote path against this machine's own
// Docker: the volume flow is the same docker CLI over whichever socket
// DOCKER_HOST names (the stand-in test runs it over SSH).
func TestVolumeCarriesThePath(t *testing.T) {
	const image = "busybox:latest"
	dockerWith(t, image)
	instance := "hmtest-" + randomHex(3)
	SetRemote(&Config{Instance: instance, StateDir: t.TempDir(), Helper: image})
	defer SetRemote(nil)
	var volumes []string
	defer func() {
		for _, v := range volumes {
			_ = exec.Command("docker", "volume", "rm", "-f", v).Run()
		}
	}()
	ctx := context.Background()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "a"), []byte("one"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "sub", "b"), []byte("two"), 0o644))
	must(t, os.Symlink("a", filepath.Join(dir, "link")))

	ro, err := Bind(ctx, dir, "/model", ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	volumes = append(volumes, ro.Volume())
	cat := func(m *Mount, user string, files ...string) string {
		args := append([]string{"run", "--rm", "--network", "none", "--user", user}, m.Args()...)
		args = append(args, image, "cat")
		return run(t, append(args, files...)...)
	}
	// A 0600 file is readable by the container's user, as a bind mount of
	// the user's own file would be.
	if got := cat(ro, "65534", "/model/a", "/model/sub/b", "/model/link"); got != "onetwoone" {
		t.Fatalf("read %q", got)
	}
	labels := run(t, "volume", "inspect", "--format", "{{json .Labels}}", ro.Volume())
	if !strings.Contains(labels, `"io.cloudburrow.instance":"`+instance+`"`) {
		t.Fatalf("labels %s", labels)
	}

	// A change and a removal: only those reach the volume.
	must(t, os.WriteFile(filepath.Join(dir, "a"), []byte("ONE!"), 0o600))
	must(t, os.Remove(filepath.Join(dir, "sub", "b")))
	ro2, err := Bind(ctx, dir, "/model", ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if ro2.Volume() != ro.Volume() {
		t.Fatal("the path's volume changed")
	}
	args := append([]string{"run", "--rm", "--network", "none"}, ro2.Args()...)
	if got := run(t, append(args, image, "sh", "-c", "cat /model/a; echo; ls /model/sub | wc -l; touch /model/x 2>/dev/null || echo ro")...); got != "ONE!\n0\nro" {
		t.Fatalf("after the change %q", got)
	}

	// A writable scratch mount: the container's writes come back, and what
	// it removed goes.
	work := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(work, "out"), 0o755))
	must(t, os.WriteFile(filepath.Join(work, "in"), []byte("job"), 0o644))
	must(t, os.WriteFile(filepath.Join(work, "stale"), []byte("x"), 0o644))
	rw, err := Scratch(ctx, work, "/cb", ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	volumes = append(volumes, rw.Volume())
	args = append([]string{"run", "--rm", "--network", "none", "--user", "185"}, rw.Args()...)
	run(t, append(args, image, "sh", "-c", "cat /cb/in > /cb/out/params.json && mkdir -p /cb/out/deep && echo d > /cb/out/deep/f && rm /cb/stale")...)
	must(t, rw.Collect(ctx))
	if b, err := os.ReadFile(filepath.Join(work, "out", "params.json")); err != nil || string(b) != "job" {
		t.Fatalf("collected %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(work, "out", "deep", "f")); err != nil || string(b) != "d\n" {
		t.Fatalf("collected %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(work, "stale")); !os.IsNotExist(err) {
		t.Fatalf("a file the container removed is still here: %v", err)
	}
	rw.Release()
	if exec.Command("docker", "volume", "inspect", rw.Volume()).Run() == nil {
		t.Fatal("Release left the scratch volume")
	}

	// A file, mounted by itself.
	video := filepath.Join(t.TempDir(), "video")
	must(t, os.WriteFile(video, []byte("frames"), 0o644))
	f, err := Bind(ctx, video, "/in/video", ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	volumes = append(volumes, f.Volume())
	if got := cat(f, "65534:65534", "/in/video"); got != "frames" {
		t.Fatalf("file %q", got)
	}

	// A volume the manifest is not about (a new host) is filled in full.
	run(t, "volume", "rm", ro.Volume())
	ro3, err := Bind(ctx, dir, "/model", ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if got := cat(ro3, "0", "/model/a"); got != "ONE!" {
		t.Fatalf("refilled %q", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
