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

// TestComputeEngineThroughTheBrowser (#1965), in a shard whose instance
// serves the compute service. From the drawer's Compute row, Compute
// Engine's VM instances: Create instance on Google's Create an instance
// page (machine type, boot image, a startup script and a metadata item),
// the instance RUNNING in the list with its zone, internal IP, natIP and
// SSH port; the row's SSH button answering with the gcloud compute ssh
// command for the port; the instance's page with its tabs, the startup
// script's line on Serial port 1; Disks' Create a disk page; Attach
// existing disk on the instance's page; Create snapshot on the disk's row;
// Stop and Start / Resume from the row's menu; Delete, the boot disk going
// with it; then the disk and snapshot deleted from their pages' menus.
// Each change is read back through the console API.
func TestComputeEngineThroughTheBrowser(t *testing.T) {
	needService(t, "compute-instances")
	project := uniqueProject(t)
	type row struct {
		Name   string
		Status string
		Fields map[string]string
	}
	rows := func(service string) []row {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+service+"?project="+project, "")
		if code != http.StatusOK {
			t.Fatalf("list %s through the console API = %d: %s", service, code, body)
		}
		var l struct{ Items []row }
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		return l.Items
	}
	find := func(service, name string) *row {
		for _, r := range rows(service) {
			if r.Name == name {
				return &r
			}
		}
		return nil
	}
	// waitStatus waits, through the console API, for a long operation.
	waitStatus := func(service, name, status string, d time.Duration) *row {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			r := find(service, name)
			if r != nil && r.Status == status {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s never became %s: %+v", service, name, status, r)
			}
			time.Sleep(2 * time.Second)
		}
	}
	rowWith := func(text string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, text)
	}
	const vm, disk, snap = "browser-vm", "browser-data", "browser-snap"
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-a", vm}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instances?project="+project, string(b))
		time.Sleep(5 * time.Second)
		for _, d := range []string{disk} {
			b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-a", d}, "Action": "delete"})
			consoleDo(t, http.MethodPost, "/api/actions/compute-disks?project="+project, string(b))
		}
		consoleDo(t, http.MethodDelete, "/api/resources/compute-snapshots?project="+project+"&name="+snap, "")
	})

	p := open(t)
	p.navigateByDrawer("Compute", "/compute/instances", "Compute Engine")
	p.navigate("/compute/instances?project=" + project)
	p.clickText("#view button", "Create instance")
	p.submitForm(map[string]string{"name": vm, "machineType": "e2-small", "image": "debian-cloud/debian-12",
		"startupScript": "#!/bin/bash\necho BROWSER-MARK-$(hostname)"}, "name")
	p.waitFor(`location.pathname === "/compute/instances" && ` + rowWith(vm))
	r := waitStatus("compute-instances", vm, "RUNNING", 10*time.Minute)
	if r.Fields["External IP"] != "127.0.0.1" || !strings.HasPrefix(r.Fields["Internal IP"], "10.128.") || !strings.HasPrefix(r.Fields["Connect"], "SSH (port ") {
		t.Fatalf("the running instance's row: %+v", r)
	}

	// SSH on the row: the commands for the forwarded port.
	p.navigate("/compute/instances?project=" + project)
	p.waitFor(rowWith("SSH (port"))
	p.clickText("#view tbody button", "SSH")
	p.waitFor(`document.querySelector("#action-result-table") !== null`)
	var cmds string
	p.eval(`document.querySelector("#action-result-table").textContent`, &cmds)
	if !strings.Contains(cmds, "gcloud compute ssh "+vm) || !strings.Contains(cmds, `--ssh-flag="-p `) {
		t.Errorf("SSH answered %q", cmds)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)

	// The instance's page: its tabs, the serial port.
	p.follow(vm)
	p.waitFor(`location.pathname.endsWith("` + vm + `")`)
	for _, tab := range []string{"Details", "Storage", "Networking", "Serial port 1 (console)"} {
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll('#view [role="tab"]')].some((t) => t.textContent.trim() === %q)`, tab))
	}
	var instPage string
	p.eval(`location.pathname + location.search`, &instPage)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		code, body := consoleDo(t, http.MethodGet, "/api/detail/compute-instances?project="+project+"&name=us-central1-a&name="+vm, "")
		if code == http.StatusOK && strings.Contains(body, "BROWSER-MARK-"+vm) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the serial port never showed the startup script's line: %.2000s", body)
		}
		time.Sleep(3 * time.Second)
	}
	p.navigate(instPage)
	p.clickText(`#view [role="tab"]`, "Serial port 1 (console)")
	p.waitFor(`document.querySelector("#view").textContent.includes("BROWSER-MARK-` + vm + `")`)

	// Disks: Create a disk, then attach it on the instance's page.
	p.navigate("/compute/disks?project=" + project)
	p.clickText("#view button", "Create disk")
	p.submitForm(map[string]string{"name": disk, "type": "pd-ssd", "sizeGb": "12"}, "name")
	p.waitFor(`location.pathname === "/compute/disks" && ` + rowWith(disk))
	waitStatus("compute-disks", disk, "READY", 2*time.Minute)
	p.navigate(instPage)
	p.pageAction("Attach existing disk", map[string]string{"disk": disk, "deviceName": "data"}, "")
	deadline = time.Now().Add(2 * time.Minute)
	for find("compute-disks", disk).Fields["In use by"] != vm {
		if time.Now().After(deadline) {
			t.Fatalf("the disk was never attached: %+v", find("compute-disks", disk))
		}
		time.Sleep(2 * time.Second)
	}

	// Stop and Start / Resume from the list's row menu.
	p.navigate("/compute/instances?project=" + project)
	vmRowMenu(p, vm, "Stop", nil, vm)
	waitStatus("compute-instances", vm, "TERMINATED", 3*time.Minute)
	p.navigate("/compute/instances?project=" + project)
	vmRowMenu(p, vm, "Start / Resume", nil, "")
	waitStatus("compute-instances", vm, "RUNNING", 5*time.Minute)

	// Delete; the boot disk goes with it, the data disk stays.
	p.navigate("/compute/instances?project=" + project)
	vmRowMenu(p, vm, "Delete", nil, vm)
	deadline = time.Now().Add(3 * time.Minute)
	for find("compute-instances", vm) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the instance was never deleted")
		}
		time.Sleep(2 * time.Second)
	}
	if find("compute-disks", vm) != nil {
		t.Error("the auto-delete boot disk outlived its instance")
	}
	waitStatus("compute-disks", disk, "READY", time.Minute)
	p.navigate("/compute/disks?project=" + project)
	vmRowMenu(p, disk, "Create snapshot", map[string]string{"name": snap}, "")
	waitStatus("compute-snapshots", snap, "READY", 2*time.Minute)
	p.navigate("/compute/snapshots?project=" + project)
	p.waitFor(rowWith(snap))
	vmRowMenu(p, snap, "Delete", nil, snap)
	p.navigate("/compute/disks?project=" + project)
	vmRowMenu(p, disk, "Delete", nil, disk)
	deadline = time.Now().Add(2 * time.Minute)
	for find("compute-disks", disk) != nil || find("compute-snapshots", snap) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the disk or snapshot was never deleted")
		}
		time.Sleep(2 * time.Second)
	}
}

// TestComputeEngineScreensMeetWCAG (#1965): Compute Engine's lists, the
// Create an instance and Create a disk pages, Snapshots, Images, Machine
// types and Metadata, in the compute shard's browser run.
func TestComputeEngineScreensMeetWCAG(t *testing.T) {
	needService(t, "compute-instances")
	project := servedProject(t)
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"VM instances", "/compute/instances"}, {"Disks", "/compute/disks"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	pages = append(pages,
		screen("Snapshots", "/compute/snapshots", project),
		a11yPage{"Images", "/compute/images?project=" + url.QueryEscape(project), rowReady("debian-12"), ""},
		a11yPage{"Machine types", "/compute/machineTypes?project=" + url.QueryEscape(project), rowReady("e2-medium"), ""},
		screen("Metadata", "/compute/metadata", project),
	)
	auditScreens(t, pages)
}

// vmRowMenu chooses an item of a list row's menu, fills its fields,
// confirms it by name when asked, and waits for its dialog to close.
func vmRowMenu(p *tab, row, label string, values map[string]string, confirm string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === %q))`, row)
	p.waitFor(find + ` !== undefined`)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText(".overflow-item", label)
	if len(values) > 0 {
		p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
		for id, v := range values {
			p.setField(".modal.is-open #f-"+id, v)
		}
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	}
	if confirm != "" {
		p.confirmByName(confirm)
	}
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
}
