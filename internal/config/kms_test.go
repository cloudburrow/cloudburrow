package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The Cloud KMS port follows the same precedence as every other port
// (#385): flag over environment over config file over the 9018 default,
// with 0 meaning OS-assigned.
func TestKMSPortPrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cb.json")
	if err := os.WriteFile(file, []byte(`{"endpoints":{"kms":9124}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for name, c := range map[string]struct {
		args []string
		env  map[string]string
		want int
	}{
		"default":           {nil, nil, 9018},
		"config file":       {[]string{"--config", file}, nil, 9124},
		"environment":       {[]string{"--config", file}, map[string]string{"CLOUDBURROW_PORT_KMS": "9125"}, 9125},
		"flag":              {[]string{"--config", file, "--port-kms", "9126"}, map[string]string{"CLOUDBURROW_PORT_KMS": "9125"}, 9126},
		"flag, OS-assigned": {[]string{"--port-kms", "0"}, nil, 0},
		"env, OS-assigned":  {nil, map[string]string{"CLOUDBURROW_PORT_KMS": "0"}, 0},
	} {
		args := append([]string{"--state-dir", t.TempDir(), "--services", "storage,kms"}, c.args...)
		cfg, err := Load(Options{Args: args, Getenv: env(c.env), WorkDir: dir})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.Endpoints.KMS != c.want {
			t.Errorf("%s: endpoints.kms = %d, want %d", name, cfg.Endpoints.KMS, c.want)
		}
	}
}

// KMS is opt-in: never in the default set.
func TestKMSIsNotEnabledByDefault(t *testing.T) {
	cfg, err := Load(Options{Args: []string{"--state-dir", t.TempDir()}, Getenv: func(string) string { return "" }, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cfg.EnabledServices() {
		if s == ServiceKMS {
			t.Fatal("kms is enabled without being asked for")
		}
	}
}
