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
