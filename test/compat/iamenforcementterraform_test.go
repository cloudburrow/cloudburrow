//go:build compat

package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adminpb "cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret, google.iam.admin.v1.IAM/CreateServiceAccountKey
//
// TestIAMEnforcementTerraform: `cloudburrow terraform` on an instance
// with --iam-enforcement=on, hashicorp/google signing in itself. With no
// credentials named, the provider signs in with the instance's own
// fixture and applies a google_secret_manager_secret (before, the
// wrapper's fixed "owner" token was refused 401 on every call). With
// GOOGLE_CREDENTIALS naming a key the IAM API made for a service account
// of the test's own, the provider exchanges the key at its local
// token_uri: without a role, applying is Error 403 naming
// secretmanager.secrets.create, not 401; with roles/secretmanager.admin on
// the project the apply succeeds, plans clean and destroys, as that
// account.
func TestIAMEnforcementTerraform(t *testing.T) {
	h := New(t)
	sm := secretsClient(t, h) // asks for the Secret Manager endpoint first
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ietf-")
	body := func(id string) string {
		return fmt.Sprintf(`
resource "google_secret_manager_secret" "s" {
  project   = %q
  secret_id = %q
  replication {
    auto {}
  }
}
`, project, id)
	}
	exists := func(id string) bool {
		_, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/" + project + "/secrets/" + id})
		if err != nil && status.Code(err) != codes.NotFound {
			t.Fatalf("GetSecret %s: %v", id, err)
		}
		return err == nil
	}

	// The instance's own account.
	own := newTFModule(t)
	own.write(body("tf-owner"))
	own.must("init", "-input=false", "-no-color")
	own.apply()
	if !exists("tf-owner") {
		t.Fatal("the instance's own apply made no secret")
	}
	own.must("destroy", "-auto-approve", "-input=false", "-no-color")

	// A service account of the test's own, through its key file.
	sa := newServiceAccount(t, h, project, "tf")
	key, err := iamAdmin(t, h).CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()})
	if err != nil {
		t.Fatalf("CreateServiceAccountKey: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(keyPath, key.GetPrivateKeyData(), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTFModule(t)
	m.env = []string{"GOOGLE_CREDENTIALS=" + keyPath}
	m.write(body("tf-principal"))
	m.must("init", "-input=false", "-no-color")
	out, err := m.tf("apply", "-auto-approve", "-input=false", "-no-color")
	if err == nil || !strings.Contains(out, "Error 403") || !strings.Contains(out, "secretmanager.secrets.create") || strings.Contains(out, "Error 401") {
		t.Fatalf("apply without a role = %v; want Error 403 naming secretmanager.secrets.create:\n%s", err, lastLines(out, 20))
	}
	if exists("tf-principal") {
		t.Fatal("the refused apply made the secret")
	}

	change := projectRoles(t, h, project, "serviceAccount:"+sa.GetEmail())
	change("roles/secretmanager.admin", true)
	t.Cleanup(func() { change("roles/secretmanager.admin", false) })
	m.apply()
	if !exists("tf-principal") {
		t.Fatal("the granted apply made no secret")
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if exists("tf-principal") {
		t.Error("the secret is left after destroy")
	}
}
