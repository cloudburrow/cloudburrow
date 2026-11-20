//go:build browser

package browser

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestPubSubInfoPanelAndEditPages (#1847), in the storage shard, whose
// instance serves Pub/Sub. As Google's console has them:
//
//   - checking two topics on the Topics list opens the info panel, whose
//     Permissions tab's Add principal grants the role on both ("To set roles
//     for one or more topics, select the topics ... In the Permissions tab,
//     click Add principal"), each read back through the console API; and
//     whose Labels tab saves the checked topic's labels;
//   - a subscription's Edit opens the Edit subscription page, under Create
//     subscription's headings, ending in Update; a topic's Edit opens Edit
//     topic, whose Enable message retention reveals days, hours and
//     minutes, and a retention of 2 days is saved and read back;
//   - the edit pages, and the info panel's Permissions and Labels tabs on
//     the Topics, Subscriptions and Snapshots lists, pass the WCAG 2.1 AA
//     audit (names, contrast in both themes, keyboard, motion, reflow).
func TestPubSubInfoPanelAndEditPages(t *testing.T) {
	needService(t, "pubsub-subscriptions")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	topicA := "projects/" + project + "/topics/info-a"
	topicB := "projects/" + project + "/topics/info-b"
	sub := "projects/" + project + "/subscriptions/info-a-sub"
	for _, body := range []string{`{"name":"info-a","defaultSubscription":"true"}`, `{"name":"info-b","defaultSubscription":"false"}`} {
		if code, out := consoleDo(t, http.MethodPost, "/api/resources/pubsub"+q, body); code != http.StatusOK {
			t.Fatalf("create a topic through the console API = %d: %s", code, out)
		}
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions"+q+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub"+q+"&name="+url.QueryEscape(topicA), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub"+q+"&name="+url.QueryEscape(topicB), "")
	})
	policy := func(name string) []string {
		t.Helper()
		code, body := consoleDo(t, http.MethodPost, "/api/permissions/pubsub"+q, `{"Path":["`+name+`"],"Read":{}}`)
		var sec struct {
			Permissions struct {
				Bindings []struct {
					Role    string
					Members []string
				}
			}
		}
		if err := json.Unmarshal([]byte(body), &sec); code != http.StatusOK || err != nil {
			t.Fatalf("read %s's policy through the console API = %d (%v): %s", name, code, err, body)
		}
		var out []string
		for _, b := range sec.Permissions.Bindings {
			for _, m := range b.Members {
				out = append(out, m+" "+b.Role)
			}
		}
		return out
	}

	// Check both topics: the info panel opens on the last, with Google's tabs.
	p.navigate("/pubsub/topics" + q)
	p.waitFor(rowReady(topicB))
	for _, n := range []string{topicA, topicB} {
		p.run(chromedp.Click(chromedp.CSS(`#view input[aria-label="Select ` + n + `"]`)))
	}
	p.waitFor(`document.querySelector("#info-tab-permissions") !== null && document.querySelector("#info-tab-labels") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#info-tab-permissions`)))
	p.waitFor(`document.querySelector("#info-add-principal") !== null`)
	var scope string
	p.eval(`(document.querySelector("#info-permissions-scope") || {}).textContent || ""`, &scope)
	if !strings.Contains(scope, "each of the 2 checked rows") {
		t.Errorf("with two topics checked the Permissions tab says %q; want that Add principal grants on both", scope)
	}
	p.run(chromedp.Click(chromedp.CSS(`#info-add-principal`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	setField(p, ".modal.is-open", "f-members", "user:ada@example.com")
	setField(p, ".modal.is-open", "f-role", "roles/pubsub.publisher")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	for _, n := range []string{topicA, topicB} {
		got := policy(n)
		if len(got) != 1 || got[0] != "user:ada@example.com roles/pubsub.publisher" {
			t.Errorf("after Add principal on two checked topics, %s's policy reads %v", n, got)
		}
	}
	p.waitFor(`document.querySelector("#info-permissions") !== null`)

	// Labels: saved for the topic the panel describes.
	p.run(chromedp.Click(chromedp.CSS(`#info-tab-labels`)))
	p.waitFor(`document.querySelector("#info-tab-panel #f-labels") !== null`)
	setField(p, "#info-tab-panel", "f-labels", "env=dev")
	p.run(chromedp.Click(chromedp.CSS(`#info-tab-panel button[type="submit"]`)))
	eventually(t, "the Labels tab's labels to be saved", func() bool {
		return strings.Contains(editDefaults(t, "pubsub", project, topicB)["labels"], `"env":"dev"`)
	})

	// Edit topic: Google's page, ending in Update.
	p.navigate("/pubsub/topics/" + url.PathEscape(topicA) + q)
	p.clickText("#view .page-actions button", "Edit")
	p.waitFor(`document.querySelector("form.edit-page #f-enableRetention") !== null`)
	var page struct {
		Title, Submit string
		Hidden        bool
	}
	p.eval(`({ Title: document.querySelector("#view h1").textContent,
		Submit: document.querySelector('form.edit-page button[type="submit"]').textContent,
		Hidden: document.querySelector("form.edit-page #f-retentionDays").getClientRects().length === 0 })`, &page)
	if page.Title != "Edit topic" || page.Submit != "Update" || !page.Hidden {
		t.Errorf("the Edit topic page is %+v; want Edit topic, Update, and the days hidden until retention is enabled", page)
	}
	setField(p, "form.edit-page", "f-enableRetention", "true")
	setField(p, "form.edit-page", "f-retentionDays", "2")
	p.run(chromedp.Click(chromedp.CSS(`form.edit-page button[type="submit"]`)))
	p.waitFor(`document.querySelector("form.edit-page") === null && document.querySelector("#view .page-actions") !== null`)
	if d := editDefaults(t, "pubsub", project, topicA); d["enableRetention"] != "true" || d["retentionDays"] != "2" {
		t.Errorf("after Update the topic's edit page reads %v; want a retention of 2 days", d)
	}
	if sent := p.sent(http.MethodPatch, "/api/resources/pubsub"); len(sent) != 1 {
		t.Errorf("Update sent %d PATCHes, want 1: %v", len(sent), sent)
	}

	// Edit subscription: Google's page, under Create subscription's
	// headings.
	p.navigate("/pubsub/subscriptions/" + url.PathEscape(sub) + q)
	p.clickText("#view .page-actions button", "Edit")
	p.waitFor(`document.querySelector("form.edit-page #f-deliveryType-0") !== null`)
	var headings []string
	p.eval(`[...document.querySelectorAll("form.edit-page h2, form.edit-page h3, form.edit-page legend")].map((h) => h.textContent.trim())`, &headings)
	joined := strings.Join(headings, "|")
	for _, want := range []string{"Retry policy", "Dead lettering", "Delivery properties", "Lifetime options", "Expiration period"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the Edit subscription page has no %q heading: %v", want, headings)
		}
	}
	var exportDisabled bool
	p.eval(`document.querySelector("form.edit-page #f-deliveryType-2").disabled`, &exportDisabled)
	if !exportDisabled {
		t.Error("Edit subscription offers switching a pull subscription to Write to BigQuery, which this instance does not do")
	}

	edit := func(path string) string { return path + q + "&edit=1" }
	panelOn := func(name string) string {
		return `document.querySelector('#view input[aria-label="Select ` + name + `"]').click()`
	}
	tabOf := func(name, tab string) string {
		return panelOn(name) + `; setTimeout(() => document.querySelector("#info-tab-` + tab + `").click(), 50)`
	}
	editReady := settled + ` && document.querySelector("form.edit-page") !== null`
	auditScreens(t, []a11yPage{
		{"Edit topic page", edit("/pubsub/topics/" + url.PathEscape(topicA)), editReady, ""},
		{"Edit subscription page", edit("/pubsub/subscriptions/" + url.PathEscape(sub)), editReady, ""},
		{"Topics info panel Permissions", "/pubsub/topics" + q, rowReady(topicA), tabOf(topicA, "permissions")},
		{"Topics info panel Labels", "/pubsub/topics" + q, rowReady(topicA), tabOf(topicA, "labels")},
		{"Subscriptions info panel Permissions", "/pubsub/subscriptions" + q, rowReady(sub), tabOf(sub, "permissions")},
		{"Snapshots list", "/pubsub/snapshots" + q, settled, ""},
	})
}
