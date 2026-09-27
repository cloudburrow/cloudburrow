package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// Cloud Run v2 has CLOUDBURROW_RUN_ENDPOINT, like every other service the CLI
// hosts, so code that deploys or lists services builds its client from it
// instead of hard-coding 9004 (#707). Pinned as a golden.
func TestEnvRunGolden(t *testing.T) {
	cfg := formatsConfig(t, "run")
	var shell bytes.Buffer
	writeShell(&shell, envVars(cfg, cfg.DefaultProject(), "/host/only/adc.json"))
	envGolden(t, "run_shell", shell.Bytes())
	if !strings.Contains(shell.String(), `export CLOUDBURROW_RUN_ENDPOINT="127.0.0.1:9004"`) {
		t.Errorf("env does not export the Run endpoint:\n%s", shell.String())
	}
}

// The variable follows --port-base like the port it names.
func TestEnvRunEndpointFollowsThePortBase(t *testing.T) {
	cfg, err := config.Load(config.Options{
		Args:   []string{"--name", "golden", "--state-dir", t.TempDir(), "--services", "run", "--port-base", "9100"},
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range envVars(cfg, cfg.DefaultProject(), "/adc.json") {
		got[v.Name] = v.Value
	}
	if got["CLOUDBURROW_RUN_ENDPOINT"] != "127.0.0.1:9104" {
		t.Errorf("CLOUDBURROW_RUN_ENDPOINT = %q under --port-base 9100, want 127.0.0.1:9104", got["CLOUDBURROW_RUN_ENDPOINT"])
	}
}
