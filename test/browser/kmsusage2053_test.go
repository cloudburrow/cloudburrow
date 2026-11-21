//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/storage"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/cloudkms/v1"
	"google.golang.org/api/option"
)

// TestKMSUsageTrackingAndControlsThroughTheBrowser (#2053): a key's Usage
// tracking tab shows the key usage summary and the CMEK bucket that uses
// the key, as the Inventory API reports it, with the same-project notice;
// a version row's Destroy asks for the key's name; Starting on is a date
// picker; KMS Controls sets a default Key Access Justifications policy,
// read back through Cloud KMS's own JSON API; Disable autokey asks Confirm
// in a prompt after Submit. Every page passes the WCAG checks.
func TestKMSUsageTrackingAndControlsThroughTheBrowser(t *testing.T) {
	needService(t, "kms-config")
	needService(t, "storage")
	opts := kmsSDKOptions(t)
	sc, _ := storageSDK(t)
	ctx := context.Background()
	c, err := kms.NewKeyManagementClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := uniqueProject(t)

	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/us", KeyRingId: "usage"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "cmek",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := sc.ServiceAccount(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	ih := c.ResourceIAM(key.GetName())
	pol, err := ih.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pol.Add("serviceAccount:"+agent, iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
	if err := ih.SetPolicy(ctx, pol); err != nil {
		t.Fatal(err)
	}
	bucket := fmt.Sprintf("usage-%d", time.Now().UnixNano()%1e9)
	bh := sc.Bucket(bucket)
	if err := bh.Create(ctx, project, &storage.BucketAttrs{Location: "US", Encryption: &storage.BucketEncryption{DefaultKMSKeyName: key.GetName()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bh.Delete(context.Background()) })

	// The key's Usage tracking tab.
	p.navigate("/kms/keyrings?project=" + project)
	p.clickText("#view tbody a", ring.GetName())
	p.clickText("#view tbody a", "cmek")
	p.clickText("#view .tab-strip .tab", "Usage tracking")
	usage := kmsHere(p)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("//storage.googleapis.com/` + bucket + `"))`)
	var got struct {
		Text  string
		Heads []string
	}
	p.eval(`({ Text: document.querySelector("#view").textContent,
		Heads: [...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()) })`, &got)
	for _, want := range []string{"Key usage summary", "Protected resources", "Google Cloud products", "Data is limited to the selected project", "storage.googleapis.com/Bucket"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("Usage tracking lacks %q", want)
		}
	}
	if h := strings.Join(got.Heads, "|"); !strings.Contains(h, "Name") || !strings.Contains(h, "Crypto key version") || !strings.Contains(h, "Creation date") {
		t.Errorf("Usage tracking's columns are %v", got.Heads)
	}

	// A version row's Destroy asks for the key's name, not the number.
	p.clickText("#view .tab-strip .tab", "Versions")
	versions := kmsHere(p)
	p.waitFor(`document.querySelector('button[aria-label="Actions for 1"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for 1"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Destroy")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	var word string
	p.eval(`document.querySelector(".modal.is-open label[for=confirm-input] strong").textContent`, &word)
	if word != "cmek" {
		t.Errorf("a version row's Destroy asks for %q; want the key's name, cmek", word)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button.secondary`)))
	p.waitFor(`document.querySelector(".modal") === null`)

	// Edit Rotation Period's Starting on is a date picker.
	p.clickText("#view .page-actions button", "Edit Rotation Period")
	p.waitFor(`document.querySelector(".modal.is-open #f-nextRotationTime") !== null`)
	var kind string
	p.eval(`document.querySelector(".modal.is-open #f-nextRotationTime").type`, &kind)
	if kind != "date" {
		t.Errorf("Starting on is a %q input; want a date picker", kind)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button.secondary`)))
	p.waitFor(`document.querySelector(".modal") === null`)

	// KMS Controls: the project's default Key Access Justifications policy.
	p.navigate("/kms/config?project=" + project)
	cfgName := "projects/" + project + "/kajPolicyConfig"
	p.clickText("#view tbody a", cfgName)
	controls := kmsHere(p)
	p.clickText("#view .page-actions button", "Edit")
	p.clickText(".modal.is-open label", "Allow specific reason codes")
	p.clickText(".modal.is-open label", "Customer-initiated access")
	p.clickText(".modal.is-open label", "Google-initiated system operation")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	rest, err := cloudkms.NewService(ctx, option.WithEndpoint("http://"+strings.TrimPrefix(kmsAddr(t), "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := rest.Projects.GetKajPolicyConfig(cfgName).Context(ctx).Do()
	if err != nil || cfg.DefaultKeyAccessJustificationPolicy == nil ||
		strings.Join(cfg.DefaultKeyAccessJustificationPolicy.AllowedAccessReasons, ",") != "CUSTOMER_INITIATED_ACCESS,GOOGLE_INITIATED_SYSTEM_OPERATION" {
		t.Errorf("the default policy read back is %+v, %v", cfg, err)
	}

	// Disable autokey asks Confirm after Submit.
	admin, err := kms.NewAutokeyAdminClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	p.navigate("/kms/autokey?project=" + project)
	p.clickText("#view tbody a", "projects/"+project+"/autokeyConfig")
	p.clickText("#view .page-actions button", "Configure")
	p.waitFor(`document.querySelector(".modal.is-open #f-configurationSource") !== null`)
	p.clickText(".modal.is-open label", "Configure for project")
	p.clickText(".modal.is-open label", "Disable autokey for project")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	var title string
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm]") !== null`)
	p.eval(`document.querySelector(".modal.is-open #confirm-title").textContent`, &title)
	if title != "Disable autokey?" {
		t.Errorf("the prompt is titled %q; want Disable autokey?", title)
	}
	// The prompt opens over the Configure dialog, which stays open beneath.
	var prompt struct {
		Typed  bool
		Button string
	}
	p.eval(`(() => { const m = document.querySelector("#confirm-title").closest("form");
		return { Typed: m.querySelector("#confirm-input") !== null, Button: m.querySelector("button[type=submit]").textContent }; })()`, &prompt)
	if prompt.Typed || prompt.Button != "Confirm" {
		t.Errorf("the prompt asks for a typed word (%v) and ends in %q; want Confirm alone", prompt.Typed, prompt.Button)
	}
	p.eval(`document.querySelector("#confirm-title").closest("form").querySelector("button[type=submit]").click()`, nil)
	p.waitFor(`document.querySelector(".modal") === null`)
	if ac, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: "projects/" + project + "/autokeyConfig"}); err != nil ||
		ac.GetKeyProjectResolutionMode() != kmspb.AutokeyConfig_DISABLED {
		t.Errorf("GetAutokeyConfig after Confirm = %v, %v; want DISABLED", ac, err)
	}

	auditScreens(t, []a11yPage{
		{"key Usage tracking tab", usage, settled + ` && document.querySelector("#view tbody tr") !== null`, ""},
		{"version row Destroy prompt", versions, settled + ` && document.querySelector('button[aria-label="Actions for 1"]') !== null`, ""},
		{"KMS Controls", "/kms/config?project=" + project, settled + ` && document.querySelector("#view tbody tr") !== null`, ""},
		{"KMS Controls Edit", controls, settled + ` && [...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Edit")`,
			`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent === "Edit").click()`},
	})
}

// kmsAddr is the Cloud KMS endpoint the SDK options use.
func kmsAddr(t *testing.T) string {
	t.Helper()
	kmsSDKOptions(t) // skips without it
	return strings.TrimSpace(os.Getenv(envKMS))
}
