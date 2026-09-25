package config

import (
	"strings"
	"testing"
)

// storage.backend defaults to fake-gcs, is set by flag, environment or file,
// and refuses anything else (#488).
func TestStorageBackendSetting(t *testing.T) {
	load := func(args []string, env map[string]string) (Config, error) {
		return Load(Options{Args: append([]string{"--state-dir", t.TempDir()}, args...), Getenv: func(k string) string { return env[k] }})
	}
	if c, err := load(nil, nil); err != nil || c.Storage.Backend != StorageFakeGCS {
		t.Errorf("default = %q, %v; want fake-gcs", c.Storage.Backend, err)
	}
	if c, err := load(nil, map[string]string{"CLOUDBURROW_STORAGE_BACKEND": "builtin"}); err != nil || c.Storage.Backend != StorageBuiltin {
		t.Errorf("environment = %q, %v; want builtin", c.Storage.Backend, err)
	}
	if c, err := load([]string{"--storage-backend", "fake-gcs"}, map[string]string{"CLOUDBURROW_STORAGE_BACKEND": "builtin"}); err != nil || c.Storage.Backend != StorageFakeGCS {
		t.Errorf("flag over environment = %q, %v; want fake-gcs", c.Storage.Backend, err)
	}
	if _, err := load([]string{"--storage-backend", "minio"}, nil); err == nil || !strings.Contains(err.Error(), "storage.backend") {
		t.Errorf("an unknown backend = %v; want a validation error naming storage.backend", err)
	}
}
