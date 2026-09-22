package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// portFree must answer the question actually asked: can *we* bind it. A dial
// probe would answer a different one.
func TestPortFreeDetectsAHeldPort(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if err := portFree("127.0.0.1", port); err == nil {
		t.Errorf("port %d is held by this test but reported free", port)
	}
}

func TestPortFreeReleasesWhatItBinds(t *testing.T) {
	t.Parallel()

	// Retried, because the port is the part this test cannot control.
	//
	// It takes an ephemeral port, releases it, and checks portFree twice. The
	// first check can legitimately fail when something else on the machine
	// claims the port in between — the kernel hands ephemeral ports out to
	// whoever asks, and a busy CI runner asks constantly. That is not the
	// defect this test looks for, so it is retried rather than reported.
	//
	// The second check is the assertion, and it is never retried away: if
	// portFree held the port it bound, `up` would fail on its own probe.
	const attempts = 20
	for i := range attempts {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()

		if err := portFree("127.0.0.1", port); err != nil {
			if i == attempts-1 {
				t.Fatalf("a released port reported busy on every one of %d attempts; "+
					"the last was %v", attempts, err)
			}
			continue // someone else took it; try another port
		}
		// The probe must not still be holding it, or a second check of the
		// same port — or `up` itself — would fail.
		if err := portFree("127.0.0.1", port); err != nil {
			t.Errorf("portFree did not release the port it bound: %v", err)
		}
		return
	}
}

// A state directory may not exist yet. Reporting "unknown" for a question the
// filesystem can answer would waste the check.
func TestDiskFreeWalksUpToAnExistingDirectory(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "not", "created", "yet")

	free, measured, err := diskFree(missing)
	if err != nil {
		t.Fatalf("diskFree: %v", err)
	}
	if free == 0 {
		t.Error("free bytes = 0 on a real filesystem")
	}
	if !strings.HasPrefix(missing, measured) {
		t.Errorf("measured %q, which is not an ancestor of %q", measured, missing)
	}
}

func TestDiskFreeMeasuresAnExistingDirectoryDirectly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, measured, err := diskFree(dir)
	if err != nil {
		t.Fatalf("diskFree: %v", err)
	}
	if measured != dir {
		t.Errorf("measured %q, want the directory itself %q", measured, dir)
	}
}

func TestParentDirStopsAtTheRoot(t *testing.T) {
	t.Parallel()
	root := string(os.PathSeparator)
	if got := parentDir(root); got != root {
		t.Errorf("parentDir(%q) = %q, want the root itself so the walk terminates", root, got)
	}
	if got := parentDir(filepath.Join(root, "a", "b")); got != filepath.Join(root, "a") {
		t.Errorf("parentDir = %q", got)
	}
}

// A probe that hangs is worse than one that reports a failure, so the command
// carries its own bound.
func TestRunCommandIsBoundedEvenWithoutACallerDeadline(t *testing.T) {
	t.Parallel()
	if _, err := runCommand(context.Background(), "true"); err != nil {
		t.Fatalf("runCommand(true): %v", err)
	}
	if _, err := runCommand(context.Background(), "definitely-not-a-real-binary-xyz"); err == nil {
		t.Error("a missing binary reported success")
	}
}

func TestRealEnvIsFullyPopulated(t *testing.T) {
	t.Parallel()
	// A nil probe would panic mid-run rather than report a finding.
	e := RealEnv()
	if e.LookPath == nil || e.Run == nil || e.PortFree == nil ||
		e.DiskFree == nil || e.HomeDir == nil || e.GOOS == "" {
		t.Fatalf("RealEnv left a probe unset: %+v", e)
	}
}

// An unmeasurable filesystem must be reported as unknown, never as passing.
func TestUnmeasurableDiskIsUnknown(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.freeErr = errors.New("permission denied")
	r := Run(context.Background(), f.env(), Options{})

	got := find(t, r, "disk space")
	if got.Level != LevelUnknown {
		t.Errorf("unmeasurable disk = %s, want unknown", got.Level)
	}
	if !strings.Contains(got.Detail, "permission denied") {
		t.Errorf("the underlying cause was lost: %q", got.Detail)
	}
}

// With no home directory and a VM daemon there is nothing to measure at all.
func TestNoMeasurablePathIsUnknown(t *testing.T) {
	t.Parallel()
	e := healthy().env()
	e.HomeDir = func() (string, error) { return "", fmt.Errorf("no home") }
	e.GOOS = "darwin"
	r := Run(context.Background(), e, Options{})

	if got := find(t, r, "disk space").Level; got != LevelUnknown {
		t.Errorf("no measurable path = %s, want unknown", got)
	}
}

// `docker info` that exits non-zero but still prints JSON is a daemon that
// answered, not one that is down.
func TestDaemonThatAnsweredWithJSONIsNotReportedDown(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.dockerJS = `{"ServerVersion":"29.0.0"}`
	f.dockerErr = errors.New("exit status 1")
	r := Run(context.Background(), f.env(), Options{})

	got := find(t, r, "docker daemon")
	if got.Level == LevelFail {
		t.Errorf("a daemon that answered was reported down: %q", got.Detail)
	}
}

func TestFirstLineSkipsTheClientBanner(t *testing.T) {
	t.Parallel()
	out := "Client:\n Version: 29.0.0\n\nCannot connect to the Docker daemon.\n"
	if got := firstLine(out, errors.New("exit 1")); !strings.Contains(got, "Version") {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("", errors.New("exit 1")); got != "exit 1" {
		t.Errorf("empty output should fall back to the error: %q", got)
	}
}

func TestLevelStringsAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, l := range []Level{LevelOK, LevelWarn, LevelFail, LevelUnknown} {
		s := l.String()
		if seen[s] {
			t.Errorf("duplicate level string %q", s)
		}
		seen[s] = true
	}
}
