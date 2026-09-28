//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestPubSubEditSubscriptionThroughTheForm (#786), in the storage shard,
// whose instance serves Pub/Sub: a subscription's page has Edit
// subscription, whose dialog is prefilled from the subscription with its
// name, topic and filter shown disabled and no labels field, and fits the
// window however long the form is. Cancel after a change closes it at once,
// with no question asked and nothing sent. An ack deadline of 601 is sent and
// refused on the form with the emulator's own message; 30 with a push
// endpoint is saved with one PATCH, the dialog closes, and the console API's
// page reads the subscription as push with the new deadline.
func TestPubSubEditSubscriptionThroughTheForm(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/browser-edit"
	sub := "projects/" + project + "/subscriptions/browser-edit-sub"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project,
		`{"name":"browser-edit","defaultSubscription":"true"}`); code != http.StatusOK {
		t.Fatalf("create a topic and its default subscription through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})

	p.navigate("/pubsub/subscriptions?project=" + project)
	p.clickText("#view tbody a", sub)
	openEdit := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Edit subscription")
		p.waitFor(`document.querySelector(".modal.is-open #f-ackDeadline") !== null`)
	}
	set := func(id, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #%s");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, id, v), nil)
	}
	openEdit()

	var form struct {
		Ack, Endpoint, Name, Topic                  string
		NameDisabled, TopicDisabled, FilterDisabled bool
		HasLabels, Fits                             bool
	}
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		const r = q(".modal-body").getBoundingClientRect();
		return { Ack: q("#f-ackDeadline").value, Endpoint: q("#f-pushEndpoint").value,
		         Name: q("#f-name").value, NameDisabled: q("#f-name").disabled,
		         Topic: q("#f-topic").value, TopicDisabled: q("#f-topic").disabled,
		         FilterDisabled: q("#f-filter").disabled, HasLabels: q("#f-labels") !== null,
		         Fits: r.top >= 0 && r.bottom <= window.innerHeight }; })()`, &form)
	if form.Ack != "10" || form.Endpoint != "" || form.Name != "browser-edit-sub" || !form.NameDisabled ||
		form.Topic != topic || !form.TopicDisabled || !form.FilterDisabled || form.HasLabels {
		t.Errorf("the edit dialog is prefilled with %+v; want ack 10, no endpoint, the name, topic and filter disabled, no labels", form)
	}
	if !form.Fits {
		t.Error("the Edit subscription dialog runs past the window; a form taller than the window must scroll inside it")
	}

	// Cancel discards a change without asking (#783's rule).
	set("f-ackDeadline", "45")
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/pubsub-subscriptions"); len(sent) != 0 {
		t.Fatalf("Cancel sent the change: %v", sent)
	}

	openEdit()
	set("f-ackDeadline", "601")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal.is-open .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal.is-open .form-error").textContent`, &refusal)
	if want := "InvalidArgument: ack_deadline_secs out of bounds"; refusal != want {
		t.Errorf("an ack deadline of 601 was refused on the form with %q, want the emulator's %q", refusal, want)
	}
	// The refusal is on the page, not a browser error: it is forgiven.
	p.forgive()

	const endpoint = "http://127.0.0.1:9/cloudburrow-browser"
	set("f-ackDeadline", "30")
	set("f-pushEndpoint", endpoint)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/pubsub-subscriptions"); len(sent) != 2 {
		t.Errorf("the refused and the saved edit sent %d PATCHes, want 2: %v", len(sent), sent)
	}

	code, body := consoleDo(t, http.MethodGet, "/api/detail/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
	var detail struct {
		Summary []struct{ Label, Value string }
		Edit    struct {
			Fields []struct{ Name, Default string }
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); code != http.StatusOK || err != nil {
		t.Fatalf("read the subscription through the console API = %d (%v): %s", code, err, body)
	}
	got := map[string]string{}
	for _, f := range detail.Edit.Fields {
		got[f.Name] = f.Default
	}
	for _, pr := range detail.Summary {
		got[pr.Label] = pr.Value
	}
	if got["ackDeadline"] != "30" || got["pushEndpoint"] != endpoint || got["Delivery type"] != "Push" {
		t.Errorf("the console API does not read what the browser saved (ack 30, push to %s): %s", endpoint, body)
	}
}
