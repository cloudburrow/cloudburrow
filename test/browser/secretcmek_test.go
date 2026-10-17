//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const envSecrets = "CLOUDBURROW_TEST_SECRETS"

// TestKMSKeyChangesASecretsEncryptionThroughTheBrowser (#1453), in the run
// shard, whose instance serves Cloud KMS beside Secret Manager: a secret's
// page has Change encryption keys, empty for a Google-managed secret; a key
// that is not a CryptoKey name is refused on the form; a Cloud KMS key saved
// there is the one the official Secret Manager client's GetSecret reads under
// automatic replication, and a version added after it is encrypted with that
// key's first version; cleared, GetSecret reads no customer-managed key.
func TestKMSKeyChangesASecretsEncryptionThroughTheBrowser(t *testing.T) {
	needService(t, "kms")
	needService(t, "secrets")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envSecrets)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the secret is read back through the Secret Manager API itself", envSecrets)
	}
	ctx := context.Background()
	c, err := secretmanager.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := uniqueProject(t)

	ring := "projects/" + project + "/locations/global/keyRings/browser-cmek"
	key := ring + "/cryptoKeys/k"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms?project="+project,
		`{"keyRingId":"browser-cmek","location":"global"}`); code != http.StatusOK {
		t.Fatalf("create a key ring through the console API = %d: %s", code, body)
	}
	create, _ := json.Marshal(map[string]any{"Path": []string{ring}, "Action": "createkey",
		"Values": map[string]string{"cryptoKeyId": "k"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/kms?project="+project, string(create)); code != http.StatusOK {
		t.Fatalf("create a key through the console API = %d: %s", code, body)
	}
	id := "browser-cmek"
	name := "projects/" + project + "/secrets/" + id
	if _, err := c.CreateSecret(ctx, &smpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: id,
		Secret: &smpb.Secret{Replication: &smpb.Replication{Replication: &smpb.Replication_Automatic_{Automatic: &smpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &smpb.DeleteSecretRequest{Name: name}) })

	p.navigate("/secrets?project=" + project)
	p.clickText("#view tbody a", name)
	openForm := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Change encryption keys")
		p.waitFor(`document.querySelector(".modal.is-open #f-kmsKeys") !== null`)
	}
	submit := func(v string) {
		setField(p, ".modal.is-open", "f-kmsKeys", v)
		p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	}
	openForm()
	var prefilled string
	p.eval(`document.querySelector(".modal.is-open #f-kmsKeys").value`, &prefilled)
	if prefilled != "" {
		t.Errorf("Change encryption keys is prefilled with %q for a Google-managed secret; want empty", prefilled)
	}

	// Not a CryptoKey name: refused on the form, nothing changed.
	submit("projects/" + project + "/keyRings/browser-cmek")
	p.waitFor(`!document.querySelector(".modal.is-open .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal.is-open .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "CryptoKey") {
		t.Errorf("a key ring for a key is refused with %q; want the CryptoKey name's form", refusal)
	}
	p.forgive() // the refusal's 400, provoked on purpose

	submit(key)
	p.waitFor(`document.querySelector(".modal") === null`)
	got, err := c.GetSecret(ctx, &smpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if k := got.GetReplication().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyName(); k != key {
		t.Fatalf("after Change encryption keys GetSecret reads key %q, want %q", k, key)
	}
	v, err := c.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{Parent: name, Payload: &smpb.SecretPayload{Data: []byte("sealed")}})
	if err != nil {
		t.Fatalf("AddSecretVersion with the key set in the browser: %v", err)
	}
	if kv := v.GetReplicationStatus().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyVersionName(); kv != key+"/cryptoKeyVersions/1" {
		t.Errorf("the new version was encrypted with %q, want %s/cryptoKeyVersions/1", kv, key)
	}
	if a, err := c.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{Name: v.GetName()}); err != nil || string(a.GetPayload().GetData()) != "sealed" {
		t.Errorf("AccessSecretVersion = %v, %v; want the payload back", a, err)
	}

	// Cleared: Google-managed again.
	openForm()
	p.eval(`document.querySelector(".modal.is-open #f-kmsKeys").value`, &prefilled)
	if prefilled != key {
		t.Errorf("Change encryption keys is prefilled with %q, want %q", prefilled, key)
	}
	submit("")
	p.waitFor(`document.querySelector(".modal") === null`)
	if got, err := c.GetSecret(ctx, &smpb.GetSecretRequest{Name: name}); err != nil ||
		got.GetReplication().GetAutomatic().GetCustomerManagedEncryption() != nil {
		t.Errorf("after clearing the keys GetSecret = %v, %v; want automatic replication with no customer-managed key", got.GetReplication(), err)
	}
	if sent := p.sent(http.MethodPost, "/api/actions/secrets"); len(sent) != 3 {
		t.Errorf("three submits sent %d actions: %v", len(sent), sent)
	}
}
