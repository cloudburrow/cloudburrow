package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/unserved"
)

// The local-only guard (#1975): whatever is enabled, every endpoint argument
// of the pinned google and google-beta providers is either pointed at this
// instance or refused by it, in the provider block or in the environment the
// wrapper runs the binary with. None is left at its googleapis.com default.
func TestEveryTerraformEndpointIsLocalOrRefused(t *testing.T) {
	for _, services := range []string{"storage", "run", "storage,pubsub,tasks,secretmanager,run,kms,bigquery,spanner,firestore,bigtable"} {
		cfg := formatsConfig(t, services)
		set, _ := terraformSettings(cfg)
		covered := map[string]string{}
		for _, e := range set {
			covered[e.attr] = e.url
		}
		block, _ := terraformProvider(cfg, false)
		// google-beta's own block, written when the module uses it.
		block += terraformProviderBlock(cfg, "google-beta", false, set)
		for _, kv := range terraformRefusalEnv(cfg) {
			k, v, _ := strings.Cut(kv, "=")
			if !strings.Contains(v, unserved.PathPrefix) {
				t.Errorf("%s: %s is not a refusal", services, kv)
			}
			covered[strings.ToLower(strings.TrimPrefix(k, "GOOGLE_"))] = v
		}
		for a := range unserved.TerraformNoEnv {
			if strings.Contains(block, a+" = ") {
				covered[a] = "block"
			}
		}
		for _, a := range unserved.AllTerraformEndpoints() {
			if covered[a] == "" {
				t.Errorf("%s: %s is neither pointed here nor refused, so it would reach Google", services, a)
			}
		}
		if strings.Contains(block, "googleapis.com") {
			t.Errorf("%s: a Google address in\n%s", services, block)
		}
	}
	// Document AI, which #1953 found sent to Google, is refused when nothing
	// serves it.
	cfg := formatsConfig(t, "storage")
	want := "GOOGLE_DOCUMENT_AI_CUSTOM_ENDPOINT=http://127.0.0.1:9007" + unserved.PathPrefix + "document_ai_custom_endpoint/"
	if !slices.Contains(terraformRefusalEnv(cfg), want) {
		t.Errorf("Document AI is not refused: want %s", want)
	}
	// KMS when it is not enabled: refused, not pointed at its port.
	if !slices.Contains(terraformRefusalEnv(cfg), "GOOGLE_KMS_CUSTOM_ENDPOINT=http://127.0.0.1:9007"+unserved.PathPrefix+"kms_custom_endpoint/") {
		t.Error("kms_custom_endpoint is not refused with KMS disabled")
	}
}

// Every argument the wrapper sets locally is one the pinned provider has, so
// the table cannot drift from the schema unnoticed.
func TestTerraformEndpointsExistInThePinnedProvider(t *testing.T) {
	for _, e := range terraformEndpoints {
		if !slices.Contains(unserved.TerraformEndpoints(false), e.attr) {
			t.Errorf("%s is not an argument of hashicorp/google %s", e.attr, unserved.TerraformProviderVersion)
		}
	}
}

// Every gcloud override gcloud-setup writes is a property the pinned gcloud
// has: gcloud ignores an unknown one and calls Google (#1957 wrote
// `storagetransfer`, whose property is `transfer`).
func TestGcloudOverridesExistInThePinnedGcloud(t *testing.T) {
	for _, v := range gcloudVerified {
		if !slices.Contains(unserved.GcloudOverrides(), v.property) {
			t.Errorf("%s is not an api_endpoint_overrides property of gcloud %s", v.property, unserved.GcloudVersion)
		}
	}
}

// A module that uses google-beta gets a google-beta block too (it got none,
// and so went to Google), as an override when it configures the provider
// itself; a module that does not use it gets none, so init installs nothing
// new.
func TestTerraformWritesGoogleBetaWhenTheModuleUsesIt(t *testing.T) {
	cfg := formatsConfig(t, "storage")
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`resource "google_storage_bucket" "b" {}` + "\n")
	files, _ := terraformProviderFiles(cfg, dir)
	if len(files) != 1 || strings.Contains(files["cloudburrow_providers.tf"], "google-beta") {
		t.Errorf("without google-beta: %v", files)
	}
	write(`resource "google_storage_bucket" "b" { provider = google-beta }` + "\n")
	files, _ = terraformProviderFiles(cfg, dir)
	plain := files["cloudburrow_providers.tf"]
	if !strings.Contains(plain, `provider "google-beta" {`) || !strings.Contains(plain, `provider "google" {`) ||
		!strings.Contains(plain, `storage_custom_endpoint = "http://127.0.0.1:9001/storage/v1/"`) {
		t.Errorf("google-beta used: no beta block:\n%s", plain)
	}
	write("provider \"google-beta\" {\n  project = \"x\"\n}\n")
	files, _ = terraformProviderFiles(cfg, dir)
	if o := files["cloudburrow_providers_override.tf"]; !strings.Contains(o, `provider "google-beta" {`) ||
		!strings.Contains(o, "credentials  = null") || !strings.HasPrefix(o, generatedMarker) {
		t.Errorf("google-beta configured by the module: no override block:\n%s", o)
	}
	if p := files["cloudburrow_providers.tf"]; !strings.Contains(p, `provider "google" {`) || strings.Contains(p, "google-beta") {
		t.Errorf("google, not configured by the module, is not in the plain file:\n%s", p)
	}
	for name, body := range files {
		if strings.Contains(body, "googleapis.com") {
			t.Errorf("%s names a Google address:\n%s", name, body)
		}
	}
}

// gcloud-setup's refusals and the wrapper's land on the Resource Manager
// port, which serves the refusal (up.go), whatever port it is given.
func TestRefusalsUseTheResourceManagerPort(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoints.ResourceManager = 27299
	if got := refusalBase(cfg); got != "http://127.0.0.1:27299" {
		t.Errorf("refusalBase = %s", got)
	}
	if conf := gcloudConfiguration(cfg, "/adc.json"); !strings.Contains(conf, "dlp") ||
		!strings.Contains(conf, "workflows = http://127.0.0.1:27299"+unserved.PathPrefix+"workflows/\n") {
		t.Errorf("gcloud-setup does not refuse workflows on the Resource Manager port, or does not name the unoverridable APIs:\n%s", conf)
	}
}
