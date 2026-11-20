//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestStorageGoogleRound2ThroughTheBrowser (#1843), in the storage shard.
// Create a bucket offers Google's radio choices, Autoclass and Fine-grained
// access control, and the bucket keeps both; its Configuration tab shows
// Autoclass; an object's page has Google's addresses with Copy URL, Copy
// gsutil URI and Copy authenticated URL, Download and Delete, which deletes
// it; the Permissions tab's Switch to makes the bucket Uniform; the
// Protection tab's Edit object versioning enables versioning; the old
// soft-deleted buckets address leads to Buckets' View options; Observability
// is in the menu and says it is not available locally; and Storage
// Intelligence's Configuration page has its Overview.
func TestStorageGoogleRound2ThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-gcs2"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})
	click := func(sel string) { p.eval(fmt.Sprintf(`document.querySelector(%q).click(), true`, sel), nil) }
	submitModal := func() {
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal") === null`)
	}

	// Create a bucket: Autoclass and Fine-grained, as radio buttons.
	p.navigate("/storage/browser?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create")`)
	p.run(chromedp.Click(chromedp.CSS(`#view .state button.primary`)))
	p.waitFor(`document.querySelector("#view #f-uniformAccess-1") !== null && document.querySelector("#view #f-storageClassChoice-1") !== null`)
	var radios string
	p.eval(`[...document.querySelectorAll("#view fieldset.is-radio label")].map((l) => l.textContent).join("|")`, &radios)
	if !strings.Contains(radios, "Set a default class|Autoclass") || !strings.Contains(radios, "Uniform|Fine-grained") {
		t.Errorf("the create page's radio choices read %q", radios)
	}
	setField(p, "#view", "f-name", bucket)
	click("#view #f-storageClassChoice-1")
	click("#view #f-uniformAccess-1")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`location.pathname === "/storage/browser" && [...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("Fine-grained"))`, bucket))

	// Configuration shows Autoclass.
	p.navigate("/storage/browser/" + bucket + "?project=" + project + "&tab=configuration")
	p.waitFor(`[...document.querySelectorAll("#detail-panel dd")].some((d) => d.textContent.startsWith("Enabled (stored"))`)

	// An object's page.
	uploadObject(t, project, []string{bucket}, "a.txt", "hello")
	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "a.txt")`)
	p.eval(`[...document.querySelectorAll("#view tbody a")].find((a) => a.textContent === "a.txt").click(), true`, nil)
	p.waitFor(`document.querySelector("#object-page-download") !== null`)
	var page struct {
		Copies  []string
		GS      bool
		Actions string
	}
	p.eval(fmt.Sprintf(`({ Copies: [...document.querySelectorAll("#view .property-copy")].map((b) => b.textContent),
		GS: [...document.querySelectorAll("#view dd")].some((d) => d.textContent.startsWith(%q)),
		Actions: [...document.querySelectorAll("#view .page-actions button")].map((b) => b.textContent).join("|") })`,
		"gs://"+bucket+"/a.txt"), &page)
	if strings.Join(page.Copies, "|") != "Copy URL|Copy authenticated URL|Copy gsutil URI" || !page.GS ||
		!strings.HasPrefix(page.Actions, "Download|") || !strings.Contains(page.Actions, "|Edit metadata|") ||
		!strings.Contains(page.Actions, "|Edit access|") || !strings.HasSuffix(page.Actions, "|Delete") {
		t.Errorf("the object page reads %+v", page)
	}
	click("#object-page-delete")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`#confirm-input`), "a.txt"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open .modal-actions button.danger`)))
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/storage/browser/"+bucket))
	p.waitFor(`![...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "a.txt")`)

	// Permissions: Switch to Uniform.
	p.run(chromedp.Click(chromedp.CSS(`#tab-permissions`)))
	p.waitFor(sectionButton("Switch to"))
	p.clickText("#view .section-actions button", "Switch to")
	p.waitFor(`document.querySelector(".modal.is-open #f-uniformAccess")?.value === "Uniform"`)
	submitModal()
	p.run(chromedp.Click(chromedp.CSS(`#tab-permissions`)))
	p.waitFor(`[...document.querySelectorAll("#detail-panel p")].some((n) => n.textContent.startsWith("Access control: Uniform"))`)

	// Protection: Edit object versioning.
	p.run(chromedp.Click(chromedp.CSS(`#tab-protection`)))
	p.waitFor(sectionButton("Edit object versioning"))
	p.clickText("#view .section-actions button", "Edit object versioning")
	p.waitFor(`document.querySelector(".modal.is-open #f-versioning-0") !== null`)
	click(".modal.is-open #f-versioning-0")
	submitModal()
	p.run(chromedp.Click(chromedp.CSS(`#tab-protection`)))
	p.waitFor(`[...document.querySelectorAll("#detail-panel dt")].some((d) => d.textContent === "Object versioning" && d.nextElementSibling?.textContent === "On")`)

	// The soft-deleted buckets page leads to Buckets' View options.
	p.navigate("/storage/deleted?project=" + project)
	p.waitFor(`location.pathname === "/storage/browser" && new URLSearchParams(location.search).get("view") === "softdeleted"`)
	p.waitFor(`document.querySelector("#scope-view") !== null && document.querySelector("#scope-view").value === "softdeleted"`)

	// Observability: in Google's place, not available locally.
	p.navigate("/storage/observability?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view h2")].some((h) => h.textContent === "Not available locally")`)
	var menu string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent).join("|")`, &menu)
	if menu != "Buckets|Observability|Storage Intelligence|Batch operations|Inventory reports|Insights datasets|Transfer data|Settings" {
		t.Errorf("the Cloud Storage menu reads %q", menu)
	}

	// Storage Intelligence's Configuration page, with its Overview.
	p.navigate("/storage/intelligence?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .listing-summary dt")].some((d) => d.textContent === "Status")`)
}
