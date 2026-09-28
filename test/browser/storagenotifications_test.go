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
	"github.com/chromedp/chromedp/kb"
)

// TestStorageNotificationsThroughTheForm (#791), in the storage shard. A
// bucket's Notifications tab starts empty. Create notification opens a form
// whose topic is a picker of the project's topics, starting on the one the
// project has; Cancel after a change closes it at once and sends nothing. A
// notification saved through the form, for one event type, a prefix and a
// custom attribute, is listed on the tab and by the console API. Delete on
// its row asks for its name back, and the name sends one action, after which
// the row is gone and the console API lists none.
func TestStorageNotificationsThroughTheForm(t *testing.T) {
	needService(t, "storage")
	needService(t, "pubsub")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-notify"
	topic := "projects/" + project + "/topics/browser-notify"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project, `{"name":"browser-notify"}`); code != http.StatusOK {
		t.Fatalf("create a topic through the console API = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage?project="+project, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		// A bucket's notification configurations go with it.
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})

	// rows are the tab's notifications, as the console API lists them.
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
			if s.ID == "notifications" {
				return s.Listing.Items
			}
		}
		t.Fatalf("the bucket's page has no Notifications tab: %s", body)
		return nil
	}
	setValue := func(sel, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(%q);
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, sel, v), nil)
	}
	actionPosts := func() int { return len(p.sent(http.MethodPost, "/api/actions/storage")) }

	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	p.waitFor(`document.querySelector("#tab-notifications") !== null`)
	p.run(chromedp.Click(`#tab-notifications`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#tab-notifications").getAttribute("aria-selected") === "true"`)

	openCreate := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Create notification")
		p.waitFor(`document.querySelector(".modal.is-open #f-topic") !== null`)
	}
	openCreate()
	var form struct {
		Tag, Topic, Format string
		Options            []string
	}
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		return { Tag: q("#f-topic").tagName, Topic: q("#f-topic").value, Format: q("#f-payloadFormat").value,
		         Options: [...q("#f-topic").options].map((o) => o.value) }; })()`, &form)
	if form.Tag != "SELECT" || form.Topic != topic || strings.Join(form.Options, ",") != topic || form.Format != "JSON_API_V1" {
		t.Errorf("the create form reads %+v; want a picker of the project's one topic %s, and JSON_API_V1", form, topic)
	}

	// Cancel discards a change without asking (#783).
	setValue(".modal.is-open #f-prefix", "discarded/")
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := actionPosts(); n != 0 {
		t.Fatalf("Cancel sent the notification: %d action requests", n)
	}

	openCreate()
	setValue(".modal.is-open #f-prefix", "in/")
	setValue(".modal.is-open #f-attributes", "team=blue")
	p.run(chromedp.Click(`.modal.is-open #f-event_OBJECT_FINALIZE`, chromedp.ByQuery))
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := actionPosts(); n != 1 {
		t.Errorf("the create sent %d action requests, want 1", n)
	}
	got := rows()
	if len(got) != 1 || got[0].Fields["Topic"] != topic || got[0].Fields["Event types"] != "OBJECT_FINALIZE" ||
		got[0].Fields["Object name prefix"] != "in/" || got[0].Fields["Custom attributes"] != "team=blue" {
		t.Fatalf("after the browser's create the console API lists %+v", got)
	}
	name := got[0].Name

	// Delete from its row, confirmed by its name.
	menu := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, menu))
	p.run(chromedp.Click(menu, chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete")
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, name, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) === null`, menu))
	if n := actionPosts(); n != 2 {
		t.Errorf("the create and the delete sent %d action requests, want 2", n)
	}
	if left := rows(); len(left) != 0 {
		t.Errorf("after the browser's delete the console API lists %+v", left)
	}
}
