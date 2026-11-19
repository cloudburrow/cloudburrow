//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestCloudRunEventarcTriggersThroughTheBrowser (#1852, #1877): a service's
// Triggers tab lists its Eventarc triggers. Add trigger offers Cloud
// Pub/Sub's and Cloud Storage's event types, a topic (or one Eventarc
// makes), a bucket, the region, the account and the service URL path,
// ending in Save trigger; a Pub/Sub trigger with no topic is saved, and the
// tab then lists it with its provider, event type, path and the topic
// Eventarc made. Delete trigger removes it.
func TestCloudRunEventarcTriggersThroughTheBrowser(t *testing.T) {
	needService(t, "run")
	needService(t, "pubsub")
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-triggers-%d", time.Now().UnixNano()%1e6)
	const image = "ghcr.io/knative/helloworld-go:latest"
	body, _ := json.Marshal(map[string]string{"name": id, "image": image})
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/run?project="+project+"&name="+id, "")
	})
	code, resp := consoleDeploy(t, http.MethodPost, "/api/resources/run?project="+project, string(body))
	switch {
	case code == http.StatusOK:
	case code == http.StatusBadRequest && strings.Contains(resp, "did not answer in time") && strings.Contains(resp, `"operation"`):
		waitForRunService(t, project, id, image, 5*time.Minute)
	default:
		t.Fatalf("deploy a service through the console API = %d: %s", code, resp)
	}
	p := open(t)
	tab := "/run/" + id + "?project=" + project + "&tab=triggers"
	p.navigate(tab)
	p.clickText("#view .section-actions button", "Add trigger")
	p.waitFor(`document.querySelector(".modal.is-open #f-eventType") !== null`)
	var form struct {
		Types, Labels      []string
		Submit, Topic, Pub string
	}
	p.eval(`(() => { const m = document.querySelector(".modal.is-open"); return {
		Types: [...m.querySelectorAll("#f-eventType option")].map((o) => o.value),
		Labels: [...m.querySelectorAll("label")].map((n) => n.textContent.trim()),
		Topic: m.querySelector("#f-topic").selectedOptions[0].textContent,
		Pub: m.querySelector("#f-eventType").selectedOptions[0].textContent,
		Submit: m.querySelector('button[type="submit"]').textContent }; })()`, &form)
	labels := strings.Join(form.Labels, "|")
	if len(form.Types) != 5 || form.Types[0] != "google.cloud.pubsub.topic.v1.messagePublished" ||
		form.Types[1] != "google.cloud.storage.object.v1.finalized" || form.Submit != "Save trigger" ||
		!strings.Contains(form.Topic, "Eventarc makes one") || !strings.HasPrefix(form.Pub, "Cloud Pub/Sub: ") ||
		!strings.Contains(labels, "Bucket") || !strings.Contains(labels, "Service URL path") {
		t.Errorf("Add trigger reads %+v", form)
	}
	trigger := "tr-" + id[len(id)-6:]
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-name`), trigger))
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-path`), "/events"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.navigate(tab)
	p.waitFor(`document.querySelector("#view").textContent.includes("` + trigger + `")`)
	var row string
	p.eval(`(() => { const r = [...document.querySelectorAll("#view tr")].find((n) => n.textContent.includes("`+trigger+`"));
		return r ? r.textContent : ""; })()`, &row)
	if !strings.Contains(row, "Cloud Pub/Sub") || !strings.Contains(row, "google.cloud.pubsub.topic.v1.messagePublished") ||
		!strings.Contains(row, "/events") || !strings.Contains(row, "eventarc-") {
		t.Errorf("the trigger's row reads %q", row)
	}

	p.clickText("#view .section-actions button", "Delete trigger")
	p.waitFor(`document.querySelector(".modal.is-open #f-name") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-name`), trigger))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.navigate(tab)
	p.waitFor(`document.querySelector("#view .section-actions") !== null`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if strings.Contains(text, trigger) {
		t.Errorf("the trigger is still listed after Delete trigger")
	}
}
