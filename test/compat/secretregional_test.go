//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// regionalParent is the parent of a project's regional secrets in
// us-central1 (#1519).
func regionalParent(h *Harness) string { return "projects/" + h.Project() + "/locations/us-central1" }

// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/ListSecrets, google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret, google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret, google.cloud.secretmanager.v1.SecretManagerService/AddSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/GetSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/ListSecretVersions, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/DisableSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/EnableSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/DestroySecretVersion, google.cloud.secretmanager.v1.SecretManagerService/SetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/GetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/TestIamPermissions
//
// TestSecretManagerRegionalSecrets (#1519), through
// cloud.google.com/go/secretmanager/apiv1 over gRPC and REST, pointed as a
// program points it at secretmanager.us-central1.rep.googleapis.com, and
// cloud.google.com/go/kms/apiv1 against the instance's Cloud KMS: a regional
// secret under projects/*/locations/us-central1 takes no replication policy
// (INVALID_ARGUMENT) and a customer-managed key in its own location (a key
// elsewhere is INVALID_ARGUMENT); it lists in its location and not among the
// project's global secrets; a version reports the key version that
// encrypted it in customer_managed_encryption, reads back, disables, enables
// and is destroyed; the key changes and is removed by an UpdateSecret naming
// customer_managed_encryption; its IAM policy is stored (ADR-0006); and it
// is deleted.
func TestSecretManagerRegionalSecrets(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	clients := map[string]*secretmanager.Client{"grpc": secretsClient(t, h), "rest": secretsRESTClient(t, h)}
	kc := kmsClients(t, h)["grpc"]
	keys := cmekKeys(t, h, kc, "us-central1", "us-east1")
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			code := func(err error) codes.Code { return kmsCode(transport, err) }
			id := "regional-" + transport
			name := regionalParent(h) + "/secrets/" + id
			t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })

			if _, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: regionalParent(h), SecretId: id,
				Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); code(err) != codes.InvalidArgument {
				t.Errorf("a regional secret with replication = %v; want InvalidArgument", err)
			}
			if _, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: regionalParent(h), SecretId: id,
				Secret: &secretmanagerpb.Secret{CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{
					KmsKeyName: keys["us-east1/k"]}}}); code(err) != codes.InvalidArgument {
				t.Errorf("a key in another location = %v; want InvalidArgument", err)
			}
			sec, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: regionalParent(h), SecretId: id,
				Secret: &secretmanagerpb.Secret{Labels: map[string]string{"env": "dev"},
					CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: keys["us-central1/k"]}}})
			if err != nil {
				t.Fatal(err)
			}
			if sec.GetName() != name || sec.GetReplication() != nil || sec.GetCustomerManagedEncryption().GetKmsKeyName() != keys["us-central1/k"] {
				t.Errorf("created %v", sec)
			}
			listed := func(parent string) bool {
				it := c.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: parent})
				for {
					s, err := it.Next()
					if err != nil {
						return false
					}
					if s.GetName() == name {
						return true
					}
				}
			}
			if !listed(regionalParent(h)) || listed(secretsParent(h)) {
				t.Errorf("listed in its location %v, among global secrets %v; want true, false", listed(regionalParent(h)), listed(secretsParent(h)))
			}

			v, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: name,
				Payload: &secretmanagerpb.SecretPayload{Data: []byte("regional")}})
			if err != nil {
				t.Fatal(err)
			}
			if got := v.GetCustomerManagedEncryption().GetKmsKeyVersionName(); got != keys["us-central1/k"]+"/cryptoKeyVersions/1" {
				t.Errorf("customer_managed_encryption.kms_key_version_name = %q", got)
			}
			if v.GetReplicationStatus() != nil {
				t.Errorf("a regional version has replication_status %v", v.GetReplicationStatus())
			}
			acc, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name + "/versions/latest"})
			if err != nil || string(acc.GetPayload().GetData()) != "regional" {
				t.Fatalf("AccessSecretVersion = %v, %v", acc, err)
			}
			if _, err := c.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: v.GetName()}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: v.GetName()}); code(err) != codes.FailedPrecondition {
				t.Errorf("access a disabled version = %v; want FailedPrecondition", err)
			}
			if _, err := c.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: v.GetName()}); err != nil {
				t.Fatal(err)
			}
			if got, err := c.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: v.GetName()}); err != nil || got.GetState() != secretmanagerpb.SecretVersion_ENABLED {
				t.Errorf("GetSecretVersion = %v, %v", got, err)
			}

			// The key changes for new versions, and is removed.
			up, err := c.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
				Secret:     &secretmanagerpb.Secret{Name: name, CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: keys["us-central1/k2"]}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"customer_managed_encryption"}}})
			if err != nil || up.GetCustomerManagedEncryption().GetKmsKeyName() != keys["us-central1/k2"] {
				t.Fatalf("UpdateSecret to k2 = %v, %v", up, err)
			}
			v2, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: name,
				Payload: &secretmanagerpb.SecretPayload{Data: []byte("two")}})
			if err != nil || v2.GetCustomerManagedEncryption().GetKmsKeyVersionName() != keys["us-central1/k2"]+"/cryptoKeyVersions/1" {
				t.Errorf("version 2 = %v, %v; want encrypted with k2", v2, err)
			}
			up, err = c.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: name},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"customer_managed_encryption"}}})
			if err != nil || up.GetCustomerManagedEncryption() != nil {
				t.Errorf("UpdateSecret removing the key = %v, %v", up, err)
			}
			if _, err := c.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: name},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replication"}}}); code(err) != codes.InvalidArgument {
				t.Errorf("UpdateSecret naming replication = %v; want InvalidArgument", err)
			}
			n := 0
			for it := c.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: name}); ; n++ {
				if _, err := it.Next(); err != nil {
					break
				}
			}
			if n != 2 {
				t.Errorf("ListSecretVersions listed %d versions; want 2", n)
			}
			if d, err := c.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v.GetName()}); err != nil || d.GetState() != secretmanagerpb.SecretVersion_DESTROYED {
				t.Errorf("DestroySecretVersion = %v, %v", d, err)
			}

			// Stored, not enforced (ADR-0006).
			member := "user:regional@example.com"
			if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{
				Bindings: []*iampb.Binding{{Role: "roles/secretmanager.secretAccessor", Members: []string{member}}}}}); err != nil {
				t.Fatal(err)
			}
			pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil || len(pol.GetBindings()) != 1 || pol.GetBindings()[0].GetMembers()[0] != member {
				t.Errorf("GetIamPolicy = %v, %v", pol, err)
			}
			perms, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name,
				Permissions: []string{"secretmanager.versions.access"}})
			if err != nil || len(perms.GetPermissions()) != 1 {
				t.Errorf("TestIamPermissions = %v, %v", perms, err)
			}

			if err := c.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: name}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); code(err) != codes.NotFound {
				t.Errorf("GetSecret after delete = %v; want NotFound", err)
			}
		})
	}
}

// TestGcloudRegionalSecrets (#1519): through gcloud-setup's configuration,
// whose secretmanager override gcloud uses in place of the regional
// endpoint, `gcloud secrets --location=us-central1` creates a regional
// secret with a key from `gcloud kms`'s own instance, adds and accesses a
// version, updates its labels, describes it, lists it in its location and
// deletes it.
func TestGcloudRegionalSecrets(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvSecrets)
	keys := cmekKeys(t, h, kmsClients(t, h)["grpc"], "us-central1")
	g := newGcloudSession(t, h)
	id := "gcloud-regional"
	loc := "--location=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "secrets", "delete", id, loc, "--quiet") })
	g.must("secrets", "create", id, loc, "--labels=env=dev", "--regional-kms-key-name="+keys["us-central1/k"])
	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payload, []byte("r3gional"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.must("secrets", "versions", "add", id, loc, "--data-file="+payload)
	if got := strings.TrimSpace(g.must("secrets", "versions", "access", "latest", "--secret="+id, loc)); got != "r3gional" {
		t.Errorf("versions access = %q", got)
	}
	g.must("secrets", "update", id, loc, "--update-labels=team=a")
	got := g.must("secrets", "describe", id, loc, "--format=value(name,labels,customerManagedEncryption.kmsKeyName)")
	if !strings.Contains(got, "/locations/us-central1/secrets/"+id) || !strings.Contains(got, "team=a") || !strings.Contains(got, keys["us-central1/k"]) {
		t.Errorf("describe = %q", got)
	}
	if got := g.must("secrets", "list", loc, "--format=value(name)"); !strings.Contains(got, id) {
		t.Errorf("list = %q", got)
	}
	g.must("secrets", "delete", id, loc, "--quiet")
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret
//
// TestTerraformRegionalSecret (#1519): through `cloudburrow terraform`,
// which sets secret_manager_regional_custom_endpoint, a
// google_secret_manager_regional_secret with a customer-managed key in its
// location, a google_secret_manager_regional_secret_version and a
// google_secret_manager_regional_secret_iam_member apply, read back through
// the official client, plan clean, change in place (labels, the key, the
// member's role) and plan clean, and destroy.
func TestTerraformRegionalSecret(t *testing.T) {
	h := New(t)
	sm := secretsClient(t, h)
	keys := cmekKeys(t, h, kmsClients(t, h)["grpc"], "us-central1")
	m := newTFModule(t)
	ctx := h.Context()
	p := h.Project()
	name := regionalParent(h) + "/secrets/tf-regional"
	module := func(env, key, role string) string {
		return fmt.Sprintf(`
resource "google_secret_manager_regional_secret" "s" {
  project   = %[1]q
  location  = "us-central1"
  secret_id = "tf-regional"
  labels    = { env = %[2]q }
  customer_managed_encryption {
    kms_key_name = %[3]q
  }
}
resource "google_secret_manager_regional_secret_version" "v" {
  secret      = google_secret_manager_regional_secret.s.id
  secret_data = "tf-data"
}
resource "google_secret_manager_regional_secret_iam_member" "m" {
  project   = %[1]q
  location  = "us-central1"
  secret_id = google_secret_manager_regional_secret.s.secret_id
  role      = %[4]q
  member    = "user:tf@example.com"
}
`, p, env, key, role)
	}
	check := func(when, env, key, role string) {
		t.Helper()
		s, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, GetSecret: %v", when, err)
		}
		if s.GetLabels()["env"] != env || s.GetCustomerManagedEncryption().GetKmsKeyName() != key {
			t.Errorf("%s, secret = %v", when, s)
		}
		acc, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name + "/versions/1"})
		if err != nil || string(acc.GetPayload().GetData()) != "tf-data" {
			t.Errorf("%s, AccessSecretVersion = %v, %v", when, acc, err)
		}
		pol, err := sm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		if err != nil || len(pol.GetBindings()) != 1 || pol.GetBindings()[0].GetRole() != role {
			t.Errorf("%s, GetIamPolicy = %v, %v", when, pol, err)
		}
	}

	m.write(module("dev", keys["us-central1/k"], "roles/secretmanager.secretAccessor"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "dev", keys["us-central1/k"], "roles/secretmanager.secretAccessor")
	m.planClean()

	m.write(module("prod", keys["us-central1/k2"], "roles/secretmanager.viewer"))
	m.apply()
	check("after the change", "prod", keys["us-central1/k2"], "roles/secretmanager.viewer")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); kmsCode("grpc", err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
}
