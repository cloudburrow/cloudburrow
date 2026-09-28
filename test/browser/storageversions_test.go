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

// TestStorageObjectVersionsThroughTheBrowser (#853), in the storage shard.
// In a bucket whose versioning Edit bucket turned on, an object written twice
// is listed once; Show versions lists both generations, named
// name#generation, and stays on for the next page. Restore as live version
// on the noncurrent row makes its bytes live, as a third generation. Delete
// version on a row says it is not kept as noncurrent and asks for the
// object's name: Cancel sends nothing, and typing it deletes that generation
// alone. A generation's row opens its own page, whose crumbs lead to the
// object's. Edit holds and custom time on the object's page sets a temporary
// hold and a custom time the console API's page then reads. Run lifecycle
// now, on the bucket's page, is labelled a CloudBurrow extension and shows
// what the run did in its dialog.
func TestStorageObjectVersionsThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-versions"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage?project="+project, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}

	type row struct {
		Name   string
		Fields map[string]string
		Target []string
	}
	// versions reads Show versions through the console API.
	versions := func() []row {
		t.Helper()
		v := url.Values{"project": {project}, "name": {bucket, "docs"}}
		code, body := consoleDo(t, http.MethodGet, "/api/objects/storage/versions?"+v.Encode(), "")
		var l struct{ Items []row }
		if err := json.Unmarshal([]byte(body), &l); code != http.StatusOK || err != nil {
			t.Fatalf("versions through the console API = %d (%v): %s", code, err, body)
		}
		return l.Items
	}
	act := func(path []string, id string, values map[string]string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": id, "Values": values})
		if code, resp := consoleDo(t, http.MethodPost, "/api/actions/storage?project="+project, string(body)); code != http.StatusOK {
			t.Errorf("%s through the console API = %d: %s", id, code, resp)
		}
	}
	t.Cleanup(func() {
		act([]string{"_details", bucket, "docs/a.txt"}, "editholds",
			map[string]string{"temporaryHold": "false", "eventBasedHold": "false", "customTime": "2026-01-02T15:04:05Z"})
		for _, r := range versions() {
			act(r.Target, "deleteversion", nil)
		}
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})

	// Versioning on, through Edit bucket as its form submits it.
	v := url.Values{"project": {project}, "name": {bucket}}
	_, body := consoleDo(t, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
	var d struct {
		Edit struct {
			Fields []struct {
				Name, Default string
				Immutable     bool
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("decode %s: %v: %s", bucket, err, body)
	}
	values := map[string]string{}
	for _, f := range d.Edit.Fields {
		if !f.Immutable {
			values[f.Name] = f.Default
		}
	}
	values["versioning"] = "true"
	patch, _ := json.Marshal(map[string]any{"Path": []string{bucket}, "Values": values})
	if code, resp := consoleDo(t, http.MethodPatch, "/api/resources/storage?project="+project, string(patch)); code != http.StatusOK {
		t.Fatalf("turn on versioning through Edit bucket = %d: %s", code, resp)
	}
	uploadObject(t, project, []string{bucket, "docs"}, "a.txt", "one")
	uploadObject(t, project, []string{bucket, "docs"}, "a.txt", "two")
	rows := versions()
	if len(rows) != 2 || rows[1].Fields["State"] != "Live" {
		t.Fatalf("the console API lists %+v, want two generations of a.txt", rows)
	}
	oldName, oldGen := rows[0].Name, rows[0].Fields["Generation"]

	actionPosts := func() int { return len(p.sent(http.MethodPost, "/api/actions/storage")) }
	rowCount := func(n int) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].filter((r) => r.textContent.includes("a.txt#")).length === %d`, n)
	}
	menu := func(name, item string) {
		t.Helper()
		p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name), chromedp.ByQuery))
		p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, item)
	}

	p.navigate("/storage/browser/" + bucket + "/docs?project=" + project)
	p.waitFor(`document.querySelector("#object-versions-toggle") !== null && [...document.querySelectorAll("#view tbody tr")].length === 1`)
	p.run(chromedp.Click(`#object-versions-toggle`, chromedp.ByQuery))
	p.waitFor(rowCount(2))

	// Restore as live version.
	menu(oldName, "Restore as live version")
	p.waitFor(rowCount(3))
	if n := actionPosts(); n != 1 {
		t.Errorf("Restore as live version sent %d requests, want 1", n)
	}
	code, live := consoleDo(t, http.MethodGet, "/api/objects/storage/download?"+
		url.Values{"project": {project}, "name": {bucket, "docs/a.txt"}}.Encode(), "")
	if code != http.StatusOK || live != "one" {
		t.Errorf("after the restore the object downloads as %d %q", code, live)
	}

	// Delete version: Cancel sends nothing; the name typed back deletes it.
	menu(oldName, "Delete version")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	var warning string
	p.eval(`document.querySelector(".modal:has(#confirm-input) .confirm-detail").textContent`, &warning)
	if !strings.Contains(warning, "Generation "+oldGen) || !strings.Contains(warning, "not kept as a noncurrent version") {
		t.Errorf("Delete version's confirmation says %q", warning)
	}
	p.clickText(".modal:has(#confirm-input) .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector("#confirm-input") === null`)
	if n := actionPosts(); n != 1 {
		t.Fatalf("cancelling Delete version sent a request: %d in all", n)
	}
	menu(oldName, "Delete version")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.run(chromedp.SendKeys(`#confirm-input`, "docs/a.txt", chromedp.ByQuery))
	p.clickText(".modal:has(#confirm-input) .modal-actions button", "Delete version")
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(rowCount(2))
	for _, r := range versions() {
		if r.Name == oldName {
			t.Errorf("the deleted generation is still listed: %+v", r)
		}
	}

	// A generation's page, reached from its row, whose crumbs lead on.
	rows = versions()
	noncurrent := rows[0]
	if !strings.HasPrefix(noncurrent.Fields["State"], "Noncurrent") {
		t.Fatalf("the first remaining version is %+v", noncurrent)
	}
	p.clickText("#view tbody a", noncurrent.Name)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "docs/a.txt" &&
		document.querySelector("#view .page-actions") !== null`)
	var crumbs string
	p.eval(`[...document.querySelectorAll("#view .breadcrumb a, #view .breadcrumb span:not([aria-hidden])")].map((n) => n.textContent).join(" / ")`, &crumbs)
	if want := "Buckets / " + bucket + " / docs / a.txt / Generation " + noncurrent.Fields["Generation"]; crumbs != want {
		t.Errorf("the generation page's crumbs read %q, want %q", crumbs, want)
	}

	// Edit holds and custom time, on the object's page.
	p.clickText("#view .breadcrumb a", "a.txt")
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Edit holds and custom time")`)
	p.clickText("#view .page-actions button", "Edit holds and custom time")
	p.waitFor(`document.querySelector(".modal.is-open #f-temporaryHold") !== null`)
	p.run(chromedp.Click(`.modal.is-open #f-temporaryHold`, chromedp.ByQuery))
	p.run(chromedp.SendKeys(`.modal.is-open #f-customTime`, "2026-01-02T15:04:05Z", chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	v = url.Values{"project": {project}, "name": {"_details", bucket, "docs/a.txt"}}
	_, body = consoleDo(t, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
	var page struct {
		Sections []struct {
			Groups []struct {
				Heading    string
				Properties []struct{ Label, Value string }
			}
		}
	}
	_ = json.Unmarshal([]byte(body), &page)
	props := map[string]string{}
	for _, s := range page.Sections {
		for _, g := range s.Groups {
			for _, pr := range g.Properties {
				props[g.Heading+"/"+pr.Label] = pr.Value
			}
		}
	}
	if props["Protection/Temporary hold"] != "On" || props["Object/Custom time"] != "2026-01-02T15:04:05Z" {
		t.Errorf("after the browser's edit the object reads %v", props)
	}

	// Run lifecycle now, on the bucket's page.
	p.clickText("#view .breadcrumb a", bucket)
	label := "Run lifecycle now (CloudBurrow extension)"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === %q)`, label))
	p.clickText("#view .page-actions button", label)
	p.waitFor(`document.querySelector(".modal.is-open #f-scope") !== null`)
	var help string
	p.eval(`document.querySelector(".modal.is-open").textContent`, &help)
	if !strings.Contains(help, "/_cloudburrow/lifecycle") || !strings.Contains(help, "not a Google Cloud call") {
		t.Errorf("Run lifecycle now's form says %q", help)
	}
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#action-result") !== null`)
	var result string
	p.eval(`document.querySelector("#action-result").textContent`, &result)
	if !strings.Contains(result, "Versions deleted") || !strings.Contains(result, "Kept by a hold or retention") {
		t.Errorf("Run lifecycle now's dialog shows %q", result)
	}
	p.clickText("#action-result .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)

	// Show versions stays on for the next object list.
	p.navigate("/storage/browser/" + bucket + "/docs?project=" + project)
	p.waitFor(`document.querySelector("#object-versions-toggle") !== null && document.querySelector("#object-versions-toggle").checked`)
	p.waitFor(rowCount(2))
	p.run(chromedp.Click(`#object-versions-toggle`, chromedp.ByQuery))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].length === 1`)
}
