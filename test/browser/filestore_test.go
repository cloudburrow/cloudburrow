//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestFilestoreThroughThePage (#2192), in the served shard, whose instance
// serves Filestore. Reached from Filestore's menu under Storage: Create an
// instance on one page (a Zonal NFSv4.1 instance, its real NFS server
// started), the instance's page with its mount command, Create backup from
// the instance's page, the Backups page listing it and the backup's
// Restore form, and the Snapshots page. The console API reads each result
// back. Every screen meets WCAG 2.1 AA and is walked by keyboard.
func TestFilestoreThroughThePage(t *testing.T) {
	needService(t, "filestore")
	project := uniqueProject(t)
	detail := func(provider string, path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/"+provider+"?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	const inst = "us-central1-a/browser-fs"
	const backup = "us-central1/browser-fs-backup"
	t.Cleanup(func() {
		q := "?project=" + url.QueryEscape(project)
		consoleDo(t, http.MethodDelete, "/api/resources/filestore-backups"+q+"&name="+url.QueryEscape(backup), "")
		consoleDo(t, http.MethodDelete, "/api/resources/filestore"+q+"&name="+url.QueryEscape(inst), "")
	})
	p := open(t)
	p.navigateByDrawer("Storage", "/filestore/instances", "Filestore")
	p.waitFor(`[...document.querySelectorAll('a[href^="/filestore/backups"]')].some((a) => a.textContent.includes("Backups"))`)

	p.navigate("/filestore/instances?project=" + project)
	p.clickText("#view button, #view a", "Create instance")
	p.waitFor(`["f-instanceId", "f-tier", "f-capacityGb", "f-shareName", "f-tags"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"instanceId": "browser-fs", "tier": "ZONAL", "protocol": "NFS_V4_1"}, "instanceId")
	p.waitForWithin(`location.pathname === "/filestore/instances" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser-fs"))`, 5*time.Minute)
	if got := detail("filestore", inst); !strings.Contains(got, "Zonal") || !strings.Contains(got, "NFSv4.1") {
		t.Errorf("the instance reads back as %s", got)
	}

	find := `[...document.querySelectorAll("#view tbody a")].find((a) => a.textContent.includes("browser-fs") && a.getClientRects().length)`
	p.waitFor(find + ` !== undefined`)
	var href string
	p.eval(find+`.getAttribute("href")`, &href)
	p.navigate(href)
	p.waitFor(`document.querySelector("#view").textContent.includes("mount")`)
	var instPath string
	p.eval(`location.pathname + location.search`, &instPath)

	p.clickText("#view button", "Create backup")
	p.submitForm(map[string]string{"backupId": "browser-fs-backup"}, "backupId")
	p.waitForWithin(`document.querySelector(".modal") === null`, 5*time.Minute)
	if got := detail("filestore-backups", backup); !strings.Contains(got, "browser-fs") {
		t.Errorf("the backup reads back as %s", got)
	}

	p.navigate("/filestore/backups?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser-fs-backup"))`)
	bfind := `[...document.querySelectorAll("#view tbody a")].find((a) => a.textContent.includes("browser-fs-backup") && a.getClientRects().length)`
	p.eval(bfind+`.getAttribute("href")`, &href)
	p.navigate(href)
	var backupPath string
	p.eval(`location.pathname + location.search`, &backupPath)
	p.clickText("#view button", "Restore")
	p.waitFor(`["f-instanceId", "f-capacityGb"].every((id) => document.getElementById(id) !== null)`)

	auditScreens(t, []a11yPage{
		screen("Filestore instances", "/filestore/instances", project),
		screen("create a Filestore instance", "/filestore/instances/create", project),
		{"Filestore instance", instPath, settled, ""},
		screen("Filestore backups", "/filestore/backups", project),
		{"Filestore backup", backupPath, settled, ""},
		screen("Filestore snapshots", "/filestore/snapshots", project),
	})
}

// TestFilestoreMultishareThroughThePage (#2194): Create an instance with
// Enterprise's "Enable multishares", the instance's page with its
// Performance limits and its Shares, Create share from the page, the share
// listed, and Delete share. The console API reads each result back. Every
// screen meets WCAG 2.1 AA and is walked by keyboard.
func TestFilestoreMultishareThroughThePage(t *testing.T) {
	needService(t, "filestore")
	project := uniqueProject(t)
	detail := func(path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/filestore?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	const inst = "us-central1/browser-ms"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/filestore?project="+url.QueryEscape(project)+"&name="+url.QueryEscape(inst), "")
	})
	p := openWithin(t, 10*time.Minute)
	p.navigate("/filestore/instances?project=" + project)
	p.clickText("#view button, #view a", "Create instance")
	p.waitFor(`document.getElementById("f-tier") !== null`)
	p.submitForm(map[string]string{"instanceId": "browser-ms", "tier": "ENTERPRISE", "multiShare": "true"}, "instanceId")
	p.waitForWithin(`location.pathname === "/filestore/instances" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser-ms") && r.textContent.includes("Multishare"))`, 5*time.Minute)
	if got := detail(inst); !strings.Contains(got, "Multishares") || !strings.Contains(got, "Max read IOPS") || !strings.Contains(got, "12000") {
		t.Fatalf("the instance reads back as %s", got)
	}
	find := `[...document.querySelectorAll("#view tbody a")].find((a) => a.textContent.includes("browser-ms") && a.getClientRects().length)`
	p.waitFor(find + ` !== undefined`)
	var href string
	p.eval(find+`.getAttribute("href")`, &href)
	p.navigate(href)
	p.waitFor(`document.querySelector("#view").textContent.includes("Max write throughput")`)
	var instPath string
	p.eval(`location.pathname + location.search`, &instPath)

	p.clickText("#view button", "Create share")
	p.submitForm(map[string]string{"shareId": "web-share", "mountName": "web_share", "capacityGb": "50"}, "shareId")
	p.waitForWithin(`document.querySelector(".modal") === null`, 5*time.Minute)
	if got := detail(inst); !strings.Contains(got, "web-share") || !strings.Contains(got, "web_share") {
		t.Fatalf("the share reads back as %s", got)
	}
	p.navigate(instPath)
	p.clickText("#view [role=tab]", "Shares")
	p.waitFor(`document.querySelector("#view").textContent.includes("web_share")`)
	auditScreens(t, []a11yPage{
		screen("create a Filestore instance", "/filestore/instances/create", project),
		{"Filestore multishare instance", instPath, settled, ""},
	})

	p.navigate(instPath)
	p.clickText("#view button", "Delete share")
	p.submitForm(map[string]string{"share": "web-share"}, "share")
	p.confirmOnlyDialogLabelled("Delete share")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open form[data-confirm] .modal-actions button.danger`)))
	p.waitForWithin(`document.querySelector(".modal") === null`, 5*time.Minute)
	if got := detail(inst); strings.Contains(got, "web_share") {
		t.Errorf("the share is still there: %s", got)
	}
}
