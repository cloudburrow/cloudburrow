//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
)

// TestDocumentAIProcessorCMEKThroughThePage (#1950): Create processor's
// Advanced options offer Google's encryption choice; Customer-managed
// encryption key (CMEK) with a key of the instance's Cloud KMS in
// us-central1 makes a processor whose kms_key_name ListProcessors reads,
// and the processor's page shows the key.
func TestDocumentAIProcessorCMEKThroughThePage(t *testing.T) {
	needService(t, "documentai")
	kmsOpts := kmsSDKOptions(t)
	ctx := context.Background()
	c, err := documentai.NewDocumentProcessorClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	kc, err := kms.NewKeyManagementClient(ctx, kmsOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = kc.Close() }()
	project := uniqueProject(t)
	q := "?project=" + project
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/us-central1",
		KeyRingId: fmt.Sprintf("docai-%d", time.Now().UnixNano()%1e9)})
	if err != nil {
		t.Fatal(err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "docai",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("browser-cmek-%d", time.Now().UnixNano()%1e6)

	p := open(t)
	p.navigate("/document-ai/processor-library/OCR_PROCESSOR" + q)
	p.waitFor(`location.pathname === "/document-ai/processor-library/OCR_PROCESSOR"`)
	where := p.findAction("Create processor")
	if where == "" {
		t.Fatal("the page offers no Create processor")
	}
	p.clickText(where, "Create processor")
	p.waitFor(`document.querySelector(".modal.is-open #f-encryption-1") !== null`)
	var labels string
	p.eval(`document.querySelector(".modal.is-open form").textContent`, &labels)
	if !strings.Contains(labels, "Advanced options") || !strings.Contains(labels, "Google-managed encryption key") ||
		!strings.Contains(labels, "Customer-managed encryption key (CMEK)") {
		t.Errorf("Create processor's panel reads %q", labels)
	}
	p.setField(".modal.is-open #f-name", name)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-encryption-1`)))
	p.waitFor(`!document.querySelector(".modal.is-open #f-kmsKeyName").closest(".form-row").hidden`)
	p.setField(".modal.is-open #f-kmsKeyName", key.GetName())
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null || document.querySelector("#action-result-table") !== null`)

	var proc *documentaipb.Processor
	it := c.ListProcessors(ctx, &documentaipb.ListProcessorsRequest{Parent: "projects/" + project + "/locations/us"})
	for {
		pr, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListProcessors: %v", err)
		}
		if pr.GetDisplayName() == name {
			proc = pr
		}
	}
	if proc == nil || proc.GetKmsKeyName() != key.GetName() {
		t.Fatalf("ListProcessors: %v; want kms_key_name %s", proc, key.GetName())
	}
	t.Cleanup(func() {
		if op, err := c.DeleteProcessor(context.Background(), &documentaipb.DeleteProcessorRequest{Name: proc.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	id := proc.GetName()[strings.LastIndex(proc.GetName(), "/")+1:]
	p.navigate("/document-ai/processors/" + id + q)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, key.GetName()))
}
