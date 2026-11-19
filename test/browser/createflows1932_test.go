//go:build browser

package browser

import (
	"testing"
)

// TestCreateFlowsArePagesWhereGooglesAre (#1932): the Projects list's
// Create project opens the New project page, /projectcreate, as the project
// picker's New project and Manage resources' Create project do, rather than
// a dialog of its own; its old create address leads there too. Cloud
// Logging's Create log scope is a page, as Google's is
// (docs.cloud.google.com/logging/docs/log-scope/create-and-manage), ending
// in Create log scope.
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
