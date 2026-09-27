package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// Cloud SQL for PostgreSQL has a fixed port (#584), so `env` — a separate
// process, here with no instance running — exports the libpq variables and a
// connection string, as it does MySQL's. It used to export nothing, and
// docs/cloudsql.md had the reader copy a new OS-assigned port after every
// restart.
func TestEnvOfflineExportsThePostgresVariables(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"env", "--offline", "--format", "plain", "--name", "pgoffline",
		"--state-dir", t.TempDir(), "--services", "cloudsql"}, &stdout, &stderr); err != nil {
		t.Fatalf("env --offline --services cloudsql = %v\n%s", err, stderr.String())
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	for name, want := range map[string]string{
		"PGHOST":                   "127.0.0.1",
		"PGPORT":                   "9019",
		"PGUSER":                   "cloudburrow",
		"PGDATABASE":               "cloudburrow",
		"PGSSLMODE":                "disable",
		"CLOUDBURROW_CLOUDSQL_URL": "postgres://cloudburrow@127.0.0.1:9019/cloudburrow?sslmode=disable",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	// Trust authentication: there is no password, so none is exported.
	if _, ok := got["PGPASSWORD"]; ok {
		t.Error("PGPASSWORD is exported, but the server has no password")
	}
	if strings.Contains(stderr.String(), "PGPORT") {
		t.Errorf("env reported PGPORT as unexportable at a fixed port:\n%s", stderr.String())
	}
}

// The shell and docker-compose forms, pinned. In the compose map the bare
// PGHOST and the host inside the URL are both rewritten: a container's
// loopback is the container.
func TestEnvCloudSQLGoldens(t *testing.T) {
	cfg := formatsConfig(t, "cloudsql")
	vars := envVars(cfg, cfg.DefaultProject(), "/host/only/adc.json")

	var shell bytes.Buffer
	writeShell(&shell, vars)
	envGolden(t, "cloudsql_shell", shell.Bytes())

	var compose bytes.Buffer
	writeCompose(&compose, cfg, vars)
	envGolden(t, "cloudsql_docker-compose", compose.Bytes())
	if strings.Contains(compose.String(), "127.0.0.1") {
		t.Errorf("the compose map kept a loopback address:\n%s", compose.String())
	}
	for _, want := range []string{`PGHOST: "host.docker.internal"`,
		`CLOUDBURROW_CLOUDSQL_URL: "postgres://cloudburrow@host.docker.internal:9019/cloudburrow?sslmode=disable"`} {
		if !strings.Contains(compose.String(), want) {
			t.Errorf("the compose map lacks %s:\n%s", want, compose.String())
		}
	}
}

// Nothing Postgres is exported unless cloudsql is enabled.
func TestPostgresVariablesOnlyWithCloudSQL(t *testing.T) {
	cfg := formatsConfig(t, "storage,pubsub,cloudsql-mysql")
	for _, v := range envVars(cfg, cfg.DefaultProject(), "/tmp/adc.json") {
		if strings.HasPrefix(v.Name, "PG") || v.Name == "CLOUDBURROW_CLOUDSQL_URL" {
			t.Errorf("%s=%s exported without cloudsql enabled", v.Name, v.Value)
		}
	}
}

// `up` binds the port `env` exports: the tunnel's host address is the
// configured one, before it starts.
func TestUpAndEnvAgreeOnTheCloudSQLPort(t *testing.T) {
	cfg := formatsConfig(t, "cloudsql")
	var tunnel string
	for _, f := range buildForwarders(cfg) {
		if f.Name() == "forward:cloudsql" {
			tunnel = f.HostAddr()
		}
	}
	if tunnel != "127.0.0.1:9019" {
		t.Errorf("cloudsql tunnel host address = %q, want 127.0.0.1:9019", tunnel)
	}
}

// --port-cloudsql 0 still means OS-assigned: nothing is exported, and env
// says why, naming the variable it left out.
func TestAnOSAssignedCloudSQLPortIsNotExported(t *testing.T) {
	cfg := formatsConfig(t, "cloudsql")
	cfg.Endpoints.CloudSQL = 0
	for _, v := range envVars(cfg, cfg.DefaultProject(), "/tmp/adc.json") {
		if strings.HasPrefix(v.Name, "PG") || v.Name == "CLOUDBURROW_CLOUDSQL_URL" {
			t.Errorf("exported %s=%q for a port only `up` knows", v.Name, v.Value)
		}
	}
	if got := unexportableEmulators(cfg); len(got) != 1 || got[0] != config.ServiceCloudSQL {
		t.Errorf("unexportableEmulators = %v, want [cloudsql]", got)
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"env", "--offline", "--name", "pgzero", "--state-dir", t.TempDir(),
		"--services", "cloudsql", "--port-cloudsql", "0"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "PGPORT is not exported") || !strings.Contains(stderr.String(), "--port-cloudsql") {
		t.Errorf("stderr does not explain the missing PGPORT:\n%s", stderr.String())
	}
}

// status shows the configured address, not "OS-assigned".
func TestStatusShowsTheCloudSQLPort(t *testing.T) {
	var out bytes.Buffer
	printConfiguredEndpoints(&out, formatsConfig(t, "cloudsql"))
	s := out.String()
	if !strings.Contains(s, "cloudsql         127.0.0.1:9019") {
		t.Errorf("status endpoints do not show cloudsql at 127.0.0.1:9019:\n%s", s)
	}
	if strings.Contains(s, "OS-assigned") {
		t.Errorf("status still calls the cloudsql port OS-assigned:\n%s", s)
	}
}

// doctor and `up`'s preflight check the Cloud SQL port when it is enabled,
// where --port-base moved it, and not otherwise.
func TestHostPortsIncludeCloudSQL(t *testing.T) {
	if got := hostPorts(loadForTest(t, "--services", "cloudsql"))["cloudsql"]; got != 9019 {
		t.Errorf("cloudsql port = %d, want 9019", got)
	}
	cfg := loadForTest(t, "--services", "storage,cloudsql", "--port-base", "9200")
	if got := hostPorts(cfg)["cloudsql"]; got != 9219 {
		t.Errorf("cloudsql port at --port-base 9200 = %d, want 9219", got)
	}
	if got := doctorOptions(cfg).Ports["cloudsql"]; got != 9219 {
		t.Errorf("doctor's cloudsql port at --port-base 9200 = %d, want 9219", got)
	}
	if _, ok := hostPorts(loadForTest(t, "--services", "storage"))["cloudsql"]; ok {
		t.Error("cloudsql is checked but not enabled")
	}
	if _, ok := hostPorts(loadForTest(t, "--services", "cloudsql", "--port-cloudsql", "0"))["cloudsql"]; ok {
		t.Error("an OS-assigned cloudsql port is checked")
	}
}
