package secrets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Regional secrets (#1519): projects/*/locations/*/secrets, kept apart from
// the project's global secrets, with the secret's own customer-managed key
// in its location and no replication policy.
func TestRegionalSecretLifecycle(t *testing.T) {
	s, _ := cmekStore(t)
	g := NewGRPCServer(s)
	ctx := context.Background()
	parent := "projects/p/locations/us-central1"

	if _, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "r",
		Secret: &secretmanagerpb.Secret{Replication: automaticCMEK(globalKey)}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a regional secret with replication: %v; want InvalidArgument", err)
	}
	if _, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "r",
		Secret: &secretmanagerpb.Secret{CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: eastKey}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a key in another location: %v; want InvalidArgument", err)
	}
	sec, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "r",
		Secret: &secretmanagerpb.Secret{Labels: map[string]string{"a": "b"},
			CustomerManagedEncryption: &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: centralKey}}})
	if err != nil {
		t.Fatal(err)
	}
	if sec.GetName() != parent+"/secrets/r" || sec.GetReplication() != nil || sec.GetCustomerManagedEncryption().GetKmsKeyName() != centralKey {
		t.Fatalf("created %v", sec)
	}
	// A global secret of the same ID is another secret.
	if _, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/p", SecretId: "r"}); err != nil {
		t.Fatalf("global secret of the same ID: %v", err)
	}
	for parent, want := range map[string]int{"projects/p": 1, parent: 1, "projects/p/locations/us-east1": 0} {
		l, err := g.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: parent})
		if err != nil || len(l.GetSecrets()) != want {
			t.Errorf("ListSecrets(%s) = %v, %v; want %d", parent, l.GetSecrets(), err, want)
		}
	}
	v, err := g.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: sec.GetName(),
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("hush")}})
	if err != nil {
		t.Fatal(err)
	}
	if v.GetName() != sec.GetName()+"/versions/1" || v.GetReplicationStatus() != nil ||
		v.GetCustomerManagedEncryption().GetKmsKeyVersionName() != centralKey+"/cryptoKeyVersions/1" {
		t.Fatalf("version %v", v)
	}
	acc, err := g.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: sec.GetName() + "/versions/latest"})
	if err != nil || string(acc.GetPayload().GetData()) != "hush" {
		t.Fatalf("access: %v, %v", acc, err)
	}
	// The key can be removed; replication cannot be named.
	if _, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: sec.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replication"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("update replication: %v; want InvalidArgument", err)
	}
	up, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: sec.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"customer_managed_encryption"}}})
	if err != nil || up.GetCustomerManagedEncryption() != nil {
		t.Fatalf("remove the key: %v, %v", up, err)
	}

	// The JSON API serves the same secret under the regional path.
	router := rest.NewRouter()
	NewRESTServer(s).Routes(router)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/"+sec.GetName(), nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "replication\"") || !strings.Contains(rec.Body.String(), sec.GetName()) {
		t.Errorf("GET %s = %d %s", sec.GetName(), rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/"+parent+"/secrets?secretId=j",
		strings.NewReader(`{"customerManagedEncryption":{"kmsKeyName":"`+centralKey+`"}}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"customerManagedEncryption"`) {
		t.Errorf("POST a regional secret = %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/"+sec.GetName()+"/versions/1:access", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "aHVzaA==") {
		t.Errorf("access over JSON = %d %s", rec.Code, rec.Body)
	}

	if _, err := g.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: sec.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/p/secrets/r"}); err != nil {
		t.Errorf("deleting the regional secret touched the global one: %v", err)
	}
}
