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

// TestPubSubLabelsAreCheckedOnTheForms (#962), in the storage shard, whose
// instance serves Pub/Sub: Create topic's labels field holds the lines to
// Google's label rules in the browser. A key with an uppercase letter is
// refused on the field, naming the line and the rule, and nothing is sent;
// a key and a value Google takes, one of them international, create the
// topic, whose labels the console API reads back. On Edit topic a value with
// a space is refused the same way, with no PATCH sent.
func TestPubSubLabelsAreCheckedOnTheForms(t *testing.T) {
	needService(t, "pubsub")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/browser-labels"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})

	p.navigate("/pubsub/topics?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view button.primary")].some((b) => b.textContent === "Create topic")`)
	p.clickText("#view button.primary", "Create topic")
	p.waitFor(`document.querySelector("#view #f-labels") !== null`)
	setField(p, "#view", "f-name", "browser-labels")
	setField(p, "#view", "f-defaultSubscription", "false")
	setField(p, "#view", "f-labels", "team=a\nEnv=dev")
	p.run(chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector("#view #e-labels").hidden`)
	var refusal string
	p.eval(`document.querySelector("#view #e-labels").textContent`, &refusal)
	if !strings.Contains(refusal, `Line 2`) || !strings.Contains(refusal, `"Env"`) || !strings.Contains(refusal, "lowercase") {
		t.Errorf("an uppercase key was refused on the field with %q; want line 2, the key and the rule", refusal)
	}
	if sent := p.sent(http.MethodPost, "/api/resources/pubsub"); len(sent) != 0 {
		t.Fatalf("a label Google refuses was sent: %v", sent)
	}

	setField(p, "#view", "f-labels", "team=a\nenv=dev\n日本=東京")
	p.waitFor(`document.querySelector("#view #e-labels").hidden`)
	p.run(chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`location.pathname === "/pubsub/topics"`)
	var labels map[string]string
	if err := json.Unmarshal([]byte(editDefaults(t, "pubsub", project, topic)["labels"]), &labels); err != nil ||
		labels["team"] != "a" || labels["env"] != "dev" || labels["日本"] != "東京" || len(labels) != 3 {
		t.Errorf("the topic created in the browser reads labels %v (%v); want team=a, env=dev, 日本=東京", labels, err)
	}

	p.navigate("/pubsub/topics?project=" + project)
	p.clickText("#view tbody a", topic)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Edit topic")
	p.waitFor(`document.querySelector(".modal.is-open #f-labels") !== null`)
	setField(p, ".modal.is-open", "f-labels", "team=a b")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal.is-open #e-labels").hidden`)
	p.eval(`document.querySelector(".modal.is-open #e-labels").textContent`, &refusal)
	if !strings.Contains(refusal, `Line 1`) || !strings.Contains(refusal, `"a b"`) {
		t.Errorf("a value with a space was refused on the field with %q; want line 1 and the value", refusal)
	}
	if sent := p.sent(http.MethodPatch, "/api/resources/pubsub"); len(sent) != 0 {
		t.Errorf("a label value Google refuses was sent: %v", sent)
	}
}
