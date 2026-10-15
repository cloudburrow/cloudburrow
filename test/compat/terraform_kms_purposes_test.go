//go:build compat

package compat

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
)

// TestTerraformKMSPurposesAndImportJob (#1304): crypto keys of the purposes
// beyond ENCRYPT_DECRYPT (#1212), an ASYMMETRIC_SIGN, an ASYMMETRIC_DECRYPT
// and a MAC key, each with a version_template, and a
// google_kms_key_ring_import_job, apply through `cloudburrow terraform` with
// egress blocked; the official gRPC client reads back each key's purpose,
// algorithm, and first version, with no primary, and the import job's method,
// protection level, state and public key; the plan is clean; a label change
// on each key applies in place and plans clean; and destroy does what the
// provider does on Google: the ring, keys and import job stay, and every
// key version is DESTROY_SCHEDULED. The ring is named per run, since KMS
// never deletes one.
// covers: google.cloud.kms.v1.KeyManagementService/CreateKeyRing, google.cloud.kms.v1.KeyManagementService/GetKeyRing, google.cloud.kms.v1.KeyManagementService/CreateCryptoKey, google.cloud.kms.v1.KeyManagementService/GetCryptoKey, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKey, google.cloud.kms.v1.KeyManagementService/ListCryptoKeyVersions, google.cloud.kms.v1.KeyManagementService/DestroyCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/CreateImportJob, google.cloud.kms.v1.KeyManagementService/GetImportJob
func TestTerraformKMSPurposesAndImportJob(t *testing.T) {
	h := New(t)
	c := kmsClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_KMS
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	ringID := "tf-purposes-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ring := "projects/" + project + "/locations/us-central1/keyRings/" + ringID
	job := ring + "/importJobs/tf-job"
	keys := []struct {
		id      string
		purpose kmspb.CryptoKey_CryptoKeyPurpose
		alg     kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm
	}{
		{"sign", kmspb.CryptoKey_ASYMMETRIC_SIGN, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256},
		{"decrypt", kmspb.CryptoKey_ASYMMETRIC_DECRYPT, kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_2048_SHA256},
		{"mac", kmspb.CryptoKey_MAC, kmspb.CryptoKeyVersion_HMAC_SHA256},
	}
	module := func(label string) string {
		src := fmt.Sprintf(`
resource "google_kms_key_ring" "kr" {
  project  = %q
  name     = %q
  location = "us-central1"
}
resource "google_kms_key_ring_import_job" "j" {
  key_ring         = google_kms_key_ring.kr.id
  import_job_id    = "tf-job"
  import_method    = "RSA_OAEP_3072_SHA1_AES_256"
  protection_level = "SOFTWARE"
}
`, project, ringID)
		for _, k := range keys {
			src += fmt.Sprintf(`
resource "google_kms_crypto_key" %[1]q {
  name     = %[1]q
  key_ring = google_kms_key_ring.kr.id
  purpose  = %[2]q
  labels   = { env = %[4]q }
  version_template {
    algorithm = %[3]q
  }
}
`, k.id, k.purpose.String(), k.alg.String(), label)
		}
		return src
	}
	check := func(when, label string) {
		t.Helper()
		for _, k := range keys {
			name := ring + "/cryptoKeys/" + k.id
			got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: name})
			if err != nil {
				t.Fatalf("%s, GetCryptoKey %s: %v", when, k.id, err)
			}
			if got.GetPurpose() != k.purpose || got.GetVersionTemplate().GetAlgorithm() != k.alg || got.GetLabels()["env"] != label {
				t.Errorf("%s, key %s = %v %v labels %v; want %v %v env=%s", when, k.id, got.GetPurpose(),
					got.GetVersionTemplate().GetAlgorithm(), got.GetLabels(), k.purpose, k.alg, label)
			}
			// Only an ENCRYPT_DECRYPT key has a primary (resources.proto).
			if got.GetPrimary() != nil {
				t.Errorf("%s, key %s has a primary %s", when, k.id, got.GetPrimary().GetName())
			}
			v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: name + "/cryptoKeyVersions/1"})
			if err != nil || v.GetAlgorithm() != k.alg || v.GetState() != kmspb.CryptoKeyVersion_ENABLED {
				t.Errorf("%s, key %s version 1 = %v %v, %v; want %v ENABLED", when, k.id, v.GetAlgorithm(), v.GetState(), err, k.alg)
			}
		}
		j, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: job})
		if err != nil {
			t.Fatalf("%s, GetImportJob: %v", when, err)
		}
		if j.GetImportMethod() != kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256 || j.GetProtectionLevel() != kmspb.ProtectionLevel_SOFTWARE ||
			j.GetState() != kmspb.ImportJob_ACTIVE || j.GetPublicKey().GetPem() == "" {
			t.Errorf("%s, import job = %v %v %v, public key %d bytes", when, j.GetImportMethod(), j.GetProtectionLevel(), j.GetState(), len(j.GetPublicKey().GetPem()))
		}
	}

	m.write(module("local"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring}); err != nil {
		t.Fatalf("the applied key ring: %v", err)
	}
	check("after apply", "local")
	m.planClean()

	m.write(module("changed"))
	m.apply()
	check("after the label change", "changed")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring}); err != nil {
		t.Errorf("the key ring is gone after destroy, which the provider never deletes: %v", err)
	}
	if _, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: job}); err != nil {
		t.Errorf("the import job is gone after destroy, which KMS never deletes: %v", err)
	}
	for _, k := range keys {
		it := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: ring + "/cryptoKeys/" + k.id})
		n := 0
		for {
			v, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			n++
			if v.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
				t.Errorf("%s after destroy = %v; want DESTROY_SCHEDULED", v.GetName(), v.GetState())
			}
		}
		if n == 0 {
			t.Errorf("key %s has no versions after destroy", k.id)
		}
	}
}
