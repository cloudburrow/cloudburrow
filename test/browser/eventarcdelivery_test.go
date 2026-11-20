//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestEventarcAdvancedDeliveryThroughTheBrowser (#2006), in the run shard:
// Eventarc Advanced end to end through its pages. A Cloud Run service is
// deployed; through Create bus, Create pipeline (an HTTP endpoint
// destination, the service's URL, logging at INFO) and Create enrollment
// (the bus to the pipeline, on the event's type) the three are made; the
// bus's Publish message puts an event on it, and the pipeline's platform
// log, read through the console's log store as the Logs Explorer reads
// it, says the message was delivered (written only when the endpoint
// answered 2xx). Edit on the pipeline's page changes its retry policy and
// on a Google API source's page its log severity, which their pages then
// read; Delete through the console removes each.
func TestEventarcAdvancedDeliveryThroughTheBrowser(t *testing.T) {
	needService(t, "run")
	needService(t, "eventarc-buses")
	needService(t, "logging-buckets")
	project := uniqueProject(t)
	n := fmt.Sprintf("%06d", time.Now().UnixNano()%1e6)
	svc, bus, pipeline, enrollment, source := "ea-sink-"+n, "dbus-"+n, "dpl-"+n, "den-"+n, "dgas-"+n
	q := "?project=" + project
	const image = "ghcr.io/knative/helloworld-go:latest"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc-enrollments"+q+"&name="+enrollment, "")
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc-google-api-sources"+q+"&name="+source, "")
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc-pipelines"+q+"&name="+pipeline, "")
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc-buses"+q+"&name="+bus, "")
		consoleDo(t, http.MethodDelete, "/api/resources/run"+q+"&name="+svc, "")
	})

	// The destination: a Cloud Run service of this instance.
	body, _ := json.Marshal(map[string]string{"name": svc, "image": image})
	code, resp := consoleDeploy(t, http.MethodPost, "/api/resources/run"+q, string(body))
	switch {
	case code == http.StatusOK:
	case code == http.StatusBadRequest && strings.Contains(resp, "did not answer in time") && strings.Contains(resp, `"operation"`):
	default:
		t.Fatalf("deploy a service through the console API = %d: %s", code, resp)
	}
	waitForRunService(t, project, svc, image, 5*time.Minute)
	uri := eaDetailValue(t, "run", project, svc, "URL")
	if !strings.HasPrefix(uri, "http") {
		t.Fatalf("the service's URL reads %q", uri)
	}

	p := open(t)
	create := func(path string, values map[string]string, waitOption string) {
		t.Helper()
		p.navigate(path + "/create" + q)
		p.waitFor(`document.querySelector("#f-name") !== null`)
		if waitOption != "" {
			p.waitFor(`[...document.querySelectorAll("#f-` + waitOption + ` option")].some((o) => o.value === "` + values[waitOption] + `")`)
		}
		p.submitForm(values, "name")
		p.waitFor(`!location.pathname.endsWith("/create")`)
	}
	create("/eventarc/message-buses", map[string]string{"name": bus, "logSeverity": "DEBUG"}, "")
	create("/eventarc/pipelines", map[string]string{"name": pipeline, "destination": "HTTP endpoint", "uri": uri,
		"logSeverity": "INFO", "maxAttempts": "3"}, "")
	create("/eventarc/enrollments", map[string]string{"name": enrollment, "bus": bus, "pipeline": pipeline,
		"celMatch": `message.type == "order.created"`}, "bus")
	if got := eaDetailValue(t, "eventarc-pipelines", project, pipeline, "Destination"); got != uri {
		t.Errorf("the pipeline's destination reads %q, want %q", got, uri)
	}

	// Publish message on the bus's page.
	p.navigate("/eventarc/message-buses/" + bus + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Publish message (CloudBurrow extension)")`)
	evID := "order-" + n
	p.pageAction("Publish message (CloudBurrow extension)", map[string]string{"eventId": evID, "eventSource": "//shop/orders",
		"eventType": "order.created", "eventData": `{"total":42}`}, "")

	// Delivered: the pipeline's platform log, through the console's log
	// store, which the Logs Explorer shows.
	deadline := time.Now().Add(2 * time.Minute)
	var last string
	for {
		code, logs := consoleDo(t, http.MethodGet, "/api/logs?"+url.Values{"project": {project}, "contains": {evID}, "limit": {"50"}}.Encode(), "")
		last = fmt.Sprintf("%d %s", code, logs)
		if code == http.StatusOK && strings.Contains(logs, "message delivered") {
			break
		}
		if strings.Contains(logs, "delivery, attempt") && strings.Contains(logs, "ERROR") {
			t.Fatalf("the pipeline failed to deliver: %s", logs)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no delivery of %s logged within 2 minutes: %s", evID, lastChars(last, 800))
		}
		time.Sleep(3 * time.Second)
	}
	p.navigate("/logs" + q)
	p.waitFor(`document.querySelector("#view") !== null`)

	// Edit on the pipeline's page.
	p.navigate("/eventarc/pipelines/" + pipeline + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("` + uri + `")`)
	p.pageAction("Edit", map[string]string{"maxAttempts": "4", "transformation": `message.setField("data.seen", true)`}, "")
	if got := eaDetailValue(t, "eventarc-pipelines", project, pipeline, "Maximum attempts"); got != "4" {
		t.Errorf("the pipeline's maximum attempts after Edit = %q", got)
	}
	if got := eaDetailValue(t, "eventarc-pipelines", project, pipeline, "Destination"); got != uri {
		t.Errorf("Edit changed the pipeline's destination to %q", got)
	}

	// A Google API source to the bus, and Edit on its page.
	create("/eventarc/google-api-sources", map[string]string{"name": source, "bus": bus}, "bus")
	p.navigate("/eventarc/google-api-sources/" + source + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("` + bus + `")`)
	p.pageAction("Edit", map[string]string{"logSeverity": "ERROR"}, "")
	if got := eaDetailValue(t, "eventarc-google-api-sources", project, source, "Log severity"); got != "ERROR" {
		t.Errorf("the source's log severity after Edit = %q", got)
	}

	// The pages audited, with the resources on them.
	auditScreens(t, []a11yPage{
		screen("bus page", "/eventarc/message-buses/"+bus, project),
		screen("pipeline page", "/eventarc/pipelines/"+pipeline, project),
		screen("enrollment page", "/eventarc/enrollments/"+enrollment, project),
		screen("Google API source page", "/eventarc/google-api-sources/"+source, project),
	})

	for _, d := range [][2]string{{"eventarc-enrollments", enrollment}, {"eventarc-google-api-sources", source},
		{"eventarc-pipelines", pipeline}, {"eventarc-buses", bus}} {
		if code, body := consoleDo(t, http.MethodDelete, "/api/resources/"+d[0]+q+"&name="+d[1], ""); code != http.StatusOK {
			t.Errorf("Delete %s %s = %d: %s", d[0], d[1], code, body)
		}
	}
}

// TestEventarcAdvancedScreensMeetWCAG (#2006), in the run shard: Eventarc
// Advanced's Buses, Pipelines, Enrollments, Google API sources and
// Channels lists and their Create pages pass the console's WCAG 2.1 AA
// audit.
func TestEventarcAdvancedScreensMeetWCAG(t *testing.T) {
	needService(t, "eventarc-buses")
	project := uniqueProject(t)
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"Buses", "/eventarc/message-buses"}, {"Pipelines", "/eventarc/pipelines"}, {"Enrollments", "/eventarc/enrollments"},
		{"Google API sources", "/eventarc/google-api-sources"}, {"Channels", "/eventarc/channels"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	auditScreens(t, pages)
}

// eaDetailValue is one property of a resource's page, as the console API
// serves it.
func eaDetailValue(t *testing.T, service, project, name, label string) string {
	t.Helper()
	code, body := consoleDo(t, http.MethodGet, "/api/detail/"+service+"?project="+project+"&name="+name, "")
	if code != http.StatusOK {
		t.Fatalf("the %s page of %s = %d: %s", service, name, code, body)
	}
	type prop struct{ Label, Value string }
	var d struct {
		Summary  []prop
		Sections []struct {
			Groups []struct{ Properties []prop }
		}
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("decode the %s page of %s: %v", service, name, err)
	}
	all := d.Summary
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			all = append(all, g.Properties...)
		}
	}
	for _, p := range all {
		if p.Label == label {
			return p.Value
		}
	}
	t.Fatalf("the %s page of %s has no %q: %s", service, name, label, lastChars(body, 600))
	return ""
}
