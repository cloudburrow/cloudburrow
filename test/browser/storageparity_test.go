//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// sectionButton is a JS test for a button among the current tab's own
// actions (#1843).
func sectionButton(label string) string {
	return fmt.Sprintf(`[...document.querySelectorAll("#view .section-actions button")].some((b) => b.textContent === %q)`, label)
}

// TestStorageGooglePagesThroughTheBrowser (#1843), in the storage shard.
// Create a bucket is a page titled "Create a bucket" with Google's numbered
// steps, each with Continue, ending in Create; the bucket it makes is listed
// with Google's columns; its page has Google's tabs in Google's order, with
// Permissions third; the Objects tab offers Upload files, Upload folder and
// Create folder, which makes a folder on a flat bucket; the Lifecycle tab's
// Add a rule adds a rule its table then lists, and the rule's Delete rule
// removes it.
func TestStorageGooglePagesThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-gcs"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/objects/storage?project="+project+"&name="+bucket+"&name=reports/", "")
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})

	p.navigate("/storage/browser?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create")`)
	p.run(chromedp.Click(chromedp.CSS(`#view .state button.primary`)))
	p.waitFor(`document.querySelector("#view #f-locationType") !== null`)
	var form struct {
		Title   string
		Steps   []string
		Button  string
		Loc     string
		Soft    bool
		Days    string
		Uniform bool
	}
	p.eval(`(() => ({
		Title: document.querySelector("#view h1").textContent,
		Steps: [...document.querySelectorAll("#view .create-step-title")].map((n) => n.textContent),
		Button: document.querySelector("#view button[type=submit]").textContent,
		Loc: document.querySelector("#view #f-location").value,
		Soft: document.querySelector("#view #f-softDelete").checked,
		Days: document.querySelector("#view #f-softDeleteDays").value,
		Uniform: document.querySelector("#view #f-uniformAccess").checked }))()`, &form)
	want := "Get started|Choose where to store your data|Choose how to store your data|" +
		"Choose how to control access to objects|Choose how to protect object data"
	if form.Title != "Create a bucket" || strings.Join(form.Steps, "|") != want || form.Button != "Create" ||
		form.Loc != "US" || !form.Soft || form.Days != "7" || !form.Uniform {
		t.Errorf("the create page reads %+v", form)
	}
	// Continue folds Get started and opens the next step.
	setField(p, "#view", "f-name", bucket)
	p.clickText("#view .create-step-continue button", "Continue")
	p.waitFor(`document.querySelector("#view .create-step").classList.contains("is-folded")`)
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`location.pathname === "/storage/browser"`)

	// Google's columns, with the bucket's values.
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, bucket))
	var heads string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean).join("|")`, &heads)
	for _, col := range []string{"Name", "Created", "Location type", "Location", "Default storage class",
		"Access control", "Protection", "Lifecycle"} {
		if !strings.Contains("|"+heads+"|", "|"+col+"|") {
			t.Errorf("the bucket list's headers %q lack %q", heads, col)
		}
	}

	// The bucket's tabs, in Google's order.
	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	p.waitFor(`document.querySelector("#tab-lifecycle") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((b) => b.textContent).join("|")`, &tabs)
	if !strings.HasPrefix(tabs, "Objects|Configuration|Permissions|Protection|Lifecycle|") {
		t.Errorf("the bucket's tabs read %q", tabs)
	}

	// The Objects tab's buttons, and Create folder on a flat bucket.
	var buttons string
	p.eval(`[...document.querySelectorAll("#detail-panel button")].map((b) => b.textContent).join("|")`, &buttons)
	for _, b := range []string{"Upload files", "Upload folder", "Create folder", "Create managed folder"} {
		if !strings.Contains("|"+buttons+"|", "|"+b+"|") {
			t.Errorf("the Objects tab's buttons %q lack %q", buttons, b)
		}
	}
	p.clickText("#view .section-actions button", "Create folder")
	p.waitFor(`document.querySelector(".modal.is-open #f-name") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-name`), "reports"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("reports/") && r.textContent.includes("Folder"))`)

	// Lifecycle: Add a rule, then Delete rule.
	p.run(chromedp.Click(chromedp.CSS(`#tab-lifecycle`)))
	p.waitFor(sectionButton("Add a rule"))
	p.clickText("#view .section-actions button", "Add a rule")
	p.waitFor(`document.querySelector(".modal.is-open #f-action") !== null`)
	setField(p, ".modal.is-open", "f-action", "Set storage class to Coldline")
	setField(p, ".modal.is-open", "f-age", "30")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelector("#tab-lifecycle.is-selected") !== null && ` +
		`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Set to Coldline") && r.textContent.includes("Age: 30 days"))`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for Rule 1"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete rule")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`#confirm-input`), "Rule 1"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open .modal-actions button.danger`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Set to Coldline"))`)
}
