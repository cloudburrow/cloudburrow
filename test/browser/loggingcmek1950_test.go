//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/chromedp/chromedp"
)

// TestLoggingAdminCreatesACMEKBucketThroughThePage (#1950), in the run
// shard, whose instance serves Cloud KMS beside Cloud Logging: Create log
// bucket offers Google's Encryption choice; Customer-managed key shows the
// key field, and a bucket made in us-central1 with a key of that region is
// listed with the key and its version 1 on its page, read back through the
// console API.
func TestLoggingAdminCreatesACMEKBucketThroughThePage(t *testing.T) {
	needService(t, "logging-buckets")
	needService(t, "kms")
	ctx := context.Background()
	kc, err := kms.NewKeyManagementClient(ctx, kmsSDKOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	p := open(t)
	project := uniqueProject(t)
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/us-central1",
		KeyRingId: fmt.Sprintf("logs-%d", time.Now().UnixNano()%1e9)})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "logs",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}

	p.navigate("/logs/storage/create?project=" + project)
	p.waitFor(`document.querySelector("form.create-page #f-location") !== null`)
	var hidden bool
	p.eval(`document.querySelector("form.create-page #f-kmsKey").closest(".form-row, fieldset").hidden`, &hidden)
	if !hidden {
		t.Error("the key field shows with Google-managed encryption chosen")
	}
	p.setField("form.create-page #f-name", "cmek-logs")
	p.eval(`(() => { const s = document.querySelector("form.create-page #f-location"); s.value = "us-central1";
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.eval(`[...document.querySelectorAll("form.create-page fieldset.is-radio input[type=radio]")].find((r) => r.value === "Customer-managed key").click(), true`, nil)
	p.waitFor(`!document.querySelector("form.create-page #f-kmsKey").closest(".form-row, fieldset").hidden`)
	p.setField("form.create-page #f-kmsKey", key.GetName())
	p.run(chromedp.Click(chromedp.CSS(`form.create-page button[type="submit"]`)))

	bucket := "projects/" + project + "/locations/us-central1/buckets/cmek-logs"
	detail := "/api/detail/logging-buckets?project=" + project + "&name=" + url.QueryEscape(bucket)
	var body string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		var code int
		if code, body = consoleDo(t, http.MethodGet, detail, ""); code == http.StatusOK && strings.Contains(body, key.GetName()) {
			break
		}
	}
	if !strings.Contains(body, "Customer-managed key: "+key.GetName()) || !strings.Contains(body, key.GetName()+"/cryptoKeyVersions/1") {
		t.Fatalf("the bucket's page does not show its key and version: %s", body)
	}
}
