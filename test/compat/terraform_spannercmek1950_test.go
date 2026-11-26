//go:build compat

package compat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
)

// TestTerraformSpannerCMEK (#1950), in the emulators shard: a
// google_spanner_database with encryption_config { kms_key_name } applies
// through `cloudburrow terraform` with the official hashicorp/google
// provider over Spanner's REST API, reads back through the official admin
// client with the key and its version, plans clean (encryption_config is
// read back, so no replacement is planned) and destroys, with egress
// blocked. The key is made in the instance's Cloud KMS first.
func TestTerraformSpannerCMEK(t *testing.T) {
	h := New(t)
	toolBinary(t, "terraform") // in the tools container, never on the host (#2174)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	if os.Getenv(EnvSpanner) == "" || os.Getenv(EnvSpannerREST) == "" {
		t.Skipf("%s and %s must both be set", EnvSpanner, EnvSpannerREST)
	}
	addr := h.Endpoint(EnvSpanner)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	ctx := h.Context()
	k := kmsClients(t, h)["grpc"]
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/us-central1", KeyRingId: "tf-spanner-cmek"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "db",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	dc, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.Close() })

	flags := cliArgs()
	dir := t.TempDir()
	const instanceID, dbID = "tf-spanner-cmek", "tfcmekdb"
	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", h.Project(), instanceID, dbID)
	module := googleProviderRequirement + fmt.Sprintf(`resource "google_spanner_instance" "i" {
  project      = %q
  name         = %q
  config       = "emulator-config"
  display_name = "tf cmek"
  num_nodes    = 1
}
resource "google_spanner_database" "d" {
  project             = %q
  instance            = google_spanner_instance.i.name
  name                = %q
  deletion_protection = false
  ddl = [
    "CREATE TABLE Items (\n  Id INT64 NOT NULL,\n) PRIMARY KEY(Id)",
  ]
  encryption_config {
    kms_key_name = %q
  }
}
`, h.Project(), instanceID, h.Project(), dbID, key.GetName())
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	tf := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(cli, append(append(append([]string{"terraform"}, flags...), "--"), args...)...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if args[0] != "init" {
			cmd.Env = noGoogleEgress()
		}
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := tf(args...); err != nil {
			t.Fatalf("cloudburrow terraform %s: %v\n%s", strings.Join(args, " "), err, lastLines(out, 30))
		}
	}
	useGoogleProviderLock(t, dir)
	run("init", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color")
	t.Cleanup(func() { _, _ = tf("destroy", "-auto-approve", "-input=false", "-no-color") })
	got, err := dc.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: dbName})
	if err != nil || got.GetEncryptionConfig().GetKmsKeyName() != key.GetName() || len(got.GetEncryptionInfo()) != 1 ||
		got.GetEncryptionInfo()[0].GetKmsKeyVersion() != key.GetName()+"/cryptoKeyVersions/1" {
		t.Fatalf("the database after apply = %v, %v", got, err)
	}
	if out, err := tf("plan", "-detailed-exitcode", "-input=false", "-no-color"); err != nil {
		t.Errorf("plan right after apply is not clean (%v):\n%s", err, lastLines(out, 40))
	}
	run("destroy", "-auto-approve", "-input=false", "-no-color")
}
