//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// Round 2 of the shared layout (#1842): a resource's details in its page
// header rather than in a card between the header and the tabs; a Logs tab
// only where Google's page has one; the toolbar search's pages across
// products and its "/" key; the Help menu and the "?" list; the Select a
// project dialog's Name and ID columns and the New project page; and the
// Welcome page.

// TestResourceDetailsAreInTheHeaderAndABucketHasNoLogsTab: a bucket's page
// is titled "Bucket details", its properties are a row of the page header
// above the tab strip (no "Details" card), and, as on Google's bucket page,
// it has no Logs tab.
func TestResourceDetailsAreInTheHeaderAndABucketHasNoLogsTab(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	bucket := fmt.Sprintf("shell2-%d", time.Now().UnixNano()%1e9)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/storage"+q+"&name="+url.QueryEscape(bucket), "")
	})

	p.navigate("/storage/browser/" + bucket + q)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Bucket details" &&
		document.querySelector("#view [role=tablist]") !== null`)
	var got struct {
		Summary, InHeader, AboveTabs, Card bool
		Tabs                               []string
	}
	p.eval(`(() => { const s = document.querySelector("#view .resource-summary"), h = document.querySelector("#view .page-header"),
		tabs = document.querySelector("#view [role=tablist]");
		return { Summary: s !== null, InHeader: !!(s && h.contains(s)),
		         AboveTabs: !!(s && (s.compareDocumentPosition(tabs) & Node.DOCUMENT_POSITION_FOLLOWING)),
		         Card: [...document.querySelectorAll("#view > .card.properties")].some((c) => c.querySelector("h2") && c.querySelector("h2").textContent === "Details"),
		         Tabs: [...tabs.querySelectorAll("[role=tab]")].map((b) => b.textContent) }; })()`, &got)
	if !got.Summary || !got.InHeader || !got.AboveTabs || got.Card {
		t.Errorf("the bucket's details: %+v; want them in the page header above the tabs, with no Details card", got)
	}
	for _, tab := range got.Tabs {
		if tab == "Logs" {
			t.Errorf("a bucket's page has a Logs tab (%v); Google's has none", got.Tabs)
		}
	}
}

// TestAPodsPageHasALogsTab: a pod's page, whose Google page has Logs, is
// "Pod details" with a Logs tab that reads the instance's log store and
// links to the Logs Explorer with the pod's filter.
func TestAPodsPageHasALogsTab(t *testing.T) {
	needService(t, "pods")
	p := open(t)
	p.navigate("/kubernetes/pods")
	p.waitFor(`document.querySelector("#view tbody a") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#view tbody a`)))
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Pod details" &&
		[...document.querySelectorAll("#view [role=tab]")].some((b) => b.textContent === "Logs")`)
	p.clickText("#view [role=tab]", "Logs")
	p.waitFor(`[...document.querySelectorAll("#detail-panel a")].some((a) => a.textContent === "Open in Logs Explorer" && a.href.includes("/logs?"))`)
}

// TestSearchFindsPagesAcrossProductsAndOpensOnSlash: "/" puts the cursor in
// the toolbar search, whose placeholder says so, and typing a page's name
// lists that page in the dropdown.
func TestSearchFindsPagesAcrossProductsAndOpensOnSlash(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	p.navigate("/")
	p.waitFor(`document.querySelector("#view h1") !== null`)
	p.run(chromedp.KeyEvent("/"))
	p.waitFor(`document.activeElement && document.activeElement.id === "search"`)
	var placeholder string
	p.eval(`document.getElementById("search").placeholder`, &placeholder)
	if !strings.HasPrefix(placeholder, "Search (/)") {
		t.Errorf("the search placeholder reads %q, want it to name the / key", placeholder)
	}
	// The key event chromedp sends also types its character; the field is
	// filled as a whole instead.
	p.setField("#search", "deleted b")
	p.waitFor(`!document.getElementById("search-suggest").hidden &&
		[...document.querySelectorAll("#search-suggest a")].some((a) => new URL(a.href).pathname === "/storage/deleted")`)
	p.run(chromedp.KeyEvent(kb.Escape))
}

// TestHelpMenuListsTheKeyboardShortcuts: the toolbar's Help opens a menu
// whose Keyboard shortcuts lists "/" and "?", as "?" does.
func TestHelpMenuListsTheKeyboardShortcuts(t *testing.T) {
	p := open(t)
	p.navigate("/")
	p.waitFor(`document.querySelector("#view h1") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#help`)))
	p.waitFor(`!document.getElementById("help-panel").hidden && document.getElementById("help").getAttribute("aria-expanded") === "true"`)
	p.run(chromedp.Click(chromedp.CSS(`#help-shortcuts`)))
	p.waitFor(`document.getElementById("shortcut-help") !== null && document.getElementById("help-panel").hidden`)
	var keys []string
	p.eval(`[...document.querySelectorAll("#shortcut-help kbd")].map((k) => k.textContent)`, &keys)
	if !strings.Contains(strings.Join(keys, " "), "/") || !strings.Contains(strings.Join(keys, " "), "?") {
		t.Errorf("the shortcut list reads %v", keys)
	}
	p.run(chromedp.KeyEvent(kb.Escape))
	p.waitFor(`document.getElementById("shortcut-help") === null`)
	p.run(chromedp.KeyEvent("?"))
	p.waitFor(`document.getElementById("shortcut-help") !== null`)
	p.run(chromedp.KeyEvent(kb.Escape))
}

// TestSelectAProjectAndTheNewProjectPage: the picker lists Name and ID
// columns with Recent, Starred and All, and Manage resources; New project opens the New project page:
// Project name first, the Project ID made from it and editable, Location;
// Create makes the project and selects it, on the Welcome page.
func TestSelectAProjectAndTheNewProjectPage(t *testing.T) {
	p := open(t)
	project := uniqueProject(t)
	p.navigate("/?project=" + project)
	p.waitFor(`document.getElementById("project-current").textContent === "` + project + `"`)
	p.run(chromedp.Click(chromedp.CSS(`#project-button`)))
	p.waitFor(`[...document.querySelectorAll("#project-list .picker-item")].some((b) => b.textContent.includes("` + project + `"))`)
	var cols, manage string
	p.eval(`document.querySelector("#project-panel .picker-columns").textContent`, &cols)
	p.eval(`new URL(document.getElementById("project-manage").href).pathname`, &manage)
	if cols != "NameID" || manage != "/projects" {
		t.Errorf("the picker's columns read %q and Manage resources goes to %q", cols, manage)
	}
	// Star the project, and Starred lists it; Recent lists the selected one.
	p.eval(`[...document.querySelectorAll("#project-list .picker-row")].find((r) => r.textContent.includes("`+project+`")).querySelector(".picker-star").click()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`#project-views [data-view=starred]`)))
	p.waitFor(`[...document.querySelectorAll("#project-list .picker-item-id")].map((n) => n.textContent).join() === "` + project + `"`)
	p.run(chromedp.Click(chromedp.CSS(`#project-views [data-view=recent]`)))
	p.waitFor(`[...document.querySelectorAll("#project-list .picker-item-id")].some((n) => n.textContent === "` + project + `")`)
	p.run(chromedp.Click(chromedp.CSS(`#project-views [data-view=all]`)))
	p.run(chromedp.Click(chromedp.CSS(`#project-new`)))
	p.waitFor(`location.pathname === "/projectcreate" && document.getElementById("new-project-name") !== null`)
	var h1 string
	p.eval(`document.querySelector("#view h1").textContent`, &h1)
	if h1 != "New project" {
		t.Errorf("the page is titled %q", h1)
	}
	p.setField("#new-project-name", "Shell Two")
	var derived string
	p.eval(`document.getElementById("new-project-id").value`, &derived)
	if !strings.HasPrefix(derived, "shell-two-") {
		t.Errorf("the project ID made from %q is %q", "Shell Two", derived)
	}
	id := fmt.Sprintf("shell-two-%d", time.Now().UnixNano()%1e8)
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/projects?name="+id, "") })
	p.clickText("#view .link-button", "Edit")
	p.waitFor(`!document.getElementById("new-project-id").closest(".form-row").hidden`)
	p.setField("#new-project-id", id)
	p.clickText("#view form button.primary", "Create")
	p.waitFor(`location.pathname === "/" && document.getElementById("project-current").textContent === "` + id + `" &&
		document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Welcome"`)
}

// TestWelcomePage: Home is Google's Welcome: the project's name, number and
// ID, its resources, quick access to products and its activity, then the
// instance's own state.
func TestWelcomePage(t *testing.T) {
	p := open(t)
	project := uniqueProject(t)
	p.navigate("/?project=" + project)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Welcome" &&
		document.querySelector("#welcome-project dl") !== null`)
	var got struct {
		Info, Headings []string
		Activity       bool
	}
	p.eval(`({ Info: [...document.querySelectorAll("#welcome-project dt, #welcome-project dd")].map((n) => n.textContent),
	           Headings: [...document.querySelectorAll("#view h2")].map((h) => h.textContent),
	           Activity: [...document.querySelectorAll("#view a")].some((a) => new URL(a.href).pathname === "/activity") })`, &got)
	info := strings.Join(got.Info, "|")
	if !strings.Contains(info, "Project ID|"+project) || !strings.Contains(info, "Project number|") {
		t.Errorf("Project info reads %q", info)
	}
	for _, h := range []string{"Project info", "Resources", "Quick access", "Activity", "Instance"} {
		if !strings.Contains(strings.Join(got.Headings, "|"), h) {
			t.Errorf("the Welcome page's headings %v lack %q", got.Headings, h)
		}
	}
	if !got.Activity {
		t.Error("Activity is not reachable from the Welcome page")
	}
}
