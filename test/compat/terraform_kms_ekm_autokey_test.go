//go:build compat

package compat

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
)

// TestTerraformKMSHSMEkmAndAutokey (#1452): with the official
// hashicorp/google provider and egress blocked, an HSM google_kms_crypto_key
// (the GA provider has no key_access_justifications_policy; gcloud and the
// SDK cover it), a google_kms_ekm_connection, a
// google_kms_autokey_config on a google_folder naming the test project as
// key project, and a google_kms_key_handle in a google_project made in that
// folder apply, plan clean, and read back through the official clients.
func TestTerraformKMSHSMEkmAndAutokey(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	c := kmsClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_KMS
	ctx := h.Context()
	fc, _, _ := foldersClients(t, h)
	project := h.Project()
	ringID := "tf-hsm-" + project
	folder := folderLabel(h, "tfak-")
	resourceProject := v1ID(h, "tfak-")
	der := base64.StdEncoding.EncodeToString(ekmCertificate(t))
	m.write(fmt.Sprintf(`
resource "google_kms_key_ring" "kr" {
  project  = %[1]q
  name     = %[2]q
  location = "us-central1"
}
resource "google_kms_crypto_key" "hsm" {
  name     = "hsm"
  key_ring = google_kms_key_ring.kr.id
  version_template {
    algorithm        = "GOOGLE_SYMMETRIC_ENCRYPTION"
    protection_level = "HSM"
  }
}
resource "google_kms_ekm_connection" "ekm" {
  project  = %[1]q
  name     = "tf-ekm"
  location = "asia-east1"
  key_management_mode = "MANUAL"
  service_resolvers {
    service_directory_service = "projects/%[1]s/locations/asia-east1/namespaces/ekm/services/replica"
    hostname                  = "ekm.example.com"
    server_certificates {
      raw_der = %[3]q
    }
  }
}
resource "google_folder" "f" {
  display_name        = %[4]q
  parent              = %[5]q
  deletion_protection = false
}
resource "google_project" "p" {
  project_id      = %[6]q
  name            = "Autokey resources"
  folder_id       = google_folder.f.folder_id
  deletion_policy = "DELETE"
}
resource "google_kms_autokey_config" "ak" {
  folder      = google_folder.f.folder_id
  key_project = "projects/%[1]s"
}
resource "google_kms_key_handle" "kh" {
  project                = google_project.p.project_id
  name                   = "tf-bucket"
  location               = "us-central1"
  resource_type_selector = "storage.googleapis.com/Bucket"
  depends_on             = [google_kms_autokey_config.ak]
}
`, project, ringID, der, folder, localOrg, resourceProject))
	m.must("init", "-input=false", "-no-color")
	m.apply()

	key := "projects/" + project + "/locations/us-central1/keyRings/" + ringID + "/cryptoKeys/hsm"
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil || k.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM || k.GetPrimary().GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Errorf("the HSM key = %v, %v", k, err)
	}
	ec, err := newEkmClient(h)
	if err != nil {
		t.Fatal(err)
	}
	defer ec.Close()
	conn, err := ec.GetEkmConnection(ctx, &kmspb.GetEkmConnectionRequest{Name: "projects/" + project + "/locations/asia-east1/ekmConnections/tf-ekm"})
	if err != nil || conn.GetServiceResolvers()[0].GetHostname() != "ekm.example.com" || !conn.GetServiceResolvers()[0].GetServerCertificates()[0].GetParsed() {
		t.Errorf("the EKM connection = %v, %v", conn, err)
	}
	f, err := fc.SearchFolders(ctx, &rmpb.SearchFoldersRequest{Query: fmt.Sprintf("displayName=%q", folder)}).Next()
	if err != nil {
		t.Fatalf("SearchFolders: %v", err)
	}
	cfg, err := kmsAdminClient(t, h).GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: f.GetName() + "/autokeyConfig"})
	if err != nil || cfg.GetKeyProject() != "projects/"+project {
		t.Errorf("the AutokeyConfig = %v, %v", cfg, err)
	}
	ak, err := newAutokeyClient(h)
	if err != nil {
		t.Fatal(err)
	}
	defer ak.Close()
	kh, err := ak.GetKeyHandle(ctx, &kmspb.GetKeyHandleRequest{Name: "projects/" + resourceProject + "/locations/us-central1/keyHandles/tf-bucket"})
	if err != nil || !strings.HasPrefix(kh.GetKmsKey(), "projects/"+project+"/locations/us-central1/keyRings/autokey/cryptoKeys/") {
		t.Errorf("the key handle = %v, %v", kh, err)
	}
	m.planClean()
}
