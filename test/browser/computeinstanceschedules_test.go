//go:build browser

package browser

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestComputeInstanceSchedulesThroughTheBrowser (#2206), in a shard whose
// instance serves the compute service. Instance schedules' Create a
// schedule makes a schedule with Google's Start time, Stop time and
// Frequency (#2260), weekly on Monday to Saturday, the CRON expressions
// shown only under Use CRON expression; its page says the start in those
// terms and Adds a VM instance to it, which the list counts and the VM's
// resourcePolicies name, then Removes it, and Delete removes the schedule.
// Create a disk's Encryption shows Customer-supplied key only for a
// customer-supplied key. Each change is read back through the console API.
// Its WCAG subtest audits Instance schedules, Create a schedule, a
// schedule's page and Create a disk with a customer-supplied key; its
// keyboard subtest reaches Start time and Frequency by Tab, turns Use CRON
// expression on with Space and types a Start CRON expression, and opens Remove instances
// from schedule, by keys alone.
func TestComputeInstanceSchedulesThroughTheBrowser(t *testing.T) {
	needService(t, "compute-instance-schedules")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	type row struct {
		Name   string
		Fields map[string]string
	}
	find := func(service, name string) *row {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+service+q, "")
		if code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", service, code, body)
		}
		var l struct{ Items []row }
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		for _, r := range l.Items {
			if r.Name == name {
				return &r
			}
		}
		return nil
	}
	waitRow := func(service, name string, ok func(*row) bool) *row {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for {
			if r := find(service, name); r != nil && ok(r) {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s: %+v", service, name, find(service, name))
			}
			time.Sleep(2 * time.Second)
		}
	}
	const sched, vm = "browser-isched", "browser-isched-vm"
	vmPath := "zones/us-central1-a/instances/" + vm
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1", sched}, "Action": "remove", "Values": map[string]string{"instance": vmPath}})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-schedules"+q, string(b))
		b, _ = json.Marshal(map[string]any{"Path": []string{"us-central1", sched}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-schedules"+q, string(b))
		b, _ = json.Marshal(map[string]any{"Path": []string{"us-central1-a", vm}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instances"+q, string(b))
	})
	b, _ := json.Marshal(map[string]string{"name": vm, "zone": "us-central1-a", "machineType": "e2-small", "image": "debian-cloud/debian-12"})
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instances"+q, string(b)); code != http.StatusOK {
		t.Fatalf("create the VM: %d %s", code, body)
	}

	shown := func(p *tab, id string) bool {
		var v bool
		p.eval(`(() => { const e = document.querySelector("#f-`+id+`"); return !!e && e.getClientRects().length > 0; })()`, &v)
		return v
	}

	// Create a schedule.
	p := open(t)
	p.navigate("/compute/instanceSchedules" + q)
	p.clickText("#view button", "Create schedule")
	p.waitFor(`document.querySelector("#f-name") !== null`)
	// Google's Start time, Stop time and Frequency (#2260): the CRON
	// expressions show only with Use CRON expression on, and the days of
	// the week only for Weekly.
	if !shown(p, "startClock") || !shown(p, "stopClock") || !shown(p, "frequency") || shown(p, "startSchedule") || shown(p, "weekdays") {
		t.Error("Create a schedule does not open on Start time, Stop time and Frequency alone")
	}
	p.setField("#f-useCron", "true")
	p.waitFor(`document.querySelector("#f-startSchedule").getClientRects().length > 0 && document.querySelector("#f-startClock").getClientRects().length === 0`)
	p.setField("#f-useCron", "false")
	p.waitFor(`document.querySelector("#f-startClock").getClientRects().length > 0`)
	p.setField("#f-frequency", "Weekly")
	p.waitFor(`document.querySelector("#f-weekdays").getClientRects().length > 0`)
	var chosen string
	p.eval(`document.querySelector("#f-weekdays-value").value`, &chosen)
	if chosen != "1\n2\n3\n4\n5" {
		t.Errorf("Weekly's days start as %q, want Monday to Friday", chosen)
	}
	// Saturday too: its box in the days menu.
	p.run(chromedp.Click(chromedp.CSS(`#f-weekdays`)))
	p.clickExpr(`document.querySelector('#f-weekdays-menu input[data-value="6"]')`)
	p.waitFor(`document.querySelector("#f-weekdays-value").value.split("\n").includes("6")`)
	p.submitForm(map[string]string{"name": sched, "region": "us-central1", "startClock": "09:00", "stopClock": "18:00",
		"timeZone": "Europe/Paris"}, "name")
	waitRow("compute-instance-schedules", sched, func(r *row) bool {
		return r.Fields["Start schedule"] == "0 9 * * 1,2,3,4,5,6" && r.Fields["Stop schedule"] == "0 18 * * 1,2,3,4,5,6" && r.Fields["Time zone"] == "Europe/Paris"
	})
	waitRow("compute-instances", vm, func(r *row) bool { return true })

	// Its page: Add instances to schedule.
	p.navigate("/compute/instanceSchedules/us-central1/" + sched + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Stop schedule")`)
	var page string
	p.eval(`document.querySelector("#view").textContent`, &page)
	if !strings.Contains(page, "09:00, weekly on Monday, Tuesday, Wednesday, Thursday, Friday, Saturday") {
		t.Errorf("the schedule's page does not say its start time in the form's terms: %.600s", page)
	}
	p.pageAction("Add instances to schedule", map[string]string{"instance": vmPath}, "")
	waitRow("compute-instance-schedules", sched, func(r *row) bool { return r.Fields["In use by"] == "1 VM instances" })
	code, body := consoleDo(t, http.MethodGet, "/api/detail/compute-instances"+q+"&name=us-central1-a&name="+vm, "")
	if code != http.StatusOK || !strings.Contains(body, sched) {
		t.Errorf("the VM's page does not name its schedule: %d %.600s", code, body)
	}

	// Create a disk: the customer-supplied key field only for it.
	p.navigate("/compute/disks/create" + q)
	p.waitFor(`document.querySelector("#f-name") !== null`)
	if shown(p, "rawKey") {
		t.Error("Customer-supplied key shown with Google-managed encryption")
	}
	p.eval(`[...document.querySelectorAll('input[name="f-encryption-choice"]')].find((r) => r.value === "Customer-supplied encryption key").click()`, nil)
	p.waitFor(`document.querySelector("#f-rawKey").getClientRects().length > 0`)

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"Instance schedules", "/compute/instanceSchedules" + q, rowReady(sched), ""},
			{"Create a schedule", "/compute/instanceSchedules/create" + q, `document.querySelector("#f-name") !== null`, ""},
			{"Create a schedule, weekly", "/compute/instanceSchedules/create" + q, `document.querySelector("#f-name") !== null`,
				`(() => { const f = document.querySelector("#f-frequency"); f.value = "Weekly"; f.dispatchEvent(new Event("change", { bubbles: true })); })()`},
			{"Create a schedule, monthly", "/compute/instanceSchedules/create" + q, `document.querySelector("#f-name") !== null`,
				`(() => { const f = document.querySelector("#f-frequency"); f.value = "Monthly"; f.dispatchEvent(new Event("change", { bubbles: true })); })()`},
			{"Create a schedule with CRON expressions", "/compute/instanceSchedules/create" + q, `document.querySelector("#f-name") !== null`,
				`document.querySelector("#f-useCron").click()`},
			{"an instance schedule's page", "/compute/instanceSchedules/us-central1/" + sched + q,
				`document.querySelector("#view").textContent.includes("Stop schedule")`, ""},
			{"Create a disk with a customer-supplied key", "/compute/disks/create" + q, `document.querySelector("#f-name") !== null`,
				`[...document.querySelectorAll('input[name="f-encryption-choice"]')].find((r) => r.value === "Customer-supplied encryption key").click()`},
		})
	})
	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/compute/instanceSchedules/create" + q)
		k.waitFor(`document.querySelector("#f-name") !== null`)
		// Start time and Frequency are reached by Tab.
		k.tabTo(`document.activeElement.id === "f-startClock"`)
		k.tabTo(`document.activeElement.id === "f-frequency"`)
		// Use CRON expression turned on with Space, and its Start CRON
		// expression typed into.
		k.navigate("/compute/instanceSchedules/create" + q)
		k.waitFor(`document.querySelector("#f-name") !== null`)
		k.tabTo(`document.activeElement.id === "f-useCron"`)
		k.run(chromedp.KeyEvent(" "))
		k.waitFor(`document.querySelector("#f-useCron").checked && document.querySelector("#f-startSchedule").getClientRects().length > 0`)
		k.tabTo(`document.activeElement.id === "f-startSchedule"`)
		k.run(chromedp.KeyEvent("5"))
		k.waitFor(`document.querySelector("#f-startSchedule").value.endsWith("5")`)
		k.navigate("/compute/instanceSchedules/us-central1/" + sched + q)
		k.waitFor(`document.querySelector("#view").textContent.includes("Stop schedule")`)
		k.tabTo(`document.activeElement.tagName === "BUTTON" && document.activeElement.textContent.trim() === "Remove instances from schedule"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open #f-instance") !== null`)
	})

	// Remove the VM, then Delete from the list's row menu.
	p.navigate("/compute/instanceSchedules/us-central1/" + sched + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Stop schedule")`)
	p.pageAction("Remove instances from schedule", map[string]string{"instance": vmPath}, "")
	waitRow("compute-instance-schedules", sched, func(r *row) bool { return r.Fields["In use by"] == "0 VM instances" })
	p.navigate("/compute/instanceSchedules" + q)
	vmRowMenu(p, sched, "Delete", nil, byButton)
	gone := time.Now().Add(time.Minute)
	for find("compute-instance-schedules", sched) != nil {
		if time.Now().After(gone) {
			t.Fatal("the instance schedule was never deleted")
		}
		time.Sleep(2 * time.Second)
	}
}
