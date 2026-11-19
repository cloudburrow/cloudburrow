package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Microsoft's SQL Server licence (#1916) is accepted only by the user's
// own flag or environment variable, never by default, never by a
// configuration file, and never by an environment value other than Y.
func TestAcceptMSSQLEULA(t *testing.T) {
	dir := t.TempDir()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	load := func(args []string, e map[string]string) (Config, error) {
		return Load(Options{Args: args, Getenv: env(e), WorkDir: dir})
	}
	cfg, err := load(nil, nil)
	if err != nil || cfg.AcceptMSSQLEULA || cfg.Source.MSSQLEULA != "" {
		t.Fatalf("default: %v %v %q", err, cfg.AcceptMSSQLEULA, cfg.Source.MSSQLEULA)
	}
	if cfg, err = load([]string{"--accept-mssql-eula"}, nil); err != nil || !cfg.AcceptMSSQLEULA || cfg.Source.MSSQLEULA != "flag" {
		t.Errorf("flag: %v %v %q", err, cfg.AcceptMSSQLEULA, cfg.Source.MSSQLEULA)
	}
	if cfg, err = load(nil, map[string]string{"CLOUDBURROW_ACCEPT_MSSQL_EULA": "Y"}); err != nil || !cfg.AcceptMSSQLEULA || cfg.Source.MSSQLEULA != "env" {
		t.Errorf("env: %v %v %q", err, cfg.AcceptMSSQLEULA, cfg.Source.MSSQLEULA)
	}
	for _, v := range []string{"y", "yes", "1", "true"} {
		if _, err := load(nil, map[string]string{"CLOUDBURROW_ACCEPT_MSSQL_EULA": v}); err == nil {
			t.Errorf("CLOUDBURROW_ACCEPT_MSSQL_EULA=%s accepted", v)
		}
	}
	if cfg, err = load([]string{"--accept-mssql-eula=false"}, map[string]string{"CLOUDBURROW_ACCEPT_MSSQL_EULA": "Y"}); err != nil || cfg.AcceptMSSQLEULA {
		t.Errorf("flag false over env: %v %v", err, cfg.AcceptMSSQLEULA)
	}
	// A discovered ./cloudburrow.json, which a repository can ship, cannot
	// accept it.
	if err := os.WriteFile(filepath.Join(dir, DefaultFileName), []byte(`{"acceptMssqlEula": true, "AcceptMSSQLEULA": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = load(nil, nil); err == nil && cfg.AcceptMSSQLEULA {
		t.Error("the configuration file accepted Microsoft's licence")
	}
}
