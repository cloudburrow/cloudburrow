//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestComputeEngineParityScreens2139 (#2139), in the compute shard: the VM
// instances page's Instances and Instance schedules tabs, Snapshots' and
// Metadata's tabs, and the new pages in Compute Engine's menu. Create
// snapshot schedule, Create an instance template and Create instance
// schedule are made on their pages, read back through the console API,
// and deleted from the row's menu.
func TestComputeEngineParityScreens2139(t *testing.T) {
	needService(t, "compute-instance-templates")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	list := func(service string) string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+service+q, "")
		if code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", service, code, body)
		}
		return body
	}

	p := open(t)
	// The tabs, Google's: Instances | Instance schedules, and so on.
	for _, c := range []struct{ path, want string }{
		{"/compute/instances", "Instances*,Instance schedules"},
		{"/compute/instanceSchedules", "Instances,Instance schedules*"},
		{"/compute/snapshots", "Snapshots*,Snapshot schedules"},
		{"/compute/metadata/sshKeys", "Metadata,SSH keys*"},
	} {
		p.navigate(c.path + q)
		p.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		var strip string
		p.eval(`[...document.querySelectorAll("#view nav.page-tabs a")].map((a) => a.textContent + (a.getAttribute("aria-current") ? "*" : "")).join(",")`, &strip)
		if strip != c.want {
			t.Errorf("%s's tabs = %s, want %s", c.path, strip, c.want)
		}
	}

	// Snapshot schedules.
	p.navigate("/compute/snapshotSchedules" + q)
	p.clickText("#view button", "Create snapshot schedule")
	p.submitForm(map[string]string{"name": "browser-nightly", "region": "us-central1", "frequency": "Daily", "startTime": "02:00",
		"retention": "3"}, "name")
	p.waitFor(`location.pathname === "/compute/snapshotSchedules" && ` + rowReady("browser-nightly"))
	if body := list("compute-snapshot-schedules"); !strings.Contains(body, `"Autodelete after":"3 days"`) {
		t.Errorf("the schedule made on the page reads %s", body)
	}
	vmRowMenu(p, "browser-nightly", "Delete", nil, byButton)

	// Instance templates.
	p.navigate("/compute/instanceTemplates" + q)
	p.clickText("#view button", "Create instance template")
	p.submitForm(map[string]string{"name": "browser-tpl", "machineType": "e2-small"}, "name")
	p.waitFor(`location.pathname === "/compute/instanceTemplates" && ` + rowReady("browser-tpl"))
	if body := list("compute-instance-templates"); !strings.Contains(body, `"Machine type":"e2-small"`) {
		t.Errorf("the template made on the page reads %s", body)
	}
	vmRowMenu(p, "browser-tpl", "Delete", nil, byButton)

	// Instance schedules.
	p.navigate("/compute/instanceSchedules" + q)
	p.clickText("#view button", "Create schedule")
	p.submitForm(map[string]string{"name": "browser-office", "region": "us-central1", "startTime": "08:00", "stopTime": "17:00",
		"timeZone": "UTC", "frequency": "Daily"}, "name")
	p.waitFor(`location.pathname === "/compute/instanceSchedules" && ` + rowReady("browser-office"))
	if body := list("compute-instance-schedules"); !strings.Contains(body, `"Start":"0 8 * * *"`) {
		t.Errorf("the schedule made on the page reads %s", body)
	}
	vmRowMenu(p, "browser-office", "Delete", nil, byButton)
}

// TestComputeEngineParityScreensMeetWCAG2139 (#2139): the new pages and
// their create pages meet WCAG (contrast in both themes, keyboard, names,
// motion, reflow), as the compute shard's other screens do.
func TestComputeEngineParityScreensMeetWCAG2139(t *testing.T) {
	needService(t, "compute-instance-templates")
	project := servedProject(t)
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"Instance templates", "/compute/instanceTemplates"}, {"Instance groups", "/compute/instanceGroups"},
		{"Machine images", "/compute/machineImages"}, {"Instance schedules", "/compute/instanceSchedules"},
		{"Snapshot schedules", "/compute/snapshotSchedules"}, {"Images", "/compute/images"},
		{"Snapshots", "/compute/snapshots"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	pages = append(pages, screen("SSH keys", "/compute/metadata/sshKeys", project),
		screen("Effective routes", "/networking/routes/effective", project),
		screen("Internal IP addresses", "/networking/addresses/internal", project),
		screen("Subnets", "/networking/subnets/list", project),
		screen("VPC network peering", "/networking/peering/list", project))
	auditScreens(t, pages)
}
