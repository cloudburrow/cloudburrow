//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestKMSKeyManagementIsGooglesFlow (#1851), in the run shard: Key
// Management's menu is Google's (Key management, Autokey, EKM connections,
// KMS config, Key inventory, then Single-tenant HSM), KMS config says it is
// not available locally, Create key ring is a page that goes on to the
// ring's Create key, whose form starts at Google's defaults; the key's page
// has Overview, Permissions and Logs; Rotate makes version 2 primary; the
// checked versions are disabled from the Overview's Disable; and Key
// inventory lists the key.
func TestKMSKeyManagementIsGooglesFlow(t *testing.T) {
	needService(t, "kms")
	opts := kmsSDKOptions(t)
	ctx := context.Background()
	c, err := kms.NewKeyManagementClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := uniqueProject(t)

	p.navigate("/kms/keyrings?project=" + project)
	p.waitFor(`document.querySelector("#product-nav a[aria-current=page]") !== null`)
	var menu []string
	p.eval(`[...document.querySelectorAll("#product-nav li a")].map((a) => a.textContent)`, &menu)
	if got := strings.Join(menu, ","); !strings.HasPrefix(got, "Key management,Autokey,EKM connections,KMS config,Key inventory") {
		t.Errorf("Key Management's menu is %v", menu)
	}

	p.navigate("/kms/config?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Not available locally") || document.querySelector("#view").textContent.includes("not available")`)

	// Create key ring, then Create key on the new ring.
	p.navigate("/kms/keyrings?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view button.primary")].some((b) => b.textContent.includes("Create key ring"))`)
	p.eval(`[...document.querySelectorAll("#view button.primary")].find((b) => b.textContent.includes("Create key ring")).click()`, nil)
	p.waitFor(`document.querySelector("#view #f-keyRingId") !== null`)
	var ringLabels []string
	p.eval(`[...document.querySelectorAll("#view form label")].map((l) => l.textContent)`, &ringLabels)
	if got := strings.Join(ringLabels, "|"); !strings.Contains(got, "Key ring name") || !strings.Contains(got, "Key ring location") {
		t.Errorf("Create key ring's labels are %v", ringLabels)
	}
	setField(p, "#view", "f-keyRingId", "browser-km")
	setField(p, "#view", "f-location", "global")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-cryptoKeyId") !== null`)
	var defaults struct{ Rotation, Material, Destroy, Level string }
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s).value;
		return { Rotation: q("#f-rotationPeriod"), Material: q("#f-keyMaterial"), Destroy: q("#f-destroyScheduledDays"),
		         Level: q("#f-protectionLevel") }; })()`, &defaults)
	if defaults.Rotation != "90d" || defaults.Material != "generated" || defaults.Destroy != "30" || defaults.Level != "SOFTWARE" {
		t.Errorf("Create key starts at %+v; want 90 days, Generated key, 30 days, Software", defaults)
	}
	var purposeWords string
	p.eval(`document.querySelector('.modal.is-open #f-purpose option[value="ENCRYPT_DECRYPT"]').textContent`, &purposeWords)
	if purposeWords != "Symmetric encrypt/decrypt" {
		t.Errorf("Purpose shows ENCRYPT_DECRYPT as %q", purposeWords)
	}
	setField(p, ".modal.is-open", "f-cryptoKeyId", "k")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	ring := "projects/" + project + "/locations/global/keyRings/browser-km"
	key := ring + "/cryptoKeys/k"
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil || k.GetRotationPeriod().AsDuration().Hours() != 90*24 || k.GetPrimary() == nil {
		t.Fatalf("the key made on the page reads back as %v, %v; want a primary version and 90-day rotation", k, err)
	}

	// The key's page: Overview, Permissions, Logs; Rotate.
	p.clickText("#view tbody a", "k")
	p.waitFor(`document.querySelector("#view .tab-strip .tab") !== null`)
	var tabs []string
	p.eval(`[...document.querySelectorAll("#view .tab-strip .tab")].map((b) => b.textContent)`, &tabs)
	if strings.Join(tabs, ",") != "Overview,Permissions,Logs" {
		t.Errorf("the key's tabs are %v", tabs)
	}
	confirm := func(word string) {
		t.Helper()
		p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
		p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), word), chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".modal") === null`)
	}
	p.clickText("#view .page-actions button", "Rotate")
	confirm("k")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Primary") && r.querySelector("a") && r.querySelector("a").textContent === "2")`)
	if k, _ := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); !strings.HasSuffix(k.GetPrimary().GetName(), "/2") {
		t.Errorf("after Rotate the primary is %s", k.GetPrimary().GetName())
	}

	// Check version 1, then the Overview's Disable.
	p.run(chromedp.Click(chromedp.CSS(`#view tbody input[aria-label="Select 1"]`)))
	p.eval(`[...document.querySelectorAll("#view button")].find((b) => b.textContent === "Disable" && !b.closest(".page-actions") && !b.disabled).click()`, nil)
	confirm("k")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("DISABLED"))`)
	if v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/1"}); err != nil ||
		v.GetState() != kmspb.CryptoKeyVersion_DISABLED {
		t.Errorf("version 1 after Disable is %v, %v", v.GetState(), err)
	}

	p.navigate("/kms/inventory?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("browser-km"))`, "k"))
}
