//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// cmekKeys creates, through the official Cloud KMS client, a key ring in
// each location with one ENCRYPT_DECRYPT key "k" (and "k2" in global), and
// returns the key names by location. Key rings cannot be deleted, so the
// ring ID is unique to the run.
func cmekKeys(t *testing.T, h *Harness, kc *kms.KeyManagementClient, locations ...string) map[string]string {
	t.Helper()
	ctx := h.Context()
	ring := fmt.Sprintf("cmek-%d", time.Now().UnixNano())
	out := map[string]string{}
	for _, l := range locations {
		parent := "projects/" + h.Project() + "/locations/" + l
		if _, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: ring, KeyRing: &kmspb.KeyRing{}}); err != nil {
			t.Fatalf("CreateKeyRing in %s: %v", l, err)
		}
		ids := []string{"k"}
		if l == "global" {
			ids = append(ids, "k2")
		}
		for _, id := range ids {
			k, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: parent + "/keyRings/" + ring, CryptoKeyId: id,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
			if err != nil {
				t.Fatalf("CreateCryptoKey %s in %s: %v", id, l, err)
			}
			out[l+"/"+id] = k.GetName()
		}
	}
	return out
}

// setKeyVersionState enables or disables a key's version 1.
func setKeyVersionState(t *testing.T, h *Harness, kc *kms.KeyManagementClient, key string, state kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	t.Helper()
	if _, err := kc.UpdateCryptoKeyVersion(h.Context(), &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: state},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatalf("UpdateCryptoKeyVersion %s to %s: %v", key, state, err)
	}
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/AddSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/GetSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret
//
// TestSecretManagerCMEK (#1453), through cloud.google.com/go/secretmanager/apiv1
// over gRPC and REST and cloud.google.com/go/kms/apiv1 against the instance's
// Cloud KMS: a secret with automatic replication takes a global key and a
// user-managed replica a key in its own location (another location is
// INVALID_ARGUMENT); a version's replication_status names the key version
// that encrypted it, and it reads back; with the key version disabled,
// AccessSecretVersion and AddSecretVersion are FAILED_PRECONDITION, and work
// again once it is enabled; UpdateSecret on replication changes the key for
// new versions only, and removes it.
func TestSecretManagerCMEK(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	kc := kmsClients(t, h)["grpc"]
	keys := cmekKeys(t, h, kc, "global", "us-east1")
	for transport, c := range map[string]*secretmanager.Client{"grpc": secretsClient(t, h), "rest": secretsRESTClient(t, h)} {
		t.Run(transport, func(t *testing.T) {
			code := func(err error) codes.Code { return kmsCode(transport, err) }
			// FAILED_PRECONDITION over REST too: the JSON error carries Google's
			// status since #1470, which kmsCode reads.
			failedPrecondition := codes.FailedPrecondition
			id := "cmek-" + transport
			name := secretsParent(h) + "/secrets/" + id
			t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })

			if _, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id + "-bad",
				Secret: &secretmanagerpb.Secret{Replication: cmekAutomatic(keys["us-east1/k"])}}); code(err) != codes.InvalidArgument {
				t.Errorf("automatic replication with a regional key = %v; want InvalidArgument", err)
			}
			if _, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id + "-bad",
				Secret: &secretmanagerpb.Secret{Replication: cmekReplica("europe-west1", keys["us-east1/k"])}}); code(err) != codes.InvalidArgument {
				t.Errorf("a replica with a key in another location = %v; want InvalidArgument", err)
			}

			sec, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
				Secret: &secretmanagerpb.Secret{Replication: cmekAutomatic(keys["global/k"])}})
			if err != nil {
				t.Fatal(err)
			}
			if got := sec.GetReplication().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyName(); got != keys["global/k"] {
				t.Errorf("created kms_key_name = %q", got)
			}
			add := func(data string) (*secretmanagerpb.SecretVersion, error) {
				return c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: name,
					Payload: &secretmanagerpb.SecretPayload{Data: []byte(data)}})
			}
			v, err := add("first")
			if err != nil {
				t.Fatal(err)
			}
			if got := v.GetReplicationStatus().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyVersionName(); got != keys["global/k"]+"/cryptoKeyVersions/1" {
				t.Errorf("kms_key_version_name = %q", got)
			}
			got, err := c.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: v.GetName()})
			if err != nil || got.GetReplicationStatus().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyVersionName() == "" {
				t.Errorf("GetSecretVersion = %v, %v", got, err)
			}
			access := func(version string) (string, error) {
				r, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name + "/versions/" + version})
				return string(r.GetPayload().GetData()), err
			}
			if data, err := access("1"); err != nil || data != "first" {
				t.Fatalf("AccessSecretVersion = %q, %v", data, err)
			}

			setKeyVersionState(t, h, kc, keys["global/k"], kmspb.CryptoKeyVersion_DISABLED)
			if _, err := access("1"); code(err) != failedPrecondition || !strings.Contains(err.Error(), keys["global/k"]) {
				t.Errorf("access with the key disabled = %v; want FailedPrecondition naming the key", err)
			}
			if _, err := add("second"); code(err) != failedPrecondition {
				t.Errorf("add with the key disabled = %v; want FailedPrecondition", err)
			}
			setKeyVersionState(t, h, kc, keys["global/k"], kmspb.CryptoKeyVersion_ENABLED)
			if data, err := access("latest"); err != nil || data != "first" {
				t.Errorf("access after re-enabling = %q, %v", data, err)
			}

			// The key changes for new versions; version 1 keeps its own.
			upd, err := c.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
				Secret:     &secretmanagerpb.Secret{Name: name, Replication: cmekAutomatic(keys["global/k2"])},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replication"}}})
			if err != nil || upd.GetReplication().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyName() != keys["global/k2"] {
				t.Fatalf("UpdateSecret to k2 = %v, %v", upd, err)
			}
			v2, err := add("second")
			if err != nil || v2.GetReplicationStatus().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyVersionName() != keys["global/k2"]+"/cryptoKeyVersions/1" {
				t.Fatalf("version 2 = %v, %v", v2, err)
			}
			if data, err := access("1"); err != nil || data != "first" {
				t.Errorf("version 1 after the key change = %q, %v", data, err)
			}
			upd, err = c.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
				Secret: &secretmanagerpb.Secret{Name: name, Replication: &secretmanagerpb.Replication{
					Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replication"}}})
			if err != nil || upd.GetReplication().GetAutomatic().GetCustomerManagedEncryption() != nil {
				t.Errorf("UpdateSecret removing the key = %v, %v", upd, err)
			}

			// A user-managed replica with its own regional key.
			umName := name + "-um"
			t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: umName}) })
			if _, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id + "-um",
				Secret: &secretmanagerpb.Secret{Replication: cmekReplica("us-east1", keys["us-east1/k"])}}); err != nil {
				t.Fatal(err)
			}
			uv, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: umName,
				Payload: &secretmanagerpb.SecretPayload{Data: []byte("regional")}})
			if err != nil {
				t.Fatal(err)
			}
			reps := uv.GetReplicationStatus().GetUserManaged().GetReplicas()
			if len(reps) != 1 || reps[0].GetLocation() != "us-east1" ||
				reps[0].GetCustomerManagedEncryption().GetKmsKeyVersionName() != keys["us-east1/k"]+"/cryptoKeyVersions/1" {
				t.Errorf("user-managed replication_status = %v", reps)
			}
		})
	}
}

func cmekAutomatic(key string) *secretmanagerpb.Replication {
	return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{
		CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: key}}}}
}

func cmekReplica(location, key string) *secretmanagerpb.Replication {
	return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_UserManaged_{UserManaged: &secretmanagerpb.Replication_UserManaged{
		Replicas: []*secretmanagerpb.Replication_UserManaged_Replica{{Location: location,
			CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: key}}}}}}
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret
//
// TestGcloudSecretsCMEK (#1453): the real gcloud creates a secret with
// `--kms-key-name`, adds and reads a version, describes the key version that
// encrypted it, and changes the key with `secrets replication update
// --set-kms-key` and removes it with `--remove-cmek`.
func TestGcloudSecretsCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvSecrets)
	h.Endpoint(EnvKMS)
	keys := cmekKeys(t, h, kmsClients(t, h)["grpc"], "global")
	g := newGcloudSession(t, h)
	id := "gcloud-cmek"
	t.Cleanup(func() { _, _ = g.run(nil, "secrets", "delete", id, "--quiet") })
	g.must("secrets", "create", id, "--replication-policy=automatic", "--kms-key-name="+keys["global/k"])
	if got := g.must("secrets", "describe", id, "--format=value(replication.automatic.customerManagedEncryption.kmsKeyName)"); strings.TrimSpace(got) != keys["global/k"] {
		t.Errorf("describe kmsKeyName = %q", got)
	}
	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payload, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.must("secrets", "versions", "add", id, "--data-file="+payload)
	if got := strings.TrimSpace(g.must("secrets", "versions", "access", "1", "--secret="+id)); got != "s3cret" {
		t.Errorf("versions access = %q", got)
	}
	if got := g.must("secrets", "versions", "describe", "1", "--secret="+id,
		"--format=value(replicationStatus.automatic.customerManagedEncryption.kmsKeyVersionName)"); strings.TrimSpace(got) != keys["global/k"]+"/cryptoKeyVersions/1" {
		t.Errorf("versions describe kmsKeyVersionName = %q", got)
	}
	g.must("secrets", "replication", "update", id, "--set-kms-key="+keys["global/k2"])
	if got := g.must("secrets", "describe", id, "--format=value(replication.automatic.customerManagedEncryption.kmsKeyName)"); strings.TrimSpace(got) != keys["global/k2"] {
		t.Errorf("kmsKeyName after set-kms-key = %q", got)
	}
	g.must("secrets", "replication", "update", id, "--remove-cmek")
	if got := g.must("secrets", "describe", id, "--format=value(replication.automatic.customerManagedEncryption.kmsKeyName)"); strings.TrimSpace(got) != "" {
		t.Errorf("kmsKeyName after remove-cmek = %q", got)
	}
	g.must("secrets", "delete", id, "--quiet")
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/AddSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion
//
// TestTerraformSecretCMEK (#1453): a google_secret_manager_secret whose
// automatic replication has a customer_managed_encryption key from a
// google_kms_crypto_key, with a google_secret_manager_secret_version,
// applies; the official client reads the key back and the version's
// key version, and accesses the data; the plan is clean; destroy removes
// the secret.
func TestTerraformSecretCMEK(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvSecrets)
	h.Endpoint(EnvKMS)
	m := newTFModule(t)
	sm := secretsClient(t, h)
	ctx := h.Context()
	p := h.Project()
	ring := fmt.Sprintf("tfcmek-%d", time.Now().UnixNano())
	key := "projects/" + p + "/locations/global/keyRings/" + ring + "/cryptoKeys/k"
	name := "projects/" + p + "/secrets/tf-cmek"
	m.write(fmt.Sprintf(`
resource "google_kms_key_ring" "kr" {
  project  = %q
  name     = %q
  location = "global"
}
resource "google_kms_crypto_key" "k" {
  name     = "k"
  key_ring = google_kms_key_ring.kr.id
}
resource "google_secret_manager_secret" "s" {
  project   = %q
  secret_id = "tf-cmek"
  replication {
    auto {
      customer_managed_encryption {
        kms_key_name = google_kms_crypto_key.k.id
      }
    }
  }
}
resource "google_secret_manager_secret_version" "v" {
  secret      = google_secret_manager_secret.s.id
  secret_data = "from terraform"
}
`, p, ring, p))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	sec, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if got := sec.GetReplication().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyName(); got != key {
		t.Errorf("kms_key_name = %q; want %q", got, key)
	}
	v, err := sm.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: name + "/versions/1"})
	if err != nil || v.GetReplicationStatus().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" {
		t.Errorf("version = %v, %v", v, err)
	}
	r, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name + "/versions/1"})
	if err != nil || string(r.GetPayload().GetData()) != "from terraform" {
		t.Errorf("AccessSecretVersion = %q, %v", r.GetPayload().GetData(), err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); err == nil {
		t.Error("the secret survived destroy")
	}
}
