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
	"github.com/chromedp/chromedp/kb"
)

// TestComputeRegionalDisksThroughTheBrowser (#2206), in a shard whose
// instance serves the compute service. Disks' Create a disk page with
// Google's Location choice: Regional, the zone and its Replica zone; the
// disk listed with both replica zones; its page with Replica zones; Edit
// size from the row's menu; a VM's Attach existing disk offering the
// regional disk replicated in its zone, attached; then the VM and the disk
// deleted; Consistency groups' Create consistency group, the group's
// page and Delete. Each change is read back through the console API. Its
// WCAG subtest audits the Disks list, Create a disk with Regional chosen,
// the regional disk's page, Asynchronous replication, Consistency groups
// and a group's page; its keyboard subtest opens the disk's page from
// its row and Create disk's Location by keys alone.
func TestComputeRegionalDisksThroughTheBrowser(t *testing.T) {
	needService(t, "compute-instances")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	type row struct {
		Name   string
		Status string
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
	waitRow := func(service, name string, ok func(*row) bool, d time.Duration) *row {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			r := find(service, name)
			if r != nil && ok(r) {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s: %+v", service, name, r)
			}
			time.Sleep(2 * time.Second)
		}
	}
	const disk, vm = "browser-regional", "browser-rvm"
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-b", vm}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instances"+q, string(b))
		for i := 0; i < 60 && find("compute-instances", vm) != nil; i++ {
			time.Sleep(2 * time.Second)
		}
		b, _ = json.Marshal(map[string]any{"Path": []string{"us-central1", disk}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-disks"+q, string(b))
	})
	chooseRadio := func(p *tab, field, value string) {
		p.t.Helper()
		sel := fmt.Sprintf(`[...document.querySelectorAll('input[type="radio"][name="f-%s-choice"]')].find((r) => r.value === %q)`, field, value)
		p.waitFor(sel + ` !== undefined`)
		p.eval(sel+`.click()`, nil)
	}

	p := open(t)
	p.navigate("/compute/disks" + q)
	p.clickText("#view button", "Create disk")
	p.waitFor(`document.querySelector("#f-name") !== null`)
	var replicaHidden bool
	p.eval(`document.querySelector("#f-replicaZone").getClientRects().length === 0`, &replicaHidden)
	if !replicaHidden {
		t.Error("Replica zone is shown for a Single zone disk")
	}
	chooseRadio(p, "location", "Regional")
	p.waitFor(`document.querySelector("#f-replicaZone").getClientRects().length > 0`)
	p.submitForm(map[string]string{"name": disk, "zone": "us-central1-a", "replicaZone": "us-central1-b", "sizeGb": "10"}, "name")
	p.waitFor(`location.pathname === "/compute/disks"`)
	r := waitRow("compute-disks", disk, func(r *row) bool { return r.Status == "READY" }, 2*time.Minute)
	if r.Fields["Zone"] != "us-central1-a, us-central1-b" || r.Fields["Size"] != "10 GB" {
		t.Fatalf("the regional disk's row: %+v", r)
	}
	p.navigate("/compute/disks" + q)
	p.waitFor(rowReady("us-central1-a, us-central1-b"))

	// Its page: the region and replica zones.
	p.follow(disk)
	p.waitFor(`decodeURIComponent(location.pathname).endsWith("us-central1/` + disk + `")`)
	p.waitFor(`document.querySelector("#view").textContent.includes("Replica zones")`)
	var page string
	p.eval(`document.querySelector("#view").textContent`, &page)
	if !strings.Contains(page, "us-central1 (us-central1-a, us-central1-b)") {
		t.Errorf("the regional disk's page: %.600s", page)
	}

	// Edit size from the list's row menu.
	p.navigate("/compute/disks" + q)
	vmRowMenu(p, disk, "Edit size", map[string]string{"sizeGb": "20"}, "")
	waitRow("compute-disks", disk, func(r *row) bool { return r.Fields["Size"] == "20 GB" }, time.Minute)

	// A VM in the replica zone us-central1-b attaches it.
	b, _ := json.Marshal(map[string]string{"name": vm, "zone": "us-central1-b", "machineType": "e2-small", "image": "debian-cloud/debian-12"})
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instances"+q, string(b)); code != http.StatusOK {
		t.Fatalf("create the VM = %d: %s", code, body)
	}
	waitRow("compute-instances", vm, func(r *row) bool { return r.Status == "RUNNING" }, 10*time.Minute)
	p.navigate("/compute/instances/us-central1-b/" + vm + q)
	p.waitFor(`document.querySelector("#view h1") !== null`)
	p.clickText(p.findAction("Attach existing disk"), "Attach existing disk")
	p.waitFor(`document.querySelector(".modal.is-open #f-disk") !== null`)
	var options string
	p.eval(`[...document.querySelectorAll(".modal.is-open #f-disk option")].map((o) => o.value + "=" + o.textContent).join(";")`, &options)
	if !strings.Contains(options, "regions/us-central1/disks/"+disk+"="+disk+" (regional)") {
		t.Fatalf("Attach existing disk offers %q", options)
	}
	p.waitFor(`document.querySelector(".modal.is-open #f-forceAttach") !== null`)
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.pageAction("Attach existing disk", map[string]string{"disk": "regions/us-central1/disks/" + disk, "deviceName": "shared"}, "")
	waitRow("compute-disks", disk, func(r *row) bool { return r.Fields["In use by"] == vm }, 3*time.Minute)

	// Consistency groups: Create consistency group, listed with its region.
	const cg = "browser-cg"
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1", cg}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-consistencygroups"+q, string(b))
	})
	p.navigate("/compute/consistencyGroups" + q)
	p.clickText("#view button", "Create consistency group")
	p.submitForm(map[string]string{"name": cg, "region": "us-central1"}, "name")
	waitRow("compute-consistencygroups", cg, func(r *row) bool { return r.Fields["Region"] == "us-central1" }, time.Minute)
	p.navigate("/compute/consistencyGroups/us-central1/" + cg + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Consistency group members")`)

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"Disks with a regional disk", "/compute/disks" + q, rowReady(disk), ""},
			{"Create a disk, Regional", "/compute/disks/create" + q, `document.querySelector("#f-location") !== null`,
				`[...document.querySelectorAll('input[name="f-location-choice"]')].find((r) => r.value === "Regional").click()`},
			{"the regional disk's page", "/compute/disks/us-central1/" + disk + q, `document.querySelector("#view").textContent.includes("Replica zones")`, ""},
			{"Asynchronous replication", "/compute/asynchronousReplication" + q, settled, ""},
			{"Consistency groups", "/compute/consistencyGroups" + q, rowReady(cg), ""},
			{"a consistency group's page", "/compute/consistencyGroups/us-central1/" + cg + q,
				`document.querySelector("#view").textContent.includes("Consistency group members")`, ""},
		})
	})
	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/compute/disks" + q)
		k.waitFor(rowReady(disk))
		k.tabTo(fmt.Sprintf(`document.activeElement.tagName === "A" && document.activeElement.closest("#view tbody tr") !== null &&
			document.activeElement.textContent.trim() === %q`, disk))
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector("#view").textContent.includes("Replica zones")`)
		k.navigate("/compute/disks/create" + q)
		k.waitFor(`document.querySelector("#f-name") !== null`)
		k.tabTo(`document.activeElement.name === "f-location-choice"`)
		k.run(chromedp.KeyEvent(kb.ArrowDown))
		k.waitFor(`document.querySelector("#f-location").value === "Regional" && document.querySelector("#f-replicaZone").getClientRects().length > 0`)
	})

	p.navigate("/compute/consistencyGroups" + q)
	vmRowMenu(p, cg, "Delete", nil, cg)
	waitGone := time.Now().Add(time.Minute)
	for find("compute-consistencygroups", cg) != nil {
		if time.Now().After(waitGone) {
			t.Fatal("the consistency group was never deleted")
		}
		time.Sleep(2 * time.Second)
	}

	// Delete the VM, then the disk from the list's row menu.
	bb, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-b", vm}, "Action": "delete"})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/compute-instances"+q, string(bb)); code != http.StatusOK {
		t.Fatalf("delete the VM = %d: %s", code, body)
	}
	waitRow("compute-disks", disk, func(r *row) bool { return r.Fields["In use by"] == "-" || r.Fields["In use by"] == "—" }, 3*time.Minute)
	p.navigate("/compute/disks" + q)
	vmRowMenu(p, disk, "Delete", nil, disk)
	deadline := time.Now().Add(2 * time.Minute)
	for find("compute-disks", disk) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the regional disk was never deleted")
		}
		time.Sleep(2 * time.Second)
	}
}
