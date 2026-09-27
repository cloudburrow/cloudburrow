package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

func loadForTest(t *testing.T, args ...string) config.Config {
	t.Helper()
	cfg, err := config.Load(config.Options{
		Args:   append([]string{"--state-dir", t.TempDir()}, args...),
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Every address `env` exports for an instance at --port-base 9100 is in the
// 91xx block, the ingress included, so it can run beside a default instance
// (#584).
func TestEnvAtAPortBaseExportsOnlyThatBlock(t *testing.T) {
	cfg := loadForTest(t, "--name", "beta", "--port-base", "9100",
		"--services", "storage,pubsub,tasks,run,secretmanager,kms,scheduler,logging,firestore,datastore,bigtable,spanner,bigquery,memorystore,cloudsql-mysql,cloudsql")
	vars := envVars(cfg, cfg.DefaultProject(), "/tmp/adc.json")

	port := regexp.MustCompile(`(?:127\.0\.0\.1|localhost):(\d+)`)
	sawIngress := false
	for _, v := range vars {
		if v.Name == "CLOUDBURROW_INGRESS" {
			sawIngress = true
		}
		for _, m := range port.FindAllStringSubmatch(v.Value, -1) {
			if n, _ := strconv.Atoi(m[1]); n < 9100 || n > 9199 {
				t.Errorf("%s=%s: port %d is outside the 91xx block", v.Name, v.Value, n)
			}
		}
	}
	if !sawIngress {
		t.Error("CLOUDBURROW_INGRESS is not exported")
	}
}

// The preflight names every taken port, with the flag that moves it, rather
// than the first one a component happens to bind.
func TestCheckHostPortsNamesEveryCollision(t *testing.T) {
	cfg := loadForTest(t, "--name", "beta")
	taken := map[int]bool{9000: true, 9003: true, 9080: true, 9090: true}
	free := func(_ string, port int) error {
		if taken[port] {
			return errors.New("address already in use")
		}
		return nil
	}

	err := checkHostPorts(cfg, free, func() bool { return false })
	if err == nil {
		t.Fatal("checkHostPorts = nil, want the taken ports named")
	}
	msg := err.Error()
	for _, want := range []string{
		"127.0.0.1:9000 (--port-control)", "127.0.0.1:9003 (--port-tasks)",
		"127.0.0.1:9080 (--port-ingress)", "127.0.0.1:9090 (--port-console)",
		"--port-base 9100",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "9001") {
		t.Errorf("error = %q names a free port", msg)
	}

	// The instance's own running cluster holds its ingress; that is not a
	// collision.
	taken = map[int]bool{9080: true}
	if err := checkHostPorts(cfg, free, func() bool { return true }); err != nil {
		t.Errorf("checkHostPorts with our own cluster on the ingress = %v, want nil", err)
	}
	if err := checkHostPorts(cfg, free, func() bool { return false }); err == nil {
		t.Error("checkHostPorts with someone else on the ingress = nil, want an error")
	}
}

// The ports checked are the ports bound: an opt-in service's port only when
// it is enabled, and never an OS-assigned one.
func TestHostPortsFollowTheEnabledServices(t *testing.T) {
	ports := hostPorts(loadForTest(t))
	for _, name := range []string{"control", "metadata", "resourcemanager", "console", "ingress",
		"storage", "pubsub", "tasks", "run", "secrets"} {
		if ports[name] == 0 {
			t.Errorf("default instance: %s is not checked", name)
		}
	}
	for _, name := range []string{"kms", "firestore", "cloudsql-mysql", "cloudsql", "localai"} {
		if _, ok := ports[name]; ok {
			t.Errorf("default instance: %s is checked but not enabled", name)
		}
	}

	ports = hostPorts(loadForTest(t, "--services", "storage,bigquery,cloudsql,scheduler", "--port-storage", "0"))
	if ports["bigquery"] != 9014 || ports["bigquery-storage"] != 9015 || ports["scheduler"] != 9008 {
		t.Errorf("ports = %v, want bigquery 9014, bigquery-storage 9015, scheduler 9008", ports)
	}
	if _, ok := ports["storage"]; ok {
		t.Error("an OS-assigned storage port is checked")
	}
	if _, ok := ports["pubsub"]; ok {
		t.Error("pubsub is checked but not enabled")
	}

	// doctor reports on the same set.
	cfg := loadForTest(t, "--port-base", "9200")
	if got, want := doctorOptions(cfg).Ports, hostPorts(cfg); len(got) != len(want) || got["console"] != 9290 {
		t.Errorf("doctor ports = %v, want %v", got, want)
	}
}
