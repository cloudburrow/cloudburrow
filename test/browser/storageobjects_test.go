//go:build browser

package browser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestStorageObjectMetadataAndCopyThroughTheForms (#790), in the storage
// shard. An object's row opens its own page, whose crumbs are its bucket and
// folder. Edit metadata there is prefilled from the object; Cancel after a
// change closes it at once and sends nothing; Content-Type and a custom key
// saved through the form are what the console API's page for the object then
// reads. Copy onto the object's own name, the form's default, is refused on
// the form with the API's precondition failure; to a new name it is made.
// Copy with Replace checked asks for the destination's name back before
// anything is sent, Cancel there sends nothing, and typing it sends one copy.
func TestStorageObjectMetadataAndCopyThroughTheForms(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-objects"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage?project="+project, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	objectQuery := func(name string) string {
		return url.Values{"project": {project}, "name": {bucket, name}}.Encode()
	}
	t.Cleanup(func() {
		for _, o := range []string{"docs/a.txt", "docs/copy.txt"} {
			consoleDo(t, http.MethodDelete, "/api/objects/storage?"+objectQuery(o), "")
		}
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})
	uploadObject(t, project, []string{bucket, "docs"}, "a.txt", "alpha")

	// objectPage reads an object's page through the console API: its custom
	// metadata and headers by label.
	objectPage := func(name string) map[string]string {
		t.Helper()
		v := url.Values{"project": {project}, "name": {"_details", bucket, name}}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d struct {
			Unavailable string
			Sections    []struct {
				Groups []struct {
					Heading    string
					Properties []struct{ Label, Value string }
				}
			}
		}
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
			t.Fatalf("read %s through the console API = %d (%v): %s", name, code, err, body)
		}
		out := map[string]string{"unavailable": d.Unavailable}
		for _, s := range d.Sections {
			for _, g := range s.Groups {
				for _, pr := range g.Properties {
					out[g.Heading+"/"+pr.Label] = pr.Value
				}
			}
		}
		return out
	}
	setValue := func(sel, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(%q);
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, sel, v), nil)
	}
	actionPosts := func() int { return len(p.sent(http.MethodPost, "/api/actions/storage")) }

	p.navigate("/storage/browser/" + bucket + "/docs?project=" + project)
	p.clickText("#view tbody a", "a.txt")
	// The page's actions are drawn with its trail, once the page has loaded.
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "docs/a.txt" &&
		document.querySelector("#view .page-actions") !== null`)
	var crumbs string
	p.eval(`[...document.querySelectorAll("#view .breadcrumb a, #view .breadcrumb span:not([aria-hidden])")].map((n) => n.textContent).join(" / ")`, &crumbs)
	if crumbs != "Cloud Storage / "+bucket+" / docs / a.txt" {
		t.Errorf("the object page's crumbs read %q", crumbs)
	}

	openAction := func(label, field string) {
		p.clickText("#view .page-actions button", label)
		p.waitFor(fmt.Sprintf(`document.querySelector(".modal.is-open #f-%s") !== null`, field))
	}

	// Edit metadata: prefilled, Cancel discards, Save patches.
	openAction("Edit metadata", "contentType")
	var prefilled string
	p.eval(`document.querySelector(".modal #f-contentType").value`, &prefilled)
	if before := objectPage("docs/a.txt")["Object/Content-Type"]; prefilled == "" || prefilled != before {
		t.Errorf("Edit metadata is prefilled with Content-Type %q; the object's is %q", prefilled, before)
	}
	setValue(".modal #f-contentType", "application/json")
	p.clickText(".modal .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := actionPosts(); n != 0 {
		t.Fatalf("Cancel sent the edit: %d action requests", n)
	}
	openAction("Edit metadata", "contentType")
	setValue(".modal #f-contentType", "application/json")
	setValue(".modal #f-cacheControl", "no-cache")
	setValue(".modal #f-metadata", "stage=edited")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	after := objectPage("docs/a.txt")
	if after["Object/Content-Type"] != "application/json" || after["Object/Cache-Control"] != "no-cache" ||
		after["Custom metadata/stage"] != "edited" || after["Version/Metageneration"] != "2" {
		t.Errorf("after the browser's edit the object reads %v", after)
	}

	// Copy onto its own name: the API's refusal, on the form.
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Copy")`)
	openAction("Copy", "destination")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "412") || !strings.Contains(refusal, "pre-conditions you specified did not hold") {
		t.Errorf("a copy onto the object's own name was refused with %q, want the API's precondition failure", refusal)
	}
	p.forgive()
	setValue(".modal #f-destination", "docs/copy.txt")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if c := objectPage("docs/copy.txt"); c["unavailable"] != "" || c["Custom metadata/stage"] != "edited" {
		t.Errorf("the copy made in the browser reads %v", c)
	}
	sent := actionPosts()

	// Copy with Replace: asks for the name first; Cancel there sends nothing.
	openAction("Copy", "destination")
	setValue(".modal #f-destination", "docs/copy.txt")
	p.run(chromedp.Click(`.modal #f-replace`, chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.clickText(".modal:has(#confirm-input) .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector("#confirm-input") === null`)
	if n := actionPosts(); n != sent {
		t.Fatalf("cancelling the replace confirmation sent %d requests", n-sent)
	}
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.run(chromedp.SendKeys(`#confirm-input`, "docs/copy.txt", chromedp.ByQuery))
	p.clickText(".modal:has(#confirm-input) .modal-actions button", "Replace")
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := actionPosts(); n != sent+1 {
		t.Errorf("a confirmed replace sent %d requests, want 1", n-sent)
	}
}

// uploadObject writes an object through the console's upload route.
func uploadObject(t *testing.T, project string, prefix []string, filename, data string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", filename)
	_, _ = io.WriteString(part, data)
	_ = mw.Close()
	v := url.Values{"project": {project}, "name": prefix}
	resp, err := http.Post("http://"+endpoint(t, envConsole)+"/api/objects/storage/upload?"+v.Encode(),
		mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK {
		t.Fatalf("upload %s through the console = %d: %s", filename, resp.StatusCode, b)
	}
}
