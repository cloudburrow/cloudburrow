package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

func envGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "env_"+name+".golden")
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("env --format %s changed.\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func formatsConfig(t *testing.T, services string) config.Config {
	t.Helper()
	cfg, err := config.Load(config.Options{
		Args:   []string{"--name", "golden", "--state-dir", t.TempDir(), "--services", services},
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEnvFormatGoldens(t *testing.T) {
	cfg := formatsConfig(t, "storage,pubsub,tasks,secretmanager,kms")
	var tf bytes.Buffer
	writeTerraformEnv(&tf, cfg)
	envGolden(t, "terraform", tf.Bytes())

	var compose bytes.Buffer
	writeCompose(&compose, cfg, envVars(cfg, cfg.DefaultProject(), "/host/only/adc.json"))
	envGolden(t, "docker-compose", compose.Bytes())
	if strings.Contains(compose.String(), "127.0.0.1") || strings.Contains(compose.String(), "/host/only/adc.json") {
		t.Errorf("the compose map kept a loopback address or the host credentials path:\n%s", compose.String())
	}
}

// The terraform format sets only Verified services, and never a Google
// address, whatever is enabled.
func TestEnvTerraformSetsOnlyVerifiedLocalEndpoints(t *testing.T) {
	for _, services := range []string{"storage", "pubsub", "tasks", "secretmanager", "run", "kms", "storage,pubsub,tasks,secretmanager,run,kms"} {
		cfg := formatsConfig(t, services)
		var out bytes.Buffer
		writeTerraformEnv(&out, cfg)
		got := out.String()
		if strings.Contains(got, "googleapis.com") || strings.Contains(got, "google.com") {
			t.Fatalf("%s: a Google address:\n%s", services, got)
		}
		for _, e := range terraformEndpoints {
			if strings.Contains(got, "export "+e.env+"=") && !e.verified {
				t.Errorf("%s: unverified %s was exported:\n%s", services, e.env, got)
			}
		}
	}
}

func TestToContainerHost(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:9001":             "http://host.docker.internal:9001",
		"127.0.0.1:9002":                    "host.docker.internal:9002",
		"http://localhost:9001/storage/v1/": "http://host.docker.internal:9001/storage/v1/",
		"http://[::1]:9001":                 "http://host.docker.internal:9001",
		"cloudburrow.localhost":             "cloudburrow.localhost",
		"http://10.0.0.5:9001":              "http://10.0.0.5:9001",
	} {
		if got := toContainerHost(in); got != want {
			t.Errorf("toContainerHost(%q) = %q, want %q", in, got, want)
		}
	}
}
