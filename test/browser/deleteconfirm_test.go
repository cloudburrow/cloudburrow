//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// openRowDelete opens a list row's menu and chooses its Delete item.
func (p *tab) openRowDelete(name, item string) {
	p.t.Helper()
	menu := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, menu))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.run(chromedp.Click(chromedp.CSS(menu)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, item)
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm]") !== null`)
}

// confirmOnlyDialog checks that the open delete dialog is confirmed as
// Google's is for this kind, by its button alone (#1931): nothing to type,
// and focus on Cancel, so one reflexive Enter does not delete.
func (p *tab) confirmOnlyDialog() {
	p.t.Helper()
	p.confirmOnlyDialogLabelled("Delete")
}

// confirmOnlyDialogLabelled is confirmOnlyDialog for a dialog whose button
// is its own verb: Model Garden's Download and Deploy (#1931).
func (p *tab) confirmOnlyDialogLabelled(button string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm]") !== null`)
	var got struct {
		Style, Focus, Button string
		Input                bool
	}
	p.eval(`(() => { const f = document.querySelector(".modal.is-open form[data-confirm]");
		return { Style: f.dataset.confirm, Input: f.querySelector("#confirm-input") !== null,
		         Focus: document.activeElement ? document.activeElement.textContent : "",
		         Button: (f.querySelector(".modal-actions button.danger") || {}).textContent || "" }; })()`, &got)
	if got.Style != "confirm" || got.Input || got.Focus != "Cancel" || got.Button != button {
		p.t.Fatalf("the delete dialog is %+v; want Google's confirm-only dialog, nothing typed and focus on Cancel", got)
	}
}

// pressConfirm checks the open dialog is confirm-only with the button
// given, and presses it.
func (p *tab) pressConfirm(button string) {
	p.t.Helper()
	p.confirmOnlyDialogLabelled(button)
	p.pressDelete()
}

// pressDelete presses the open confirm-only dialog's Delete.
func (p *tab) pressDelete() {
	p.t.Helper()
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open .modal-actions button.danger`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
}

// TestDeleteConfirmFollowsGooglePerKind (#1931): a delete is confirmed as
// Google's console confirms that kind. A Pub/Sub topic's asks for the word
// delete (docs.cloud.google.com/pubsub/docs/delete-topic): the topic's own
// name is refused and nothing is sent, delete deletes it. A Cloud Storage
// bucket's asks for nothing typed (docs.cloud.google.com/storage/docs/
// deleting-buckets): Enter on the dialog cancels, focus being on Cancel, and
// Delete deletes it. A subscription's, which Google's docs do not describe,
// keeps the name typed back (TestSubscriptionsDeleteConfirmedByName).
func TestDeleteConfirmFollowsGooglePerKind(t *testing.T) {
	needService(t, "pubsub")
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	short := "confirm-topic"
	topic := "projects/" + project + "/topics/" + short
	bucket := fmt.Sprintf("confirm-%d", time.Now().UnixNano()%1e9)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project, `{"name":"`+short+`"}`); code != http.StatusOK {
		t.Fatalf("create a topic through the console API = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage?project="+project, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+url.QueryEscape(bucket), "")
	})

	// The topic: type delete.
	p.navigate("/pubsub/topics?project=" + project)
	p.openRowDelete(topic, "Delete")
	var label struct{ Style, Word string }
	p.eval(`(() => { const f = document.querySelector(".modal.is-open form[data-confirm]");
		return { Style: f.dataset.confirm, Word: f.querySelector("label[for=confirm-input] strong").textContent }; })()`, &label)
	if label.Style != "delete" || label.Word != "delete" {
		t.Fatalf("the topic's delete dialog asks %+v; want the word delete, as Google's does", label)
	}
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), topic), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if refusal != "Type delete exactly to confirm." {
		t.Errorf("the topic's name typed back is refused with %q", refusal)
	}
	if sent := p.sent(http.MethodDelete, "/api/resources/pubsub"); len(sent) != 0 {
		t.Fatalf("a topic delete confirmed with its name, not delete, was sent: %v", sent)
	}
	p.eval(`(() => { document.querySelector(".modal #confirm-input").value = ""; return true; })()`, nil)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), "delete"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) === null`,
		fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+topic)))
	if sent := p.sent(http.MethodDelete, "/api/resources/pubsub"); len(sent) != 1 {
		t.Fatalf("delete sent %d topic deletes, want 1: %v", len(sent), sent)
	}

	// The bucket: nothing typed; Enter cancels.
	p.navigate("/storage/browser?project=" + project)
	p.openRowDelete(bucket, "Delete")
	p.confirmOnlyDialog()
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if sent := p.sent(http.MethodDelete, "/api/resources/storage"); len(sent) != 0 {
		t.Fatalf("Enter on the bucket's delete dialog deleted it: %v", sent)
	}
	p.openRowDelete(bucket, "Delete")
	p.confirmOnlyDialog()
	p.pressDelete()
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) === null`, fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+bucket)))
	if sent := p.sent(http.MethodDelete, "/api/resources/storage"); len(sent) != 1 {
		t.Fatalf("Delete sent %d bucket deletes, want 1: %v", len(sent), sent)
	}
}

// confirmEntityDelete confirms a Datastore entity's Delete entity as
// Google's is, "Click Delete, and then confirm"
// (docs.cloud.google.com/datastore/docs/store-query-data, #1931): nothing
// typed, the dialog naming the entity by the key its page is headed by.
func (p *tab) confirmEntityDelete(heading string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm]") !== null`)
	p.confirmOnlyDialog()
	var title string
	p.eval(`document.querySelector(".modal.is-open #confirm-title").textContent`, &title)
	if want := "Delete entity " + heading + "?"; title != want {
		p.t.Errorf("the delete dialog is titled %q, want %q", title, want)
	}
	p.pressDelete()
}
