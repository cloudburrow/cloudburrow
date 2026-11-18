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

// Build (prefetch's) waits for the image a build makes.
func TestDockerBuildWaits(t *testing.T) {
	dir := t.TempDir()
	built := filepath.Join(dir, "built")
	bin := filepath.Join(dir, "docker")
	script := "#!/bin/sh\ncase \"$1\" in image) [ -f '" + built + "' ] ;; build) cat >/dev/null; sleep 1; : > '" + built + "' ;; esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Docker{Image: "cb/tf:test", Docker: bin, Context: func() (io.Reader, error) { return bytes.NewReader(nil), nil }}
	if err := d.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(built); err != nil {
		t.Fatalf("Build returned before the image was built: %v", err)
	}
}
