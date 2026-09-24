package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func script(t *testing.T, dir, stage, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, stage), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stage, name), []byte("#!/bin/sh\n"+body+"\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestAnAbsentDirectoryRunsNothing(t *testing.T) {
	res, err := Run(context.Background(), filepath.Join(t.TempDir(), "none"), Ready, nil, time.Second, &strings.Builder{})
	if err != nil || len(res) != 0 {
		t.Errorf("Run on an absent directory = %v, %v", res, err)
	}
}

// Lexical order, a failure reported by name and status with the next script
// still run, the environment given, and output prefixed per script.
func TestScriptsRunInOrderAndAFailureDoesNotStopTheRest(t *testing.T) {
	dir := t.TempDir()
	script(t, dir, Ready, "20-second.sh", `echo "second sees $GREETING"`, 0o755)
	script(t, dir, Ready, "10-first.sh", "echo first; exit 1", 0o755)
	script(t, dir, Ready, "30-third.sh", "echo third >&2", 0o755)
	script(t, dir, Ready, "15-notes.txt", "never run", 0o644)

	var out strings.Builder
	res, err := Run(context.Background(), dir, Ready, []string{"GREETING=hello", "PATH=/usr/bin:/bin"}, 10*time.Second, &out)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range res {
		order = append(order, r.Name)
	}
	if strings.Join(order, ",") != "10-first.sh,15-notes.txt,20-second.sh,30-third.sh" {
		t.Errorf("order %v", order)
	}
	if res[0].ExitCode != 1 || res[0].OK() {
		t.Errorf("the failing script was reported as %+v", res[0])
	}
	if res[1].Skipped != "not executable" {
		t.Errorf("a non-executable file was %+v", res[1])
	}
	if !res[2].OK() || !res[3].OK() {
		t.Errorf("the scripts after a failure did not run: %+v %+v", res[2], res[3])
	}
	got := out.String()
	for _, want := range []string{
		"[ready.d/10-first.sh] first\n",
		"[ready.d/10-first.sh] failed with exit status 1",
		"[ready.d/15-notes.txt] skipped: not executable",
		"[ready.d/20-second.sh] second sees hello\n",
		"[ready.d/30-third.sh] third\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "never run") {
		t.Error("a non-executable file was run")
	}
}

// A script past its timeout is killed, and so is what it started, and the
// next script still runs.
func TestAScriptPastItsTimeoutIsKilledWithItsChildren(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "child-survived")
	// The child would write the marker after 2s; killing the group stops it.
	script(t, dir, Ready, "10-slow.sh", "(sleep 2; touch "+marker+") &\nsleep 30", 0o755)
	script(t, dir, Ready, "20-next.sh", "echo next", 0o755)

	start := time.Now()
	var out strings.Builder
	res, err := Run(context.Background(), dir, Ready, []string{"PATH=/usr/bin:/bin"}, 300*time.Millisecond, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].TimedOut || res[0].OK() {
		t.Errorf("the slow script was reported as %+v", res[0])
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the stage took %v; the timeout did not stop it", time.Since(start))
	}
	if !res[1].OK() {
		t.Errorf("the next script did not run: %+v", res[1])
	}
	if !strings.Contains(out.String(), "[ready.d/10-slow.sh] killed after its") {
		t.Errorf("the kill was not reported:\n%s", out.String())
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a child of the timed-out script survived the kill")
	}
}
