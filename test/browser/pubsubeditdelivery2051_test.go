//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestPubSubEditSubscriptionChangesDeliveryType (#2051), in the storage
// shard, whose instance serves Pub/Sub and Cloud Storage. As Google's
// console has it ("In the subscription details page, click Edit. For
// Delivery type, select the new delivery type. Fill in the properties for
// the new subscription type ... Click Update",
// https://docs.cloud.google.com/pubsub/docs/manage-subscriptions): a pull
// subscription's Edit offers every delivery type; choosing Write to Cloud
// Storage reveals the bucket and file fields, and Update makes it a Cloud
// Storage subscription, whose Edit page is then prefilled with them; Pull
// makes it a pull subscription again. The page with Write to Cloud Storage
// chosen passes the WCAG 2.1 AA audit.
func TestPubSubEditSubscriptionChangesDeliveryType(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	bucket := project + "-switch"
	topic := "projects/" + project + "/topics/switch"
	sub := "projects/" + project + "/subscriptions/switch-sub"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/pubsub"+q, `{"name":"switch","defaultSubscription":"false"}`); code != http.StatusOK {
		t.Fatalf("create a topic through the console API = %d: %s", code, out)
	}
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/pubsub-subscriptions"+q,
		`{"name":"switch-sub","topic":"switch","deliveryType":"Pull"}`); code != http.StatusOK {
		t.Fatalf("create a subscription through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions"+q+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub"+q+"&name="+url.QueryEscape(topic), "")
		consoleDo(t, http.MethodDelete, "/api/resources/storage"+q+"&name="+bucket, "")
	})

	openEdit := func() {
		p.navigate("/pubsub/subscriptions/" + url.PathEscape(sub) + q)
		p.clickText("#view .page-actions button", "Edit")
		p.waitFor(`document.querySelector("form.edit-page #f-deliveryType-3") !== null`)
	}
	openEdit()
	var state struct {
		BigQuery, Storage, BucketShown bool
	}
	p.eval(`({ BigQuery: !document.querySelector("form.edit-page #f-deliveryType-2").disabled,
		Storage: !document.querySelector("form.edit-page #f-deliveryType-3").disabled,
		BucketShown: document.querySelector("form.edit-page #f-storageBucket").getClientRects().length > 0 })`, &state)
	if !state.BigQuery || !state.Storage || state.BucketShown {
		t.Errorf("a pull subscription's Edit page reads %+v; want both export types offered and the bucket hidden until chosen", state)
	}
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page #f-deliveryType-3`)))
	p.waitFor(`document.querySelector("form.edit-page #f-storageBucket").getClientRects().length > 0`)
	setField(p, "form.edit-page", "f-storageBucket", bucket)
	setField(p, "form.edit-page", "f-filenamePrefix", "edited/")
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
	p.waitFor(`document.querySelector("form.edit-page") === null && document.querySelector("#view .page-actions") !== null`)
	if d := editDefaults(t, "pubsub-subscriptions", project, sub); d["deliveryType"] != "Write to Cloud Storage" ||
		d["storageBucket"] != bucket || d["filenamePrefix"] != "edited/" || d["maxDurationMinutes"] != "5" {
		t.Errorf("after Update the subscription's Edit page reads %v; want a Cloud Storage subscription to %s", d, bucket)
	}
	if sent := p.sent(http.MethodPatch, "/api/resources/pubsub-subscriptions"); len(sent) != 1 {
		t.Errorf("Update sent %d PATCHes, want 1: %v", len(sent), sent)
	}

	auditScreens(t, []a11yPage{
		{"Edit subscription page, Write to Cloud Storage", "/pubsub/subscriptions/" + url.PathEscape(sub) + q + "&edit=1",
			settled + ` && document.querySelector("form.edit-page #f-storageBucket") !== null`, ""},
	})

	// Back to Pull.
	openEdit()
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page #f-deliveryType-0`)))
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
	p.waitFor(`document.querySelector("form.edit-page") === null && document.querySelector("#view .page-actions") !== null`)
	if d := editDefaults(t, "pubsub-subscriptions", project, sub); d["deliveryType"] != "Pull" || d["storageBucket"] != "" {
		t.Errorf("after switching back the subscription's Edit page reads %v; want a pull subscription", d)
	}
}
