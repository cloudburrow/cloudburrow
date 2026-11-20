//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestCreateFlowsArePagesWhereGooglesAre (#1932): the Projects list's
// Create project opens the New project page, /projectcreate, as the project
// picker's New project and Manage resources' Create project do, rather than
// a dialog of its own; its old create address leads there too. Cloud
// Logging's Create log scope is a page, as Google's is
// (docs.cloud.google.com/logging/docs/log-scope/create-and-manage), ending
// in Create log scope. Create folder is a page too, on the Resource
// hierarchy list and on Manage resources (whose Create project is the New
// project page), one form ending in Create, and from Manage resources it
// comes back there (docs.cloud.google.com/resource-manager/docs/creating-managing-folders).
func TestCreateFlowsArePagesWhereGooglesAre(t *testing.T) {
	p := open(t)
	project := uniqueProject(t)
	create := `#view .page-header-actions button.header-button`

	p.navigate("/projects?project=" + project)
	p.waitFor(`[...document.querySelectorAll("` + create + `")].some((b) => b.textContent === "Create project")`)
	p.clickText(create, "Create project")
	p.waitFor(`location.pathname === "/projectcreate" && document.querySelector("#new-project-name") !== null`)
	var modal bool
	p.eval(`document.querySelector(".modal.is-open") !== null`, &modal)
	if modal {
		t.Error("Create project opened a dialog as well as the New project page")
	}
	p.navigate("/projects/create?project=" + project)
	p.waitFor(`location.pathname === "/projectcreate" && document.querySelector("#new-project-name") !== null`)

	needService(t, "hierarchy")
	type createPage struct {
		Modal          bool
		Title, Submit  string
		Fields, Labels []string
		Steps          int
		Cancel         string
	}
	readPage := func() createPage {
		var c createPage
		p.eval(`({ Modal: document.querySelector(".modal.is-open") !== null,
			Title: document.querySelector("#view h1").textContent.trim(),
			Submit: document.querySelector('#view form.create-page button[type="submit"]').textContent,
			Fields: [...document.querySelectorAll("#view form.create-page [id^=f-]")].map((f) => f.id),
			Labels: [...document.querySelectorAll("#view form.create-page select#f-parent option")].map((o) => o.textContent),
			Steps: document.querySelectorAll("#view form.create-page .create-step").length,
			Cancel: new URL(document.querySelector("#view form.create-page a.button.secondary").href).pathname })`, &c)
		return c
	}
	p.navigate("/resource-hierarchy?project=" + project)
	p.waitFor(`[...document.querySelectorAll("` + create + `")].some((b) => b.textContent === "Create folder")`)
	p.clickText(create, "Create folder")
	p.waitFor(`location.pathname === "/resource-hierarchy/create" && document.querySelector("#view form.create-page #f-displayName") !== null`)
	got := readPage()
	if got.Modal || got.Title != "Create folder" || got.Submit != "Create" || got.Steps != 0 || got.Cancel != "/resource-hierarchy" {
		t.Errorf("Resource hierarchy's Create folder opened %+v; want its page headed Create folder, without numbered steps, ending in Create, Cancel to the list", got)
	}
	if !slices.Equal(got.Fields, []string{"f-displayName", "f-parent"}) || len(got.Labels) == 0 || !strings.HasSuffix(got.Labels[0], "(organization)") {
		t.Errorf("Create folder's fields are %q with parents %q; want Folder name and Location, the organization first", got.Fields, got.Labels)
	}

	p.navigate("/cloud-resource-manager?project=" + project)
	p.waitFor(`document.querySelector("#mr-create-folder-button") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#mr-create-folder-button`)))
	p.waitFor(`location.pathname === "/resource-hierarchy/create" && document.querySelector("#view form.create-page #f-displayName") !== null`)
	if got := readPage(); got.Modal || got.Title != "Create folder" || got.Cancel != "/cloud-resource-manager" {
		t.Errorf("Manage resources' Create folder opened %+v; want the Create folder page, Cancel back to Manage resources", got)
	}
	folder := fmt.Sprintf("CF %d", time.Now().UnixNano()%1e8)
	p.submitForm(map[string]string{"displayName": folder}, "displayName")
	p.waitFor(fmt.Sprintf(`location.pathname === "/cloud-resource-manager" &&
		[...document.querySelectorAll("#mr-tree tbody tr")].some((r) => r.textContent.includes(%q))`, folder))
	t.Cleanup(func() { deleteFolderNamed(t, folder) })

	needService(t, "logging-log-scopes")
	p.navigate("/logs/log-scopes?project=" + project)
	p.waitFor(`[...document.querySelectorAll("` + create + `")].some((b) => b.textContent === "Create log scope")`)
	p.clickText(create, "Create log scope")
	p.waitFor(`location.pathname === "/logs/log-scopes/create" && document.querySelector("#view form.create-page #f-id") !== null`)
	var page struct {
		Modal  bool
		Title  string
		Submit string
	}
	p.eval(`({ Modal: document.querySelector(".modal.is-open") !== null,
		Title: document.querySelector("#view h1").textContent.trim(),
		Submit: document.querySelector('#view form.create-page button[type="submit"]').textContent })`, &page)
	if page.Modal || page.Title != "Create log scope" || page.Submit != "Create log scope" {
		t.Errorf("Create log scope opened %+v; want its page headed and ending in Create log scope", page)
	}
}

// deleteFolderNamed deletes, through the console API, the folder the test
// made with that name.
func deleteFolderNamed(t *testing.T, display string) {
	code, body := consoleDo(t, http.MethodGet, "/api/resources/hierarchy", "")
	if code != http.StatusOK {
		t.Logf("list folders to clean up = %d: %s", code, body)
		return
	}
	var l struct {
		Items []struct {
			Name   string
			Fields map[string]string
		}
	}
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Logf("decode folders: %v", err)
		return
	}
	for _, f := range l.Items {
		if f.Fields["Name"] == display {
			if code, body := consoleDo(t, http.MethodDelete, "/api/resources/hierarchy?name="+url.QueryEscape(f.Name), ""); code != http.StatusOK && code != http.StatusNoContent {
				t.Logf("delete folder %s = %d: %s", f.Name, code, body)
			}
		}
	}
}
