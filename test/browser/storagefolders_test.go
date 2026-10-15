//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestStorageHierarchicalFoldersThroughTheBrowser (#1203), in the storage
// shard. On the Create form, checking Enable hierarchical namespace turns
// uniform bucket-level access on and versioning off and holds both;
// unchecking frees them. The bucket it creates offers Create folder, which
// lists a folder typed Folder; its row's Rename folder shows the operation
// it answered with; Delete folder and contents says what it deletes and
// asks for the folder's name: Cancel sends nothing, and typing it deletes
// the folder. The Operations tab of the page read again then lists the
// rename and the delete.
func TestStorageHierarchicalFoldersThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-hns"
	t.Cleanup(func() {
		for _, f := range []string{"reports/", "renamed/"} {
			body, _ := json.Marshal(map[string]any{"Path": []string{"_folder", bucket, f}, "Action": "deletefolderrecursive"})
			consoleDo(t, http.MethodPost, "/api/actions/storage?project="+project, string(body))
		}
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})

	p.navigate("/storage/browser?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create")`)
	p.run(chromedp.Click(`#view .state button.primary`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view #f-hierarchicalNamespace") !== null`)
	type boxes struct{ Uniform, UniformHeld, Versioning, VersioningHeld bool }
	read := func() boxes {
		var b boxes
		p.eval(`(() => { const q = (s) => document.querySelector("#view " + s);
			return { Uniform: q("#f-uniformAccess").checked, UniformHeld: q("#f-uniformAccess").disabled,
			         Versioning: q("#f-versioning").checked, VersioningHeld: q("#f-versioning").disabled }; })()`, &b)
		return b
	}
	setField(p, "#view", "f-uniformAccess", "false")
	setField(p, "#view", "f-versioning", "true")
	setField(p, "#view", "f-hierarchicalNamespace", "true")
	if b := read(); !b.Uniform || !b.UniformHeld || b.Versioning || !b.VersioningHeld {
		t.Errorf("with hierarchical namespace checked the form holds %+v; want uniform on, versioning off, both held", b)
	}
	setField(p, "#view", "f-hierarchicalNamespace", "false")
	if b := read(); b.UniformHeld || b.VersioningHeld {
		t.Errorf("with hierarchical namespace unchecked the form still holds %+v", b)
	}
	setField(p, "#view", "f-hierarchicalNamespace", "true")
	setField(p, "#view", "f-name", bucket)
	p.run(chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`location.pathname === "/storage/browser"`)
	if got := editDefaults(t, "storage", project, bucket); got["uniformAccess"] != "true" || got["versioning"] != "false" || got["Hierarchical namespace"] != "Enabled" {
		t.Errorf("the bucket created in the browser reads uniform %q, versioning %q, hierarchical namespace %q",
			got["uniformAccess"], got["versioning"], got["Hierarchical namespace"])
	}

	actionPosts := func() int { return len(p.sent(http.MethodPost, "/api/actions/storage")) }
	listed := func(name string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("Folder"))`, name)
	}
	menu := func(row, item string) {
		p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+row), chromedp.ByQuery))
		p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, item)
	}

	// Create folder.
	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Create folder")`)
	p.clickText("#view .page-actions button", "Create folder")
	p.waitFor(`document.querySelector(".modal.is-open #f-name") !== null`)
	p.run(chromedp.SendKeys(`.modal.is-open #f-name`, "reports", chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(listed("reports/"))

	// Rename folder, which answers with its operation.
	menu("reports/", "Rename folder")
	p.waitFor(`document.querySelector(".modal.is-open #f-destination") !== null`)
	setField(p, ".modal.is-open", "f-destination", "renamed/")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal #action-result") !== null`)
	var answer string
	p.eval(`document.querySelector(".modal #action-result").textContent`, &answer)
	if !strings.Contains(answer, "RENAME_FOLDER") || !strings.Contains(answer, "Done") {
		t.Errorf("Rename folder answered %q", answer)
	}
	p.clickText(".modal .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(listed("renamed/") + ` && !` + listed("reports/"))

	// Delete folder and contents, confirmed by the folder's name.
	openDelete := func() {
		menu("renamed/", "Delete folder and contents")
		p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	}
	openDelete()
	var warning string
	p.eval(`document.querySelector(".modal:has(#confirm-input) .confirm-detail").textContent`, &warning)
	if !strings.Contains(warning, "every object in it are deleted") {
		t.Errorf("the delete's confirmation says %q", warning)
	}
	before := actionPosts()
	p.clickText(".modal:has(#confirm-input) .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector("#confirm-input") === null`)
	if n := actionPosts(); n != before {
		t.Fatalf("cancelling the delete sent %d requests", n-before)
	}
	openDelete()
	p.run(chromedp.SendKeys(`#confirm-input`, "renamed/", chromedp.ByQuery))
	p.clickText(".modal:has(#confirm-input) .modal-actions button", "Delete folder and contents")
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`!` + listed("renamed/"))
	if n := actionPosts(); n != before+1 {
		t.Errorf("a confirmed delete sent %d requests, want 1", n-before)
	}

	// The Operations tab lists the rename and the delete. A row's action
	// refreshes only its own listing, so the page is read again first.
	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Operations")`)
	p.clickText("#view .tab-strip .tab", "Operations")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("RENAME_FOLDER")) &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("DELETE_FOLDER"))`)
}
