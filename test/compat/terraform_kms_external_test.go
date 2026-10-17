//go:build compat

package compat

import (
	"encoding/base64"
	"fmt"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
)

// TestTerraformKMSExternalKeys (#1469): with the official hashicorp/google
// provider and egress blocked, an EXTERNAL google_kms_crypto_key with
// skip_initial_version_creation and a google_kms_crypto_key_version naming
// its external_key_uri, and an EXTERNAL_VPC key whose crypto_key_backend is
// a google_kms_ekm_connection, with a version naming its
// ekm_connection_key_path, apply, plan clean, and read back through the
// official client.
func TestTerraformKMSExternalKeys(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	c := kmsClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_KMS
	ctx := h.Context()
	project := h.Project()
	ringID := "tf-ext-" + project
	der := base64.StdEncoding.EncodeToString(ekmCertificate(t))
	m.write(fmt.Sprintf(`
resource "google_kms_key_ring" "kr" {
  project  = %[1]q
  name     = %[2]q
  location = "me-west1"
}
resource "google_kms_crypto_key" "ext" {
  name                          = "ext"
  key_ring                      = google_kms_key_ring.kr.id
  skip_initial_version_creation = true
  version_template {
    algorithm        = "EXTERNAL_SYMMETRIC_ENCRYPTION"
    protection_level = "EXTERNAL"
  }
}
resource "google_kms_crypto_key_version" "ext" {
  crypto_key = google_kms_crypto_key.ext.id
  external_protection_level_options {
    external_key_uri = "https://ekm.example.com/v0/keys/terraform"
  }
}
resource "google_kms_ekm_connection" "ekm" {
  project  = %[1]q
  name     = "tf-ext-ekm"
  location = "me-west1"
  service_resolvers {
    service_directory_service = "projects/%[1]s/locations/me-west1/namespaces/ekm/services/replica"
    hostname                  = "ekm.example.com"
    server_certificates {
      raw_der = %[3]q
    }
  }
}
resource "google_kms_crypto_key" "vpc" {
  name                          = "vpc"
  key_ring                      = google_kms_key_ring.kr.id
  skip_initial_version_creation = true
  crypto_key_backend            = google_kms_ekm_connection.ekm.id
  version_template {
    algorithm        = "EXTERNAL_SYMMETRIC_ENCRYPTION"
    protection_level = "EXTERNAL_VPC"
  }
}
resource "google_kms_crypto_key_version" "vpc" {
  crypto_key = google_kms_crypto_key.vpc.id
  external_protection_level_options {
    ekm_connection_key_path = "v0/terraform/key"
  }
}
`, project, ringID, der))
	m.must("init", "-input=false", "-no-color")
	m.apply()

	ring := "projects/" + project + "/locations/me-west1/keyRings/" + ringID
	v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: ring + "/cryptoKeys/ext/cryptoKeyVersions/1"})
	if err != nil || v.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL || v.GetExternalProtectionLevelOptions().GetExternalKeyUri() != "https://ekm.example.com/v0/keys/terraform" {
		t.Errorf("the EXTERNAL version = %v, %v", v, err)
	}
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring + "/cryptoKeys/vpc"})
	if err != nil || k.GetCryptoKeyBackend() != "projects/"+project+"/locations/me-west1/ekmConnections/tf-ext-ekm" {
		t.Errorf("the EXTERNAL_VPC key = %v, %v", k, err)
	}
	v, err = c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: ring + "/cryptoKeys/vpc/cryptoKeyVersions/1"})
	if err != nil || v.GetProtectionLevel() != kmspb.ProtectionLevel_EXTERNAL_VPC || v.GetExternalProtectionLevelOptions().GetEkmConnectionKeyPath() != "v0/terraform/key" {
		t.Errorf("the EXTERNAL_VPC version = %v, %v", v, err)
	}
	m.planClean()
}
