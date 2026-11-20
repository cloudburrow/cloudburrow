//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestTasksTabMenuAndMetricsAsGoogles (#2064), in the storage shard (Cloud
// Tasks and Cloud Monitoring), follows
// https://docs.cloud.google.com/tasks/docs/monitor: on a logging queue's
// Logs tab, "Click the ellipsis icon > Disable logs. To confirm, click
// Disable." The ellipsis is the row menus' own, opened from the keyboard
// and closed by Escape with focus back on it. The Metrics tab charts
// Google's API requests, Queue depth, Task attempt count and Task attempt
// delays from the instance's Cloud Monitoring, after a task is dispatched
// to a local HTTP target. The Logs tab with its menu open and the Metrics
// tab pass the WCAG 2.1 AA audit.
func TestTasksTabMenuAndMetricsAsGoogles(t *testing.T) {
	needService(t, "tasks")
	needService(t, "monitoring-explorer")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	name := "projects/" + project + "/locations/us-central1/queues/menu-q"
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/tasks"+q,
		`{"name":"menu-q","location":"us-central1","samplingRatio":"1"}`); code != http.StatusOK {
		t.Fatalf("create a queue through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/tasks"+q+"&name="+url.QueryEscape(name), "")
	})
	body, _ := json.Marshal(map[string]any{"Path": []string{name}, "Action": "createtask", "Values": map[string]string{
		"target": "HTTP", "url": target.URL + "/hook", "httpMethod": "POST", "taskId": "dispatched", "authHeader": "None"}})
	if code, out := consoleDo(t, http.MethodPost, "/api/actions/tasks"+q, string(body)); code != http.StatusOK {
		t.Fatalf("create a task through the console API = %d: %s", code, out)
	}
	eventually(t, "the task to reach the local target", func() bool { return hits.Load() >= 1 })
	detail := func() string {
		code, body := consoleDo(t, http.MethodGet, "/api/detail/tasks"+q+"&name="+url.QueryEscape(name), "")
		if code != http.StatusOK {
			t.Fatalf("read the queue through the console API = %d: %s", code, body)
		}
		return body
	}
	if !strings.Contains(detail(), "Disable logs") {
		t.Fatal("a queue created with sampling ratio 1 does not offer Disable logs")
	}

	// The Metrics tab: Google's four charts, the attempt among them.
	eventually(t, "the Metrics tab to chart the dispatched attempt", func() bool {
		var d struct {
			Sections []struct {
				ID     string
				Series []struct {
					Label  string
					Points []struct{ Value *float64 }
				}
			}
		}
		if err := json.Unmarshal([]byte(detail()), &d); err != nil {
			t.Fatal(err)
		}
		for _, s := range d.Sections {
			if s.ID != "metrics" {
				continue
			}
			for _, sv := range s.Series {
				for _, pt := range sv.Points {
					if sv.Label == "Task attempt count" && pt.Value != nil && *pt.Value >= 1 {
						return true
					}
				}
			}
		}
		return false
	})
	page := "/tasks/queues/" + url.PathEscape(name) + q
	p.navigate("/tasks/queues" + q)
	p.waitFor(rowReady("menu-q"))
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].find((a) => a.textContent === %q).getAttribute("href")`, name), &page)
	if !strings.Contains(page, "?") {
		page += q
	}
	p.navigate(page)
	p.waitFor(settled)
	p.clickText(`#view [role="tab"]`, "Metrics")
	// A chart with fewer than two readings says so in its card instead of a
	// line, as every console chart does.
	p.waitFor(`document.querySelectorAll("#view .charts .chart-card").length === 4`)
	var charts []string
	p.eval(`[...document.querySelectorAll("#view .charts .chart-card h3")].map((h) => h.textContent)`, &charts)
	for i, want := range []string{"API requests", "Queue depth", "Task attempt count", "Task attempt delays"} {
		if i >= len(charts) || charts[i] != want {
			t.Errorf("chart %d is %v; want Google's %s", i, charts, want)
		}
	}

	// The Logs tab: no Disable logs button; the ellipsis holds it.
	p.navigate(page)
	p.waitFor(settled)
	p.clickText(`#view [role="tab"]`, "Logs")
	trigger := `#section-actions-logs .overflow-trigger`
	p.waitFor(settled + fmt.Sprintf(` && document.querySelector(%q) !== null`, trigger)) // the lines loaded
	var buttons []string
	p.eval(`[...document.querySelectorAll("#section-actions-logs > button")].map((b) => b.textContent)`, &buttons)
	if contains(buttons, "Disable logs") {
		t.Errorf("the Logs tab draws Disable logs as a button %v; Google's is in the ellipsis menu", buttons)
	}
	type state struct {
		Open      bool
		Expanded  string
		OnTrigger bool
		Items     []string
	}
	read := func() state {
		var s state
		p.eval(fmt.Sprintf(`(() => { const b = document.querySelector(%q), m = b.parentElement.querySelector(".overflow-menu");
			return { Open: !m.hidden, Expanded: b.getAttribute("aria-expanded"), OnTrigger: document.activeElement === b,
			         Items: [...m.querySelectorAll('[role="menuitem"]')].map((i) => i.textContent) }; })()`, trigger), &s)
		return s
	}
	menuOpen := fmt.Sprintf(`!document.querySelector(%q).parentElement.querySelector(".overflow-menu").hidden`, trigger)
	p.tabTo(fmt.Sprintf(`a === document.querySelector(%q)`, trigger))
	var label string
	p.eval(fmt.Sprintf(`document.querySelector(%q).getAttribute("aria-label")`, trigger), &label)
	if label != "Actions for the Logs tab" {
		t.Errorf("the ellipsis is named %q; want Actions for the Logs tab", label)
	}
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(menuOpen)
	if s := read(); s.Expanded != "true" || len(s.Items) != 1 || s.Items[0] != "Disable logs" {
		t.Errorf("the open menu: aria-expanded %q, items %v; want Disable logs", s.Expanded, s.Items)
	}
	p.run(chromedp.KeyEvent(kb.Tab))
	var inMenu bool
	p.eval(`document.activeElement.getAttribute("role") === "menuitem"`, &inMenu)
	if !inMenu {
		t.Error("Tab from the open menu's trigger did not reach Disable logs")
	}
	p.run(chromedp.KeyEvent(kb.Escape))
	if s := read(); s.Open || s.Expanded != "false" || !s.OnTrigger {
		t.Errorf("Escape: menu open %v, aria-expanded %q, focus on its trigger %v; want it closed with focus back", s.Open, s.Expanded, s.OnTrigger)
	}

	auditScreens(t, []a11yPage{
		{"queue Metrics tab with its charts", page, settled,
			`[...document.querySelectorAll('#view [role="tab"]')].find((b) => b.textContent === "Metrics").click()`},
		{"queue Logs tab with its ellipsis menu open", page, settled,
			`[...document.querySelectorAll('#view [role="tab"]')].find((b) => b.textContent === "Logs").click();
			setTimeout(() => document.querySelector("#section-actions-logs .overflow-trigger").click(), 0)`},
	})

	// Ellipsis > Disable logs, then Disable.
	p.navigate(page)
	p.waitFor(settled)
	p.clickText(`#view [role="tab"]`, "Logs")
	p.waitFor(settled + fmt.Sprintf(` && document.querySelector(%q) !== null`, trigger)) // the lines loaded
	p.eval(fmt.Sprintf(`document.querySelector(%q).click()`, trigger), nil)
	p.waitFor(menuOpen)
	p.clickText(`#section-actions-logs [role="menuitem"]`, "Disable logs")
	p.waitFor(`document.querySelector(".modal.is-open button[type=submit]") !== null`)
	var submit string
	p.eval(`document.querySelector(".modal.is-open button[type=submit]").textContent`, &submit)
	if submit != "Disable" {
		t.Errorf("Disable logs' prompt ends in %q; want Disable", submit)
	}
	p.clickText(".modal.is-open button", "Disable")
	p.waitFor(`document.querySelector(".modal") === null`)
	eventually(t, "Disable logs to turn the queue's logging off", func() bool { return strings.Contains(detail(), "Enable logs") })
}
