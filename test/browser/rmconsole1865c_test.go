//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/chromedp/chromedp"
)

// TestResourceSelectorChoosesFoldersAndTheOrganization (#1865): the
// toolbar's picker is Google's Select a resource: All is the organization,
// its folders and the projects under each; a project row selects the
// project, and a folder row opens that folder's page with the project kept.
func TestResourceSelectorChoosesFoldersAndTheOrganization(t *testing.T) {
	needService(t, "projects")
	needService(t, "hierarchy")
	ctx := context.Background()
	opts := rmClientOptions(t)
	folders, err := resourcemanager.NewFoldersClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := resourcemanager.NewOrganizationsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = folders.Close(); _ = orgs.Close() })
	org, err := orgs.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{}).Next()
	if err != nil {
		t.Fatalf("SearchOrganizations: %v", err)
	}
	name := fmt.Sprintf("Picker %d", time.Now().UnixNano()%1e8)
	fop, err := folders.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: org.GetName(), DisplayName: name}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	t.Cleanup(func() {
		if op, err := folders.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	folderID := strings.TrimPrefix(folder.GetName(), "folders/")
	project := uniqueProject(t)

	// From a page that does not work at the folder's level, the folder
	// opens its own page and the project is kept; from IAM it scopes the
	// page (#2023, rmconsole2023_test.go).
	p := open(t)
	p.navigate("/projects?project=" + project)
	p.waitFor(`document.getElementById("project-current").textContent === "` + project + `"`)
	p.run(chromedp.Click(chromedp.CSS(`#project-button`)))
	p.waitFor(`document.querySelector('#project-list li[data-kind="organization"]') !== null`)
	var head struct{ Title, Label string }
	p.eval(`({ Title: document.querySelector("#project-panel .picker-head strong").textContent,
		Label: document.getElementById("project-panel").getAttribute("aria-label") })`, &head)
	if head.Title != "Select a resource" || head.Label != "Select a resource" {
		t.Errorf("the picker is headed %+v; want Google's Select a resource", head)
	}
	row := fmt.Sprintf(`[...document.querySelectorAll('#project-list li[data-kind="folder"] a')].find((a) => a.textContent.includes(%q))`, name)
	p.waitFor(row + ` !== undefined`)
	var kinds []string
	p.eval(`[...new Set([...document.querySelectorAll("#project-list li")].map((l) => l.dataset.kind))]`, &kinds)
	if len(kinds) < 3 {
		t.Errorf("All lists the kinds %q; want the organization, folders and projects", kinds)
	}
	p.eval(row+`.click(), true`, nil)
	p.waitFor(`location.pathname === "/resource-hierarchy/` + folderID + `" && !location.search.includes("folder=")`)
	p.waitFor(`document.getElementById("project-panel").hidden`)
	var current string
	p.eval(`document.getElementById("project-current").textContent`, &current)
	if current != project {
		t.Errorf("after choosing the folder the toolbar names %q; want the project %s kept", current, project)
	}
}

// TestTagKeyPagesAsGooglesThroughTheBrowser (#1865): Create tag key's Tag
// values section is Predefined values with Add value rows (Tag value, Tag
// value description) or Dynamic values (choosable since #2023), ending in
// Create tag key, and the values are made with the key; a key's row menu
// has View details and Manage access (the key's Permissions); its page has
// Edit next to Description, an edit panel ending in Save.
func TestTagKeyPagesAsGooglesThroughTheBrowser(t *testing.T) {
	needService(t, "projects")
	ctx := context.Background()
	opts := rmClientOptions(t)
	keys, err := resourcemanager.NewTagKeysClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	values, err := resourcemanager.NewTagValuesClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := resourcemanager.NewOrganizationsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keys.Close(); _ = values.Close(); _ = orgs.Close() })
	org, err := orgs.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{}).Next()
	if err != nil {
		t.Fatalf("SearchOrganizations: %v", err)
	}
	project := uniqueProject(t)
	short := fmt.Sprintf("tk-%d", time.Now().UnixNano()%1e8)

	p := open(t)
	p.navigate("/iam-admin/tags?project=" + project)
	p.clickText("#view button", "Create tag key")
	p.waitFor(`document.querySelector("#view form.create-page #f-shortName") !== null`)
	var form struct {
		Submit, AddValue string
		DynamicOff       bool
	}
	p.eval(`({ Submit: document.querySelector('#view form.create-page button[type="submit"]').textContent,
		AddValue: ([...document.querySelectorAll("#view form.create-page button")].find((b) => b.textContent === "Add value") || {}).textContent || "",
		DynamicOff: [...document.querySelectorAll("#view form.create-page input[type=radio]")].find((r) => r.value === "dynamic").disabled })`, &form)
	if form.Submit != "Create tag key" || form.AddValue != "Add value" || form.DynamicOff {
		t.Errorf("Create tag key reads %+v; want Add value, Dynamic values choosable, ending in Create tag key", form)
	}
	p.setField("#f-shortName", short)
	p.eval(`[...document.querySelectorAll("#view form.create-page button")].find((b) => b.textContent === "Add value").click(), true`, nil)
	p.waitFor(`document.querySelector('#view form.create-page input[aria-label="Tag value 1"]') !== null`)
	for sel, v := range map[string]string{`input[aria-label="Tag value 1"]`: "prod", `input[aria-label="Tag value description 1"]`: "Serving"} {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector('#view form.create-page %s'); f.value = %q;
			f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, sel, v), nil)
	}
	p.run(chromedp.Click(chromedp.CSS(`#view form.create-page button[type="submit"]`)))
	p.waitFor(`document.querySelector("#view form.create-page") === null`)
	var key *rmpb.TagKey
	for deadline := time.Now().Add(15 * time.Second); key == nil; time.Sleep(250 * time.Millisecond) {
		key, _ = keys.GetNamespacedTagKey(ctx, &rmpb.GetNamespacedTagKeyRequest{Name: strings.TrimPrefix(org.GetName(), "organizations/") + "/" + short})
		if key == nil && time.Now().After(deadline) {
			t.Fatalf("no tag key %s after Create tag key", short)
		}
	}
	t.Cleanup(func() {
		vs := values.ListTagValues(context.Background(), &rmpb.ListTagValuesRequest{Parent: key.GetName()})
		for v, err := vs.Next(); err == nil; v, err = vs.Next() {
			if op, err := values.DeleteTagValue(context.Background(), &rmpb.DeleteTagValueRequest{Name: v.GetName()}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		}
		if op, err := keys.DeleteTagKey(context.Background(), &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	v, err := values.ListTagValues(ctx, &rmpb.ListTagValuesRequest{Parent: key.GetName()}).Next()
	if err != nil || v.GetShortName() != "prod" || v.GetDescription() != "Serving" {
		t.Errorf("the key's value = %v, %v; want prod, Serving", v, err)
	}

	id := strings.TrimPrefix(key.GetName(), "tagKeys/")
	p.navigate("/iam-admin/tags?project=" + project)
	p.tagKeyMenu(id, "Manage access")
	p.waitFor(`location.pathname === "/tags/` + id + `" && document.querySelector('#tab-permissions[aria-selected="true"]') !== null`)
	p.navigate("/iam-admin/tags?project=" + project)
	p.tagKeyMenu(id, "View details")
	p.waitFor(`location.pathname === "/tags/` + id + `" && document.querySelector("#view button.property-edit") !== null`)
	var headerEdit bool
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.startsWith("Edit"))`, &headerEdit)
	if headerEdit {
		t.Error("the key's page has an Edit button by its title; Google's Edit is next to Description")
	}
	p.run(chromedp.Click(chromedp.CSS(`#view button.property-edit`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-description") !== null`)
	var submit string
	p.eval(`document.querySelector('.modal.is-open button[type="submit"]').textContent`, &submit)
	if submit != "Save" {
		t.Errorf("the edit panel ends in %q, want Save", submit)
	}
	p.mrSubmitTop("description", "edited next to Description")
	got, err := keys.GetTagKey(ctx, &rmpb.GetTagKeyRequest{Name: key.GetName()})
	if err != nil || got.GetDescription() != "edited next to Description" {
		t.Errorf("after Edit, Save the key is %v, %v", got, err)
	}
}

// tagKeyMenu chooses item from the Actions menu of a list row whose cell
// reads id: a tag key's on the Tags page, a project's on Projects.
func (p *tab) tagKeyMenu(id, item string) {
	p.t.Helper()
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === %q))`, id)
	p.waitFor(find + ` !== undefined`)
	p.eval(find+`.querySelector(".overflow-trigger").click(), true`, nil)
	p.clickText(".overflow-item", item)
}

// TestProjectPageShutDownAndRestore (#1865): a project's page has no Scope
// tab (the registry's caveats are the Details tab's note); its Delete, in
// the Projects list's row menu, is
// Google's shut down, confirmed by the project ID with Shut down, and a
// project pending deletion has Restore.
func TestProjectPageShutDownAndRestore(t *testing.T) {
	needService(t, "projects")
	ctx := context.Background()
	projects, err := resourcemanager.NewProjectsClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	project := uniqueProject(t)
	state := func() rmpb.Project_State {
		got, err := projects.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + project})
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		return got.GetState()
	}

	p := open(t)
	p.navigate("/projects/" + project + "?project=" + project)
	p.waitFor(`document.querySelector("#tab-details") !== null`)
	var page struct {
		Scope bool
		Note  string
	}
	p.eval(`({ Scope: document.querySelector("#tab-scope") !== null, Note: document.querySelector("#view").textContent })`, &page)
	if page.Scope || !strings.Contains(page.Note, "A local registry") {
		t.Errorf("the project page has a Scope tab (%v), or its Details lack the registry's note", page.Scope)
	}
	p.navigate("/projects?project=" + project)
	p.tagKeyMenu(project, "Delete")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	var dialog struct{ Title, Button string }
	p.eval(`({ Title: document.querySelector(".modal.is-open h2").textContent,
		Button: document.querySelector('.modal.is-open button[type="submit"]').textContent })`, &dialog)
	if dialog.Title != "Shut down "+project+"?" || dialog.Button != "Shut down" {
		t.Errorf("the project's Delete asks %+v; want Shut down %s? and Shut down", dialog, project)
	}
	p.confirmByName(project)
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(15 * time.Second); state() != rmpb.Project_DELETE_REQUESTED; time.Sleep(250 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after Shut down the project is %v", state())
		}
	}
	p.navigate("/projects/" + project + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Restore")`)
	p.clickText("#view .page-actions button", "Restore")
	for deadline := time.Now().Add(15 * time.Second); state() != rmpb.Project_ACTIVE; time.Sleep(250 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after Restore the project is %v", state())
		}
	}
}

// TestResourceManagerPagesOf1865MeetWCAG: the resource selector open, the
// Create tag key page, a tag key's page and Manage resources' Tags panel.
func TestResourceManagerPagesOf1865MeetWCAG(t *testing.T) {
	needService(t, "projects")
	needService(t, "hierarchy")
	project := uniqueProject(t)
	tagsPanel := fmt.Sprintf(`(() => { const b = document.querySelector('#mr-tree tr[data-resource="project:%s"] button[aria-label="Actions for %s"]');
		b.click(); [...document.querySelectorAll('[role="menuitem"]')].find((n) => n.textContent.trim() === "Tags").click(); })()`, project, project)
	auditScreens(t, []a11yPage{
		{"resource selector", "/iam-admin/iam?project=" + project, settled,
			`document.getElementById("project-button").click()`},
		{"create tag key page", "/tags/create?project=" + project,
			`document.querySelector("#view form.create-page #f-shortName") !== null`, ""},
		{"manage resources tags panel", "/cloud-resource-manager?project=" + project,
			fmt.Sprintf(`document.querySelector('#mr-tree tr[data-resource="project:%s"]') !== null`, project), tagsPanel},
		{"project page", "/projects/" + project + "?project=" + project, `document.querySelector("#tab-details") !== null`, ""},
	})
}
