package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
)

// doctor checks the ports `up` binds, derived from the configuration's
// endpoints: the console and Resource Manager always, an opt-in service's only
// when it is enabled, and the local generation endpoint only with a model. It
// once checked a hand-kept eight, so a taken console port passed (#587).
func TestDoctorChecksEveryPortUpBinds(t *testing.T) {
	cfg, err := config.Load(config.Options{
		Args:   []string{"--name", "doctorports", "--state-dir", t.TempDir(), "--services", "storage,scheduler,logging,firestore,bigquery"},
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	ports := doctorOptions(cfg).Ports
	for _, want := range []string{"control", "metadata", "ingress", "console", "resourcemanager", "storage",
		"scheduler", "logging", "firestore", "bigquery", "bigquery-storage"} {
		if _, ok := ports[want]; !ok {
			t.Errorf("doctor does not check the %s port; checks %v", want, ports)
		}
	}
	for _, not := range []string{"pubsub", "tasks", "run", "secrets", "kms", "datastore", "bigtable", "spanner",
		"memorystore", "cloudsql-mysql", "localai"} {
		if _, ok := ports[not]; ok {
			t.Errorf("doctor checks the %s port, which `up` does not bind here", not)
		}
	}
	if ports["console"] != cfg.Endpoints.Console || ports["scheduler"] != cfg.Endpoints.Scheduler {
		t.Errorf("ports = %v, want the configured ones", ports)
	}

	model := filepath.Join(t.TempDir(), "m.litertlm")
	if err := os.WriteFile(model, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withModel, err := config.Load(config.Options{
		Args:   []string{"--name", "doctorports", "--state-dir", t.TempDir(), "--local-ai-model", model},
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doctorOptions(withModel).Ports["localai"]; !ok {
		t.Error("doctor does not check the localai port with a model configured")
	}
}

// A held console port fails the port check by name.
func TestDoctorFailsATakenConsolePort(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port
	cfg, err := config.Load(config.Options{
		Args:   append([]string{"--state-dir", t.TempDir(), "--port-console", strconv.Itoa(port)}, osAssignedPortsBut("console")...),
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	report := doctor.Ports(doctor.RealEnv(), doctorOptions(cfg))
	var out bytes.Buffer
	report.Write(&out)
	if !report.Blocking() || !regexp.MustCompile(`FAIL\s+port console\s+127\.0\.0\.1:`+strconv.Itoa(port)+` is in use`).MatchString(out.String()) {
		t.Errorf("report with the console port held:\n%s", out.String())
	}
}

// `up` refuses a taken port before it creates anything, with doctor's report,
// rather than finding it when that listener starts after the cluster exists.
func TestUpRefusesATakenPortBeforeCreatingACluster(t *testing.T) {
	t.Parallel()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	// Resource Manager is bound whatever services are enabled, and is not
	// the first listener to start, so before this it failed only after the
	// cluster had been created.
	dir := t.TempDir()
	args := append([]string{"--name", "preflight", "--state-dir", dir, "--port-resourcemanager", strconv.Itoa(port)},
		osAssignedPortsBut("resourcemanager")...)
	var stderr bytes.Buffer
	start := time.Now()
	err = runUp(context.Background(), args, io.Discard, &stderr)
	if err == nil {
		t.Fatal("runUp() = nil, want a refusal")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("refusing took %v", elapsed)
	}
	if !strings.Contains(err.Error(), "resourcemanager (127.0.0.1:"+strconv.Itoa(port)+")") {
		t.Errorf("error = %v, want it to name the port", err)
	}
	if !strings.Contains(stderr.String(), "FAIL    port resourcemanager") {
		t.Errorf("stderr has no doctor report:\n%s", stderr.String())
	}
	// Nothing was created: not even the kind configuration the cluster is
	// made from.
	if _, err := os.Stat(filepath.Join(dir, "preflight", cluster.ConfigFileName)); !os.IsNotExist(err) {
		t.Errorf("a cluster configuration was written: %v", err)
	}
}

// osAssignedPortsBut asks the OS to choose every port but one, so a test is
// never affected by what else is running on the machine.
func osAssignedPortsBut(keep string) []string {
	var args []string
	for _, np := range (config.Endpoints{}).Named() {
		if np.Name != keep && np.Name != "localai" {
			args = append(args, "--port-"+np.Name, "0")
		}
	}
	return args
}
