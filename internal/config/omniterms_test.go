package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Google's AlloyDB Omni terms (#2207) are accepted only by the user's own
// flag, environment variable or a configuration file the user named: never
// by default, never by a discovered ./cloudburrow.json, and never by an
// environment value other than Y.
func TestAcceptOmniTerms(t *testing.T) {
	dir := t.TempDir()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	load := func(args []string, e map[string]string) (Config, error) {
		return Load(Options{Args: args, Getenv: env(e), WorkDir: dir})
	}
	cfg, err := load(nil, nil)
	if err != nil || cfg.AlloyDB.AcceptOmniTerms || cfg.Source.OmniTerms != "" {
		t.Fatalf("default: %v %v %q", err, cfg.AlloyDB.AcceptOmniTerms, cfg.Source.OmniTerms)
	}
	if cfg, err = load([]string{"--accept-alloydb-omni-terms"}, nil); err != nil || !cfg.AlloyDB.AcceptOmniTerms || cfg.Source.OmniTerms != "flag" {
		t.Errorf("flag: %v %v %q", err, cfg.AlloyDB.AcceptOmniTerms, cfg.Source.OmniTerms)
	}
	if cfg, err = load(nil, map[string]string{"CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS": "Y"}); err != nil || !cfg.AlloyDB.AcceptOmniTerms || cfg.Source.OmniTerms != "env" {
		t.Errorf("env: %v %v %q", err, cfg.AlloyDB.AcceptOmniTerms, cfg.Source.OmniTerms)
	}
	for _, v := range []string{"y", "yes", "1", "true"} {
		if _, err := load(nil, map[string]string{"CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS": v}); err == nil {
			t.Errorf("CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS=%s accepted", v)
		}
	}
	if cfg, err = load([]string{"--accept-alloydb-omni-terms=false"}, map[string]string{"CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS": "Y"}); err != nil || cfg.AlloyDB.AcceptOmniTerms {
		t.Errorf("flag false over env: %v %v", err, cfg.AlloyDB.AcceptOmniTerms)
	}
	// A file the user named accepts them.
	named := filepath.Join(t.TempDir(), "mine.json")
	if err := os.WriteFile(named, []byte(`{"alloydb": {"acceptOmniTerms": true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = load([]string{"--config", named}, nil); err != nil || !cfg.AlloyDB.AcceptOmniTerms || cfg.Source.OmniTerms != "file" {
		t.Errorf("named file: %v %v %q", err, cfg.AlloyDB.AcceptOmniTerms, cfg.Source.OmniTerms)
	}
	// A discovered ./cloudburrow.json, which a repository can ship, cannot.
	if err := os.WriteFile(filepath.Join(dir, DefaultFileName), []byte(`{"alloydb": {"acceptOmniTerms": true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = load(nil, nil); err == nil || !strings.Contains(err.Error(), "--accept-alloydb-omni-terms") {
		t.Errorf("the discovered file: %v, want a refusal naming the flag", err)
	}
	// ... and one that sets it false is harmless.
	if err := os.WriteFile(filepath.Join(dir, DefaultFileName), []byte(`{"alloydb": {"acceptOmniTerms": false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = load(nil, nil); err != nil || cfg.AlloyDB.AcceptOmniTerms {
		t.Errorf("discovered false: %v %v", err, cfg.AlloyDB.AcceptOmniTerms)
	}
}
