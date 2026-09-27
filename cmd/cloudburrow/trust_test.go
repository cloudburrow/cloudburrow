package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trustDir makes a fresh working directory and home for the trust gate, with
// no CLOUDBURROW_* setting from the environment the tests run in.
func trustDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"CONFIG", "HOOKS_DIR", "STATE_DIR", "BIND_ADDRESS", "ALLOW_REMOTE"} {
		t.Setenv("CLOUDBURROW_"+k, "")
	}
	return dir
}

// TestUpRefusesAnUntrustedHookUntilTrusted is the acceptance case of #598:
// a fresh directory with .cloudburrow/hooks/ready.d/x.sh and no trust record
// is refused before anything is created, naming the script; once trusted it
// passes; an edit asks again.
func TestUpRefusesAnUntrustedHookUntilTrusted(t *testing.T) {
	dir := trustDir(t)
	hooksDir := filepath.Join(dir, ".cloudburrow", "hooks")
	hookScript(t, hooksDir, "ready.d", "x.sh", "echo hi")
	hookScript(t, hooksDir, "ready.d", "notes.txt", "")
	_ = os.Chmod(filepath.Join(hooksDir, "ready.d", "notes.txt"), 0o644)

	// Through run, as `cloudburrow up` would be: the gate refuses before
	// runUp, so no cluster is created. The missing seed file is a backstop:
	// were the gate to pass, runUp would still stop before creating one.
	err := run([]string{"up", "--name", "trust-gate-test", "--seed-file", filepath.Join(dir, "absent.json")}, io.Discard, io.Discard)
	if !errors.Is(err, errUntrusted) {
		t.Fatalf("up in an untrusted directory = %v, want the trust refusal", err)
	}
	for _, want := range []string{filepath.Join("ready.d", "x.sh"), "--trust", "cloudburrow trust"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "notes.txt") {
		t.Errorf("a file that would not run was listed:\n%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".cloudburrow", "trust-gate-test")); statErr == nil {
		t.Error("the refused up created its instance directory")
	}

	var out strings.Builder
	if err := runTrust([]string{"--name", "trust-gate-test"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "x.sh") {
		t.Errorf("trust did not list what it trusted:\n%s", out.String())
	}
	if err := gateUp([]string{"--name", "trust-gate-test"}, false, io.Discard, io.Discard); err != nil {
		t.Errorf("a trusted directory was refused: %v", err)
	}

	hookScript(t, hooksDir, "ready.d", "x.sh", "echo changed")
	err = gateUp([]string{"--name", "trust-gate-test"}, false, io.Discard, io.Discard)
	if !errors.Is(err, errUntrusted) || !strings.Contains(err.Error(), "changed") {
		t.Errorf("an edited hook was not refused as changed: %v", err)
	}
	// up --trust records it and goes on.
	if err := gateUp([]string{"--name", "trust-gate-test"}, true, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := gateUp([]string{"--name", "trust-gate-test"}, false, io.Discard, io.Discard); err != nil {
		t.Errorf("after --trust the directory was refused: %v", err)
	}
}

func TestUpRefusesAnUntrustedConfigFile(t *testing.T) {
	dir := trustDir(t)
	if err := os.WriteFile(filepath.Join(dir, "cloudburrow.json"), []byte(`{"logLevel":"warn"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := gateUp(nil, false, io.Discard, io.Discard)
	if !errors.Is(err, errUntrusted) || !strings.Contains(err.Error(), "cloudburrow.json") {
		t.Fatalf("an untrusted cloudburrow.json = %v", err)
	}
	// Named with --config, it is the developer's own choice.
	if err := gateUp([]string{"--config", "cloudburrow.json"}, false, io.Discard, io.Discard); err != nil {
		t.Errorf("an explicitly named config file needed trust: %v", err)
	}
}

// A hooks directory the developer names, as CI does with --hooks-dir, needs
// no trust record.
func TestANamedHooksDirectoryNeedsNoTrust(t *testing.T) {
	dir := trustDir(t)
	hookScript(t, filepath.Join(dir, "fixtures"), "ready.d", "x.sh", "echo hi")
	hookScript(t, filepath.Join(dir, ".cloudburrow", "hooks"), "ready.d", "y.sh", "echo hi")
	if err := gateUp([]string{"--hooks-dir", "fixtures"}, false, io.Discard, io.Discard); err != nil {
		t.Errorf("--hooks-dir needed trust: %v", err)
	}
	t.Setenv("CLOUDBURROW_HOOKS_DIR", "fixtures")
	if err := gateUp(nil, false, io.Discard, io.Discard); err != nil {
		t.Errorf("CLOUDBURROW_HOOKS_DIR needed trust: %v", err)
	}
}

func TestNothingToTrustPasses(t *testing.T) {
	trustDir(t)
	if err := gateUp(nil, false, io.Discard, io.Discard); err != nil {
		t.Errorf("an empty directory was refused: %v", err)
	}
	var out strings.Builder
	if err := runTrust(nil, &out, io.Discard); err != nil || !strings.Contains(out.String(), "nothing here needs trust") {
		t.Errorf("trust in an empty directory: %q %v", out.String(), err)
	}
}

func TestTrustFlag(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want bool
		rest string
	}{
		{[]string{"--name", "x"}, false, "--name,x"},
		{[]string{"--trust", "--name", "x"}, true, "--name,x"},
		{[]string{"-trust=false", "--detach"}, false, "--detach"},
	} {
		got, rest, err := trustFlag(c.in)
		if err != nil || got != c.want || strings.Join(rest, ",") != c.rest {
			t.Errorf("trustFlag(%v) = %v %v %v", c.in, got, rest, err)
		}
	}
	if _, _, err := trustFlag([]string{"--trust=maybe"}); err == nil {
		t.Error("an invalid -trust value was accepted")
	}
}
