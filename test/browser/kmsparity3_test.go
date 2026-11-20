//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/chromedp/chromedp"
)

// kmsConfirmWithButton waits for a confirmation that asks for nothing typed,
// as Google's Rotate, Make primary, Disable, Enable and Restore prompts are,
// and presses its button, which must read label.
func kmsConfirmWithButton(t *testing.T, p *tab, label string) {
	t.Helper()
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm]") !== null`)
	var got struct {
		Typed  bool
		Button string
	}
	p.eval(`(() => { const m = document.querySelector(".modal.is-open");
		return { Typed: m.querySelector("#confirm-input") !== null, Button: m.querySelector("button[type=submit]").textContent }; })()`, &got)
	if got.Typed || got.Button != label {
		t.Errorf("the confirmation asks for a typed word (%v) and ends in %q; want %q alone", got.Typed, got.Button, label)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
}

// kmsHere is the page's console address, path and query.
func kmsHere(p *tab) string {
	var here string
	p.eval(`location.pathname + location.search`, &here)
	return here
}

// TestKMSCreateKeyPageAndGooglesPrompts (#1851), in the run shard: Create
// key is a page of its own at the ring's address, ending in Create, whose
// Algorithm lists the chosen purpose's algorithms only; a version row's
// Make primary version is confirmed with Make primary; the checked
// versions' Destroy asks for the key's name and ends in Schedule
// Destruction, Restore in Restore alone; Key inventory hides EKM via VPC
// connection until it is turned on. Every state is read back through the
// official Cloud KMS client, and the Create key page, the key's Versions
// tab and Key inventory pass the WCAG checks.
func TestKMSCreateKeyPageAndGooglesPrompts(t *testing.T) {
	needService(t, "kms")
	opts := kmsSDKOptions(t)
	ctx := context.Background()
	c, err := kms.NewKeyManagementClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	ring := "projects/" + project + "/locations/global/keyRings/browser-prompts"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms?project="+project,
		`{"keyRingId":"browser-prompts","location":"global"}`); code != http.StatusOK {
		t.Fatalf("create a key ring through the console API = %d: %s", code, body)
	}
	p := open(t)
	p.navigate("/kms/keyrings?project=" + project)
	p.clickText("#view tbody a", ring)
	p.clickText("#view .page-actions button", "Create key")
	p.waitFor(`document.querySelector("#view form.create-page #f-algorithm") !== null && location.search.includes("action=createkey")`)
	if p.has(".modal.is-open") {
		t.Error("Create key opened a dialog, not a page")
	}
	var page struct{ Title, Submit string }
	p.eval(`({ Title: document.querySelector("#view h1").textContent,
		Submit: document.querySelector("#view form.create-page button[type=submit]").textContent })`, &page)
	if page.Title != "Create key" || page.Submit != "Create" {
		t.Errorf("Create key's page is %+v; want Create key ending in Create", page)
	}
	algorithms := func() (string, []string) {
		var got struct {
			Value   string
			Options []string
		}
		p.eval(`({ Value: document.querySelector("#view #f-algorithm").value,
			Options: [...document.querySelector("#view #f-algorithm").options].map((o) => o.value) })`, &got)
		return got.Value, got.Options
	}
	if v, o := algorithms(); v != "GOOGLE_SYMMETRIC_ENCRYPTION" || strings.Contains(strings.Join(o, ","), "EC_SIGN") {
		t.Errorf("Symmetric encrypt/decrypt's algorithms are %v, %q chosen", o, v)
	}
	setField(p, "#view form.create-page", "f-purpose", "ASYMMETRIC_SIGN")
	if v, o := algorithms(); !strings.HasPrefix(v, "EC_SIGN") && !strings.HasPrefix(v, "RSA_SIGN") && !strings.HasPrefix(v, "PQ_SIGN") ||
		strings.Contains(strings.Join(o, ","), "GOOGLE_SYMMETRIC_ENCRYPTION") || strings.Contains(strings.Join(o, ","), "HMAC") {
		t.Errorf("Asymmetric sign's algorithms are %v, %q chosen", o, v)
	}
	setField(p, "#view form.create-page", "f-purpose", "ENCRYPT_DECRYPT")
	if v, _ := algorithms(); v != "GOOGLE_SYMMETRIC_ENCRYPTION" {
		t.Errorf("back on Symmetric encrypt/decrypt the algorithm is %q", v)
	}
	createPage := kmsHere(p)
	setField(p, "#view form.create-page", "f-cryptoKeyId", "k")
	p.run(chromedp.Click(chromedp.CSS(`#view form.create-page button[type="submit"]`)))
	p.waitFor(`!location.search.includes("action=") && [...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "k")`)
	key := ring + "/cryptoKeys/k"
	if k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); err != nil ||
		k.GetVersionTemplate().GetAlgorithm() != kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION {
		t.Fatalf("the key made on the page reads back as %v, %v", k, err)
	}
	if _, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key}); err != nil {
		t.Fatal(err)
	}

	// Version 2's row menu: Make primary version, then Make primary.
	p.clickText("#view tbody a", "k")
	p.clickText("#view .tab-strip .tab", "Versions")
	keyPage := kmsHere(p)
	p.waitFor(`document.querySelector('button[aria-label="Actions for 2"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for 2"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Make primary version")
	kmsConfirmWithButton(t, p, "Make primary")
	if k, _ := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); !strings.HasSuffix(k.GetPrimary().GetName(), "/2") {
		t.Errorf("after Make primary the primary is %s", k.GetPrimary().GetName())
	}

	// Check version 1: Destroy asks for the key's name, then Schedule
	// Destruction; Restore asks for nothing.
	selectVersion := func() {
		t.Helper()
		p.waitFor(`document.querySelector('#view tbody input[aria-label="Select 1"]') !== null`)
		p.run(chromedp.Click(chromedp.CSS(`#view tbody input[aria-label="Select 1"]`)))
	}
	pressSelection := func(label string) {
		t.Helper()
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view button")].some((b) => b.textContent === %q && !b.closest(".page-actions") && !b.disabled)`, label))
		p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view button")].find((b) => b.textContent === %q && !b.closest(".page-actions") && !b.disabled).click()`, label), nil)
	}
	version := func() kmspb.CryptoKeyVersion_CryptoKeyVersionState {
		v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/1"})
		if err != nil {
			t.Fatal(err)
		}
		return v.GetState()
	}
	selectVersion()
	pressSelection("Destroy")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	var destroyButton string
	p.eval(`document.querySelector(".modal.is-open button[type=submit]").textContent`, &destroyButton)
	if destroyButton != "Schedule Destruction" {
		t.Errorf("Destroy's confirmation ends in %q; want Schedule Destruction", destroyButton)
	}
	p.confirmByName("k")
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("DESTROY_SCHEDULED"))`)
	if s := version(); s != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
		t.Errorf("version 1 after Schedule Destruction is %v", s)
	}
	selectVersion()
	pressSelection("Restore")
	kmsConfirmWithButton(t, p, "Restore")
	p.waitFor(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("DESTROY_SCHEDULED"))`)
	if s := version(); s != kmspb.CryptoKeyVersion_DISABLED {
		t.Errorf("version 1 after Restore is %v; want DISABLED", s)
	}

	// Key inventory: EKM via VPC connection is off until turned on.
	inventory := "/kms/inventory?project=" + url.QueryEscape(project)
	p.eval(`(() => { try { const all = JSON.parse(localStorage.getItem("cloudburrow.columns") || "{}");
		delete all["kms-inventory"]; localStorage.setItem("cloudburrow.columns", JSON.stringify(all)); } catch (e) {} return true; })()`, nil)
	p.navigate(inventory)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser-prompts"))`)
	var heads []string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim())`, &heads)
	if strings.Contains(strings.Join(heads, "|"), "EKM via VPC connection") || !strings.Contains(strings.Join(heads, "|"), "Protection level") {
		t.Errorf("Key inventory's columns are %v; want EKM via VPC connection hidden by default", heads)
	}

	auditScreens(t, []a11yPage{
		{"Create key page", createPage, settled + ` && document.querySelector("#view form.create-page #f-algorithm") !== null`, ""},
		{"key Versions tab", keyPage, settled + ` && document.querySelector('#view tbody input[aria-label="Select 1"]') !== null`, ""},
		{"Key inventory", inventory, settled + ` && document.querySelector("#view tbody tr") !== null`, ""},
	})
}

// TestKMSAutokeyConfigurationSourceThroughTheBrowser (#1851), in the run
// shard: Configure starts at Google's Configuration source; Configure for
// project with same-project key storage, then Inherit, are read back by the
// official AutokeyAdmin client; the Configure dialog passes the WCAG checks.
func TestKMSAutokeyConfigurationSourceThroughTheBrowser(t *testing.T) {
	needService(t, "kms-autokey")
	ctx := context.Background()
	admin, err := kms.NewAutokeyAdminClient(ctx, kmsSDKOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	p := open(t)
	project := uniqueProject(t)
	cfgName := "projects/" + project + "/autokeyConfig"
	configure := func() {
		t.Helper()
		p.clickText("#view .page-actions button", "Configure")
		p.waitFor(`document.querySelector(".modal.is-open #f-configurationSource") !== null`)
	}
	p.navigate("/kms/autokey?project=" + project)
	p.clickText("#view tbody a", cfgName)
	cfgPage := kmsHere(p)
	configure()
	var labels []string
	p.eval(`[...document.querySelectorAll(".modal.is-open fieldset label")].filter((l) => l.getClientRects().length).map((l) => l.textContent)`, &labels)
	if got := strings.Join(labels, "|"); got != "Configure for project|Inherit" {
		t.Errorf("Configure first shows %v; want Configuration source alone, Inherit chosen", labels)
	}
	p.clickText(".modal.is-open label", "Configure for project")
	p.clickText(".modal.is-open label", "Enable same-project key storage")
	var submit string
	p.eval(`document.querySelector(".modal.is-open button[type=submit]").textContent`, &submit)
	if submit != "Submit" {
		t.Errorf("Configure ends in %q; want Submit", submit)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if cfg, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: cfgName}); err != nil ||
		cfg.GetKeyProjectResolutionMode() != kmspb.AutokeyConfig_RESOURCE_PROJECT {
		t.Fatalf("GetAutokeyConfig after Configure for project = %v, %v", cfg, err)
	}

	configure()
	p.clickText(".modal.is-open label", "Inherit")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if cfg, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: cfgName}); err != nil ||
		cfg.GetKeyProjectResolutionMode() != kmspb.AutokeyConfig_KEY_PROJECT_RESOLUTION_MODE_UNSPECIFIED || cfg.GetKeyProject() != "" {
		t.Errorf("GetAutokeyConfig after Inherit = %v, %v; want it cleared", cfg, err)
	}

	auditScreens(t, []a11yPage{
		{"Autokey Configure dialog", cfgPage, settled + ` && [...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Configure")`,
			`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent === "Configure").click()`},
	})
}
