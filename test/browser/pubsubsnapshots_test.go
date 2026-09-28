//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestPubSubSnapshotSeekAndDeleteThroughTheForms (#787), in the storage
// shard, whose instance serves Pub/Sub. On a subscription's page, Create
// snapshot asks for an ID and no labels; Cancel after typing one closes it at
// once with nothing sent (#783). The snapshot created through the form makes
// Seek to snapshot appear, prefilled with it. Submitting the seek asks for
// the subscription's name back, saying what the seek changes and not that it
// cannot be undone: the short name is refused with nothing sent, Cancel on
// the confirmation leaves the form open with nothing sent, and the full name
// sends one seek. On the Snapshots screen the snapshot is listed and deleted
// from its row, confirmed by its name, after which the console API no longer
// lists it.
func TestPubSubSnapshotSeekAndDeleteThroughTheForms(t *testing.T) {
	needService(t, "pubsub-snapshots")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/browser-snap"
	sub := "projects/" + project + "/subscriptions/browser-snap-sub"
	snap := "projects/" + project + "/snapshots/browser-snap-1"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project,
		`{"name":"browser-snap","defaultSubscription":"true"}`); code != http.StatusOK {
		t.Fatalf("create a topic and its default subscription through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-snapshots?project="+project+"&name="+url.QueryEscape(snap), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})
	const actions = "/api/actions/pubsub-subscriptions"

	p.navigate("/pubsub/subscriptions?project=" + project)
	p.clickText("#view tbody a", sub)
	openAction := func(label, field string) {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", label)
		p.waitFor(fmt.Sprintf(`document.querySelector(".modal.is-open #f-%s") !== null`, field))
	}

	// Create snapshot: an ID, no labels; Cancel discards without asking.
	openAction("Create snapshot", "name")
	var hasLabels bool
	p.eval(`document.querySelector(".modal.is-open #f-labels") !== null`, &hasLabels)
	if hasLabels {
		t.Error("Create snapshot offers labels, which the emulator does not keep")
	}
	p.run(chromedp.SendKeys(`.modal.is-open #f-name`, "discarded-snap", chromedp.ByQuery))
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 0 {
		t.Fatalf("Cancel sent the snapshot: %v", sent)
	}

	openAction("Create snapshot", "name")
	p.run(chromedp.SendKeys(`.modal.is-open #f-name`, "browser-snap-1", chromedp.ByQuery))
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 1 {
		t.Fatalf("Create snapshot sent %d requests, want 1: %v", len(sent), sent)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub-snapshots?project="+project, ""); code != http.StatusOK ||
		!strings.Contains(body, `"`+snap+`"`) {
		t.Fatalf("the console API does not list the snapshot created in the browser: %d %s", code, body)
	}

	// Seek to snapshot, offered now that the topic has one, prefilled.
	openAction("Seek to snapshot", "snapshot")
	var prefilled string
	p.eval(`document.querySelector(".modal.is-open #f-snapshot").value`, &prefilled)
	if prefilled != "browser-snap-1" {
		t.Errorf("Seek to snapshot is prefilled with %q, want browser-snap-1", prefilled)
	}
	submitSeek := func() {
		p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
		p.waitFor(`document.querySelector("#confirm-input") !== null && document.activeElement === document.querySelector("#confirm-input")`)
	}
	submitSeek()
	var confirm struct{ Title, Text string }
	p.eval(`(() => { const f = document.querySelector("#confirm-input").closest("form");
		return { Title: f.querySelector("h2").textContent, Text: f.textContent }; })()`, &confirm)
	if !strings.Contains(confirm.Title, sub) || !strings.Contains(confirm.Text, "redelivered") ||
		strings.Contains(confirm.Text, "cannot be undone") {
		t.Errorf("the seek confirmation reads %+v; want it to name %s, say what is redelivered, and not claim it cannot be undone", confirm, sub)
	}
	p.run(chromedp.SendKeys(`#confirm-input`, "browser-snap-sub", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector("#confirm-input").closest("form").querySelector(".form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector("#confirm-input").closest("form").querySelector(".form-error").textContent`, &refusal)
	if want := "Type " + sub + " exactly to confirm."; refusal != want {
		t.Errorf("a wrong name was refused with %q, want %q", refusal, want)
	}
	if sent := p.sent(http.MethodPost, actions); len(sent) != 1 {
		t.Fatalf("a seek confirmed with the wrong name was sent: %v", sent)
	}
	// Cancel on the confirmation: the seek form is still open, nothing sent.
	p.clickText(`form:has(#confirm-input) .modal-actions button`, "Cancel")
	p.waitFor(`document.querySelector("#confirm-input") === null && document.querySelector(".modal.is-open #f-snapshot") !== null`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 1 {
		t.Fatalf("cancelling the confirmation sent the seek: %v", sent)
	}
	submitSeek()
	p.run(chromedp.SendKeys(`#confirm-input`, sub, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 2 {
		t.Errorf("the create and the confirmed seek sent %d requests, want 2: %v", len(sent), sent)
	}

	// Delete from the Snapshots screen, confirmed by the snapshot's name.
	p.navigate("/pubsub/snapshots?project=" + project)
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+snap)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+snap), chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete")
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, snap, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) === null`, row))
	if sent := p.sent(http.MethodDelete, "/api/resources/pubsub-snapshots"); len(sent) != 1 {
		t.Errorf("the delete was sent %d times, want once: %v", len(sent), sent)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub-snapshots?project="+project, ""); code != http.StatusOK ||
		strings.Contains(body, `"`+snap+`"`) {
		t.Errorf("the console API still lists the snapshot deleted in the browser: %d %s", code, body)
	}
}
