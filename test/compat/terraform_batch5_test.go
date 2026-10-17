//go:build compat

package compat

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// #1304's fifth batch of google provider resources, each through
// `cloudburrow terraform` with hashicorp/google 8.4.0 and egress blocked,
// read back through an official SDK, planned clean, changed in place where
// the resource has an updatable field, planned clean again and destroyed.
// Each test needs only its own service, so it runs in the shard serving it.

// policyMap is a policy's members by role.
func policyMap(pol *iampb.Policy) map[string][]string {
	out := map[string][]string{}
	for _, b := range pol.GetBindings() {
		out[b.GetRole()] = b.GetMembers()
	}
	return out
}

// TestTerraformStorageManagedFolderIAM (#1304):
// google_storage_managed_folder_iam_member, _binding and _policy on a
// google_storage_managed_folder in a uniform bucket, through
// TestTerraformBigQueryIAM's phases (member and binding, none, policy,
// none), each read back with Google's generated JSON API client
// (managedFolders.getIamPolicy) and planned clean. Stored, never enforced
// (ADR-0006). Destroy removes the folder and the bucket.
// covers: storage.managedFolders.insert, storage.managedFolders.get, storage.managedFolders.delete, storage.managedFolders.getIamPolicy, storage.managedFolders.setIamPolicy
func TestTerraformStorageManagedFolderIAM(t *testing.T) {
	h := New(t)
	_ = storageClient(t, h) // skips without CLOUDBURROW_TEST_STORAGE
	s := storageJSON(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	bucket := h.Project() + "-tfmfiam"
	const folder = "team/shared/"
	base := fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %q
  name                        = %q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
}
resource "google_storage_managed_folder" "f" {
  bucket = google_storage_bucket.b.name
  name   = %q
}
`, h.Project(), bucket, folder)
	targets := []tfIAMTarget{{
		kind: "google_storage_managed_folder", label: "f", policyData: true,
		ref: map[string]string{"bucket": "google_storage_managed_folder.f.bucket", "managed_folder": "google_storage_managed_folder.f.name"},
		read: func() map[string][]string {
			t.Helper()
			p, err := s.ManagedFolders.GetIamPolicy(bucket, folder).Context(ctx).Do()
			if err != nil {
				t.Fatalf("managedFolders.getIamPolicy: %v", err)
			}
			out := map[string][]string{}
			for _, b := range p.Bindings {
				out[b.Role] = b.Members
			}
			return out
		},
	}}
	runTFIAMPhases(t, m, base, [2]string{"roles/storage.objectViewer", "roles/storage.objectAdmin"}, targets)
	_, err := s.Buckets.Get(bucket).Context(ctx).Do()
	if e, ok := err.(*googleapi.Error); !ok || e.Code != 404 {
		t.Errorf("bucket %s after destroy: %v; want 404", bucket, err)
	}
}

// TestTerraformStorageObjectCustomerKey (#1304): a google_storage_bucket_object
// with customer_encryption (a customer-supplied AES-256 key, #1345) applies;
// the official client reads its content back with the key, and is refused
// it without one; its attributes name the key's SHA-256; the plan is clean;
// releasing its temporary_hold applies in place (an objects.patch; every
// other argument but event_based_hold and retention replaces the object),
// the object keeping its key and content; the plan is clean again; destroy
// deletes it.
// covers: storage.objects.insert, storage.objects.get, storage.objects.patch, storage.objects.delete
func TestTerraformStorageObjectCustomerKey(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	bucket := h.Project() + "-tfcsek"
	key := bytes.Repeat([]byte{0x5c}, 32)
	sum := sha256.Sum256(key)
	const object, content = "secret.txt", "encrypted with a customer-supplied key"
	module := func(hold bool) string {
		return fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %q
  name                        = %q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
}
resource "google_storage_bucket_object" "o" {
  bucket   = google_storage_bucket.b.name
  name     = %q
  content  = %q
  metadata = { team = "burrow" }
  temporary_hold = %t
  customer_encryption {
    encryption_algorithm = "AES256"
    encryption_key       = %q
  }
}
`, h.Project(), bucket, object, content, hold, base64.StdEncoding.EncodeToString(key))
	}
	check := func(when string, hold bool) {
		t.Helper()
		o := sc.Bucket(bucket).Object(object)
		r, err := o.Key(key).NewReader(ctx)
		if err != nil {
			t.Fatalf("%s, reading with the key: %v", when, err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || string(got) != content {
			t.Errorf("%s, the content = %q, %v; want %q", when, got, err, content)
		}
		if r, err := o.NewReader(ctx); err == nil {
			_ = r.Close()
			t.Errorf("%s, reading without the key succeeded", when)
		}
		a, err := o.Attrs(ctx)
		if err != nil {
			t.Fatalf("%s, Attrs: %v", when, err)
		}
		if a.CustomerKeySHA256 != base64.StdEncoding.EncodeToString(sum[:]) || a.Metadata["team"] != "burrow" || a.TemporaryHold != hold {
			t.Errorf("%s, the object's key SHA-256 = %q, metadata = %v, temporary hold %t; want %q, team burrow and %t",
				when, a.CustomerKeySHA256, a.Metadata, a.TemporaryHold, base64.StdEncoding.EncodeToString(sum[:]), hold)
		}
	}

	m.write(module(true))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", true)
	m.planClean()

	m.write(module(false))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("releasing the hold was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", false)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sc.Bucket(bucket).Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket after destroy: %v; want ErrBucketNotExist", err)
	}
}

// TestTerraformKMSSecretCiphertext (#1304): a google_kms_secret_ciphertext
// encrypts a plaintext, with additional authenticated data, under a
// google_kms_crypto_key; the official client decrypts the ciphertext
// Terraform stored back to the plaintext, and refuses it with other
// authenticated data; the plan is clean. Every argument of the resource
// replaces it, so there is no in-place update: a new plaintext is a new
// ciphertext, which decrypts to it. Destroy only drops it from state, as on
// Google.
// covers: google.cloud.kms.v1.KeyManagementService/Encrypt, google.cloud.kms.v1.KeyManagementService/Decrypt
func TestTerraformKMSSecretCiphertext(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	kc := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	p := h.Project()
	ringID := "tfct-" + p
	keyName := "projects/" + p + "/locations/us-central1/keyRings/" + ringID + "/cryptoKeys/k"
	const aad = "burrow"
	module := func(plaintext string) string {
		return fmt.Sprintf(`
resource "google_kms_key_ring" "kr" {
  project  = %q
  name     = %q
  location = "us-central1"
}
resource "google_kms_crypto_key" "k" {
  name     = "k"
  key_ring = google_kms_key_ring.kr.id
}
resource "google_kms_secret_ciphertext" "c" {
  crypto_key                    = google_kms_crypto_key.k.id
  plaintext                     = %q
  additional_authenticated_data = %q
}
output "ciphertext" {
  value = google_kms_secret_ciphertext.c.ciphertext
}
`, p, ringID, plaintext, aad)
	}
	check := func(when, plaintext string) {
		t.Helper()
		out := strings.TrimSpace(m.must("output", "-raw", "ciphertext"))
		ct, err := base64.StdEncoding.DecodeString(out)
		if err != nil {
			t.Fatalf("%s, the ciphertext %q is not base64: %v", when, out, err)
		}
		r, err := kc.Decrypt(ctx, &kmspb.DecryptRequest{Name: keyName, Ciphertext: ct, AdditionalAuthenticatedData: []byte(aad)})
		if err != nil || string(r.GetPlaintext()) != plaintext {
			t.Errorf("%s, Decrypt = %q, %v; want %q", when, r.GetPlaintext(), err, plaintext)
		}
		if _, err := kc.Decrypt(ctx, &kmspb.DecryptRequest{Name: keyName, Ciphertext: ct, AdditionalAuthenticatedData: []byte("other")}); err == nil {
			t.Errorf("%s, Decrypt with other authenticated data succeeded", when)
		}
	}

	m.write(module("first secret"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first secret")
	m.planClean()

	m.write(module("second secret"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "1 added, 0 changed, 1 destroyed") {
		t.Errorf("a new plaintext did not replace the ciphertext:\n%s", lastLines(out, 20))
	}
	check("after the replacement", "second secret")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := kc.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: keyName}); err != nil {
		t.Errorf("GetCryptoKey after destroy: %v; want the key kept, as on Google", err)
	}
}

// TestTerraformBigtableGCPolicy (#1304), in the emulators shard: on a
// google_bigtable_instance and a google_bigtable_table, a
// google_bigtable_gc_policy with gc_rules (a max age) applies; the official
// table admin client reads the column family's GC rule back; the plan is
// clean; a new age applies in place (max_age's block would replace the
// policy, gc_rules is updated); the plan is clean again; removing the
// policy, deletion_policy = "ABANDON", keeps the rule; destroy removes the
// instance. The provider's gc_policy uses the Go admin client, which the
// wrapper's BIGTABLE_EMULATOR_HOST points here; google_bigtable_app_profile
// and google_bigtable_instance_iam_* use the bigtableadmin REST API, in
// TestTerraformBigtableAppProfileAndIAM (#1440).
// covers: google.bigtable.admin.v2.BigtableTableAdmin/ModifyColumnFamilies, google.bigtable.admin.v2.BigtableTableAdmin/GetTable
func TestTerraformBigtableGCPolicy(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	iac := bigtableInstanceAdmin(t, h)
	ctx := h.Context()
	const id, cluster, table = "tf-bt-batch5", "tf-bt-batch5-c1", "tf-gc"
	admin, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	base := fmt.Sprintf(`
resource "google_bigtable_instance" "i" {
  project             = %q
  name                = %q
  deletion_protection = false
  cluster {
    cluster_id   = %q
    zone         = "us-central1-b"
    num_nodes    = 1
    storage_type = "SSD"
  }
}
resource "google_bigtable_table" "t" {
  project             = google_bigtable_instance.i.project
  instance_name       = google_bigtable_instance.i.name
  name                = %q
  deletion_protection = "UNPROTECTED"
  column_family { family = "cf" }
}
`, h.Project(), id, cluster, table)
	module := func(age string) string {
		return base + fmt.Sprintf(`
resource "google_bigtable_gc_policy" "g" {
  project         = google_bigtable_table.t.project
  instance_name   = google_bigtable_table.t.instance_name
  table           = google_bigtable_table.t.name
  column_family   = "cf"
  deletion_policy = "ABANDON"
  gc_rules        = jsonencode({ rules = [{ max_age = %q }] })
}
`, age)
	}
	check := func(when string, age time.Duration) {
		t.Helper()
		info, err := admin.TableInfo(ctx, table)
		if err != nil {
			t.Fatalf("%s, TableInfo: %v", when, err)
		}
		var rule string
		for _, f := range info.FamilyInfos {
			if f.Name == "cf" {
				rule = f.GCPolicy
			}
		}
		if want := bigtable.MaxAgePolicy(age).String(); rule != want {
			t.Errorf("%s, the column family's GC rule = %q; want %q", when, rule, want)
		}
	}

	m.write(module("168h"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 168*time.Hour)
	m.planClean()

	m.write(module("72h"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the policy was not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change", 72*time.Hour)
	m.planClean()

	// Abandoned, the policy leaves the family's rule as it was.
	m.write(base)
	m.apply()
	check("after the policy was abandoned", 72*time.Hour)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := iac.InstanceInfo(ctx, id); status.Code(err) != codes.NotFound {
		t.Errorf("InstanceInfo after destroy: %v; want NotFound", err)
	}
}

// TestTerraformArtifactRegistryRepositoryIAM (#1304), in the run shard:
// google_artifact_registry_repository_iam_member, _binding and _policy on a
// google_artifact_registry_repository, through TestTerraformBigQueryIAM's
// phases, each read back through the official artifactregistry client's
// GetIamPolicy and planned clean. Stored, never enforced (ADR-0006); a
// policy change leaves the repository alone. Destroy removes the
// repository, and its policy with it.
// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/GetIamPolicy, google.devtools.artifactregistry.v1.ArtifactRegistry/SetIamPolicy
func TestTerraformArtifactRegistryRepositoryIAM(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx := h.Context()
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ar.Close() }()
	id := "tf-repo-iam-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/repositories/" + id
	base := fmt.Sprintf(`
resource "google_artifact_registry_repository" "r" {
  project       = %q
  location      = "us-central1"
  repository_id = %q
  format        = "DOCKER"
  description   = "stored IAM"
}
`, h.Project(), id)
	targets := []tfIAMTarget{{
		kind: "google_artifact_registry_repository", label: "r", policyData: true,
		ref: map[string]string{"project": "google_artifact_registry_repository.r.project", "location": "google_artifact_registry_repository.r.location",
			"repository": "google_artifact_registry_repository.r.name"},
		read: func() map[string][]string {
			t.Helper()
			pol, err := ar.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", name, err)
			}
			if r, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name}); err != nil || r.GetDescription() != "stored IAM" {
				t.Errorf("the repository beside its policy = %v, %v", r, err)
			}
			return policyMap(pol)
		},
	}}
	runTFIAMPhases(t, m, base, [2]string{"roles/artifactregistry.reader", "roles/artifactregistry.writer"}, targets)
	if _, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after destroy: %v; want NotFound", err)
	}
	if _, err := ar.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy after destroy: %v; want NotFound", err)
	}
}
