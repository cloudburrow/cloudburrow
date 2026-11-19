//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestEventarcTriggersPageThroughTheBrowser (#1877): Eventarc's own
// Triggers page. Create trigger is a page (/eventarc/triggers/create) with
// Google's trigger name, trigger type, event type, region, service account
// and event destination; a Pub/Sub trigger to a Cloud Run service made
// there is listed with Google's columns, its page shows its event filters
// and destination, Edit changes its service URL path, which its page then
// reads, and Delete removes it.
func TestEventarcTriggersPageThroughTheBrowser(t *testing.T) {
	needService(t, "run")
	needService(t, "eventarc")
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-ea-%d", time.Now().UnixNano()%1e6)
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
	trigger := "ea-" + id[len(id)-6:]
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc?project="+project+"&name="+trigger, "")
	})

	p := open(t)
	p.navigate("/eventarc/triggers/create?project=" + project)
	p.waitFor(`document.querySelector("#f-eventType") !== null`)
	var form struct {
		Labels       []string
		Destinations []string
		Title        string
	}
	p.eval(`(() => ({
		Labels: [...document.querySelectorAll("#view label")].map((n) => n.textContent.trim()),
		Destinations: [...document.querySelectorAll("#f-destination option")].map((o) => o.value),
		Title: document.querySelector("#view h1, #view h2") ? document.querySelector("#view h1, #view h2").textContent : "" }))()`, &form)
	labels := strings.Join(form.Labels, "|")
	for _, want := range []string{"Trigger name", "Trigger type", "Event type", "Region", "Service account", "Event destination", "Service URL path"} {
		if !strings.Contains(labels, want) {
			t.Errorf("Create trigger has no %q field: %s", want, labels)
		}
	}
	if len(form.Destinations) == 0 || form.Destinations[0] != "Cloud Run" {
		t.Errorf("Event destination offers %v", form.Destinations)
	}
	p.submitForm(map[string]string{"name": trigger, "eventType": "google.cloud.pubsub.topic.v1.messagePublished",
		"service": id, "path": "/first"}, "name")
	p.waitFor(`location.pathname === "/eventarc/triggers" || location.pathname === "/eventarc/triggers/` + trigger + `"`)

	p.navigate("/eventarc/triggers?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("` + trigger + `")`)
	var row, head string
	p.eval(`(() => { const r = [...document.querySelectorAll("#view tr")].find((n) => n.textContent.includes("`+trigger+`"));
		return r ? r.textContent : ""; })()`, &row)
	p.eval(`document.querySelector("#view thead") ? document.querySelector("#view thead").textContent : ""`, &head)
	if !strings.Contains(row, "Cloud Pub/Sub") || !strings.Contains(row, "Cloud Run: "+id) || !strings.Contains(row, "Google sources") {
		t.Errorf("the trigger's row reads %q", row)
	}
	for _, col := range []string{"Event provider", "Event type", "Destination", "Region"} {
		if !strings.Contains(head, col) {
			t.Errorf("the Triggers list has no %q column: %q", col, head)
		}
	}

	p.clickText("#view tbody a", trigger)
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/eventarc/triggers/"+trigger))
	p.pageAction("Edit", map[string]string{"path": "/second"}, "")
	code, page := consoleDo(t, http.MethodGet, "/api/detail/eventarc?project="+project+"&name="+trigger, "")
	if code != http.StatusOK || !strings.Contains(page, "/second") || !strings.Contains(page, "messagePublished") {
		t.Errorf("the trigger's page after Edit = %d: %s", code, page)
	}

	if code, body := consoleDo(t, http.MethodDelete, "/api/resources/eventarc?project="+project+"&name="+trigger, ""); code != http.StatusOK {
		t.Errorf("Delete = %d: %s", code, body)
	}
	p.navigate("/eventarc/triggers?project=" + project)
	p.waitFor(`document.querySelector("#view tbody") !== null || document.querySelector("#view").textContent.includes("No triggers yet")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if strings.Contains(text, trigger) {
		t.Errorf("the trigger is still listed after Delete")
	}
}
