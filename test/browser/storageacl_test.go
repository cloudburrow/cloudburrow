//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestStorageACLsThroughTheBrowser (#1233), in the storage shard. A
// bucket's Access control tab lists its ACL and its default object ACL.
// Add ACL entry, for the bucket's own ACL, saves one entry, listed on the
// tab and by the console API; Remove on its row asks for its name back, and
// the name sends one action, after which the row is gone.
func TestStorageACLsThroughTheBrowser(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-acl"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage?project="+project, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})
	type row struct {
		Name   string
		Fields map[string]string
	}
	rows := func() []row {
		t.Helper()
		v := url.Values{"project": {project}, "name": {bucket}}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d struct {
			Sections []struct {
				ID      string
				Listing struct{ Items []row }
			}
		}
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
			t.Fatalf("read the bucket through the console API = %d (%v): %s", code, err, body)
		}
		for _, s := range d.Sections {
			if s.ID == "acl" {
				return s.Listing.Items
			}
		}
		t.Fatalf("the bucket's page has no Access control tab: %s", body)
		return nil
	}
	find := func(rs []row, entity string) (row, bool) {
		for _, r := range rs {
			if r.Name == entity {
				return r, true
			}
		}
		return row{}, false
	}
	if got := rows(); len(got) != 6 {
		t.Fatalf("a new bucket's Access control tab lists %+v; want projectPrivate's entries for the bucket and its objects", got)
	}
	setValue := func(sel, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(%q);
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true }));
			f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, sel, v), nil)
	}
	actionPosts := func() int { return len(p.sent(http.MethodPost, "/api/actions/storage")) }

	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	p.waitFor(`document.querySelector("#tab-acl") !== null`)
	p.run(chromedp.Click(`#tab-acl`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#tab-acl").getAttribute("aria-selected") === "true"`)

	p.clickText("#view .page-actions button", "Add ACL entry")
	p.waitFor(`document.querySelector(".modal.is-open #f-entity") !== null`)
	setValue(".modal.is-open #f-scope", "bucket")
	setValue(".modal.is-open #f-entity", "user-dev@example.com")
	setValue(".modal.is-open #f-role", "WRITER")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := actionPosts(); n != 1 {
		t.Errorf("Add ACL entry sent %d action requests, want 1", n)
	}
	added, ok := find(rows(), "user-dev@example.com")
	if !ok || added.Fields["Role"] != "WRITER" || added.Fields["ACL"] != "Bucket" {
		t.Fatalf("after the browser's add the console API lists %+v", rows())
	}

	menu := fmt.Sprintf(`button[aria-label=%q]`, "Actions for user-dev@example.com")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, menu))
	p.run(chromedp.Click(menu, chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Remove")
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "user-dev@example.com", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) === null`, menu))
	if n := actionPosts(); n != 2 {
		t.Errorf("the add and the remove sent %d action requests, want 2", n)
	}
	if _, still := find(rows(), "user-dev@example.com"); still {
		t.Errorf("after the browser's remove the console API still lists the entry: %+v", rows())
	}
}
