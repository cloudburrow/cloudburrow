package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGcloudSetupWritesOnlyItsOwnConfiguration(t *testing.T) {
	gdir := t.TempDir()
	t.Setenv("CLOUDSDK_CONFIG", gdir)
	// A user's existing gcloud state, which must not change.
	_ = os.MkdirAll(filepath.Join(gdir, "configurations"), 0o755)
	def := filepath.Join(gdir, "configurations", "config_default")
	_ = os.WriteFile(def, []byte("[core]\nproject = my-real-project\n"), 0o644)
	active := filepath.Join(gdir, "active_config")
	_ = os.WriteFile(active, []byte("default"), 0o644)

	args := []string{"--name", "gcs", "--state-dir", t.TempDir(), "--services", "storage,pubsub,tasks,secretmanager,scheduler,run"}
	var out, errOut bytes.Buffer
	if err := runGcloudSetup(args, &out, &errOut); err != nil {
		t.Fatalf("gcloud-setup: %v\n%s", err, errOut.String())
	}
	if got := out.String(); got != "export CLOUDSDK_ACTIVE_CONFIG_NAME=cloudburrow-gcs\n" {
		t.Errorf("stdout = %q", got)
	}
	b, err := os.ReadFile(filepath.Join(gdir, "configurations", "config_cloudburrow-gcs"))
	if err != nil {
		t.Fatal(err)
	}
	conf := string(b)
	for _, want := range []string{"storage = http://127.0.0.1:9001/storage/v1/", "pubsub = http://127.0.0.1:9002/",
		"secretmanager = http://127.0.0.1:9006/", "cloudtasks = http://127.0.0.1:9003/",
		"cloudscheduler = http://127.0.0.1:9008/",
		// Always served, so written whatever --services selects (#682).
		"cloudresourcemanager = http://127.0.0.1:9007/",
		"disable_credentials = true", "credential_file_override = ", "[core]\nproject = "} {
		if !strings.Contains(conf, want) {
			t.Errorf("configuration lacks %q:\n%s", want, conf)
		}
	}
	// Only Verified gcloud endpoints: Cloud Run is enabled and not written,
	// since gcloud run calls the v1 API (#591).
	if strings.Contains(conf, "run =") || strings.Contains(conf, "9004") {
		t.Errorf("an unverified endpoint was written:\n%s", conf)
	}
	if d, _ := os.ReadFile(def); string(d) != "[core]\nproject = my-real-project\n" {
		t.Errorf("the default configuration changed: %q", d)
	}
	if a, _ := os.ReadFile(active); string(a) != "default" {
		t.Errorf("active_config changed: %q", a)
	}

	// Teardown removes it, and a second teardown is harmless.
	for i := 0; i < 2; i++ {
		out.Reset()
		if err := runGcloudTeardown(args, &out, &errOut); err != nil {
			t.Fatalf("teardown %d: %v", i+1, err)
		}
		if out.String() != "unset CLOUDSDK_ACTIVE_CONFIG_NAME\n" {
			t.Errorf("teardown stdout = %q", out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(gdir, "configurations", "config_cloudburrow-gcs")); !os.IsNotExist(err) {
		t.Errorf("the configuration survived teardown: %v", err)
	}
	if _, err := os.Stat(def); err != nil {
		t.Errorf("teardown touched the default configuration: %v", err)
	}
}

// A configuration of the same name the user wrote themselves is never
// overwritten or deleted.
func TestGcloudSetupLeavesAForeignConfigurationAlone(t *testing.T) {
	gdir := t.TempDir()
	t.Setenv("CLOUDSDK_CONFIG", gdir)
	p := filepath.Join(gdir, "configurations", "config_cloudburrow-mine")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("[core]\nproject = hand-made\n"), 0o644)
	args := []string{"--name", "mine", "--state-dir", t.TempDir()}
	var out, errOut bytes.Buffer
	if err := runGcloudSetup(args, &out, &errOut); err == nil {
		t.Error("setup overwrote a configuration it did not write")
	}
	if err := runGcloudTeardown(args, &out, &errOut); err == nil {
		t.Error("teardown removed a configuration it did not write")
	}
	if b, _ := os.ReadFile(p); string(b) != "[core]\nproject = hand-made\n" {
		t.Errorf("the hand-made configuration changed: %q", b)
	}
}
