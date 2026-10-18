package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
)

// TestEnvWritesTheLiveMetadataPortIntoTheADC (#1304): `env` for a running
// instance whose ports are not the configured ones (another --port-base,
// not passed to `env`) writes the ADC fixture's token_uri with the metadata
// port `up` bound, not the default one.
func TestEnvWritesTheLiveMetadataPortIntoTheADC(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Name = "adc-ports"
	info := runtimeInfo{Endpoints: map[string]string{"metadata": "127.0.0.1:24705"}}
	if _, _, err := exportedEnv(cfg, info, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.InstanceDir(), metadata.CredentialsFileName))
	if err != nil {
		t.Fatal(err)
	}
	var adc struct {
		TokenURI string `json:"token_uri"`
	}
	if err := json.Unmarshal(b, &adc); err != nil {
		t.Fatal(err)
	}
	if adc.TokenURI != "http://127.0.0.1:24705/token" {
		t.Errorf("token_uri = %q; want the live metadata port 24705", adc.TokenURI)
	}
}
