//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestStorageGoogleRound3ThroughTheBrowser (#1843), in the storage shard.
// The Protection tab's Edit object versioning opens Google's "Object
// versioning" dialog, with "Add recommended lifecycle rules to manage version
// costs", ending in Confirm. The bucket's tabs are Google's, with
// Observability and Inventory reports and no Deleted objects tab: the
// Objects tab's Show list holds "Live and noncurrent objects" and
// "Soft-deleted objects only", and the old ?tab=deleted address opens it
// there. An object's Version history lists its generations, Restore opens
// "Restore object version" ending in Confirm, and its Show list holds the
// object's soft-deleted generations. Create restore job is Google's pane,
// ending in Create; the Buckets list's View options is a dialog with Save.
// Every one of those screens and dialogs passes the WCAG 2.1 AA audit.
func TestStorageGoogleRound3ThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-gcs3"
	q := "?project=" + project
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	type row struct {
		Name    string
		Target  []string
		Actions []struct{ ID string }
	}
	history := func() []row {
		t.Helper()
		v := url.Values{"project": {project}, "name": {"_details", bucket, "v.txt"}}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d struct {
			Sections []struct {
				ID      string
				Listing struct{ Items []row }
			}
		}
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
			t.Fatalf("the object page through the console API = %d (%v): %s", code, err, body)
		}
		for _, s := range d.Sections {
			if s.ID == "versions" {
				return s.Listing.Items
			}
		}
		t.Fatalf("the object page has no Version history: %s", body)
		return nil
	}
	act := func(path []string, id string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": id})
		if code, resp := consoleDo(t, http.MethodPost, "/api/actions/storage"+q, string(body)); code != http.StatusOK {
			t.Errorf("%s through the console API = %d: %s", id, code, resp)
		}
	}
	t.Cleanup(func() {
		for _, r := range history() {
			act(r.Target, "deleteversion")
		}
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})
	click := func(sel string) { p.eval(fmt.Sprintf(`document.querySelector(%q).click(), true`, sel), nil) }
	show := func(id, choice string) {
		p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, "#"+id))
		p.eval(fmt.Sprintf(`(() => { const s = document.querySelector(%q); s.value = %q;
			s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, "#"+id, choice), nil)
	}
	rows := func(n int, has string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].filter((r) => r.textContent.includes(%q)).length === %d`, has, n)
	}

	// Protection: Google's Object versioning dialog, ending in Confirm.
	p.navigate("/storage/browser/" + bucket + q + "&tab=protection")
	p.waitFor(sectionButton("Edit object versioning"))
	p.clickText("#view .section-actions button", "Edit object versioning")
	p.waitFor(`document.querySelector(".modal.is-open #f-versioning-0") !== null`)
	click(".modal.is-open #f-versioning-0")
	p.waitFor(`document.querySelector(".modal.is-open #f-recommended")?.closest("[hidden]") === null`)
	var dialog struct{ Title, Box, Submit string }
	p.eval(`({ Title: document.querySelector(".modal.is-open h2").textContent,
		Box: document.querySelector(".modal.is-open label[for=f-recommended]").textContent,
		Submit: document.querySelector(".modal.is-open button[type=submit]").textContent })`, &dialog)
	if dialog.Title != "Object versioning" || !strings.HasPrefix(dialog.Box, "Add recommended lifecycle rules to manage version costs") || dialog.Submit != "Confirm" {
		t.Errorf("the Object versioning dialog reads %+v", dialog)
	}
	p.clickText(".modal.is-open button", "Confirm")
	p.waitFor(`document.querySelector(".modal") === null`)
	uploadObject(t, project, []string{bucket}, "v.txt", "one")
	uploadObject(t, project, []string{bucket}, "v.txt", "two")

	// The bucket's tabs, and the Objects tab's Show list.
	p.navigate("/storage/browser/" + bucket + q)
	p.waitFor(`document.querySelector("#objects-show") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view [role=tab]:not([hidden])")].map((b) => b.textContent).join("|")`, &tabs)
	if !strings.HasPrefix(tabs, "Objects|Configuration|Permissions|Protection|Lifecycle|Observability|Inventory reports|") ||
		strings.Contains(tabs, "Deleted objects") {
		t.Errorf("the bucket's tabs read %q", tabs)
	}
	var options string
	p.eval(`[...document.querySelectorAll("#objects-show option")].map((o) => o.textContent).join("|")`, &options)
	if options != "Live objects only|Live and noncurrent objects|Soft-deleted objects only" {
		t.Errorf("the Objects tab's Show list reads %q", options)
	}
	show("objects-show", "Live and noncurrent objects")
	p.waitFor(rows(2, "v.txt#"))
	show("objects-show", "Live objects only")
	p.waitFor(rows(1, "v.txt"))
	p.run(chromedp.Click(chromedp.CSS(`#tab-observability`)))
	p.waitFor(`document.querySelector("#detail-panel")?.textContent.includes("Not available locally")`)

	// Version history: Restore, confirmed by Confirm.
	h := history()
	if len(h) != 2 {
		t.Fatalf("Version history lists %+v", h)
	}
	p.navigate("/storage/browser/_details/" + bucket + "/v.txt" + q + "&tab=versions")
	p.waitFor(rows(2, "v.txt#"))
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+h[0].Name))))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Restore")
	p.waitFor(`document.querySelector(".modal.is-open h2")?.textContent === "Restore object version"`)
	p.clickText(".modal.is-open .modal-actions button", "Confirm")
	p.waitFor(rows(3, "v.txt#"))

	// A deleted noncurrent generation is soft-deleted: Version history's
	// Show list and the Objects tab's list it, and ?tab=deleted opens there.
	act(h[0].Target, "deleteversion")
	p.navigate("/storage/browser/_details/" + bucket + "/v.txt" + q + "&tab=versions")
	show("versions-show", "Soft-deleted objects only")
	p.waitFor(rows(1, "v.txt#"))
	p.navigate("/storage/browser/" + bucket + q + "&tab=deleted")
	p.waitFor(`document.querySelector("#objects-show")?.value === "Soft-deleted objects only" && document.querySelector("#tab-objects.is-selected") !== null`)
	p.waitFor(rows(1, "v.txt"))

	// Create restore job: Google's pane, ending in Create.
	p.navigate("/storage/browser/" + bucket + q)
	p.clickText("#view .page-actions button, #view .page-actions [role=menuitem]", "Create restore job")
	p.waitFor(`document.querySelector(".modal.is-open #f-matchGlobs") !== null`)
	var pane string
	p.eval(`document.querySelector(".modal.is-open").textContent`, &pane)
	for _, want := range []string{"Choose which soft-deleted objects to restore", "Filter by glob pattern", "Restore options", "Create"} {
		if !strings.Contains(pane, want) {
			t.Errorf("the restore job pane does not say %q: %s", want, pane)
		}
	}
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)

	// The Buckets list's View options: a dialog, applied by Save.
	p.navigate("/storage/browser" + q)
	p.clickText("#view button", "View options")
	p.waitFor(`document.querySelector(".modal.is-open #view-options-view-softdeleted") !== null`)
	click(".modal.is-open #view-options-view-softdeleted")
	p.clickText(".modal.is-open button", "Save")
	p.waitFor(`new URLSearchParams(location.search).get("view") === "softdeleted" && document.querySelector(".modal") === null`)

	bucketPage := "/storage/browser/" + bucket + q
	ready := settled + ` && document.querySelector("#objects-show") !== null`
	auditScreens(t, []a11yPage{
		{"bucket Objects tab with the Show list", bucketPage, ready, ""},
		{"bucket Observability tab", bucketPage + "&tab=observability", settled, ""},
		{"bucket Inventory reports tab", bucketPage + "&tab=inventory", settled, ""},
		{"object Version history", "/storage/browser/_details/" + bucket + "/v.txt" + q + "&tab=versions", settled + ` && document.querySelector("#versions-show") !== null`, ""},
		{"object versioning dialog", bucketPage + "&tab=protection", settled + " && " + sectionButton("Edit object versioning"),
			`[...document.querySelectorAll("#view .section-actions button")].find((b) => b.textContent === "Edit object versioning").click()`},
		{"buckets View options dialog", "/storage/browser" + q, settled + ` && [...document.querySelectorAll("#view button")].some((b) => b.textContent === "View options")`,
			`[...document.querySelectorAll("#view button")].find((b) => b.textContent === "View options").click()`},
	})
}
