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

// TestPubSubEditSubscriptionThroughTheForm (#786), in the storage shard,
// whose instance serves Pub/Sub: a subscription's page has Edit
// subscription, whose dialog is prefilled from the subscription with its
// name, topic and filter shown disabled and a labels field (#949, applied by
// CloudBurrow's Pub/Sub front), and fits the window however long the form is. Cancel after a change closes it at once,
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
		form.Topic != topic || !form.TopicDisabled || !form.FilterDisabled || !form.HasLabels {
		t.Errorf("the edit dialog is prefilled with %+v; want ack 10, no endpoint, the name, topic and filter disabled, labels", form)
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

// TestPubSubEditSubscriptionExactlyOnceThroughTheForm (#880): Edit
// subscription offers exactly-once delivery as a checkbox, unchecked for a
// subscription without it. Checked and saved, the console API reads it on;
// checked with a push endpoint, the pair is refused on the form with its
// reason and nothing changes; unchecked and saved, it is off again.
func TestPubSubEditSubscriptionExactlyOnceThroughTheForm(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/browser-eod"
	sub := "projects/" + project + "/subscriptions/browser-eod-sub"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project,
		`{"name":"browser-eod","defaultSubscription":"true"}`); code != http.StatusOK {
		t.Fatalf("create a topic and its default subscription through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})
	exactlyOnce := func() string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/detail/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		var detail struct {
			Edit struct {
				Fields []struct{ Name, Default string }
			}
		}
		if err := json.Unmarshal([]byte(body), &detail); code != http.StatusOK || err != nil {
			t.Fatalf("read the subscription through the console API = %d (%v): %s", code, err, body)
		}
		for _, f := range detail.Edit.Fields {
			if f.Name == "exactlyOnce" {
				return f.Default
			}
		}
		return "absent"
	}

	p.navigate("/pubsub/subscriptions?project=" + project)
	p.clickText("#view tbody a", sub)
	openEdit := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Edit subscription")
		p.waitFor(`document.querySelector(".modal.is-open #f-exactlyOnce") !== null`)
	}
	save := func() {
		p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
		p.waitFor(`document.querySelector(".modal") === null`)
	}
	openEdit()
	var box struct {
		Type              string
		Checked, Disabled bool
	}
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-exactlyOnce");
		return { Type: f.type, Checked: f.checked, Disabled: f.disabled }; })()`, &box)
	if box.Type != "checkbox" || box.Checked || box.Disabled {
		t.Errorf("exactly-once on the form is %+v; want an enabled, unchecked checkbox", box)
	}
	setField(p, ".modal.is-open", "f-exactlyOnce", "true")
	save()
	if got := exactlyOnce(); got != "true" {
		t.Fatalf("after saving exactly-once, the console API reads %q", got)
	}

	openEdit()
	setField(p, ".modal.is-open", "f-pushEndpoint", "http://127.0.0.1:9/cloudburrow-browser")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal.is-open .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal.is-open .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "pull subscriptions only") {
		t.Errorf("exactly-once with a push endpoint was refused with %q; want the pull-only reason", refusal)
	}
	p.forgive()
	if got := exactlyOnce(); got != "true" {
		t.Errorf("after the refusal the console API reads exactly-once %q", got)
	}

	setField(p, ".modal.is-open", "f-pushEndpoint", "")
	setField(p, ".modal.is-open", "f-exactlyOnce", "false")
	save()
	if got := exactlyOnce(); got != "false" {
		t.Errorf("after turning exactly-once off, the console API reads %q", got)
	}
}

// TestPubSubEditSubscriptionExpirationThroughTheForm (#891), in the storage
// shard: Edit subscription's expiration period is an enabled field,
// prefilled with Google's 31-day default. A period under a day is refused on
// the open form with the reason CloudBurrow's Pub/Sub front gives, and
// nothing changes; 10d is saved and the console API reads it back, and so is
// never.
func TestPubSubEditSubscriptionExpirationThroughTheForm(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/browser-expiry"
	sub := "projects/" + project + "/subscriptions/browser-expiry-sub"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project,
		`{"name":"browser-expiry","defaultSubscription":"true"}`); code != http.StatusOK {
		t.Fatalf("create a topic and its default subscription through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})
	expiration := func() string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/detail/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		var detail struct {
			Edit struct {
				Fields []struct{ Name, Default string }
			}
		}
		if err := json.Unmarshal([]byte(body), &detail); code != http.StatusOK || err != nil {
			t.Fatalf("read the subscription through the console API = %d (%v): %s", code, err, body)
		}
		for _, f := range detail.Edit.Fields {
			if f.Name == "expiration" {
				return f.Default
			}
		}
		return "absent"
	}

	p.navigate("/pubsub/subscriptions?project=" + project)
	p.clickText("#view tbody a", sub)
	openEdit := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Edit subscription")
		p.waitFor(`document.querySelector(".modal.is-open #f-expiration") !== null`)
	}
	save := func() {
		p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
		p.waitFor(`document.querySelector(".modal") === null`)
	}
	openEdit()
	var field struct {
		Value    string
		Disabled bool
	}
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-expiration");
		return { Value: f.value, Disabled: f.disabled }; })()`, &field)
	if field.Value != "31d" || field.Disabled {
		t.Errorf("the expiration period on the form is %+v; want an enabled field reading Google's default, 31d", field)
	}

	setField(p, ".modal.is-open", "f-expiration", "12h")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal.is-open .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal.is-open .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "at least 1 day") {
		t.Errorf("an expiration of 12h was refused with %q; want the front's reason, at least 1 day", refusal)
	}
	p.forgive()
	if got := expiration(); got != "31d" {
		t.Errorf("after the refusal the console API reads expiration %q, want 31d", got)
	}

	setField(p, ".modal.is-open", "f-expiration", "10d")
	save()
	if got := expiration(); got != "10d" {
		t.Fatalf("after saving 10d, the console API reads expiration %q", got)
	}
	openEdit()
	setField(p, ".modal.is-open", "f-expiration", "never")
	save()
	if got := expiration(); got != "never" {
		t.Errorf("after saving never, the console API reads expiration %q", got)
	}
}
