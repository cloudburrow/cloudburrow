//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// permissionsOf reads a queue's Permissions tab through the console API.
func permissionsOf(t *testing.T, project, queue string) (etag string, rows map[string]string) {
	t.Helper()
	code, body := consoleDo(t, http.MethodGet, "/api/detail/tasks?project="+project+"&name="+url.QueryEscape(queue), "")
	var d struct {
		Sections []struct {
			ID          string
			Permissions *struct {
				Etag     string
				Bindings []struct {
					Role    string
					Members []string
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
		t.Fatalf("read the queue through the console API = %d (%v): %s", code, err, body)
	}
	rows = map[string]string{}
	for _, s := range d.Sections {
		if s.ID == "permissions" && s.Permissions != nil {
			for _, b := range s.Permissions.Bindings {
				for _, m := range b.Members {
					rows[m] = b.Role
				}
			}
			return s.Permissions.Etag, rows
		}
	}
	t.Fatalf("the queue's page has no Permissions tab: %s", body)
	return "", nil
}

// TestTasksPermissionsThroughTheTab (#793), in the storage shard: a queue's
// page has a Permissions tab that says first that nothing is enforced. Grant
// access opens a form whose Cancel closes it at once with nothing sent; saved,
// its principals are listed with the role. A change made through the API
// since the page read the policy makes Remove principal refused on the
// confirmation with the service's own message; after a reload the removal,
// confirmed by typing the principal back, is what the console API reads.
func TestTasksPermissionsThroughTheTab(t *testing.T) {
	needService(t, "tasks")
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/locations/us-central1/queues/browser-perms"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/tasks?project="+project, `{"name":"browser-perms","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a queue through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/tasks?project="+project+"&name="+url.QueryEscape(name), "")
	})
	const role = "roles/cloudtasks.enqueuer"
	ada, svc := "user:ada@example.com", "serviceAccount:svc@example.iam.gserviceaccount.com"

	openTab := func() {
		p.navigate("/tasks/queues?project=" + project)
		p.clickText("#view tbody a", name)
		p.clickText("#view .tab-strip .tab", "Permissions")
		p.waitFor(`document.querySelector("#permissions-note") !== null`)
	}
	openTab()
	var note string
	p.eval(`document.querySelector("#permissions-note").textContent`, &note)
	if want := "CloudBurrow stores IAM policies but does not enforce them (ADR-0006). Every caller can still do everything"; len(note) < len(want) || note[:len(want)] != want {
		t.Errorf("the Permissions tab says %q first; want it to say nothing is enforced", note)
	}

	fill := func(members, role string) {
		p.eval(fmt.Sprintf(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
			q("#f-members").value = %q; q("#f-members").dispatchEvent(new Event("input", { bubbles: true }));
			q("#f-role").value = %q; q("#f-role").dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, members, role), nil)
	}
	openGrant := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view #grant-access", "Grant access")
		p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	}

	// Cancel discards without asking (#783's rule).
	openGrant()
	fill(ada, role)
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.posts("/api/permissions/tasks"); len(sent) != 0 {
		t.Fatalf("Cancel sent the grant: %v", sent)
	}

	openGrant()
	fill(ada+",\n"+svc, role)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelectorAll("#permissions-table tbody tr").length === 2`)
	if _, rows := permissionsOf(t, project, name); rows[ada] != role || rows[svc] != role {
		t.Errorf("after Grant access the console API reads %v", rows)
	}

	// A change through the API since the page was drawn.
	etag, _ := permissionsOf(t, project, name)
	change, _ := json.Marshal(map[string]any{"Path": []string{name}, "Etag": etag,
		"Grant": map[string]any{"Members": []string{"group:other@example.com"}, "Role": "roles/viewer"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/permissions/tasks?project="+project, string(change)); code != http.StatusOK {
		t.Fatalf("a grant through the console API = %d: %s", code, body)
	}
	remove := fmt.Sprintf(`button[aria-label=%q]`, "Remove "+ada+" from "+role)
	confirmRemove := func() {
		p.run(chromedp.Click(remove, chromedp.ByQuery))
		p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #confirm-input");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, ada), nil)
		p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	}
	confirmRemove()
	p.waitFor(`!document.querySelector(".modal.is-open .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal.is-open .form-error").textContent`, &refusal)
	if want := "Aborted: the policy's etag does not match the current policy; read it again and retry"; refusal != want {
		t.Errorf("a removal with a stale etag was refused with %q, want the service's %q", refusal, want)
	}
	p.forgive()
	if _, rows := permissionsOf(t, project, name); rows[ada] != role {
		t.Errorf("a refused removal changed the policy: %v", rows)
	}
	p.clickText(".modal.is-open .modal-actions button", "Cancel")

	openTab()
	p.waitFor(`document.querySelectorAll("#permissions-table tbody tr").length === 3`)
	confirmRemove()
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelectorAll("#permissions-table tbody tr").length === 2`)
	if _, rows := permissionsOf(t, project, name); rows[ada] != "" || rows[svc] != role || rows["group:other@example.com"] != "roles/viewer" {
		t.Errorf("after Remove principal the console API reads %v", rows)
	}
	if sent := p.posts("/api/permissions/tasks"); len(sent) != 3 {
		t.Errorf("the grant, the refused and the confirmed removal sent %d requests, want 3: %v", len(sent), sent)
	}
}

// folderPermissionsOf reads a storage folder page's Permissions tab through
// the console API, and whether the page has one.
func folderPermissionsOf(t *testing.T, project string, path ...string) (rows map[string]string, ok bool) {
	t.Helper()
	v := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
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
		t.Fatalf("read %v through the console API = %d (%v): %s", path, code, err, body)
	}
	rows = map[string]string{}
	for _, s := range d.Sections {
		if s.ID == "permissions" && s.Permissions != nil {
			for _, b := range s.Permissions.Bindings {
				for _, m := range b.Members {
					rows[m] = b.Role
				}
			}
			return rows, true
		}
	}
	return rows, false
}

// TestStorageManagedFolderPermissionsThroughTheTab (#847), in the storage
// shard: a managed folder's row in the bucket browser opens its folder page,
// which says it is a managed folder and has a Permissions tab that says first
// that nothing is enforced. Grant access, saved, lists the principal, which
// the console API then reads on the managed folder and not on the bucket;
// Remove principal, confirmed by typing the principal back, empties it.
func TestStorageManagedFolderPermissionsThroughTheTab(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-mfperms"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage?project="+project, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		del, _ := json.Marshal(map[string]any{"Path": []string{"_managedfolder", bucket, "team/"}, "Action": "deletemanagedfolder"})
		consoleDo(t, http.MethodPost, "/api/actions/storage?project="+project, string(del))
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})
	// Edit bucket, as its form submits it: every value it is prefilled with,
	// and uniform bucket-level access on, which a managed folder requires.
	v := url.Values{"project": {project}, "name": {bucket}}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
	var d struct {
		Edit struct {
			Fields []struct {
				Name, Default string
				Immutable     bool
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
		t.Fatalf("read %s through the console API = %d (%v): %s", bucket, code, err, body)
	}
	values := map[string]string{}
	for _, f := range d.Edit.Fields {
		if !f.Immutable {
			values[f.Name] = f.Default
		}
	}
	values["uniformAccess"] = "true"
	patch, _ := json.Marshal(map[string]any{"Path": []string{bucket}, "Values": values})
	if code, body := consoleDo(t, http.MethodPatch, "/api/resources/storage?project="+project, string(patch)); code != http.StatusOK {
		t.Fatalf("turn on uniform bucket-level access through Edit bucket = %d: %s", code, body)
	}
	create, _ := json.Marshal(map[string]any{"Path": []string{bucket}, "Action": "createmanagedfolder",
		"Values": map[string]string{"name": "team"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/storage?project="+project, string(create)); code != http.StatusOK {
		t.Fatalf("create a managed folder through the console API = %d: %s", code, body)
	}
	const role = "roles/storage.objectViewer"
	ada := "user:ada@example.com"

	p.navigate("/storage/browser/" + bucket + "?project=" + project)
	p.clickText("#view tbody a", "team/")
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Permissions")`)
	var page string
	p.eval(`document.querySelector("#view").textContent`, &page)
	if !strings.Contains(page, "Managed folder") {
		t.Errorf("the managed folder's page does not say it is one: %q", page)
	}
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#permissions-note") !== null`)
	var note string
	p.eval(`document.querySelector("#permissions-note").textContent`, &note)
	if !strings.Contains(note, "does not enforce them (ADR-0006)") {
		t.Errorf("the Permissions tab says %q first; want it to say nothing is enforced", note)
	}

	p.clickText("#view #grant-access", "Grant access")
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	p.eval(fmt.Sprintf(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		q("#f-members").value = %q; q("#f-members").dispatchEvent(new Event("input", { bubbles: true }));
		q("#f-role").value = %q; q("#f-role").dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, ada, role), nil)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelectorAll("#permissions-table tbody tr").length === 1`)
	if rows, _ := folderPermissionsOf(t, project, bucket, "team"); rows[ada] != role {
		t.Errorf("after Grant access the managed folder's policy reads %v", rows)
	}
	if rows, ok := folderPermissionsOf(t, project, bucket); !ok || len(rows) != 0 {
		t.Errorf("the bucket's policy took the managed folder's grant: %v (tab %v)", rows, ok)
	}

	p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Remove "+ada+" from "+role), chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #confirm-input");
		f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, ada), nil)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelector("#permissions-table") === null`)
	if rows, _ := folderPermissionsOf(t, project, bucket, "team"); len(rows) != 0 {
		t.Errorf("after Remove principal the managed folder's policy reads %v", rows)
	}
	if sent := p.posts("/api/permissions/storage"); len(sent) != 2 {
		t.Errorf("the grant and the removal sent %d requests, want 2: %v", len(sent), sent)
	}
}
