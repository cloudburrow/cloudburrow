//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestEventarcAdvancedPagesThroughTheBrowser (#1947): Eventarc's Buses,
// Pipelines, Enrollments, Google API sources and Channels pages. Through
// each Create page, a bus with a log severity, a pipeline to a Pub/Sub
// topic with a JSON input format and a transformation, an enrollment of
// the pipeline on the bus, a Google API source to the bus and a custom
// channel are made; each is listed with Google's columns and its page
// reads what was set (the bus's lists its enrollment, the channel is
// Active); Edit changes the enrollment's CEL expression, which its page
// then reads; Delete removes each.
func TestEventarcAdvancedPagesThroughTheBrowser(t *testing.T) {
	needService(t, "run")
	needService(t, "eventarc")
	needService(t, "eventarc-buses")
	project := uniqueProject(t)
	n := fmt.Sprintf("%06d", time.Now().UnixNano()%1e6)
	bus, pipeline, enrollment, source, channel := "bus-"+n, "pl-"+n, "en-"+n, "gas-"+n, "ch-"+n
	q := "?project=" + project
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc-enrollments"+q+"&name="+enrollment, "")
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc-google-api-sources"+q+"&name="+source, "")
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc-pipelines"+q+"&name="+pipeline, "")
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc-buses"+q+"&name="+bus, "")
		consoleDo(t, http.MethodDelete, "/api/resources/eventarc-channels"+q+"&name="+channel, "")
	})
	p := open(t)

	labelsOn := func(path string) string {
		t.Helper()
		p.navigate(path + q)
		p.waitFor(`document.querySelector("#f-name") !== null`)
		var labels []string
		p.eval(`[...document.querySelectorAll("#view label")].map((n) => n.textContent.trim())`, &labels)
		return strings.Join(labels, "|")
	}
	listed := func(path, name string, columns ...string) string {
		t.Helper()
		p.navigate(path + q)
		p.waitFor(`document.querySelector("#view").textContent.includes("` + name + `")`)
		var row, head string
		p.eval(`(() => { const r = [...document.querySelectorAll("#view tr")].find((n) => n.textContent.includes("`+name+`"));
			return r ? r.textContent : ""; })()`, &row)
		p.eval(`document.querySelector("#view thead") ? document.querySelector("#view thead").textContent : ""`, &head)
		for _, c := range columns {
			if !strings.Contains(head, c) {
				t.Errorf("%s has no %q column: %q", path, c, head)
			}
		}
		return row
	}
	detail := func(service, name string) string {
		t.Helper()
		code, page := consoleDo(t, http.MethodGet, "/api/detail/"+service+q+"&name="+name, "")
		if code != http.StatusOK {
			t.Fatalf("the %s page of %s = %d: %s", service, name, code, page)
		}
		return page
	}

	// Buses.
	labels := labelsOn("/eventarc/message-buses/create")
	for _, want := range []string{"Bus name", "Region", "Display name", "Log severity", "Cloud KMS key", "Labels"} {
		if !strings.Contains(labels, want) {
			t.Errorf("Create bus has no %q field: %s", want, labels)
		}
	}
	p.submitForm(map[string]string{"name": bus, "displayName": "Orders", "logSeverity": "INFO"}, "name")
	p.waitFor(`!location.pathname.endsWith("/create")`)
	if row := listed("/eventarc/message-buses", bus, "Region", "Log severity", "Encryption"); !strings.Contains(row, "INFO") || !strings.Contains(row, "Google-managed") {
		t.Errorf("the bus's row reads %q", row)
	}

	// Pipelines.
	labels = labelsOn("/eventarc/pipelines/create")
	for _, want := range []string{"Pipeline name", "Destination", "Destination URI", "Pub/Sub topic", "Input payload format",
		"Output payload format", "Transformation (CEL)", "Maximum attempts", "Log severity"} {
		if !strings.Contains(labels, want) {
			t.Errorf("Create pipeline has no %q field: %s", want, labels)
		}
	}
	p.submitForm(map[string]string{"name": pipeline, "destination": "Pub/Sub topic", "topic": "orders-" + n, "inputFormat": "JSON",
		"transformation": `message.setField("data.seen", true)`, "maxAttempts": "3"}, "name")
	p.waitFor(`!location.pathname.endsWith("/create")`)
	if row := listed("/eventarc/pipelines", pipeline, "Region", "Destination", "Input format"); !strings.Contains(row, "Pub/Sub topic: projects/"+project+"/topics/orders-"+n) ||
		!strings.Contains(row, "JSON") {
		t.Errorf("the pipeline's row reads %q", row)
	}
	if page := detail("eventarc-pipelines", pipeline); !strings.Contains(page, "data.seen") || !strings.Contains(page, "Maximum attempts") {
		t.Errorf("the pipeline's page: %s", page)
	}

	// Enrollments, choosing the bus and pipeline the project has.
	labels = labelsOn("/eventarc/enrollments/create")
	for _, want := range []string{"Enrollment name", "Message bus", "CEL expression", "Destination pipeline"} {
		if !strings.Contains(labels, want) {
			t.Errorf("Create enrollment has no %q field: %s", want, labels)
		}
	}
	p.waitFor(`[...document.querySelectorAll("#f-bus option")].some((o) => o.value === "` + bus + `")`)
	p.submitForm(map[string]string{"name": enrollment, "bus": bus, "pipeline": pipeline, "celMatch": `message.type == "order.created"`}, "name")
	p.waitFor(`!location.pathname.endsWith("/create")`)
	if row := listed("/eventarc/enrollments", enrollment, "Message bus", "CEL match", "Destination pipeline"); !strings.Contains(row, bus) ||
		!strings.Contains(row, pipeline) || !strings.Contains(row, "order.created") {
		t.Errorf("the enrollment's row reads %q", row)
	}
	if page := detail("eventarc-buses", bus); !strings.Contains(page, enrollment) || !strings.Contains(page, "Orders") {
		t.Errorf("the bus's page does not list its enrollment: %s", page)
	}
	p.clickText("#view tbody a", enrollment)
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/eventarc/enrollments/"+enrollment))
	p.pageAction("Edit", map[string]string{"celMatch": `message.type == "order.shipped"`}, "")
	if page := detail("eventarc-enrollments", enrollment); !strings.Contains(page, "order.shipped") {
		t.Errorf("the enrollment's page after Edit: %s", page)
	}

	// Google API sources.
	labels = labelsOn("/eventarc/google-api-sources/create")
	for _, want := range []string{"Google API source name", "Destination bus", "Log severity"} {
		if !strings.Contains(labels, want) {
			t.Errorf("Create Google API source has no %q field: %s", want, labels)
		}
	}
	p.waitFor(`[...document.querySelectorAll("#f-bus option")].some((o) => o.value === "` + bus + `")`)
	p.submitForm(map[string]string{"name": source, "bus": bus}, "name")
	p.waitFor(`!location.pathname.endsWith("/create")`)
	if row := listed("/eventarc/google-api-sources", source, "Destination bus", "Log severity"); !strings.Contains(row, bus) {
		t.Errorf("the Google API source's row reads %q", row)
	}

	// Channels: one with no provider is Active at once.
	labels = labelsOn("/eventarc/channels/create")
	for _, want := range []string{"Channel name", "Region", "Provider"} {
		if !strings.Contains(labels, want) {
			t.Errorf("Create channel has no %q field: %s", want, labels)
		}
	}
	p.submitForm(map[string]string{"name": channel}, "name")
	p.waitFor(`!location.pathname.endsWith("/create")`)
	if row := listed("/eventarc/channels", channel, "Provider", "State", "Region"); !strings.Contains(row, "Active") || !strings.Contains(row, "Custom") {
		t.Errorf("the channel's row reads %q", row)
	}
	if page := detail("eventarc-channels", channel); !strings.Contains(page, "Activation token") {
		t.Errorf("the channel's page: %s", page)
	}

	// Delete, dependants first.
	for _, d := range [][2]string{{"eventarc-enrollments", enrollment}, {"eventarc-google-api-sources", source},
		{"eventarc-pipelines", pipeline}, {"eventarc-buses", bus}, {"eventarc-channels", channel}} {
		if code, body := consoleDo(t, http.MethodDelete, "/api/resources/"+d[0]+q+"&name="+d[1], ""); code != http.StatusOK {
			t.Errorf("Delete %s %s = %d: %s", d[0], d[1], code, body)
		}
	}
	code, body := consoleDo(t, http.MethodGet, "/api/resources/eventarc-buses"+q, "")
	if code != http.StatusOK || strings.Contains(body, bus) {
		t.Errorf("the bus is still listed after Delete: %d %s", code, body)
	}
}
