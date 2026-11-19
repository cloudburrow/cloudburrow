//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// The shared layout of #1842, which every console-parity category builds on:
// a product's own left-hand menu, one drawer row per product, the title
// line's actions, the filter bar's chips and View options, status as the
// first column's icon, "<Kind> details" resource pages with a back arrow,
// numbered create sections, and a project that is always selected.

// TestProductNavIsOnTheLeftAndCollapses: inside Pub/Sub the product's own
// menu is beside the page, on its left, named Pub/Sub, listing its pages
// with Topics current; it stays on a topic's page; its control collapses it
// to a rail and opens it again. Cloud Logging's menu holds Logs Explorer
// with the admin pages, one product rather than two (#1842).
func TestProductNavIsOnTheLeftAndCollapses(t *testing.T) {
	needService(t, "pubsub")
	p := open(t)
	project := uniqueProject(t)
	topic := "projects/" + project + "/topics/shell-nav"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub?project="+project,
		`{"name":"shell-nav","defaultSubscription":"false"}`); code != http.StatusOK {
		t.Fatalf("create a topic through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+project+"&name="+url.QueryEscape(topic), "")
	})

	p.navigate("/pubsub/topics?project=" + project)
	p.waitFor(`document.querySelectorAll("#product-nav a").length >= 2 && document.querySelector("#view tbody tr") !== null`)
	var nav struct {
		Title, Current string
		Pages          []string
		Left           bool
	}
	p.eval(`(() => { const n = document.getElementById("product-nav"), m = document.getElementById("main");
		return { Title: n.querySelector(".product-nav-title").textContent,
		         Current: n.querySelector("a[aria-current=page]").textContent,
		         Pages: [...n.querySelectorAll("a")].map((a) => a.textContent),
		         Left: n.getBoundingClientRect().right <= m.getBoundingClientRect().left + 1 }; })()`, &nav)
	if nav.Title != "Pub/Sub" || nav.Current != "Topics" || !nav.Left ||
		strings.Join(nav.Pages, ",") != "Topics,Subscriptions,Snapshots,Schemas" {
		t.Errorf("Pub/Sub's menu reads %+v; want Pub/Sub on the left of the page, Topics current among its four pages", nav)
	}

	// A resource's page keeps its product's menu, its list marked.
	p.clickText("#view tbody a", topic)
	// Settled: the title is drawn at once, the tabs once the topic is read.
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Topic details" &&
		document.querySelector("#view [role=tablist]") !== null`)
	var detail struct {
		Current, Name, Back string
	}
	p.eval(`({ Current: (document.querySelector("#product-nav a[aria-current=page]") || {}).textContent || "",
	           Name: document.querySelector("#view .resource-name").textContent,
	           Back: new URL(document.querySelector("#view .page-header .back-arrow").href).pathname })`, &detail)
	if detail.Current != "Topics" || detail.Name != topic || detail.Back != "/pubsub/topics" {
		t.Errorf("the topic's page reads %+v; want Topics current, its name under the title and a back arrow to the list", detail)
	}
	p.run(chromedp.Click(chromedp.CSS(`#view .page-header .back-arrow`)))
	p.waitFor(`location.pathname === "/pubsub/topics" && document.querySelector("#view tbody tr") !== null`)

	// Collapse and open again, from the keyboard.
	p.tabTo(`a.classList.contains("product-nav-toggle")`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.getElementById("product-nav-pages").hidden && document.documentElement.dataset.productNav === "collapsed"`)
	var label string
	p.eval(`document.querySelector(".product-nav-toggle").getAttribute("aria-label")`, &label)
	if label != "Expand Pub/Sub navigation menu" {
		t.Errorf("the collapsed menu's control is named %q", label)
	}
	p.run(chromedp.Click(chromedp.CSS(`.product-nav-toggle`)))
	p.waitFor(`!document.getElementById("product-nav-pages").hidden && document.documentElement.dataset.productNav === "open"`)

	// Cloud Logging is one product: Logs Explorer and the admin pages.
	p.navigate("/logs/sinks?project=" + project)
	p.waitFor(`document.querySelector("#product-nav .product-nav-title") !== null && document.querySelector("#product-nav .product-nav-title").textContent === "Cloud Logging"`)
	var logging []string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent)`, &logging)
	if len(logging) < 2 || logging[0] != "Logs Explorer" || !strings.Contains(strings.Join(logging, ","), "Log Router") {
		t.Errorf("Cloud Logging's menu lists %v; want Logs Explorer first, with Log Router (#1863)", logging)
	}
	var rows int
	p.eval(`[...document.querySelectorAll("#nav-list a")].filter((a) => a.textContent.includes("Logs Explorer")).length`, &rows)
	if rows != 0 {
		t.Errorf("the drawer still has a row of its own for Logs Explorer")
	}
}

// TestListHeaderFilterChipsViewOptionsAndStatusIcon: a list's Create,
// Refresh and info-panel toggle are on its title line; choosing a property
// in the filter and typing a value makes a chip that narrows the rows to
// that column's matches, and removing it brings them back; View options
// hides a column and shows it again; status is the first column, an icon
// whose text is its name for assistive technology (#1842).
func TestListHeaderFilterChipsViewOptionsAndStatusIcon(t *testing.T) {
	needService(t, "tasks")
	p := open(t)
	project := uniqueProject(t)
	names := []string{"shell-alpha", "shell-beta"}
	for _, n := range names {
		if code, body := consoleDo(t, http.MethodPost, "/api/resources/tasks?project="+project,
			fmt.Sprintf(`{"name":%q,"location":"us-central1"}`, n)); code != http.StatusOK {
			t.Fatalf("create queue %s through the console API = %d: %s", n, code, body)
		}
		full := "projects/" + project + "/locations/us-central1/queues/" + n
		t.Cleanup(func() {
			consoleDo(t, http.MethodDelete, "/api/resources/tasks?project="+project+"&name="+url.QueryEscape(full), "")
		})
	}

	p.navigate("/tasks/queues?project=" + project)
	p.waitFor(`document.querySelectorAll("#view tbody tr[role=button]").length === 2`)
	var head struct {
		Controls []string
		First    string
		Icon     bool
		Text     string
	}
	p.eval(`(() => { const cols = [...document.querySelectorAll("#view thead th")].map((th) => th.textContent.trim());
		const cell = document.querySelector("#view tbody tr td.status-cell");
		return { Controls: [...document.querySelectorAll("#view .page-header-actions > button")].map((b) => b.textContent.trim()),
		         First: cols.find((c) => c !== ""),
		         Icon: cell !== null && cell.querySelector("svg") !== null,
		         Text: cell ? cell.textContent.trim() : "" }; })()`, &head)
	if strings.Join(head.Controls, ",") != "Create queue,Refresh,Show info panel" {
		t.Errorf("the title line carries %v; want Create queue, Refresh and the info-panel toggle", head.Controls)
	}
	if head.First != "Status" || !head.Icon || head.Text != "RUNNING" {
		t.Errorf("the first column is %q, with an icon %v reading %q; want Status, an icon, RUNNING", head.First, head.Icon, head.Text)
	}

	// The property menu, then a value: one chip, one row.
	p.run(chromedp.Click(chromedp.CSS(`#view input.filter`)))
	p.waitFor(`!document.querySelector("#view .filter-menu").hidden`)
	p.clickText("#view .filter-menu-item", "Name")
	p.waitFor(`document.querySelector("#view input.filter").value === "Name: "`)
	p.run(chromedp.SendKeys(chromedp.CSS(`#view input.filter`), "alpha"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelectorAll("#view .filter-chips .chip").length === 1 && document.querySelectorAll("#view tbody tr[role=button]").length === 1`)
	var chip string
	p.eval(`document.querySelector("#view .filter-chips .chip").textContent`, &chip)
	if chip != "Name: alpha" {
		t.Errorf("the chip reads %q, want Name: alpha", chip)
	}
	// A value no column holds empties the table and offers to clear it.
	p.run(chromedp.SendKeys(chromedp.CSS(`#view input.filter`), "nowhere"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`[...document.querySelectorAll("#view tbody button")].some((b) => b.textContent === "Clear filter")`)
	p.clickText("#view tbody button", "Clear filter")
	p.waitFor(`document.querySelectorAll("#view .filter-chips .chip").length === 0 && document.querySelectorAll("#view tbody tr[role=button]").length === 2`)

	// View options turns a column off and on.
	var column string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((th) => th.textContent.trim()).filter((c) => c && c !== "Status" && c !== "Name" && c !== "Actions")[0] || ""`, &column)
	if column == "" {
		t.Fatal("the queue list has no data column to hide")
	}
	hasColumn := fmt.Sprintf(`[...document.querySelectorAll("#view thead th")].some((th) => th.textContent.trim() === %q)`, column)
	p.run(chromedp.Click(chromedp.CSS(`#view .view-options`)))
	p.waitFor(`!document.querySelector("#view .view-options-menu").hidden`)
	p.clickText("#view .view-options-item", column)
	p.waitFor(`!` + hasColumn)
	p.clickText("#view .view-options-item", column)
	p.waitFor(hasColumn)
}

// TestCreatePageHasNumberedSectionsAndABackArrow: Create a bucket is a
// page with a back arrow to the list; its sections are numbered, Continue
// folds the finished one and opens the next, and Create comes before Cancel
// at the bottom (#1842). Create queue, a dialog before, is a page too.
func TestCreatePageHasNumberedSectionsAndABackArrow(t *testing.T) {
	needService(t, "storage")
	p := open(t)
	project := uniqueProject(t)
	p.navigate("/storage/browser/create?project=" + project)
	p.waitFor(`document.querySelectorAll("#view .create-step").length > 1`)
	var form struct {
		Steps   []string
		Back    string
		Buttons []string
	}
	p.eval(`({ Steps: [...document.querySelectorAll("#view .create-step")].map((s) => s.querySelector(".create-step-number").textContent + " " + s.querySelector(".create-step-title").textContent),
	           Back: new URL(document.querySelector("#view .page-header .back-arrow").href).pathname,
	           Buttons: [...document.querySelectorAll("#view .form-actions > *")].map((b) => b.textContent.trim()) })`, &form)
	if len(form.Steps) < 3 || form.Steps[0] != "1 Get started" || !strings.HasPrefix(form.Steps[1], "2 Choose") ||
		form.Back != "/storage/browser" || strings.Join(form.Buttons, ",") != "Create,Cancel" {
		t.Errorf("Create a bucket reads %+v; want numbered sections from 1 Get started, a back arrow to Buckets, Create then Cancel", form)
	}
	p.clickText("#view .create-step-continue button", "Continue")
	p.waitFor(`document.querySelector("#view .create-step").classList.contains("is-folded") &&
		!document.querySelectorAll("#view .create-step")[1].querySelector(".create-step-body").hidden`)
	// The folded section opens again from its title.
	p.run(chromedp.Click(chromedp.CSS(`#view .create-step-head`)))
	p.waitFor(`!document.querySelector("#view .create-step-body").hidden`)

	needService(t, "tasks")
	p.navigate("/tasks/queues?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create queue")`)
	p.run(chromedp.Click(chromedp.CSS(`#view .state button.primary`)))
	p.waitFor(`location.pathname === "/tasks/queues/create" && document.querySelector("#view form.create-page #f-name") !== null`)
	var dialog bool
	p.eval(`document.querySelector(".modal") !== null`, &dialog)
	if dialog {
		t.Error("Create queue still opens a dialog")
	}
}

// TestAProjectIsAlwaysSelected: the picker lists projects only, with no
// "All projects" entry, and names the selected one (#1842).
func TestAProjectIsAlwaysSelected(t *testing.T) {
	p := open(t)
	project := uniqueProject(t)
	p.navigate("/storage/browser?project=" + project)
	p.waitFor(`document.getElementById("project-current").textContent === "` + project + `"`)
	p.run(chromedp.Click(chromedp.CSS(`#project-button`)))
	p.waitFor(`document.querySelectorAll("#project-list .picker-item").length > 0`)
	var names []string
	p.eval(`[...document.querySelectorAll("#project-list .picker-item-name")].map((n) => n.textContent)`, &names)
	for _, n := range names {
		if n == "All projects" {
			t.Errorf("the picker still offers All projects: %v", names)
		}
	}
}
