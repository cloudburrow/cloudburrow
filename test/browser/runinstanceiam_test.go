//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// instancePermissionsOf reads a Cloud Run instance's Permissions tab
// through the console API: each principal and its role.
func instancePermissionsOf(t *testing.T, project, id string) map[string]string {
	t.Helper()
	code, body := consoleDo(t, http.MethodGet, "/api/detail/run-instances?"+url.Values{"project": {project}, "name": {id}}.Encode(), "")
	var d struct {
		Sections []struct {
			ID          string
			Permissions *struct {
				Bindings []struct {
					Role    string
					Members []string
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
		t.Fatalf("read the instance through the console API = %d (%v): %s", code, err, body)
	}
	for _, s := range d.Sections {
		if s.ID == "permissions" && s.Permissions != nil {
			rows := map[string]string{}
			for _, b := range s.Permissions.Bindings {
				for _, m := range b.Members {
					rows[m] = b.Role
				}
			}
			return rows
		}
	}
	t.Fatalf("the instance's page has no Permissions tab: %s", body)
	return nil
}

// TestCloudRunInstancePermissionsThroughTheTab (#1556), in the run shard: a
// Cloud Run instance's page has a Permissions tab, read and written through
// v1's projects.locations.instances IAM methods since v2's Instances has
// none. It says first that nothing is enforced; Grant access lists the
// principal with its role, as the console API reads it; Test permissions
// answers Held; Remove principal, confirmed by typing the principal back,
// removes it.
func TestCloudRunInstancePermissionsThroughTheTab(t *testing.T) {
	needService(t, "run-instances")
	p := open(t)
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-inst-%d", time.Now().UnixNano()%1e6)
	q := "?project=" + project
	t.Cleanup(func() { consoleDeploy(t, http.MethodDelete, "/api/resources/run-instances"+q+"&name="+id, "") })
	if code, body := consoleDeploy(t, http.MethodPost, "/api/resources/run-instances"+q, fmt.Sprintf(
		`{"name":%q,"image":"docker.io/library/busybox:1.36","command":"sh -c","args":"'exec sleep 3600'"}`, id)); code != http.StatusOK {
		t.Fatalf("create an instance through the console API = %d: %s", code, body)
	}
	const role = "roles/run.invoker"
	ada := "user:ada@example.com"

	openTab := func() {
		p.navigate("/run/instances" + q)
		p.clickText("#view tbody a", id)
		p.clickText("#view .tab-strip .tab", "Permissions")
		p.waitFor(`document.querySelector("#permissions-note") !== null`)
	}
	openTab()
	var note string
	p.eval(`document.querySelector("#permissions-note").textContent`, &note)
	if want := "CloudBurrow stores IAM policies but does not enforce them (ADR-0006)."; len(note) < len(want) || note[:len(want)] != want {
		t.Errorf("the Permissions tab says %q first; want it to say nothing is enforced", note)
	}

	p.clickText("#view #grant-access", "Grant access")
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	p.eval(fmt.Sprintf(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		q("#f-members").value = %q; q("#f-members").dispatchEvent(new Event("input", { bubbles: true }));
		q("#f-role").value = %q; q("#f-role").dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, ada, role), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelectorAll("#permissions-table tbody tr").length === 1`)
	if rows := instancePermissionsOf(t, project, id); rows[ada] != role {
		t.Errorf("after Grant access the console API reads %v", rows)
	}

	p.clickText("#view #test-permissions", "Test permissions")
	p.waitFor(`document.querySelector(".modal.is-open #f-permissions") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-permissions");
		f.value = "run.instances.get,\nrun.instances.delete"; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelectorAll("#test-permissions-table tbody tr").length === 2`)
	var tested string
	p.eval(`[...document.querySelectorAll("#test-permissions-table tbody tr")].map((r) => r.textContent).join("|")`, &tested)
	if want := "run.instances.getHeld|run.instances.deleteHeld"; tested != want {
		t.Errorf("Test permissions reads %q; want %q", tested, want)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)

	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Remove "+ada+" from "+role))))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #confirm-input");
		f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, ada), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelectorAll("#permissions-table tbody tr").length === 0`)
	if rows := instancePermissionsOf(t, project, id); len(rows) != 0 {
		t.Errorf("after Remove principal the console API reads %v", rows)
	}
	if sent := p.posts("/api/permissions/run-instances"); len(sent) != 3 {
		t.Errorf("the grant, the test and the removal sent %d requests, want 3: %v", len(sent), sent)
	}
}
