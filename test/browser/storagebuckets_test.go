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

// TestStorageBucketSettingsThroughTheForm (#789), in the storage shard. On a
// bucket's page, Edit bucket opens prefilled from the bucket, its location
// disabled. After a change, Cancel closes it at once and sends nothing. A
// soft delete retention the API refuses is refused on the form with the API's
// message; labels, a retention period and a lifecycle rule saved through it
// are what the console API's page for the bucket reads. The page then offers
// Lock retention policy, whose confirmation says it is permanent and asks for
// the bucket's name: Cancel there sends nothing, and typing it locks the
// policy, after which the page no longer offers it.
func TestStorageBucketSettingsThroughTheForm(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-settings"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage?project="+project, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})

	// bucketPage reads the bucket's page through the console API: the edit
	// form's defaults and the configuration by label.
	bucketPage := func() (edit, config map[string]string) {
		t.Helper()
		v := url.Values{"project": {project}, "name": {bucket}}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d struct {
			Edit struct {
				Fields []struct{ Name, Default string }
			}
			Sections []struct {
				Groups []struct {
					Properties []struct{ Label, Value string }
				}
			}
		}
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
			t.Fatalf("read %s through the console API = %d (%v): %s", bucket, code, err, body)
		}
		edit, config = map[string]string{}, map[string]string{}
		for _, f := range d.Edit.Fields {
			edit[f.Name] = f.Default
		}
		for _, s := range d.Sections {
			for _, g := range s.Groups {
				for _, pr := range g.Properties {
					config[pr.Label] = pr.Value
				}
			}
		}
		return edit, config
	}
	setValue := func(sel, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(%q);
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, sel, v), nil)
	}
	patches := func() int { return len(p.sent(http.MethodPatch, "/api/resources/storage")) }
	actionPosts := func() int { return len(p.sent(http.MethodPost, "/api/actions/storage")) }
	pageButton := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === %q)`, label)
	}
	openEdit := func() {
		p.waitFor(pageButton("Edit bucket"))
		p.clickText("#view .page-actions button", "Edit bucket")
		p.waitFor(`document.querySelector(".modal.is-open #f-storageClass") !== null`)
	}

	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	openEdit()
	var form struct {
		Class, SoftDelete, Location string
		LocationDisabled            bool
	}
	p.eval(`(() => { const q = (s) => document.querySelector(".modal " + s);
		return { Class: q("#f-storageClass").value, SoftDelete: q("#f-softDeleteSeconds").value,
		         Location: q("#f-location").value, LocationDisabled: q("#f-location").disabled }; })()`, &form)
	if form.Class != "STANDARD" || form.SoftDelete != "604800" || form.Location != "US" || !form.LocationDisabled {
		t.Errorf("Edit bucket is prefilled with %+v; want STANDARD, 604800 and the location US, disabled", form)
	}

	// Cancel discards a change without asking (#783's rule).
	setValue(".modal #f-labels", "team=blue")
	p.clickText(".modal .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := patches(); n != 0 {
		t.Fatalf("Cancel sent the edit: %d PATCHes", n)
	}

	// The API's refusal, on the form.
	openEdit()
	setValue(".modal #f-softDeleteSeconds", "5")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "between 604800 (7 days) and 7776000 (90 days)") {
		t.Errorf("a soft delete retention of 5 s was refused with %q, want the API's message", refusal)
	}
	p.forgive()

	setValue(".modal #f-softDeleteSeconds", "604800")
	setValue(".modal #f-labels", "team=blue")
	setValue(".modal #f-retentionPeriod", "60")
	setValue(".modal #f-lifecycle", `[{"action":{"type":"Delete"},"condition":{"age":30}}]`)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := patches(); n != 2 {
		t.Errorf("the refused and the saved edit sent %d PATCHes, want 2", n)
	}
	edit, config := bucketPage()
	if edit["labels"] != `{"team":"blue"}` || edit["retentionPeriod"] != "60" ||
		config["Lifecycle rules"] != "1" || config["Retention policy"] != "60 s" {
		t.Errorf("after the browser's edit the bucket reads %v, %v", edit, config)
	}

	// Lock retention policy: permanent, said so, and confirmed by name.
	p.waitFor(pageButton("Lock retention policy"))
	p.clickText("#view .page-actions button", "Lock retention policy")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	var warning string
	p.eval(`document.querySelector(".modal:has(#confirm-input) .confirm-detail").textContent`, &warning)
	if !strings.Contains(warning, "Locking is permanent") || !strings.Contains(warning, "never be removed") {
		t.Errorf("the lock's confirmation says %q", warning)
	}
	p.clickText(".modal:has(#confirm-input) .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector("#confirm-input") === null`)
	if n := actionPosts(); n != 0 {
		t.Fatalf("cancelling the lock sent %d requests", n)
	}
	p.clickText("#view .page-actions button", "Lock retention policy")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.run(chromedp.SendKeys(`#confirm-input`, bucket, chromedp.ByQuery))
	p.clickText(".modal:has(#confirm-input) .modal-actions button", "Lock retention policy")
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := actionPosts(); n != 1 {
		t.Errorf("a confirmed lock sent %d requests, want 1", n)
	}
	if _, config := bucketPage(); config["Retention policy"] != "60 s, locked" {
		t.Errorf("after the browser's lock the retention policy reads %q", config["Retention policy"])
	}
	p.waitFor(`!` + pageButton("Lock retention policy") + ` && ` + pageButton("Edit bucket"))
}
