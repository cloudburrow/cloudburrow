package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestTheTerraformProviderNeverPointsAtGoogle: whatever is enabled, the
// generated configuration sets endpoints only for Verified services, never a
// googleapis.com one, and names what it left out.
func TestTheTerraformProviderNeverPointsAtGoogle(t *testing.T) {
	all := []config.Service{config.ServiceStorage, config.ServicePubSub, config.ServiceTasks, config.ServiceSecrets, config.ServiceRun}
	for mask := 1; mask < 1<<len(all); mask++ {
		var services []config.Service
		for i, s := range all {
			if mask&(1<<i) != 0 {
				services = append(services, s)
			}
		}
		for _, override := range []bool{false, true} {
			cfg := config.Default()
			cfg.Services = services
			body, skipped := terraformProvider(cfg, override)
			if strings.Contains(body, "googleapis.com") || strings.Contains(body, "google.com") {
				t.Fatalf("%v: a Google endpoint in\n%s", services, body)
			}
			set := 0
			for _, e := range terraformEndpoints {
				in := strings.Contains(body, e.attr)
				if in && !e.verified {
					t.Errorf("%v: unverified %s was set", services, e.attr)
				}
				if in {
					set++
				}
			}
			// The Resource Manager rows are always set: the project registry
			// always runs, whichever services are enabled.
			if set+len(skipped) != len(services)+2 {
				t.Errorf("%v: %d set and %d named as skipped, want every enabled service accounted for", services, set, len(skipped))
			}
			if override != strings.Contains(body, "credentials  = null") {
				t.Errorf("credentials is cleared only in an override (override=%v):\n%s", override, body)
			}
		}
	}
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub}
	body, _ := terraformProvider(cfg, false)
	for _, want := range []string{
		`storage_custom_endpoint = "http://127.0.0.1:9001/storage/v1/"`,
		`pubsub_custom_endpoint = "http://127.0.0.1:9002/v1/"`,
		`project      = "` + cfg.DefaultProject() + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("provider lacks %s:\n%s", want, body)
		}
	}
}

func TestTerraformArguments(t *testing.T) {
	ours, theirs, bin, err := terraformArgs([]string{"--name", "x", "--binary", "tofu", "--", "-chdir=infra", "apply", "-auto-approve"})
	if err != nil || strings.Join(ours, " ") != "--name x" || bin != "tofu" || strings.Join(theirs, " ") != "-chdir=infra apply -auto-approve" {
		t.Errorf("terraformArgs = %v %v %q %v", ours, theirs, bin, err)
	}
	if dir := moduleDir(theirs); dir != "infra" {
		t.Errorf("moduleDir = %q, want infra", dir)
	}
	// Without "--", every argument is terraform's.
	ours, theirs, bin, _ = terraformArgs([]string{"plan", "-out", "p"})
	if len(ours) != 0 || strings.Join(theirs, " ") != "plan -out p" || bin != "terraform" {
		t.Errorf("terraformArgs without -- = %v %v %q", ours, theirs, bin)
	}
	if dir := moduleDir([]string{"plan", "-chdir=late"}); dir != "." {
		t.Errorf("-chdir after the subcommand is not terraform's -chdir; got %q", dir)
	}
}

// fakeTerraform puts a terraform on PATH that shows the provider file it
// finds, then behaves as script says.
func fakeTerraform(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	body := "#!/bin/sh\nfor f in cloudburrow_providers*.tf; do echo \"== $f\"; cat \"$f\"; done\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func inModule(t *testing.T, mainTF string) string {
	t.Helper()
	dir := t.TempDir()
	if mainTF != "" {
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(mainTF), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The file is written for the run, passed to terraform, removed afterwards,
// and terraform's own exit status is the wrapper's.
func TestTheWrapperRemovesItsFileAndKeepsTheExitStatus(t *testing.T) {
	fakeTerraform(t, "exit 3")
	for _, c := range []struct {
		module, file string
	}{
		{"", "cloudburrow_providers.tf"},
		{"provider \"google\" {\n  project = \"real-project\"\n}\n", "cloudburrow_providers_override.tf"},
	} {
		dir := inModule(t, c.module)
		var out strings.Builder
		err := runTerraform([]string{"--state-dir", t.TempDir(), "--", "plan"}, &out, &strings.Builder{})
		var exit *exitError
		if !errors.As(err, &exit) || exit.code != 3 {
			t.Fatalf("returned %v, want terraform's exit status 3", err)
		}
		if !strings.Contains(out.String(), "== "+c.file) || !strings.Contains(out.String(), "storage_custom_endpoint") {
			t.Errorf("terraform did not see %s:\n%s", c.file, out.String())
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "cloudburrow_providers*")); len(left) != 0 {
			t.Errorf("left behind: %v", left)
		}
	}
}

func TestTheWrapperRefusesAFileItDidNotWrite(t *testing.T) {
	fakeTerraform(t, "exit 0")
	dir := inModule(t, "")
	mine := filepath.Join(dir, "cloudburrow_providers.tf")
	if err := os.WriteFile(mine, []byte("# someone's own file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runTerraform([]string{"--state-dir", t.TempDir(), "--", "plan"}, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("a file cloudburrow did not write was replaced")
	}
	if b, _ := os.ReadFile(mine); string(b) != "# someone's own file\n" {
		t.Errorf("the user's file was changed: %q", b)
	}
}

// Ctrl-C reaches the whole process group, as a terminal delivers it. The
// wrapper leaves terraform to stop itself, then removes the file and exits
// with terraform's status.
func TestTheWrapperRemovesItsFileOnSIGINT(t *testing.T) {
	fakeTerraform(t, "trap 'echo interrupted; exit 130' INT\necho started\nwhile :; do sleep 0.1; done")
	dir := inModule(t, "")
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "terraform", "--state-dir", t.TempDir(), "--", "apply")
	cmd.Env = append(os.Environ(), runCLIEnv+"=1")
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// exec copies the child's output from another goroutine, so the buffer
	// polled below must be safe to read while it is written.
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "started") {
		if time.Now().After(deadline) {
			t.Fatalf("terraform never started:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "cloudburrow_providers*")); len(left) != 1 {
		t.Fatalf("the provider file is not in place during the run: %v", left)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Errorf("exited %v, want terraform's 130\n%s", err, out.String())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "cloudburrow_providers*")); len(left) != 0 {
		t.Errorf("SIGINT left behind: %v", left)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
