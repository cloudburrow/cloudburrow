package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
)

// Cloud KMS is named where it is left out, never silently omitted (#388).

func TestTerraformLeavesOutKMS(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServiceKMS}
	body, skipped := terraformProvider(cfg, false)
	if !slices.Contains(skipped, config.ServiceKMS) {
		t.Errorf("kms is not named as left out: %v", skipped)
	}
	if strings.Contains(body, "kms_custom_endpoint") {
		t.Errorf("kms_custom_endpoint was written:\n%s", body)
	}
}

func TestEnvTerraformDoesNotExportKMS(t *testing.T) {
	var out bytes.Buffer
	writeTerraformEnv(&out, formatsConfig(t, "storage,kms"))
	if strings.Contains(out.String(), "GOOGLE_KMS_CUSTOM_ENDPOINT") {
		t.Errorf("GOOGLE_KMS_CUSTOM_ENDPOINT was exported:\n%s", out.String())
	}
}

func TestStateManifestSaysKMSIsNotCaptured(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceKMS}
	control := lifecycle.NewControlServer(0, nil)
	mountAdmin(control, admin.NewRecorder(10, nil), cfg, adminDeps{})
	ctx := context.Background()
	if err := control.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Stop(ctx) }()
	resp, err := http.Post("http://"+control.Addr()+"/admin/state/export", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "manifest.json" {
		t.Fatalf("first entry %v, %v", hdr, err)
	}
	var m admin.Manifest
	if err := json.NewDecoder(tr).Decode(&m); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Services {
		if s.Name == "kms" {
			if s.Captured || !strings.Contains(s.Reason, "ciphertext produced before a state load cannot be decrypted") {
				t.Errorf("kms manifest line = %+v", s)
			}
			return
		}
	}
	t.Errorf("the manifest does not list kms: %+v", m.Services)
}
