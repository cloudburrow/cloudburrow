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

// TestAlloyDBParityThroughThePage2251 (#2251), in the sql shard, whose
// instance serves AlloyDB. Google's AlloyDB console, round 2026-11-25:
//   - the Clusters list is one table of clusters with Google's columns,
//     each cluster's instances under it behind its arrow;
//   - the cluster's Data protection: Continuous backups and recovery
//     policy's Edit (Recovery window (days)) and Automated backup policy's
//     Edit (Automate backups), each ending in Update, read back from the
//     API;
//   - a Users row's Change password, after which the new password signs
//     in to AlloyDB Studio and the old one is refused, and its Delete;
//   - Upgrade beside the Overview's version: Choose database version,
//     Continue, the cluster ID typed back, Start upgrade; the Overview's
//     Upgrade status, and the cluster reads back on the new version.
//
// Every screen and dialog meets WCAG 2.1 AA (names, contrast in both
// themes, a keyboard Tab walk, reduced motion, reflow), and the keyboard
// subtest opens Data protection's Edit and Upgrade's steps from the
// keyboard alone.
func TestAlloyDBParityThroughThePage2251(t *testing.T) {
	needService(t, "alloydb")
	project := uniqueProject(t)
	detail := func(path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/alloydb?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	cluster := "projects/" + project + "/locations/us-central1/clusters/par-cl"
	ready := func() {
		t.Helper()
		deadline := time.Now().Add(8 * time.Minute)
		for d := detail(cluster); !strings.Contains(d, `"Ready"`) || strings.Contains(d, `"Creating"`) ||
			strings.Contains(d, `"Status":"Maintenance"`) || strings.Contains(d, `{"label":"Status","value":"Maintenance"}`) || strings.Contains(d, `"In progress"`); d = detail(cluster) {
			if time.Now().After(deadline) {
				t.Fatalf("the cluster is not ready: %s", d)
			}
			time.Sleep(2 * time.Second)
		}
	}
	signIn := func(user, password string) (int, string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"instance": cluster, "database": "postgres", "user": user, "password": password})
		code, body := consoleDo(t, http.MethodPost, "/api/studio/alloydb?project="+url.QueryEscape(project)+"&call=signin", string(b))
		var s struct{ Session string }
		if code == http.StatusOK && json.Unmarshal([]byte(body), &s) == nil && s.Session != "" {
			b, _ := json.Marshal(map[string]any{"instance": cluster, "session": s.Session})
			_, _ = consoleDo(t, http.MethodPost, "/api/studio/alloydb?project="+url.QueryEscape(project)+"&call=signout", string(b))
		}
		return code, body
	}

	p := open(t)
	p.navigate("/alloydb/clusters/create?project=" + project)
	p.waitFor(`document.getElementById("f-clusterId") !== null`)
	p.submitForm(map[string]string{"clusterId": "par-cl", "password": "burrow-pw", "version": "POSTGRES_15", "instanceId": "primary"}, "clusterId")
	p.waitFor(listedRow("/alloydb/clusters", "par-cl"))
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{cluster}, "Action": "delete", "Values": map[string]string{}})
		_, _ = consoleDo(t, http.MethodPost, "/api/actions/alloydb?project="+url.QueryEscape(project), string(b))
	})
	ready()

	// The Clusters list: Google's columns, the instance under its cluster.
	p.navigate("/alloydb/clusters?project=" + project)
	p.waitFor(listedRow("/alloydb/clusters", "par-cl"))
	var heads []string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter((h) => h)`, &heads)
	for _, want := range []string{"Resource name", "Resource type", "Location", "Status", "Private IP address", "High availability",
		"Read pool nodes", "Automated backups", "Most recent backup"} {
		if !strings.Contains(strings.Join(heads, "|"), want) {
			t.Errorf("the Clusters list has no %s column: %v", want, heads)
		}
	}
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Primary cluster"))`)
	// The arrow beside the cluster's name shows its instances.
	p.eval(`(() => { const r = [...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("par-cl"));
		const b = r.querySelector("button[aria-expanded]"); if (b && b.getAttribute("aria-expanded") !== "true") b.click(); return true; })()`, nil)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Primary instance") && r.textContent.includes("primary"))`)

	p.follow(cluster)
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	clusterPath = strings.Split(clusterPath, "&tab=")[0]

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		// Data protection's Edit, reached and opened by the keyboard.
		k.navigate(clusterPath + "&tab=backups")
		k.waitFor(`document.querySelector("#view a.property-action") !== null`)
		k.tabTo(`document.activeElement.classList.contains("property-action") && document.activeElement.getAttribute("aria-label").startsWith("Edit continuous")`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open #f-window") !== null`)
		k.waitFor(`document.querySelector(".modal.is-open").contains(document.activeElement)`)
		k.run(chromedp.KeyEvent(kb.Escape))
		k.waitFor(`document.querySelector(".modal.is-open") === null`)
		// Upgrade, from the link beside the version: its first step, then
		// Continue to the second, all by the keyboard.
		k.navigate(clusterPath + "&tab=overview")
		k.waitFor(`[...document.querySelectorAll("#view a.property-action")].some((a) => a.textContent.trim() === "Upgrade")`)
		k.tabTo(`document.activeElement.classList.contains("property-action") && document.activeElement.textContent.trim() === "Upgrade"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open [name=version]") !== null`)
		k.tabTo(`document.activeElement.tagName === "BUTTON" && document.activeElement.textContent.trim() === "Continue"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`(() => { const f = document.querySelector(".modal.is-open #f-confirm"); return f !== null && f.getClientRects().length > 0; })()`)
		k.run(chromedp.KeyEvent(kb.Escape))
		k.waitFor(`document.querySelector(".modal.is-open") === null`)
	})

	// Data protection: the two Edits, each read back.
	openEdit := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view a.property-action")].find((a) => a.getAttribute("aria-label").startsWith(%q)).click()`, label)
	}
	p.navigate(clusterPath + "&tab=backups")
	p.waitFor(`document.querySelector("#view").textContent.includes("Continuous backups and recovery policy")`)
	p.eval(openEdit("Edit continuous"), nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-window") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-window"); f.value = "21"; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button[type=submit]", "Update")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	if got := detail(cluster); !strings.Contains(got, `{"label":"Recovery window (days)","value":"21"}`) {
		t.Errorf("the recovery window does not read back as 21: %s", got)
	}
	p.navigate(clusterPath + "&tab=backups")
	p.waitFor(`document.querySelector("#view a.property-action") !== null`)
	p.eval(openEdit("Edit automated"), nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-automate") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-automate"); f.checked = true; f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button[type=submit]", "Update")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	if got := detail(cluster); !strings.Contains(got, `{"label":"Schedule","value":"every day at 23:00 UTC"}`) ||
		!strings.Contains(got, `{"label":"Retention","value":"14 days"}`) {
		t.Errorf("the automated backup policy does not read back: %s", got)
	}
	ready()

	// Users: Add user account from the tab, then the row's Change password.
	// On its tab: walking the tabs would open AlloyDB Studio's sign-in.
	p.navigate(clusterPath + "&tab=users")
	p.waitFor(`document.querySelector("#view .section-actions button") !== null`)
	p.pageAction("Add user account", map[string]string{"user": "par-user", "password": "first-pw"}, "")
	if code, body := signIn("par-user", "first-pw"); code != http.StatusOK {
		t.Fatalf("the new user does not sign in: %d %s", code, body)
	}
	p.navigate(clusterPath)
	p.rowAction("Users", "par-user", "Change password", map[string]string{"password": "second-pw"}, "")
	if code, body := signIn("par-user", "second-pw"); code != http.StatusOK {
		t.Errorf("the changed password does not sign in: %d %s", code, body)
	}
	if code, _ := signIn("par-user", "first-pw"); code == http.StatusOK {
		t.Error("the old password still signs in")
	}

	// The audit, before Upgrade rebuilds the server.
	openRowMenu := func(tab, row, item string) string {
		return fmt.Sprintf(`(() => { [...document.querySelectorAll("#view [role=tab]")].find((t) => t.textContent.trim() === %q).click();
			const r = [...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q));
			r.querySelector(".overflow-trigger").click();
			[...document.querySelectorAll(".overflow-item")].find((b) => b.textContent.trim() === %q).click(); })()`, tab, row, item)
	}
	openUpgrade := `[...document.querySelectorAll("#view a.property-action")].find((a) => a.textContent.trim() === "Upgrade").click()`
	auditScreens(t, []a11yPage{
		screen("AlloyDB clusters", "/alloydb/clusters", project),
		{"Data protection", clusterPath + "&tab=backups", settled, ""},
		{"edit continuous backups dialog", clusterPath + "&tab=backups", settled, openEdit("Edit continuous")},
		{"edit automated backup policy dialog", clusterPath + "&tab=backups", settled, openEdit("Edit automated")},
		{"Users", clusterPath + "&tab=users", settled, ""},
		{"change password dialog", clusterPath + "&tab=users", settled + ` && document.querySelector("#view tbody tr .overflow-trigger") !== null`,
			openRowMenu("Users", "par-user", "Change password")},
		{"upgrade dialog", clusterPath + "&tab=overview", settled, openUpgrade},
	})

	// The row's Delete, confirmed as Google's is.
	p.navigate(clusterPath)
	p.clickText(`#view [role="tab"]`, "Users")
	find := `[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("par-user"))`
	p.waitFor(find + ` !== undefined`)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText(".overflow-item", "Delete")
	p.confirmByName("par-user")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if got := detail(cluster); strings.Contains(got, "par-user") {
		t.Errorf("the deleted user is still listed: %s", got)
	}

	// Upgrade: two steps, the cluster's ID typed back, Start upgrade.
	p.navigate(clusterPath + "&tab=overview")
	p.waitFor(`[...document.querySelectorAll("#view a.property-action")].some((a) => a.textContent.trim() === "Upgrade")`)
	p.eval(openUpgrade, nil)
	p.waitFor(`document.querySelector(".modal.is-open [name=version]") !== null`)
	var versions []string
	p.eval(`[...document.querySelectorAll(".modal.is-open input[type=radio][name=f-version-choice]")].map((r) => r.value)`, &versions)
	if strings.Join(versions, ",") != "POSTGRES_16,POSTGRES_17" {
		t.Errorf("Upgrade offers %v; want only the newer versions", versions)
	}
	p.eval(`(() => { [...document.querySelectorAll(".modal.is-open input[type=radio][name=f-version-choice]")].find((r) => r.value === "POSTGRES_16").click(); return true; })()`, nil)
	var chosen string
	p.eval(`document.querySelector(".modal.is-open [name=version]").value`, &chosen)
	if chosen != "POSTGRES_16" {
		t.Fatalf("choosing PostgreSQL 16 set %q", chosen)
	}
	p.clickText(".modal.is-open button", "Continue")
	p.waitFor(`(() => { const f = document.querySelector(".modal.is-open #f-confirm"); return f !== null && f.getClientRects().length > 0; })()`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-confirm"); f.value = "par-cl"; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button[type=submit]", "Start upgrade")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	p.navigate(clusterPath + "&tab=overview")
	p.waitFor(`document.querySelector("#view").textContent.includes("Upgrade status")`)
	deadline := time.Now().Add(10 * time.Minute)
	for d := detail(cluster); !strings.Contains(d, `{"label":"Upgrade status","value":"Succeeded"}`); d = detail(cluster) {
		if strings.Contains(d, `{"label":"Upgrade status","value":"Failed"}`) || time.Now().After(deadline) {
			t.Fatalf("the upgrade did not succeed: %s", d)
		}
		time.Sleep(3 * time.Second)
	}
	ready()
	if got := detail(cluster); !strings.Contains(got, `"value":"PostgreSQL 16"`) {
		t.Errorf("the upgraded cluster reads back as %s", got)
	}
	if code, body := signIn("postgres", "burrow-pw"); code != http.StatusOK {
		t.Errorf("postgres does not sign in after the upgrade: %d %s", code, body)
	}
}
