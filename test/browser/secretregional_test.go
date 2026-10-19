//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestKMSKeyEncryptsARegionalSecretThroughTheBrowser (#1519), in the run
// shard, which serves Cloud KMS beside Secret Manager: Create secret with a
// Location and a Cloud KMS key in that location makes a regional secret,
// which the official Secret Manager client reads at
// projects/P/locations/us-central1/secrets/S with that key and no
// replication, its first version encrypted with the key and readable; the
// list shows it with its location, and its page opens.
func TestKMSKeyEncryptsARegionalSecretThroughTheBrowser(t *testing.T) {
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

	ring := "projects/" + project + "/locations/us-central1/keyRings/browser-regional"
	key := ring + "/cryptoKeys/k"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms?project="+project,
		`{"keyRingId":"browser-regional","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a key ring through the console API = %d: %s", code, body)
	}
	create, _ := json.Marshal(map[string]any{"Path": []string{ring}, "Action": "createkey",
		"Values": map[string]string{"cryptoKeyId": "k"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/kms?project="+project, string(create)); code != http.StatusOK {
		t.Fatalf("create a key through the console API = %d: %s", code, body)
	}
	id := "browser-regional"
	name := "projects/" + project + "/locations/us-central1/secrets/" + id
	t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &smpb.DeleteSecretRequest{Name: name}) })

	p.navigate("/secrets/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-location") !== null`)
	setField(p, "#view", "f-secretId", id)
	setField(p, "#view", "f-payload", "regional value")
	setField(p, "#view", "f-location", "us-central1")
	setField(p, "#view", "f-kmsKeys", key)
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`location.pathname === "/secrets"`)

	got, err := c.GetSecret(ctx, &smpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatalf("GetSecret of the regional secret made in the browser: %v", err)
	}
	if got.GetCustomerManagedEncryption().GetKmsKeyName() != key || got.GetReplication() != nil {
		t.Errorf("GetSecret = %v; want customer_managed_encryption %s and no replication", got, key)
	}
	v, err := c.GetSecretVersion(ctx, &smpb.GetSecretVersionRequest{Name: name + "/versions/1"})
	if err != nil || v.GetCustomerManagedEncryption().GetKmsKeyVersionName() != key+"/cryptoKeyVersions/1" {
		t.Errorf("GetSecretVersion = %v, %v; want encrypted with %s/cryptoKeyVersions/1", v, err, key)
	}
	if a, err := c.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{Name: name + "/versions/1"}); err != nil ||
		string(a.GetPayload().GetData()) != "regional value" {
		t.Errorf("AccessSecretVersion = %v, %v", a, err)
	}

	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("us-central1"))`)
	p.clickText("#view tbody a", name)
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Configuration")`)
	p.clickText("#view .tab-strip .tab", "Configuration")
	p.waitFor(`document.querySelector("#view").textContent.includes("regional secret") && ` +
		`document.querySelector("#view").textContent.includes(` + strconv.Quote(key) + `)`)
}
