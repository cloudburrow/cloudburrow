package tfconvert

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A caller that gives up on the converter image's build leaves it running,
// and the next caller gets the image it built, without a second build
// (main run 44357900271: the build was killed with each request at the
// harness's 60 s deadline, so no CREATE MODEL … TENSORFLOW ever ran).
func TestDockerBuildOutlivesTheRequestThatStartedIt(t *testing.T) {
	dir := t.TempDir()
	builds := filepath.Join(dir, "builds")
	built := filepath.Join(dir, "built")
	// A docker that has no image until a build, which takes a second.
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"image) [ -f '" + built + "' ] ;;\n" +
		"build) cat >/dev/null; echo x >> '" + builds + "'; sleep 1; : > '" + built + "' ;;\n" +
		"*) exit 2 ;;\n" +
		"esac\n"
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Docker{Image: "cb/tf:test", Docker: bin, Context: func() (io.Reader, error) { return bytes.NewReader(nil), nil }}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var rf *Refusal
	if err := d.ensure(ctx); !errors.As(err, &rf) || rf.Kind != "unavailable" || !strings.Contains(rf.Message, "still being built") {
		t.Fatalf("a caller that gave up: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := d.ensure(ctx2); err != nil {
		t.Fatalf("the next caller: %v", err)
	}
	if b, _ := os.ReadFile(builds); string(b) != "x\n" {
		t.Fatalf("builds %q, want exactly one", b)
	}
	if err := d.ensure(context.Background()); err != nil {
		t.Fatalf("once built: %v", err)
	}
}

// Warm starts the build without waiting for it.
func TestDockerWarmDoesNotWait(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$1\" in image) exit 1 ;; build) cat >/dev/null; sleep 1 ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Docker{Image: "cb/tf:test", Docker: bin, Context: func() (io.Reader, error) { return bytes.NewReader(nil), nil }}
	began := time.Now()
	d.Warm()
	if time.Since(began) > 500*time.Millisecond {
		t.Fatalf("Warm waited %s", time.Since(began))
	}
	if err := d.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
}
