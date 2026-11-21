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

// TestPubSubEditSubscriptionThroughTheForm (#786, #1847), in the storage
// shard, whose instance serves Pub/Sub: a subscription's page has Edit,
// which opens Google's Edit subscription page, prefilled from the
// subscription with its name, topic and filter shown disabled and a labels
// field (#949, applied by CloudBurrow's Pub/Sub front), ending in Update,
// and meeting WCAG 2.2 AA. Cancel after a change goes back to the
// subscription with nothing sent. An ack deadline of 601 is sent and refused
// on the page with the emulator's own message; 30 with Delivery type Push
// and an endpoint is saved with one PATCH, the page goes back to the
// subscription, and the console API's page reads it as push with the new
// deadline.
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
		p.clickText("#view .page-actions button", "Edit")
		p.waitFor(`document.querySelector("form.edit-page #f-ackDeadline") !== null`)
	}
	set := func(id, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector("form.edit-page #%s");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, id, v), nil)
	}
	openEdit()

	var form struct {
		Ack, Endpoint, Name, Topic                  string
		NameDisabled, TopicDisabled, FilterDisabled bool
		HasLabels, Fits                             bool
	}
	p.eval(`(() => { const q = (s) => document.querySelector("form.edit-page " + s);
		const h = document.querySelector("#view h1").textContent;
		const sub = q('button[type="submit"]').textContent;
		return { Ack: q("#f-ackDeadline").value, Endpoint: q("#f-pushEndpoint").value,
		         Name: q("#f-name").value, NameDisabled: q("#f-name").disabled,
		         Topic: q("#f-topic").value, TopicDisabled: q("#f-topic").disabled,
		         FilterDisabled: q("#f-filter").disabled, HasLabels: q("#f-labels") !== null,
		         Fits: h === "Edit subscription" && sub === "Update" }; })()`, &form)
	if form.Ack != "10" || form.Endpoint != "" || form.Name != "browser-edit-sub" || !form.NameDisabled ||
		form.Topic != topic || !form.TopicDisabled || !form.FilterDisabled || !form.HasLabels {
		t.Errorf("the edit dialog is prefilled with %+v; want ack 10, no endpoint, the name, topic and filter disabled, labels", form)
	}
	if !form.Fits {
		t.Error("the Edit page is not headed Edit subscription, ending in Update, as Google's is")
	}

	// Cancel goes back to the subscription, sending nothing.
	set("f-ackDeadline", "45")
	p.clickText("form.edit-page .form-actions a", "Cancel")
	p.waitFor(`document.querySelector("form.edit-page") === null && document.querySelector("#view .page-actions") !== null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/pubsub-subscriptions"); len(sent) != 0 {
		t.Fatalf("Cancel sent the change: %v", sent)
	}

	openEdit()
	set("f-ackDeadline", "601")
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
	p.waitFor(`!document.querySelector("form.edit-page .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector("form.edit-page .form-error").textContent`, &refusal)
	if want := "InvalidArgument: ack_deadline_secs out of bounds"; refusal != want {
		t.Errorf("an ack deadline of 601 was refused on the form with %q, want the emulator's %q", refusal, want)
	}
	// The refusal is on the page, not a browser error: it is forgiven.
	p.forgive()

	const endpoint = "http://127.0.0.1:9/cloudburrow-browser"
	set("f-ackDeadline", "30")
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page #f-deliveryType-1`)))
	set("f-pushEndpoint", endpoint)
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
	p.waitFor(`document.querySelector("form.edit-page") === null && document.querySelector("#view .page-actions") !== null`)
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
		p.clickText("#view .page-actions button", "Edit")
		p.waitFor(`document.querySelector("form.edit-page #f-exactlyOnce") !== null`)
	}
	save := func() {
		p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
		p.waitFor(`document.querySelector("form.edit-page") === null && document.querySelector("#view .page-actions") !== null`)
	}
	openEdit()
	var box struct {
		Type              string
		Checked, Disabled bool
	}
	p.eval(`(() => { const f = document.querySelector("form.edit-page #f-exactlyOnce");
		return { Type: f.type, Checked: f.checked, Disabled: f.disabled }; })()`, &box)
	if box.Type != "checkbox" || box.Checked || box.Disabled {
		t.Errorf("exactly-once on the form is %+v; want an enabled, unchecked checkbox", box)
	}
	setField(p, "form.edit-page", "f-exactlyOnce", "true")
	save()
	if got := exactlyOnce(); got != "true" {
		t.Fatalf("after saving exactly-once, the console API reads %q", got)
	}

	openEdit()
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page #f-deliveryType-1`)))
	setField(p, "form.edit-page", "f-pushEndpoint", "http://127.0.0.1:9/cloudburrow-browser")
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
	p.waitFor(`!document.querySelector("form.edit-page .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector("form.edit-page .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "pull subscriptions only") {
		t.Errorf("exactly-once with a push endpoint was refused with %q; want the pull-only reason", refusal)
	}
	p.forgive()
	if got := exactlyOnce(); got != "true" {
		t.Errorf("after the refusal the console API reads exactly-once %q", got)
	}

	p.run(chromedp.Click(chromedp.CSS(`form.edit-page #f-deliveryType-0`)))
	setField(p, "form.edit-page", "f-exactlyOnce", "false")
	save()
	if got := exactlyOnce(); got != "false" {
		t.Errorf("after turning exactly-once off, the console API reads %q", got)
	}
}

// TestPubSubEditSubscriptionExpirationThroughTheForm (#891, #1847), in the
// storage shard: Edit subscription's Expiration period is Google's choice,
// "Expire after this many days of inactivity" with its days prefilled with
// Google's 31, or "Never expire". A period shorter than the message
// retention is refused on the open page with the reason CloudBurrow's
// Pub/Sub front gives, and nothing changes; 10 days is saved and the console
// API reads it back, and so is Never expire.
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
		days := ""
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
			if f.Name == "expirationDays" {
				days = f.Default
			}
			if f.Name == "expirationMode" && f.Default == "Never expire" {
				return "never"
			}
		}
		return days + "d"
	}

	p.navigate("/pubsub/subscriptions?project=" + project)
	p.clickText("#view tbody a", sub)
	openEdit := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Edit")
		p.waitFor(`document.querySelector("form.edit-page #f-expirationDays") !== null`)
	}
	save := func() {
		p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
		p.waitFor(`document.querySelector("form.edit-page") === null && document.querySelector("#view .page-actions") !== null`)
	}
	openEdit()
	var field struct {
		Value    string
		Disabled bool
	}
	p.eval(`(() => { const f = document.querySelector("form.edit-page #f-expirationDays");
		return { Value: f.value, Disabled: f.disabled }; })()`, &field)
	if field.Value != "31" || field.Disabled {
		t.Errorf("the expiration period on the form is %+v; want an enabled field reading Google's default, 31 days", field)
	}

	setField(p, "form.edit-page", "f-expirationDays", "3")
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
	p.waitFor(`!document.querySelector("form.edit-page .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector("form.edit-page .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "must be at least as long as message_retention_duration") {
		t.Errorf("an expiration of 3 days was refused with %q; want the front's reason, no shorter than the retention", refusal)
	}
	p.forgive()
	if got := expiration(); got != "31d" {
		t.Errorf("after the refusal the console API reads expiration %q, want 31d", got)
	}

	setField(p, "form.edit-page", "f-expirationDays", "10")
	save()
	if got := expiration(); got != "10d" {
		t.Fatalf("after saving 10d, the console API reads expiration %q", got)
	}
	openEdit()
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page #f-expirationMode-1`)))
	save()
	if got := expiration(); got != "never" {
		t.Errorf("after saving never, the console API reads expiration %q", got)
	}
}

// TestPubSubIdleTimeAndPushAttributesThroughTheForms (#996), in the storage
// shard, whose instance serves Pub/Sub: a topic's Create subscription offers
// push attributes beside the push endpoint, and a push subscription created
// with one reads it back through the console API and shows it on its
// Delivery tab; the Configuration tab shows its expiration period, how long
// it was idle and when expiration deletes it.
func TestPubSubIdleTimeAndPushAttributesThroughTheForms(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/browser-idle"
	sub := "projects/" + project + "/subscriptions/browser-idle-push"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project,
		`{"name":"browser-idle","defaultSubscription":"false"}`); code != http.StatusOK {
		t.Fatalf("create a topic through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})

	// The topic page's Create subscription opens Google's page with the
	// topic filled in (#1847); Push reveals the endpoint and attributes.
	p.navigate("/pubsub/topics?project=" + project)
	p.clickText("#view tbody a", topic)
	p.clickText("#view .page-actions button", "Create subscription")
	p.waitFor(`location.pathname === "/pubsub/subscriptions/create" && document.querySelector("#view #f-pushAttributes") !== null`)
	// One page under headings, as Google's (#1847), with Write to BigQuery
	// and Write to Cloud Storage enabled since #1874, each revealing its
	// settings.
	var page struct {
		Steps    int
		Choices  []string
		Disabled []bool
		Reasons  int
	}
	p.eval(`(() => { const r = [...document.querySelectorAll("#view input[name='f-deliveryType-choice']")];
		return { Steps: document.querySelectorAll("#view .create-step").length,
			Choices: r.map((x) => x.value), Disabled: r.map((x) => x.disabled),
			Reasons: document.querySelectorAll("#view .is-radio .form-unavailable").length }; })()`, &page)
	if page.Steps != 0 || strings.Join(page.Choices, ",") != "Pull,Push,Write to BigQuery,Write to Cloud Storage" ||
		fmt.Sprint(page.Disabled) != "[false false false false]" || page.Reasons != 0 {
		t.Errorf("Create subscription reads %+v; want one page, and every delivery type enabled", page)
	}
	shown := func(id string) bool {
		var h bool
		p.eval(fmt.Sprintf(`!document.querySelector("#view #%s").closest(".form-row").hidden`, id), &h)
		return h
	}
	p.run(chromedp.Click(chromedp.CSS(`#view #f-deliveryType-2`)))
	if !shown("f-bigqueryTable") || !shown("f-bigquerySchema-0") || shown("f-storageBucket") || shown("f-pushEndpoint") {
		t.Error("Write to BigQuery does not reveal only its table and schema configuration")
	}
	p.run(chromedp.Click(chromedp.CSS(`#view #f-deliveryType-3`)))
	if !shown("f-storageBucket") || !shown("f-filenamePrefix") || !shown("f-maxDurationMinutes") || shown("f-bigqueryTable") {
		t.Error("Write to Cloud Storage does not reveal only its bucket, file names and batching")
	}
	// Avro reveals Use topic schema and Write metadata (#1874).
	if shown("f-storageUseTopicSchema") {
		t.Error("Use topic schema shows with the Text file format")
	}
	p.run(chromedp.Click(chromedp.CSS(`#view #f-storageFormat-1`)))
	if !shown("f-storageUseTopicSchema") || !shown("f-storageWriteMetadata") {
		t.Error("the Avro file format does not reveal Use topic schema and Write metadata")
	}
	p.run(chromedp.Click(chromedp.CSS(`#view #f-storageFormat-0`)))
	p.run(chromedp.Click(chromedp.CSS(`#view #f-deliveryType-1`)))
	setField(p, "#view", "f-name", "browser-idle-push")
	setField(p, "#view", "f-pushEndpoint", "http://127.0.0.1:1/push")
	setField(p, "#view", "f-pushAttributes", "x-goog-version=v1")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`location.pathname === "/pubsub/subscriptions"`)
	got := editDefaults(t, "pubsub-subscriptions", project, sub)
	if got["Endpoint"] != "http://127.0.0.1:1/push" || got["Attribute x-goog-version"] != "v1" {
		t.Errorf("the subscription created in the browser reads endpoint %q, attribute %q",
			got["Endpoint"], got["Attribute x-goog-version"])
	}
	if got["Idle for"] == "" || !strings.Contains(got["Deleted by expiration"], "did not restart the clock") {
		t.Errorf("the console API's page reads idle %q, deleted %q", got["Idle for"], got["Deleted by expiration"])
	}

	// The Details tab (#1847) holds the push attributes and the expiry.
	p.navigate("/pubsub/subscriptions?project=" + project)
	p.clickText("#view tbody a", sub)
	// The page opens on Details, not Metrics: Google's guide opens the page,
	// then says to click the Metrics tab.
	p.waitFor(`document.querySelector("#tab-details") !== null`)
	var opened string
	p.eval(`document.querySelector("#view [role=tab][aria-selected=true]").id`, &opened)
	if opened != "tab-details" {
		t.Errorf("the subscription page opens on %s, want tab-details", opened)
	}
	p.run(chromedp.Click(chromedp.CSS(`#tab-details`)))
	p.waitFor(`document.querySelector("#view").textContent.includes("Attribute x-goog-version")`)
	p.waitFor(`document.querySelector("#view").textContent.includes("Deleted by expiration")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	for _, want := range []string{"Expiration period", "31d", "Idle for", "did not restart the clock"} {
		if !strings.Contains(text, want) {
			t.Errorf("the subscription's page does not show %q", want)
		}
	}
}
