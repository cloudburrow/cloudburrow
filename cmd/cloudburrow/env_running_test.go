package main

import (
	"bytes"
	"context"
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
	for _, format := range []string{"shell", "json", "plain", "terraform", "docker-compose", "kubernetes"} {
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
// ones. The runtime file names this test process and its start time, as a
// live `up`'s does, so the check treats it as running.
func TestEnvPrintsARunningInstancesLivePorts(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(config.Options{Args: []string{"--name", "live", "--state-dir", dir}, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	info := runtimeInfo{PID: os.Getpid(), ProcStart: selfStart(t), Endpoints: map[string]string{"storage": "127.0.0.1:9722"}}
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

// A running instance's own services decide what env exports, not env's flags.
// `env --name x` without the --services up was given used to export every
// default service, and the ones the instance never started kept their
// default ports: another instance's (#652).
func TestEnvExportsOnlyTheServicesARunningInstanceServes(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(config.Options{Args: []string{"--name", "only-scheduler", "--state-dir", dir}, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	info := runtimeInfo{PID: os.Getpid(), ProcStart: selfStart(t), Services: []config.Service{config.ServiceScheduler},
		Endpoints: map[string]string{"scheduler": "127.0.0.1:9733"}}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"env", "--format", "plain", "--name", "only-scheduler", "--state-dir", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("env = %v\n%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "CLOUDBURROW_SCHEDULER_ENDPOINT=127.0.0.1:9733") {
		t.Errorf("env did not export the running instance's Scheduler endpoint:\n%s", out)
	}
	for _, v := range []string{"STORAGE_EMULATOR_HOST", "PUBSUB_EMULATOR_HOST", "CLOUDBURROW_TASKS_ENDPOINT",
		"CLOUDBURROW_SECRETMANAGER_ENDPOINT", "CLOUDBURROW_KMS_ENDPOINT"} {
		if strings.Contains(out, v+"=") {
			t.Errorf("env exported %s for an instance that does not serve it:\n%s", v, out)
		}
	}
}

// An instance started with --port-base publishes its ingress at base+80, and
// `env --name x`, run without that --port-base, used to export the default
// 9080 for it: another instance's port. The console's Connect page, served
// by `up` with the real configuration, gave base+80, so the two disagreed
// (#863). The runtime file now records the ingress, and env, the page and
// `status` all report the one it records.
func TestEnvReportsTheIngressOfAnInstanceAtAPortBase(t *testing.T) {
	dir := t.TempDir()
	// The configuration `up --port-base 62700` ran with, and the addresses
	// it records for it.
	served, err := config.Load(config.Options{Args: []string{"--name", "based", "--state-dir", dir,
		"--services", "storage", "--port-base", "62700"}, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	host := func(port int) string { return net.JoinHostPort(served.BindAddress, strconv.Itoa(port)) }
	ingress := host(served.Endpoints.Ingress)
	if served.Endpoints.Ingress != 62780 {
		t.Fatalf("the ingress at --port-base 62700 = %d, want 62780", served.Endpoints.Ingress)
	}
	if err := os.MkdirAll(served.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	info := runtimeInfo{PID: os.Getpid(), ProcStart: selfStart(t), Services: served.EnabledServices(),
		Control: host(served.Endpoints.Control), Endpoints: map[string]string{
			"control": host(served.Endpoints.Control), "metadata": host(served.Endpoints.Metadata),
			"storage": host(served.Endpoints.Storage), "resourcemanager": host(served.Endpoints.ResourceManager),
			"console": host(served.Endpoints.Console), "ingress": ingress,
		}}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath(served), b, 0o600); err != nil {
		t.Fatal(err)
	}

	// env is given only the name, as scripts/compat-env.sh and a developer
	// give it.
	args := []string{"--name", "based", "--state-dir", dir}
	var env map[string]string
	if err := json.Unmarshal([]byte(runEnvFormat(t, args, "json")), &env); err != nil {
		t.Fatalf("env --format json is not JSON: %v", err)
	}
	if got, want := env["CLOUDBURROW_INGRESS"], "http://"+ingress; got != want {
		t.Errorf("CLOUDBURROW_INGRESS = %q, want the instance's %q", got, want)
	}
	for name, v := range env {
		if strings.Contains(v, ":90") {
			t.Errorf("%s = %q names a default port, not the instance's 627xx block", name, v)
		}
	}

	// The Connect page, from `up`'s own configuration, agrees.
	page, err := consoleConnect{cfg: served}.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range page.Variables {
		if env[v.Name] != v.Value {
			t.Errorf("the page's %s = %q; env --format json prints %q", v.Name, v.Value, env[v.Name])
		}
	}

	// And so does status, from a configuration without the port base.
	named, err := config.Load(config.Options{Args: args, Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := buildStatusReport(named, &liveState{info: info}, "running", "")
	if want := "http://" + ingress; r.IngressURL != want {
		t.Errorf("status ingress_url = %q, want the instance's %q", r.IngressURL, want)
	}
}
