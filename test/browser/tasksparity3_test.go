//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestTasksPromptsAndCheckedTasksAsGoogles (#1848), in the storage shard,
// follows https://docs.cloud.google.com/tasks/docs/deleting-appengine-queues-and-tasks:
// on the Queues list a queue's checkbox, then Pause queue, and at the
// prompt Pause (nothing typed); Resume queue acts at once; the list's
// Delete queue over the checked queues. The Logs tab's Enable logs, then
// Enable. On the queue's Tasks tab a task's
// checkbox, then Delete task, and at the prompt Delete; Purge queue, then
// Purge; Delete queue, the queue name typed, then Confirm. The console API
// reads each change back, and the list, the prompts and the Tasks tab with
// its checkboxes pass the WCAG 2.1 AA audit.
func TestTasksPromptsAndCheckedTasksAsGoogles(t *testing.T) {
	needService(t, "tasks")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	name := "projects/" + project + "/locations/us-central1/queues/prompt-q"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/tasks"+q,
		`{"name":"prompt-q","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a queue through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/tasks"+q+"&name="+url.QueryEscape(name), "")
	})
	for _, id := range []string{"keep-me", "drop-me"} {
		body, _ := json.Marshal(map[string]any{"Path": []string{name}, "Action": "createtask", "Values": map[string]string{
			"target": "HTTP", "url": "http://127.0.0.1:9/hook", "httpMethod": "POST", "taskId": id,
			"scheduleTime": "2099-01-01T00:00:00Z", "authHeader": "None"}})
		if code, out := consoleDo(t, http.MethodPost, "/api/actions/tasks"+q, string(body)); code != http.StatusOK {
			t.Fatalf("create task %s through the console API = %d: %s", id, code, out)
		}
	}
	state := func() string {
		code, body := consoleDo(t, http.MethodGet, "/api/resources/tasks"+q, "")
		var listing struct {
			Items []struct{ Name, Status string }
		}
		if err := json.Unmarshal([]byte(body), &listing); code != http.StatusOK || err != nil {
			t.Fatalf("list queues through the console API = %d (%v): %s", code, err, body)
		}
		for _, it := range listing.Items {
			if it.Name == name {
				return it.Status
			}
		}
		return ""
	}
	taskIDs := func() string {
		code, body := consoleDo(t, http.MethodGet, "/api/detail/tasks"+q+"&name="+url.QueryEscape(name), "")
		if code != http.StatusOK {
			t.Fatalf("read the queue through the console API = %d: %s", code, body)
		}
		var ids []string
		for _, id := range []string{"keep-me", "drop-me"} {
			if strings.Contains(body, name+"/tasks/"+id+`"`) {
				ids = append(ids, id)
			}
		}
		return strings.Join(ids, ",")
	}
	modal := func() (title, button string, typed bool) {
		p.waitFor(`document.querySelector(".modal.is-open #confirm-title") !== null`)
		var m struct {
			Title, Button string
			Typed         bool
		}
		p.eval(`(() => ({ Title: document.querySelector(".modal.is-open #confirm-title").textContent,
			Button: document.querySelector(".modal.is-open button[type=submit]").textContent,
			Typed: document.querySelector(".modal.is-open #confirm-input") !== null }))()`, &m)
		return m.Title, m.Button, m.Typed
	}
	checkBox := func(label string) {
		p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, fmt.Sprintf(`#view input[aria-label="Select %s"]`, label)))
		p.eval(fmt.Sprintf(`document.querySelector(%q).click()`, fmt.Sprintf(`#view input[aria-label="Select %s"]`, label)), nil)
	}
	barButton := func(sel, text string) {
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(%q)].some((b) => b.textContent === %q && !b.disabled)`, sel, text))
		p.clickText(sel, text)
	}

	// The list: the checked queue's Pause queue, its prompt's Pause.
	p.navigate("/tasks/queues" + q)
	p.waitFor(rowReady("prompt-q"))
	var bar []string
	p.eval(`[...document.querySelectorAll("#view .action-bar button")].map((b) => b.textContent)`, &bar)
	if !contains(bar, "Delete queue") || contains(bar, "Delete") {
		t.Errorf("the Queues list's header buttons are %v; want Google's Delete queue", bar)
	}
	checkBox(name)
	barButton("#view .action-bar button", "Pause queue")
	if title, button, typed := modal(); button != "Pause" || typed {
		t.Errorf("Pause queue's prompt %q has %q (typed %v); want Pause with nothing typed", title, button, typed)
	}
	p.clickText(".modal.is-open button", "Pause")
	p.waitFor(`document.querySelector(".modal") === null`)
	eventually(t, "Pause queue to pause the queue", func() bool { return state() == "PAUSED" })

	// Resume queue at once, no prompt.
	p.navigate("/tasks/queues" + q)
	p.waitFor(rowReady("prompt-q"))
	checkBox(name)
	barButton("#view .action-bar button", "Resume queue")
	eventually(t, "Resume queue to resume the queue", func() bool { return state() == "RUNNING" })

	// The queue's page.
	p.navigate("/tasks/queues" + q)
	p.waitFor(rowReady("prompt-q"))
	var page string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].find((a) => a.textContent === %q).getAttribute("href")`, name), &page)
	if !strings.Contains(page, "?") {
		page += q
	}
	tasksReady := settled + fmt.Sprintf(` && document.querySelector(%q) !== null`, `#view input[aria-label="Select `+name+`/tasks/drop-me"]`)
	auditScreens(t, []a11yPage{
		{"Queues list with checkboxes", "/tasks/queues" + q, rowReady("prompt-q"), ""},
		{"Pause queue prompt", "/tasks/queues" + q, rowReady("prompt-q"),
			fmt.Sprintf(`document.querySelector(%q).click();
			[...document.querySelectorAll("#view .action-bar button")].find((b) => b.textContent === "Pause queue").click()`,
				`#view input[aria-label="Select `+name+`"]`)},
		{"queue Tasks tab with checkboxes", page, tasksReady, ""},
		{"Delete task prompt", page, tasksReady,
			fmt.Sprintf(`document.querySelector(%q).click();
			[...document.querySelectorAll("#view .action-bar button")].find((b) => b.textContent === "Delete task").click()`,
				`#view input[aria-label="Select `+name+`/tasks/drop-me"]`)},
	})

	// The Tasks tab: the checked task's Delete task, its prompt's Delete.
	p.navigate(page)
	p.waitFor(tasksReady)
	var header []string
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].map((b) => b.textContent)`, &header)
	if contains(header, "Delete task") {
		t.Errorf("Delete task over the checked tasks is beside the title: %v", header)
	}
	checkBox(name + "/tasks/drop-me")
	barButton("#view .action-bar button", "Delete task")
	if title, button, typed := modal(); button != "Delete" || typed {
		t.Errorf("Delete task's prompt %q has %q (typed %v); want Delete with nothing typed", title, button, typed)
	}
	p.clickText(".modal.is-open button", "Delete")
	p.waitFor(`document.querySelector(".modal") === null`)
	eventually(t, "Delete task to delete only the checked task", func() bool { return taskIDs() == "keep-me" })

	// The Logs tab's Enable logs, its prompt's Enable.
	p.navigate(page)
	p.waitFor(settled)
	p.clickText(`#view [role="tab"]`, "Logs")
	barButton("#view .section-actions button", "Enable logs")
	if title, button, typed := modal(); button != "Enable" || typed {
		t.Errorf("Enable logs' prompt %q has %q (typed %v); want Enable with nothing typed", title, button, typed)
	}
	p.clickText(".modal.is-open button", "Enable")
	p.waitFor(`document.querySelector(".modal") === null`)
	eventually(t, "Enable logs to turn the queue's logging on", func() bool {
		_, body := consoleDo(t, http.MethodGet, "/api/detail/tasks"+q+"&name="+url.QueryEscape(name), "")
		return strings.Contains(body, "Disable logs")
	})

	// Purge queue, its prompt's Purge.
	p.navigate(page)
	p.waitFor(settled)
	barButton("#view .page-actions button", "Purge queue")
	if title, button, typed := modal(); button != "Purge" || typed {
		t.Errorf("Purge queue's prompt %q has %q (typed %v); want Purge with nothing typed", title, button, typed)
	}
	p.clickText(".modal.is-open button", "Purge")
	p.waitFor(`document.querySelector(".modal") === null`)
	eventually(t, "Purge queue to delete every task", func() bool { return taskIDs() == "" })

	// Delete queue: the name typed, then Confirm.
	p.navigate(page)
	p.waitFor(settled)
	barButton("#view .page-actions button", "Delete queue")
	if title, button, typed := modal(); button != "Confirm" || !typed {
		t.Errorf("Delete queue's prompt %q has %q (typed %v); want the queue name typed, then Confirm", title, button, typed)
	}
	p.confirmByName(name)
	eventually(t, "Delete queue to delete the queue", func() bool { return state() == "" })
}
