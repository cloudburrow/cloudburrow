package config

import (
	"strings"
	"testing"
)

// storage.backend was removed (#519): set by file or environment it is
// refused by name, and the flag no longer exists.
func TestStorageBackendSettingRemoved(t *testing.T) {
	load := func(args []string, env map[string]string) (Config, error) {
		return Load(Options{Args: append([]string{"--state-dir", t.TempDir()}, args...), Getenv: func(k string) string { return env[k] }})
	}
	if _, err := load(nil, nil); err != nil {
		t.Fatalf("the default configuration = %v", err)
	}
	if _, err := load(nil, map[string]string{"CLOUDBURROW_STORAGE_BACKEND": "builtin"}); err == nil || !strings.Contains(err.Error(), "storage.backend") || !strings.Contains(err.Error(), "removed") {
		t.Errorf("the environment variable = %v; want storage.backend named as removed", err)
	}
	if _, err := load([]string{"--storage-backend", "builtin"}, nil); err == nil {
		t.Error("--storage-backend is still accepted")
	}
}

// --cors-allow-origin (#677): flag over environment over a named file, the
// flag repeatable and comma-separated, each origin in the form a browser
// sends; a discovered file cannot set it, and a non-origin is refused.
func TestCORSAllowOriginPrecedence(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		args     []string
		env      map[string]string
		file     string // a file named with CLOUDBURROW_CONFIG
		found    string // ./cloudburrow.json
		want     string
		wantErrs []string
	}{
		{name: "default is none", want: ""},
		{name: "file", file: `{"storage":{"corsAllowOrigins":["https://file.example"]}}`, want: "https://file.example"},
		{name: "environment beats file", file: `{"storage":{"corsAllowOrigins":["https://file.example"]}}`,
			env: map[string]string{EnvPrefix + "CORS_ALLOW_ORIGIN": "https://env.example, https://env2.example:8443"}, want: "https://env.example https://env2.example:8443"},
		{name: "flag beats environment, repeated and comma-separated", env: map[string]string{EnvPrefix + "CORS_ALLOW_ORIGIN": "https://env.example"},
			args: []string{"--cors-allow-origin", "https://a.example,https://b.example", "--cors-allow-origin", "HTTPS://C.Example:443/"},
			want: "https://a.example https://b.example https://c.example"},
		{name: "not an origin", args: []string{"--cors-allow-origin", "https://a.example/path"}, wantErrs: []string{"storage.corsAllowOrigins", "/path"}},
		{name: "a wildcard", env: map[string]string{EnvPrefix + "CORS_ALLOW_ORIGIN": "*"}, wantErrs: []string{"storage.corsAllowOrigins", `"*"`}},
		{name: "a discovered file cannot allow origins", found: `{"storage":{"corsAllowOrigins":["https://evil.example"]}}`,
			args: []string{"--cors-allow-origin", "https://a.example"}, wantErrs: []string{"found in the working directory", "corsAllowOrigins", "--cors-allow-origin"}},
		{name: "a discovered file without it is fine", found: `{"logLevel":"warn"}`, want: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			env := map[string]string{}
			for k, v := range c.env {
				env[k] = v
			}
			if c.file != "" {
				env[EnvPrefix+"CONFIG"] = writeFile(t, dir, "mine.json", c.file)
			}
			if c.found != "" {
				writeFile(t, dir, DefaultFileName, c.found)
			}
			cfg, err := Load(Options{Args: c.args, Getenv: envMap(env), WorkDir: dir})
			if c.wantErrs != nil {
				if err == nil {
					t.Fatalf("Load() = nil, want an error; origins %q", cfg.Storage.CORSAllowOrigins)
				}
				for _, w := range c.wantErrs {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error lacks %q: %v", w, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(cfg.Storage.CORSAllowOrigins, " "); got != c.want {
				t.Errorf("origins = %q, want %q", got, c.want)
			}
		})
	}
}
