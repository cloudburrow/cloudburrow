package config

import (
	"strings"
	"testing"
)

// The Cloud SQL connector listener (#1364) defaults to the bind address at
// 3307, which --port-base does not move, and the flag, the variable and the
// file key each set it with the usual precedence.
func TestCloudSQLConnectorAddress(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := writeFile(t, dir, "cb.json", `{"cloudsqlConnector":"127.0.0.3:3307"}`)
	for _, c := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"default", nil, nil, "127.0.0.1:3307"},
		{"port base", []string{"--port-base", "9100"}, nil, "127.0.0.1:3307"},
		{"file", []string{"--config", file}, nil, "127.0.0.3:3307"},
		{"env", []string{"--config", file}, map[string]string{"CLOUDBURROW_CLOUDSQL_CONNECTOR": "127.0.0.2:3307"}, "127.0.0.2:3307"},
		{"flag", []string{"--config", file, "--cloudsql-connector", "127.0.0.1:23307"},
			map[string]string{"CLOUDBURROW_CLOUDSQL_CONNECTOR": "127.0.0.2:3307"}, "127.0.0.1:23307"},
	} {
		cfg, err := Load(Options{Args: c.args, Getenv: envMap(c.env), WorkDir: dir})
		if err != nil {
			t.Fatalf("%s: Load: %v", c.name, err)
		}
		if got := cfg.CloudSQLConnectorAddr(); got != c.want {
			t.Errorf("%s: CloudSQLConnectorAddr = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCloudSQLConnectorValidation(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]string{
		"127.0.0.2:3307": "",
		"[::1]:3307":     "",
		"127.0.0.1":      "must be host:port",
		"127.0.0.1:0":    "must be host:port",
		"localhost:3307": "IP address literal",
		"10.1.2.3:3307":  "non-loopback",
	} {
		cfg := Default()
		cfg.CloudSQLConnector = addr
		err := cfg.Validate()
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", addr, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", addr, err, want)
		}
	}
	cfg := Default()
	cfg.CloudSQLConnector, cfg.AllowRemote = "10.1.2.3:3307", true
	cfg.BindAddress = "10.1.2.3"
	if err := cfg.Validate(); err != nil {
		t.Errorf("--allow-remote: %v", err)
	}
}
