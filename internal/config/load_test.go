package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envMap returns a Getenv function backed by a map, so tests never touch the
// real process environment and can run in parallel.
func envMap(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestPrecedence is the core of the configuration contract: flags beat
// environment, environment beats file, file beats defaults. Each case sets the
// same field at several layers and asserts the winner.
func TestPrecedence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := writeFile(t, dir, "cb.json", `{"bindAddress":"127.0.0.3","logLevel":"warn","endpoints":{"control":9100}}`)

	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		wantBind string
		wantLog  string
		wantPort int
	}{
		{
			name:     "defaults only",
			wantBind: "127.0.0.1",
			wantLog:  "info",
			wantPort: 9000,
		},
		{
			name:     "file beats defaults",
			env:      map[string]string{EnvPrefix + "CONFIG": file},
			wantBind: "127.0.0.3",
			wantLog:  "warn",
			wantPort: 9100,
		},
		{
			name: "env beats file",
			env: map[string]string{
				EnvPrefix + "CONFIG":       file,
				EnvPrefix + "BIND_ADDRESS": "127.0.0.4",
				EnvPrefix + "LOG_LEVEL":    "error",
				EnvPrefix + "PORT_CONTROL": "9200",
			},
			wantBind: "127.0.0.4",
			wantLog:  "error",
			wantPort: 9200,
		},
		{
			name: "flags beat env and file",
			args: []string{"--bind-address", "127.0.0.5", "--log-level", "debug", "--port-control", "9300"},
			env: map[string]string{
				EnvPrefix + "CONFIG":       file,
				EnvPrefix + "BIND_ADDRESS": "127.0.0.4",
				EnvPrefix + "LOG_LEVEL":    "error",
				EnvPrefix + "PORT_CONTROL": "9200",
			},
			wantBind: "127.0.0.5",
			wantLog:  "debug",
			wantPort: 9300,
		},
		{
			// The subtle case: an unset flag must not overwrite a lower layer
			// with its zero value. Only flags actually present on the command
			// line participate.
			name:     "unset flags do not clobber env",
			args:     []string{"--log-level", "debug"},
			env:      map[string]string{EnvPrefix + "BIND_ADDRESS": "127.0.0.9", EnvPrefix + "PORT_CONTROL": "9500"},
			wantBind: "127.0.0.9",
			wantLog:  "debug",
			wantPort: 9500,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(Options{Args: tt.args, Getenv: envMap(tt.env), WorkDir: t.TempDir()})
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			if cfg.BindAddress != tt.wantBind {
				t.Errorf("BindAddress = %q, want %q", cfg.BindAddress, tt.wantBind)
			}
			if cfg.LogLevel != tt.wantLog {
				t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, tt.wantLog)
			}
			if cfg.Endpoints.Control != tt.wantPort {
				t.Errorf("Endpoints.Control = %d, want %d", cfg.Endpoints.Control, tt.wantPort)
			}
		})
	}
}

// A file in the working directory is picked up without being named, but an
// explicitly requested file that does not exist is an error rather than a
// silent fallback to defaults.
func TestConfigFileDiscovery(t *testing.T) {
	t.Parallel()

	t.Run("default file in working directory is used", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, dir, DefaultFileName, `{"logLevel":"warn"}`)
		cfg, err := Load(Options{Getenv: envMap(nil), WorkDir: dir})
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if cfg.LogLevel != "warn" {
			t.Errorf("LogLevel = %q, want warn", cfg.LogLevel)
		}
	})

	t.Run("absent default file is not an error", func(t *testing.T) {
		t.Parallel()
		cfg, err := Load(Options{Getenv: envMap(nil), WorkDir: t.TempDir()})
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if cfg.LogLevel != "info" {
			t.Errorf("LogLevel = %q, want default info", cfg.LogLevel)
		}
	})

	t.Run("explicitly named missing file is an error", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "nope.json")
		_, err := Load(Options{Args: []string{"--config", missing}, Getenv: envMap(nil), WorkDir: t.TempDir()})
		if err == nil {
			t.Fatal("Load() = nil, want error for missing explicit config file")
		}
		if !strings.Contains(err.Error(), "nope.json") {
			t.Errorf("error should name the file, got: %v", err)
		}
	})

	t.Run("unknown field in config file is rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeFile(t, dir, "bad.json", `{"bindAdress":"127.0.0.1"}`) // typo
		_, err := Load(Options{Args: []string{"--config", path}, Getenv: envMap(nil), WorkDir: dir})
		if err == nil {
			t.Fatal("Load() = nil, want error for unknown field")
		}
		// A silently ignored typo is how a user ends up debugging the wrong
		// thing, so the message must name the offending key.
		if !strings.Contains(err.Error(), "bindAdress") {
			t.Errorf("error should name the unknown field, got: %v", err)
		}
	})
}

func TestDurationInConfigFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("string form", func(t *testing.T) {
		t.Parallel()
		path := writeFile(t, dir, "dur.json", `{"shutdownTimeout":"45s"}`)
		cfg, err := Load(Options{Args: []string{"--config", path}, Getenv: envMap(nil), WorkDir: dir})
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if time.Duration(cfg.ShutdownTimeout) != 45*time.Second {
			t.Errorf("ShutdownTimeout = %s, want 45s", cfg.ShutdownTimeout)
		}
	})

	t.Run("invalid form is rejected", func(t *testing.T) {
		t.Parallel()
		path := writeFile(t, dir, "baddur.json", `{"shutdownTimeout":"quickly"}`)
		_, err := Load(Options{Args: []string{"--config", path}, Getenv: envMap(nil), WorkDir: dir})
		if err == nil {
			t.Fatal("Load() = nil, want error for unparsable duration")
		}
	})
}

func TestEnvParsingErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"non-numeric port", map[string]string{EnvPrefix + "PORT_RUN": "abc"}, "PORT_RUN"},
		{"non-boolean allow-remote", map[string]string{EnvPrefix + "ALLOW_REMOTE": "maybe"}, "ALLOW_REMOTE"},
		{"bad duration", map[string]string{EnvPrefix + "SHUTDOWN_TIMEOUT": "soon"}, "SHUTDOWN_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(Options{Getenv: envMap(tt.env), WorkDir: t.TempDir()})
			if err == nil {
				t.Fatalf("Load() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should name %s, got: %v", tt.want, err)
			}
		})
	}
}

func TestServiceSelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		want    []Service
		wantErr string
	}{
		{"default is all", nil, AllServices(), ""},
		{"subset via flag", []string{"--services", "pubsub,storage"}, []Service{ServiceStorage, ServicePubSub}, ""},
		{"whitespace and case tolerated", []string{"--services", " PubSub , Storage "}, []Service{ServiceStorage, ServicePubSub}, ""},
		{"unknown service rejected", []string{"--services", "dataflow"}, nil, "dataflow"},
		{"duplicate service rejected", []string{"--services", "run,run"}, nil, "more than once"},
		{"explicitly empty rejected", []string{"--services", ""}, nil, "at least one service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(Options{Args: tt.args, Getenv: envMap(nil), WorkDir: t.TempDir()})
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			got := cfg.EnabledServices()
			if len(got) != len(tt.want) {
				t.Fatalf("EnabledServices() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("EnabledServices() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestUnexpectedPositionalArgument(t *testing.T) {
	t.Parallel()
	_, err := Load(Options{Args: []string{"surprise"}, Getenv: envMap(nil), WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("Load() = %v, want error naming the unexpected argument", err)
	}
}

func TestStateDirIsMadeAbsolute(t *testing.T) {
	t.Parallel()
	cfg, err := Load(Options{
		Args:    []string{"--state-dir", "relative/path"},
		Getenv:  envMap(nil),
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !filepath.IsAbs(cfg.StateDir) {
		t.Errorf("StateDir = %q, want an absolute path", cfg.StateDir)
	}
}

// Cluster settings participate in the same precedence as everything else.
func TestClusterSettingPrecedence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := writeFile(t, dir, "cb.json", `{"cluster":{"namespace":"from-file"},"name":"from-file"}`)

	cfg, err := Load(Options{
		Args:    []string{"--namespace", "from-flag"},
		Getenv:  envMap(map[string]string{EnvPrefix + "CONFIG": file, EnvPrefix + "NAME": "from-env"}),
		WorkDir: dir,
	})
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.Cluster.Namespace != "from-flag" {
		t.Errorf("namespace = %q, want from-flag", cfg.Cluster.Namespace)
	}
	if cfg.Name != "from-env" {
		t.Errorf("name = %q, want from-env (env beats file)", cfg.Name)
	}
}

// Two instances must not collide, which is what allows parallel environments.
func TestTwoInstancesDoNotCollide(t *testing.T) {
	t.Parallel()
	load := func(name string) Config {
		cfg, err := Load(Options{
			Args:    []string{"--name", name, "--port-control", "0", "--port-storage", "0", "--port-pubsub", "0", "--port-tasks", "0", "--port-run", "0"},
			Getenv:  envMap(nil),
			WorkDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("Load(%s) = %v", name, err)
		}
		return cfg
	}
	a, b := load("alpha"), load("beta")
	if a.ClusterName() == b.ClusterName() {
		t.Error("cluster names collide")
	}
	if a.KubeconfigPath() == b.KubeconfigPath() {
		t.Error("kubeconfig paths collide")
	}
}

// TestSeedAndHooksSettingPrecedence: flag, then environment, then file, for
// the settings `up` reads its seed file and hooks from (#285, #286). Paths
// come back absolute whichever source set them.
func TestSeedAndHooksSettingPrecedence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := writeFile(t, dir, "cb.json", `{"seedFile":"/from/file.json","hooksDir":"/from/file-hooks","hookTimeout":"1m"}`)
	for _, c := range []struct {
		name                string
		args                []string
		env                 map[string]string
		seed, hooks, reason string
		timeout             time.Duration
	}{
		{name: "defaults", seed: "", timeout: 5 * time.Minute},
		{name: "file", env: map[string]string{EnvPrefix + "CONFIG": file}, seed: "/from/file.json", hooks: "/from/file-hooks", timeout: time.Minute},
		{name: "env beats file", env: map[string]string{EnvPrefix + "CONFIG": file, EnvPrefix + "SEED_FILE": "/from/env.json",
			EnvPrefix + "HOOKS_DIR": "/from/env-hooks", EnvPrefix + "HOOK_TIMEOUT": "2m"}, seed: "/from/env.json", hooks: "/from/env-hooks", timeout: 2 * time.Minute},
		{name: "flag beats env", args: []string{"--seed-file", "/from/flag.json", "--hooks-dir", "/from/flag-hooks", "--hook-timeout", "3m"},
			env:  map[string]string{EnvPrefix + "CONFIG": file, EnvPrefix + "SEED_FILE": "/from/env.json", EnvPrefix + "HOOKS_DIR": "/from/env-hooks"},
			seed: "/from/flag.json", hooks: "/from/flag-hooks", timeout: 3 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Load(Options{Args: c.args, Getenv: func(k string) string { return c.env[k] }, WorkDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SeedFile != c.seed {
				t.Errorf("seedFile = %q, want %q", cfg.SeedFile, c.seed)
			}
			if c.hooks != "" && cfg.HooksDir != c.hooks {
				t.Errorf("hooksDir = %q, want %q", cfg.HooksDir, c.hooks)
			}
			if c.hooks == "" && !filepath.IsAbs(cfg.HooksDir) {
				t.Errorf("the default hooksDir %q is not absolute", cfg.HooksDir)
			}
			if time.Duration(cfg.HookTimeout) != c.timeout {
				t.Errorf("hookTimeout = %v, want %v", time.Duration(cfg.HookTimeout), c.timeout)
			}
		})
	}
	// Unknown keys are still refused, beside the new ones.
	bad := writeFile(t, dir, "bad.json", `{"seedFile":"x.json","seedFiles":["y.json"]}`)
	if _, err := Load(Options{Getenv: func(k string) string { return map[string]string{EnvPrefix + "CONFIG": bad}[k] }}); err == nil ||
		!strings.Contains(err.Error(), "seedFiles") {
		t.Errorf("an unknown key was accepted: %v", err)
	}
}
