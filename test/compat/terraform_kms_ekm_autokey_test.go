//go:build compat

package compat

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
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
	h.Endpoint(EnvKMS)
	shardEndpoint(h, envLoggingResourceManager, EnvResourceManager)
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

// TestTerraformKMSEkmConnectionIam (#1469): with the official
// hashicorp/google provider and egress blocked, google_kms_ekm_connection_iam_member
// and _binding on one connection, and google_kms_ekm_connection_iam_policy
// on another, apply through getIamPolicy and setIamPolicy on the
// connections, plan clean, and read back through the official EkmClient.
// The GA provider has no ekmConfig IAM resource; gcloud covers that one.
func TestTerraformKMSEkmConnectionIam(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvKMS) // skips without CLOUDBURROW_TEST_KMS
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	der := base64.StdEncoding.EncodeToString(ekmCertificate(t))
	m.write(fmt.Sprintf(`
locals {
  resolver = {
    service_directory_service = "projects/%[1]s/locations/asia-east2/namespaces/ekm/services/replica"
    hostname                  = "ekm.example.com"
    raw_der                   = %[2]q
  }
}
resource "google_kms_ekm_connection" "a" {
  project  = %[1]q
  name     = "tf-ekm-iam-a"
  location = "asia-east2"
  service_resolvers {
    service_directory_service = local.resolver.service_directory_service
    hostname                  = local.resolver.hostname
    server_certificates {
      raw_der = local.resolver.raw_der
    }
  }
}
resource "google_kms_ekm_connection" "b" {
  project  = %[1]q
  name     = "tf-ekm-iam-b"
  location = "asia-east2"
  service_resolvers {
    service_directory_service = local.resolver.service_directory_service
    hostname                  = local.resolver.hostname
    server_certificates {
      raw_der = local.resolver.raw_der
    }
  }
}
resource "google_kms_ekm_connection_iam_member" "viewer" {
  project  = %[1]q
  location = "asia-east2"
  name     = google_kms_ekm_connection.a.id
  role     = "roles/cloudkms.viewer"
  member   = "user:tf-viewer@example.com"
}
resource "google_kms_ekm_connection_iam_binding" "admins" {
  project  = %[1]q
  location = "asia-east2"
  name     = google_kms_ekm_connection.a.id
  role     = "roles/cloudkms.admin"
  members  = ["user:tf-admin@example.com", "group:tf-admins@example.com"]
}
data "google_iam_policy" "p" {
  binding {
    role    = "roles/cloudkms.ekmConnectionsAdmin"
    members = ["user:tf-policy@example.com"]
  }
}
resource "google_kms_ekm_connection_iam_policy" "b" {
  project     = %[1]q
  location    = "asia-east2"
  name        = google_kms_ekm_connection.b.id
  policy_data = data.google_iam_policy.p.policy_data
}
`, project, der))
	m.must("init", "-input=false", "-no-color")
	m.apply()

	ec, err := newEkmClient(h)
	if err != nil {
		t.Fatal(err)
	}
	defer ec.Close()
	roles := func(conn string) map[string][]string {
		t.Helper()
		p, err := ec.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project + "/locations/asia-east2/ekmConnections/" + conn})
		if err != nil {
			t.Fatalf("GetIamPolicy(%s) = %v", conn, err)
		}
		out := map[string][]string{}
		for _, b := range p.GetBindings() {
			out[b.GetRole()] = append(out[b.GetRole()], b.GetMembers()...)
		}
		return out
	}
	a := roles("tf-ekm-iam-a")
	if !slices.Equal(a["roles/cloudkms.viewer"], []string{"user:tf-viewer@example.com"}) || len(a["roles/cloudkms.admin"]) != 2 {
		t.Errorf("tf-ekm-iam-a's policy = %v; want the member and the binding", a)
	}
	if b := roles("tf-ekm-iam-b"); len(b) != 1 || !slices.Equal(b["roles/cloudkms.ekmConnectionsAdmin"], []string{"user:tf-policy@example.com"}) {
		t.Errorf("tf-ekm-iam-b's policy = %v; want the policy's one binding", b)
	}
	m.planClean()
}
