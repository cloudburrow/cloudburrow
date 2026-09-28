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

	"github.com/chromedp/chromedp"
)

// editDefaults is a resource's Edit form, as the console API's detail route
// prefills it: what the browser's create saved, read back.
func editDefaults(t *testing.T, service, project string, path ...string) map[string]string {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/"+service+"?"+q.Encode(), "")
	var d struct {
		Edit struct {
			Fields []struct{ Name, Default string }
		}
		Sections []struct {
			Groups []struct {
				Properties []struct{ Label, Value string }
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
		t.Fatalf("read %s %v through the console API = %d (%v): %s", service, path, code, err, body)
	}
	out := map[string]string{}
	for _, f := range d.Edit.Fields {
		out[f.Name] = f.Default
	}
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			for _, pr := range g.Properties {
				out[pr.Label] = pr.Value
			}
		}
	}
	return out
}

// setField sets a form control's value, or a checkbox's checked state, and
// tells the form it changed.
func setField(p *tab, scope, id, v string) {
	p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(%q);
		if (f.type === "checkbox") f.checked = %q === "true"; else f.value = %q;
		f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true }));
		return true; })()`, scope+" #"+id, v, v), nil)
}

// TestPubSubCreateOptionsThroughTheForms (#852), in the storage shard, whose
// instance serves Pub/Sub: Create topic, on its own page now that it holds
// the topic's options, offers the message retention and the schema, and a
// topic created with a retention reads it back; the topic's
// Create subscription dialog offers ordering, the filter, retry and dead
// lettering, and neither expiration nor exactly-once, and fits the window; a
// malformed filter is refused on the form with the console's explanation, and
// a valid one is saved with one action that the console API reads back.
func TestPubSubCreateOptionsThroughTheForms(t *testing.T) {
	needService(t, "pubsub")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/browser-opts"
	sub := "projects/" + project + "/subscriptions/browser-opts-eu"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})

	p.navigate("/pubsub/topics?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view button.primary")].some((b) => b.textContent === "Create topic")`)
	p.clickText("#view button.primary", "Create topic")
	p.waitFor(`document.querySelector("#view #f-messageRetention") !== null`)
	var topicForm struct {
		Encodings []string
		Schema    bool
	}
	p.eval(`(() => { const q = (s) => document.querySelector("#view " + s);
		return { Encodings: [...q("#f-schemaEncoding").options].map((o) => o.value), Schema: q("#f-schema") !== null }; })()`, &topicForm)
	if strings.Join(topicForm.Encodings, ",") != "JSON,BINARY" || !topicForm.Schema {
		t.Errorf("Create topic offers schema %v, encodings %v; want a schema and JSON or BINARY", topicForm.Schema, topicForm.Encodings)
	}
	setField(p, "#view", "f-name", "browser-opts")
	setField(p, "#view", "f-defaultSubscription", "false")
	setField(p, "#view", "f-messageRetention", "2d")
	p.run(chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`location.pathname === "/pubsub/topics"`)
	if got := editDefaults(t, "pubsub", project, topic)["messageRetention"]; got != "2d" {
		t.Errorf("the topic created in the browser reads a retention of %q; want 2d", got)
	}

	p.navigate("/pubsub/topics?project=" + project)
	p.clickText("#view tbody a", topic)
	openCreate := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Create subscription")
		p.waitFor(`document.querySelector(".modal.is-open #f-filter") !== null`)
	}
	openCreate()
	var form struct {
		Fields []string
		Fits   bool
	}
	p.eval(`(() => { const r = document.querySelector(".modal.is-open .modal-body").getBoundingClientRect();
		return { Fields: [...document.querySelectorAll(".modal.is-open [id^=f-]")].map((f) => f.id.slice(2)),
		         Fits: r.top >= 0 && r.bottom <= window.innerHeight }; })()`, &form)
	for _, want := range []string{"messageOrdering", "filter", "retainAcked", "minBackoff", "deadLetterTopic", "maxDeliveryAttempts"} {
		if !contains(form.Fields, want) {
			t.Errorf("Create subscription does not offer %s: %v", want, form.Fields)
		}
	}
	for _, absent := range []string{"expiration", "exactlyOnce"} {
		if contains(form.Fields, absent) {
			t.Errorf("Create subscription offers %s, which the emulator does not act on", absent)
		}
	}
	if !form.Fits {
		t.Error("the Create subscription dialog runs past the window; a form taller than the window must scroll inside it")
	}

	setField(p, ".modal.is-open", "f-name", "browser-opts-eu")
	setField(p, ".modal.is-open", "f-filter", "nonsense ===")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal.is-open .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal.is-open .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "the emulator refused the filter") {
		t.Errorf("a malformed filter was refused on the form with %q", refusal)
	}
	p.forgive()

	setField(p, ".modal.is-open", "f-filter", `attributes.region = "eu"`)
	setField(p, ".modal.is-open", "f-messageOrdering", "true")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPost, "/api/actions/pubsub"); len(sent) != 2 {
		t.Errorf("the refused and the saved create sent %d actions, want 2: %v", len(sent), sent)
	}
	got := editDefaults(t, "pubsub-subscriptions", project, sub)
	if got["filter"] != `attributes.region = "eu"` || got["messageOrdering"] != "Yes" {
		t.Errorf("the subscription created in the browser reads filter %q, ordering %q", got["filter"], got["messageOrdering"])
	}
}

// TestKMSCreateKeyOptionsThroughTheForm (#852), in the run shard, whose
// instance serves Cloud KMS: Create key on a ring's page offers the destroy
// scheduled duration, defaulted to Cloud KMS's 30 days, and the rotation
// schedule; a key created there with 3 days and a 30-day period reads both
// back through the console API.
func TestKMSCreateKeyOptionsThroughTheForm(t *testing.T) {
	needService(t, "kms")
	p := open(t)
	project := uniqueProject(t)
	ring := "projects/" + project + "/locations/global/keyRings/browser-opts"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms?project="+project,
		`{"keyRingId":"browser-opts","location":"global"}`); code != http.StatusOK {
		t.Fatalf("create a key ring through the console API = %d: %s", code, body)
	}
	p.navigate("/kms/keyrings?project=" + project)
	p.clickText("#view tbody a", ring)
	p.clickText("#view .page-actions button", "Create key")
	p.waitFor(`document.querySelector(".modal.is-open #f-destroyScheduledDuration") !== null`)
	var defaults struct{ Destroy, Period string }
	p.eval(`({ Destroy: document.querySelector(".modal.is-open #f-destroyScheduledDuration").value,
	           Period: document.querySelector(".modal.is-open #f-rotationPeriod").value })`, &defaults)
	if defaults.Destroy != "30d" || defaults.Period != "" {
		t.Errorf("Create key opened with destroy %q, period %q; want 30d and none", defaults.Destroy, defaults.Period)
	}
	next := time.Now().UTC().Add(10 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	setField(p, ".modal.is-open", "f-cryptoKeyId", "k")
	setField(p, ".modal.is-open", "f-destroyScheduledDuration", "3d")
	setField(p, ".modal.is-open", "f-rotationPeriod", "30d")
	setField(p, ".modal.is-open", "f-nextRotationTime", next)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	got := editDefaults(t, "kms", project, ring, "k")
	if got["Destroy scheduled duration"] != "72h0m0s" || got["rotationPeriod"] != "30d" || got["nextRotationTime"] != next {
		t.Errorf("the key created in the browser reads destroy %q, period %q, next %q",
			got["Destroy scheduled duration"], got["rotationPeriod"], got["nextRotationTime"])
	}
}

// TestStorageCreateBucketOptionsThroughTheForm (#852), in the storage shard:
// Create, on its own page, offers the storage classes the backend keeps, uniform access on as
// the documented form defaults it, versioning, the soft delete retention and
// object retention; a bucket created there with NEARLINE and versioning
// reads both back through the console API.
func TestStorageCreateBucketOptionsThroughTheForm(t *testing.T) {
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-opts"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})
	p.navigate("/storage/browser?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create")`)
	p.run(chromedp.Click(`#view .state button.primary`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view #f-storageClass") !== null`)
	var form struct {
		Classes                      []string
		Uniform, Versioning, Objects bool
		SoftDelete                   string
	}
	p.eval(`(() => { const q = (s) => document.querySelector("#view " + s);
		return { Classes: [...q("#f-storageClass").options].map((o) => o.value), Uniform: q("#f-uniformAccess").checked,
		         Versioning: q("#f-versioning").checked, Objects: q("#f-objectRetention").checked,
		         SoftDelete: q("#f-softDeleteSeconds").value }; })()`, &form)
	if strings.Join(form.Classes, ",") != "STANDARD,NEARLINE,COLDLINE,ARCHIVE" || !form.Uniform || form.Versioning ||
		form.Objects || form.SoftDelete != "604800" {
		t.Errorf("Create opened with %+v", form)
	}
	setField(p, "#view", "f-name", bucket)
	setField(p, "#view", "f-storageClass", "NEARLINE")
	setField(p, "#view", "f-versioning", "true")
	p.run(chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`location.pathname === "/storage/browser"`)
	got := editDefaults(t, "storage", project, bucket)
	if got["storageClass"] != "NEARLINE" || got["versioning"] != "true" || got["uniformAccess"] != "true" {
		t.Errorf("the bucket created in the browser reads class %q, versioning %q, uniform %q",
			got["storageClass"], got["versioning"], got["uniformAccess"])
	}
}

// TestCloudRunJobExecuteWithOverridesThroughTheForm (#852), in the run shard: the
// Create job and Deploy container forms offer labels and variables from
// Secret Manager; a job's page offers Execute with overrides, whose form
// sends one action with the overrides and puts an execution with the
// overridden task count on the job's Executions tab.
func TestCloudRunJobExecuteWithOverridesThroughTheForm(t *testing.T) {
	needService(t, "run-jobs")
	p := open(t)
	project := uniqueProject(t)
	for _, page := range []string{"/run/jobs/create", "/run/create"} {
		p.navigate(page + "?project=" + project)
		p.waitFor(`document.querySelector("#view #f-name") !== null`)
		var has struct{ Labels, Secrets bool }
		p.eval(`({ Labels: document.querySelector("#view #f-labels") !== null,
		           Secrets: document.querySelector("#view #f-secretEnv") !== null })`, &has)
		if !has.Labels || !has.Secrets {
			t.Errorf("%s offers labels %v, variables from Secret Manager %v; want both", page, has.Labels, has.Secrets)
		}
	}

	id := fmt.Sprintf("browser-opts-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/run-jobs?project="+project+"&name="+id, "") })
	body, _ := json.Marshal(map[string]string{"name": id, "image": "docker.io/library/busybox:1.36",
		"command": "sh -c", "args": `'exit 3'`, "taskCount": "1", "maxRetries": "0"})
	if code, resp := consoleDo(t, http.MethodPost, "/api/resources/run-jobs?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("create a job through the console API = %d: %s", code, resp)
	}
	p.navigate("/run/jobs?project=" + project)
	p.clickText("#view tbody a", id)
	p.clickText("#view .page-actions button", "Execute with overrides")
	p.waitFor(`document.querySelector(".modal.is-open #f-taskCount") !== null`)
	setField(p, ".modal.is-open", "f-args", `'echo overridden'`)
	setField(p, ".modal.is-open", "f-taskCount", "2")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("completed"))`)
	if sent := p.sent(http.MethodPost, "/api/actions/run-jobs"); len(sent) != 1 {
		t.Errorf("Execute with overrides sent %d actions, want one: %v", len(sent), sent)
	}
	code, resp := consoleDo(t, http.MethodGet, "/api/detail/run-jobs?"+url.Values{"project": {project}, "name": {id}}.Encode(), "")
	var job struct {
		Sections []struct {
			ID      string
			Listing struct {
				Items []struct {
					Name   string
					Fields map[string]string
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(resp), &job); code != http.StatusOK || err != nil {
		t.Fatalf("the job's detail through the console API = %d (%v): %s", code, err, resp)
	}
	var execution string
	for _, s := range job.Sections {
		if s.ID == "executions" && len(s.Listing.Items) == 1 {
			execution = s.Listing.Items[0].Name
		}
	}
	if execution == "" {
		t.Fatalf("the job has no single execution after Execute with overrides: %s", resp)
	}
	code, resp = consoleDo(t, http.MethodGet, "/api/detail/run-jobs?"+url.Values{"project": {project},
		"name": {id, lastPathSegment(execution)}}.Encode(), "")
	if code != http.StatusOK || !strings.Contains(resp, "echo overridden") {
		t.Errorf("the execution's page does not show the overridden arguments: %d %s", code, resp)
	}
}

func lastPathSegment(s string) string { return s[strings.LastIndex(s, "/")+1:] }
