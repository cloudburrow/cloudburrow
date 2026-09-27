package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// A name with no running instance has no endpoints. env used to print the
// configured ports anyway, and those are the defaults, so with another
// instance listening there `eval "$(cloudburrow env --name typo)"` pointed
// clients at it; a test expecting no endpoints wrote to the developer's
// running instance that way (#630). It also created the unknown instance's
// directory and credentials. Now it fails, prints nothing an eval would run,
// and creates nothing.
func TestEnvRefusesAnInstanceThatIsNotRunning(t *testing.T) {
	// Something is listening where the configuration says Storage is, as
	// another instance at the defaults would be.
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	port := strconv.Itoa(other.Addr().(*net.TCPAddr).Port)

	dir := t.TempDir()
	args := []string{"env", "--name", "nobody", "--state-dir", dir, "--port-storage", port}
	for _, format := range []string{"shell", "json", "plain", "terraform", "docker-compose"} {
		var stdout, stderr bytes.Buffer
		err := run(append(append([]string{}, args...), "--format", format), &stdout, &stderr)
		if err == nil {
			t.Errorf("--format %s: env succeeded for an instance that is not running:\n%s", format, stdout.String())
		}
		if err != nil && !strings.Contains(err.Error(), "not running") {
			t.Errorf("--format %s: error %q does not say the instance is not running", format, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("--format %s: env printed on stdout for an instance that is not running:\n%s", format, stdout.String())
		}
	}

	cfg, err := config.Load(config.Options{Args: []string{"--name", "nobody", "--state-dir", dir}, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.InstanceDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("env created %s for an instance that is not running (stat: %v)", cfg.InstanceDir(), err)
	}
}

// --offline is the explicit way to get the configured endpoints before `up`,
// for generating files. It prints them and says nothing is running. As a
// boolean it must not swallow the argument after it.
func TestEnvOfflinePrintsTheConfiguredEndpoints(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"env", "--offline", "--format", "plain", "--name", "later", "--state-dir", dir,
		"--port-storage", "9711"}, &stdout, &stderr); err != nil {
		t.Fatalf("env --offline = %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "STORAGE_EMULATOR_HOST=http://127.0.0.1:9711") {
		t.Errorf("env --offline did not print the configured storage port:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"env", "--offline=false", "--name", "later", "--state-dir", dir}, &stdout, &stderr); err == nil {
		t.Errorf("env --offline=false succeeded without a running instance:\n%s", stdout.String())
	}
}

// A running instance's env prints the ports it recorded, not the configured
// ones. The runtime file names this test process, whose name contains
// "cloudburrow" as a live `up` does, so the check treats it as running.
func TestEnvPrintsARunningInstancesLivePorts(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(config.Options{Args: []string{"--name", "live", "--state-dir", dir}, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if !isCloudBurrow(os.Getpid()) {
		t.Skipf("this test binary's name does not read as cloudburrow, so it cannot stand in for up")
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	info := runtimeInfo{PID: os.Getpid(), Endpoints: map[string]string{"storage": "127.0.0.1:9722"}}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"env", "--format", "plain", "--name", "live", "--state-dir", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("env for a running instance = %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "STORAGE_EMULATOR_HOST=http://127.0.0.1:9722") {
		t.Errorf("env did not print the running instance's storage port:\n%s", stdout.String())
	}
}
