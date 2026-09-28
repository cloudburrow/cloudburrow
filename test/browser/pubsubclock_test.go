//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestPubSubAdvanceClockThroughTheForm (#1040), in the storage shard, whose
// instance serves Pub/Sub. A subscription's page offers Advance clock
// (CloudBurrow extension), whose form says it is a CloudBurrow test
// extension. Submitting it asks for the subscription's name back, saying the
// clock is the whole instance's and never moves back; Cancel on that sends
// nothing, and the name sends one advance, whose answer names the front's
// new clock.
func TestPubSubAdvanceClockThroughTheForm(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/browser-clock"
	sub := "projects/" + project + "/subscriptions/browser-clock-sub"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project,
		`{"name":"browser-clock","defaultSubscription":"true"}`); code != http.StatusOK {
		t.Fatalf("create a topic and its default subscription through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})
	const actions = "/api/actions/pubsub-subscriptions"

	p.navigate("/pubsub/subscriptions?project=" + project)
	p.clickText("#view tbody a", sub)
	p.clickText("#view .page-actions button", "Advance clock (CloudBurrow extension)")
	p.waitFor(`document.querySelector(".modal.is-open #f-duration") !== null`)
	var help string
	p.eval(`document.querySelector(".modal.is-open #h-duration").textContent`, &help)
	if !strings.Contains(help, "CloudBurrow test extension") || !strings.Contains(help, "every project") {
		t.Errorf("the form's help is %q, want it to say it is a CloudBurrow test extension for the whole instance", help)
	}
	p.setField(".modal.is-open #f-duration", "1h")
	submit := func() {
		p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
		p.waitFor(`document.querySelector("#confirm-input") !== null && document.activeElement === document.querySelector("#confirm-input")`)
	}
	submit()
	var confirm string
	p.eval(`document.querySelector("#confirm-input").closest("form").textContent`, &confirm)
	if !strings.Contains(confirm, "whole instance") || !strings.Contains(confirm, "never be moved back") {
		t.Errorf("the confirmation reads %q", confirm)
	}
	p.clickText(`form:has(#confirm-input) .modal-actions button`, "Cancel")
	p.waitFor(`document.querySelector("#confirm-input") === null && document.querySelector(".modal.is-open #f-duration") !== null`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 0 {
		t.Fatalf("cancelling the confirmation sent the advance: %v", sent)
	}
	submit()
	p.run(chromedp.SendKeys(`#confirm-input`, sub, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#action-result-note") !== null`)
	var note string
	p.eval(`document.querySelector("#action-result-note").textContent`, &note)
	if !strings.HasPrefix(note, "The Pub/Sub front's clock is now ") || !strings.Contains(note, "advanced by 1h") {
		t.Errorf("the answer reads %q, want the front's new clock", note)
	}
	if sent := p.sent(http.MethodPost, actions); len(sent) != 1 {
		t.Errorf("the confirmed advance sent %d requests, want 1: %v", len(sent), sent)
	}
}
