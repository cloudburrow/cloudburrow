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

// TestComputeDiskPerformanceKeysAndSchedulesThroughTheBrowser (#2206), in
// a shard whose instance serves the compute service. Create a disk's
// Provisioned IOPS, Provisioned throughput and Access mode, shown only for
// Hyperdisk Balanced High Availability, make a regional volume with them;
// its page shows them, and Edit performance changes them by disks.update;
// Encryption's Cloud KMS key field shows only for Cloud KMS key. Snapshot
// schedules' Create a snapshot schedule makes an hourly schedule, Attach to
// disk adds it to the volume, Edit changes it, Detach from disk and Delete
// remove it. Each change is read back through the console API. Its WCAG
// subtest audits Create a disk with Hyperdisk Balanced High Availability
// and Cloud KMS key chosen, the volume's page, Edit performance's dialog,
// Snapshot schedules, Create a snapshot schedule and a schedule's page; its
// keyboard subtest reaches Disk type's select and Encryption's radios by
// keys alone and opens Edit performance from the keyboard.
func TestComputeDiskPerformanceKeysAndSchedulesThroughTheBrowser(t *testing.T) {
	needService(t, "compute-snapshotschedules")
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
		deadline := time.Now().Add(2 * time.Minute)
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
	const disk, sched = "browser-hdha", "browser-sched"
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1", sched}, "Action": "detach", "Values": map[string]string{"disk": "regions/us-central1/disks/" + disk}})
		consoleDo(t, http.MethodPost, "/api/actions/compute-snapshotschedules"+q, string(b))
		b, _ = json.Marshal(map[string]any{"Path": []string{"us-central1", sched}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-snapshotschedules"+q, string(b))
		b, _ = json.Marshal(map[string]any{"Path": []string{"us-central1", disk}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-disks"+q, string(b))
	})
	chooseRadio := func(p *tab, field, value string) {
		p.t.Helper()
		sel := fmt.Sprintf(`[...document.querySelectorAll('input[type="radio"][name="f-%s-choice"]')].find((r) => r.value === %q)`, field, value)
		p.waitFor(sel + ` !== undefined`)
		p.eval(sel+`.click()`, nil)
	}
	shown := func(p *tab, id string) bool {
		var v bool
		p.eval(fmt.Sprintf(`(() => { const e = document.querySelector("#f-%s"); return !!e && e.getClientRects().length > 0; })()`, id), &v)
		return v
	}

	// Create a disk: the performance fields only for the HA type, the key
	// only for Cloud KMS key.
	p := open(t)
	p.navigate("/compute/disks/create" + q)
	p.waitFor(`document.querySelector("#f-name") !== null`)
	if shown(p, "provisionedIops") || shown(p, "accessMode") || shown(p, "kmsKeyName") {
		t.Error("Provisioned IOPS, Access mode or Cloud KMS key shown for a pd-balanced disk with Google-managed encryption")
	}
	chooseRadio(p, "encryption", "Cloud KMS key")
	p.waitFor(`document.querySelector("#f-kmsKeyName").getClientRects().length > 0`)
	chooseRadio(p, "encryption", "Google-managed encryption key")
	p.waitFor(`document.querySelector("#f-kmsKeyName").getClientRects().length === 0`)
	chooseRadio(p, "location", "Regional")
	p.setField("#f-type", "hyperdisk-balanced-high-availability")
	p.waitFor(`document.querySelector("#f-provisionedIops").getClientRects().length > 0 && document.querySelector("#f-accessMode").getClientRects().length > 0`)
	p.submitForm(map[string]string{"name": disk, "zone": "us-central1-a", "replicaZone": "us-central1-b", "sizeGb": "100",
		"provisionedIops": "6000", "provisionedThroughput": "400", "accessMode": "READ_WRITE_MANY"}, "name")
	p.waitFor(`location.pathname === "/compute/disks"`)
	waitRow("compute-disks", disk, func(r *row) bool { return r.Fields["Size"] == "100 GB" })

	// Its page, then Edit performance.
	p.navigate("/compute/disks/us-central1/" + disk + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Provisioned IOPS")`)
	var page string
	p.eval(`document.querySelector("#view").textContent`, &page)
	if !strings.Contains(page, "6000") || !strings.Contains(page, "400 MiB/s") || !strings.Contains(page, "Multiple VMs read-write") {
		t.Errorf("the volume's page: %.800s", page)
	}
	p.pageAction("Edit performance", map[string]string{"provisionedIops": "8000", "provisionedThroughput": "500", "accessMode": "READ_WRITE_SINGLE"}, "")
	deadline := time.Now().Add(time.Minute)
	for {
		code, body := consoleDo(t, http.MethodGet, "/api/detail/compute-disks"+q+"&name=us-central1&name="+disk, "")
		if code == http.StatusOK && strings.Contains(body, "8000") && strings.Contains(body, "500 MiB/s") && strings.Contains(body, "Single VM read-write") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Edit performance: %d %.800s", code, body)
		}
		time.Sleep(2 * time.Second)
	}

	// Snapshot schedules: create, attach, edit.
	p.navigate("/compute/snapshotSchedules" + q)
	p.clickText("#view button", "Create snapshot schedule")
	p.waitFor(`document.querySelector("#f-name") !== null`)
	chooseRadio(p, "frequency", "Hourly")
	p.waitFor(`document.querySelector("#f-hours").getClientRects().length > 0`)
	p.submitForm(map[string]string{"name": sched, "region": "us-central1", "hours": "6", "startTime": "02:00", "retentionDays": "7"}, "name")
	waitRow("compute-snapshotschedules", sched, func(r *row) bool { return r.Fields["Frequency"] == "Every 6 hours from 02:00 UTC" })
	p.navigate("/compute/snapshotSchedules/us-central1/" + sched + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Auto-delete snapshots after")`)
	p.pageAction("Attach to disk", map[string]string{"disk": "regions/us-central1/disks/" + disk}, "")
	waitRow("compute-snapshotschedules", sched, func(r *row) bool { return r.Fields["Disks"] == "1" })
	p.navigate("/compute/snapshotSchedules/us-central1/" + sched + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Auto-delete snapshots after")`)
	p.pageAction("Edit", map[string]string{"retentionDays": "30"}, "")
	waitRow("compute-snapshotschedules", sched, func(r *row) bool { return r.Fields["Auto-delete snapshots after"] == "30 days" })

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"Create a disk, Hyperdisk Balanced High Availability and Cloud KMS key", "/compute/disks/create" + q, `document.querySelector("#f-type") !== null`,
				`(() => { const s = document.querySelector("#f-type"); s.value = "hyperdisk-balanced-high-availability"; s.dispatchEvent(new Event("change", { bubbles: true }));
				[...document.querySelectorAll('input[name="f-encryption-choice"]')].find((r) => r.value === "Cloud KMS key").click(); })()`},
			{"the volume's page", "/compute/disks/us-central1/" + disk + q, `document.querySelector("#view").textContent.includes("Provisioned IOPS")`, ""},
			{"Edit performance", "/compute/disks/us-central1/" + disk + q, `document.querySelector("#view").textContent.includes("Provisioned IOPS")`,
				`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent.trim() === "Edit performance").click()`},
			{"Snapshot schedules", "/compute/snapshotSchedules" + q, rowReady(sched), ""},
			{"Create a snapshot schedule", "/compute/snapshotSchedules/create" + q, `document.querySelector("#f-name") !== null`, ""},
			{"a snapshot schedule's page", "/compute/snapshotSchedules/us-central1/" + sched + q,
				`document.querySelector("#view").textContent.includes("Auto-delete snapshots after")`, ""},
		})
	})
	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/compute/disks/create" + q)
		k.waitFor(`document.querySelector("#f-name") !== null`)
		k.tabTo(`document.activeElement.id === "f-type"`)
		k.tabTo(`document.activeElement.name === "f-encryption-choice"`)
		k.run(chromedp.KeyEvent(kb.ArrowDown))
		k.waitFor(`document.querySelector("#f-encryption").value === "Cloud KMS key" && document.querySelector("#f-kmsKeyName").getClientRects().length > 0`)
		k.navigate("/compute/disks/us-central1/" + disk + q)
		k.waitFor(`document.querySelector("#view").textContent.includes("Provisioned IOPS")`)
		k.tabTo(`document.activeElement.tagName === "BUTTON" && document.activeElement.textContent.trim() === "Edit performance"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open #f-provisionedIops") !== null`)
	})

	// Detach, then Delete from the list's row menu.
	p.navigate("/compute/snapshotSchedules/us-central1/" + sched + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Auto-delete snapshots after")`)
	p.pageAction("Detach from disk", map[string]string{"disk": "regions/us-central1/disks/" + disk}, "")
	waitRow("compute-snapshotschedules", sched, func(r *row) bool { return r.Fields["Disks"] == "0" })
	p.navigate("/compute/snapshotSchedules" + q)
	vmRowMenu(p, sched, "Delete", nil, sched)
	gone := time.Now().Add(time.Minute)
	for find("compute-snapshotschedules", sched) != nil {
		if time.Now().After(gone) {
			t.Fatal("the snapshot schedule was never deleted")
		}
		time.Sleep(2 * time.Second)
	}
}
