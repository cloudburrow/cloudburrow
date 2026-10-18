package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// envConnector runs `env --offline --format plain` with args and returns the
// connector variable, never the rest: the output holds credentials.
func envConnector(t *testing.T, args ...string) (string, bool) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := run(append([]string{"env", "--offline", "--format", "plain"}, args...), &stdout, &stderr); err != nil {
		t.Fatalf("env = %v\n%s", err, stderr.String())
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "CLOUDBURROW_CLOUDSQL_CONNECTOR_ENDPOINT="); ok {
			return v, true
		}
	}
	return "", false
}

// An instance started with --cloudsql-connector records the address, and
// `env` with only --name and --state-dir reports it (#1571); restarted at the
// default, the record goes, and so does the variable.
func TestEnvReportsTheRecordedCloudSQLConnector(t *testing.T) {
	dir := t.TempDir()
	base := []string{"--name", "conn-rec", "--state-dir", dir, "--services", "cloudsql-mysql"}
	load := func(extra ...string) config.Config {
		cfg, err := config.Load(config.Options{Args: append(append([]string{}, base...), extra...),
			Getenv: func(string) string { return "" }})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	if v, ok := envConnector(t, base...); ok {
		t.Fatalf("before any up, env exported the connector at %q", v)
	}

	if err := recordCloudSQLConnector(load("--cloudsql-connector", "127.0.0.1:23307")); err != nil {
		t.Fatal(err)
	}
	if v, _ := envConnector(t, base...); v != "127.0.0.1:23307" {
		t.Errorf("env without the flag: connector = %q, want the recorded 127.0.0.1:23307", v)
	}
	// A flag passed to env itself still wins.
	if v, _ := envConnector(t, append(base, "--cloudsql-connector", "127.0.0.2:3307")...); v != "127.0.0.2:3307" {
		t.Errorf("env --cloudsql-connector: connector = %q, want 127.0.0.2:3307", v)
	}

	if err := recordCloudSQLConnector(load()); err != nil {
		t.Fatal(err)
	}
	if v, ok := envConnector(t, base...); ok {
		t.Errorf("after an up at the default, env still exported the connector at %q", v)
	}
}
