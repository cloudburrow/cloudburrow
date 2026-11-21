//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestPubSubExpirationOfNoWholeDaysThroughThePages (#2095), in the storage
// shard, whose instance serves Pub/Sub. Google's console takes the
// Expiration period as "the number of days that the subscription can remain
// inactive" ("Expire after this many days of inactivity",
// https://docs.cloud.google.com/pubsub/docs/create-subscription), while the
// API takes any ttl of at least a day ("The minimum allowed value for
// expirationPolicy.ttl is 1 day", projects.subscriptions), so gcloud's
// --expiration-period=36h makes one the days cannot say. Such a
// subscription is listed and shown as 36 hours, not 2 days; its Edit page
// leaves Days empty, saying the period, and Update with the days untouched
// keeps 36 hours; days typed replace it, and the Days field takes whole
// numbers only. The Edit page passes the WCAG 2.1 AA audit.
func TestPubSubExpirationOfNoWholeDaysThroughThePages(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	topic := "projects/" + project + "/topics/hours"
	sub := "projects/" + project + "/subscriptions/hours-sub"
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/pubsub"+q, `{"name":"hours","defaultSubscription":"false"}`); code != http.StatusOK {
		t.Fatalf("create a topic through the console API = %d: %s", code, out)
	}
	// The retention is a day, so a 36h period is no shorter than it.
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/pubsub-subscriptions"+q,
		`{"name":"hours-sub","topic":"hours","messageRetention":"1d","expiration":"36h"}`); code != http.StatusOK {
		t.Fatalf("create a subscription with a 36h expiration through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions"+q+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub"+q+"&name="+url.QueryEscape(topic), "")
	})

	listed := func() string {
		t.Helper()
		p.navigate("/pubsub/subscriptions" + q)
		p.waitFor(rowReady("hours-sub"))
		var text string
		p.eval(`(() => { const r = [...document.querySelectorAll("#view tbody tr")].find((tr) => tr.textContent.includes("hours-sub"));
			return r ? r.textContent : ""; })()`, &text)
		return text
	}
	if row := listed(); !strings.Contains(row, "36 hours") || strings.Contains(row, "2 days") {
		t.Errorf("the Subscriptions list's row reads %q; want the expiration as 36 hours", row)
	}
	p.navigate("/pubsub/subscriptions/" + url.PathEscape(sub) + q)
	p.waitFor(settled + ` && document.querySelector("#view .page-actions") !== null`)
	var page string
	p.eval(`document.querySelector("#view").textContent`, &page)
	if !strings.Contains(page, "36 hours") {
		t.Errorf("the subscription's page does not say its expiration period, 36 hours")
	}

	openEdit := func() {
		p.navigate("/pubsub/subscriptions/" + url.PathEscape(sub) + q)
		p.clickText("#view .page-actions button", "Edit")
		p.waitFor(`document.querySelector("form.edit-page #f-expirationDays") !== null`)
	}
	save := func() {
		p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
		p.waitFor(`document.querySelector("form.edit-page") === null && document.querySelector("#view .page-actions") !== null`)
	}
	openEdit()
	var days struct {
		Value, Help string
		Checked     bool
	}
	p.eval(`(() => { const f = document.querySelector("form.edit-page #f-expirationDays");
		const h = document.getElementById(f.getAttribute("aria-describedby") || "");
		return { Value: f.value, Help: h ? h.textContent : "", Checked: document.querySelector("form.edit-page #f-expirationMode-0").checked }; })()`, &days)
	if days.Value != "" || !strings.Contains(days.Help, "Now 36 hours") || !days.Checked {
		t.Errorf("Edit's Expiration period reads %+v; want Expire after this many days chosen, the days empty and the help saying 36 hours", days)
	}
	save()
	if row := listed(); !strings.Contains(row, "36 hours") {
		t.Errorf("after Update with the days untouched the row reads %q; want 36 hours kept", row)
	}

	auditScreens(t, []a11yPage{
		{"Edit subscription page, an expiration of no whole days", "/pubsub/subscriptions/" + url.PathEscape(sub) + q + "&edit=1",
			settled + ` && document.querySelector("form.edit-page #f-expirationDays") !== null`, ""},
	})

	// Days are whole numbers: 1.5 is refused by the field itself.
	openEdit()
	setField(p, "form.edit-page", "f-expirationDays", "1.5")
	var valid bool
	p.eval(`document.querySelector("form.edit-page #f-expirationDays").checkValidity()`, &valid)
	if valid {
		t.Errorf("Days took 1.5; want whole numbers only")
	}
	setField(p, "form.edit-page", "f-expirationDays", "2")
	save()
	if row := listed(); !strings.Contains(row, "2 days") {
		t.Errorf("after Update with 2 days the row reads %q; want 2 days", row)
	}
	if d := editDefaults(t, "pubsub-subscriptions", project, sub); d["expirationDays"] != "2" {
		t.Errorf("after Update with 2 days Edit's Days read %q; want 2", d["expirationDays"])
	}

}
