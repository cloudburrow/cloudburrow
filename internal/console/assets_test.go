package console

import (
	"strings"
	"testing"
)

func consoleAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestChildrenAreSetThroughTheNullSafeHelper holds a rule the console learned
// the hard way.
//
// Node.replaceChildren stringifies anything that is not a Node, so a
// conditional child that evaluated to null rendered the literal word "null" on
// the page. Every list screen without a note carried it, above the filter row.
// No Go test could see it, because the defect only exists once a browser
// builds the DOM — it was found by looking at the page.
//
// setChildren filters absent children, so the rule is: nothing sets children
// directly except setChildren itself and bare clears, which pass nothing and
// so cannot stringify anything.
func TestChildrenAreSetThroughTheNullSafeHelper(t *testing.T) {
	src := consoleAsset(t, "console.js")

	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		idx := strings.Index(trimmed, ".replaceChildren(")
		if idx < 0 {
			continue
		}
		if strings.HasPrefix(trimmed[idx+len(".replaceChildren("):], ")") {
			continue // a bare clear
		}
		if strings.HasPrefix(trimmed, "node.replaceChildren(") {
			continue // the helper itself
		}
		t.Errorf("console.js:%d sets children directly:\n  %s\n"+
			"use setChildren, which drops absent children — replaceChildren renders "+
			"a null child as the text \"null\"", i+1, trimmed)
	}

	if !strings.Contains(src, "const setChildren = (node, ...children)") {
		t.Error("setChildren is missing, so nothing enforces the rule above")
	}
}

// TestCollapsedNavKeepsItsIcons is the regression test for a defect that made
// the navigation invisible.
//
// The collapsed rail hides labels and keeps icons — that is the entire point
// of a rail. The rule was written as `nav a span { display: none }`, which
// also matched the span wrapping each icon, so below 1280px the navigation was
// an empty 72px column: no icons, no labels, nothing to click that could be
// seen. Every window narrower than that, which is most laptop windows.
func TestCollapsedNavKeepsItsIcons(t *testing.T) {
	css := consoleAsset(t, "console.css")

	if strings.Contains(css, "nav a span { display: none; }") {
		t.Error("`nav a span { display: none; }` hides the icon wrapper as well as the " +
			"label, which empties the collapsed navigation entirely")
	}
	if !strings.Contains(css, "nav a .nav-label { display: none; }") {
		t.Error("the collapsed rail does not hide the label; it should hide .nav-label only")
	}

	// The class the rule targets has to be the one the markup emits.
	js := consoleAsset(t, "console.js")
	if !strings.Contains(js, `class: "nav-label"`) {
		t.Error("nav links do not carry .nav-label, so the collapsed-rail rule matches nothing")
	}
	if !strings.Contains(js, `class: "nav-icon"`) {
		t.Error("nav links do not carry .nav-icon")
	}
	// Hiding the label removes the link's accessible name unless one is given.
	if !strings.Contains(js, `"aria-label": entry.title`) {
		t.Error("nav links have no aria-label, so the collapsed rail announces them as " +
			"unnamed links once the label is hidden")
	}
}

// TestMenuButtonDrivesTheRailAtEveryWidth is the regression test for a control
// that did nothing.
//
// The menu button set an attribute only one media query read, so above 960px
// it was visible, focusable and announced as a menu control while having no
// effect whatsoever. The rail's width is now driven by an explicit state the
// button sets, which is what makes it work at every width.
func TestMenuButtonDrivesTheRailAtEveryWidth(t *testing.T) {
	js := consoleAsset(t, "console.js")
	css := consoleAsset(t, "console.css")

	if !strings.Contains(js, "document.documentElement.dataset.nav") {
		t.Error("the menu button does not set a document-level rail state, so it can only " +
			"work inside whatever media query happens to read its attribute")
	}
	if !strings.Contains(css, `:root[data-nav="collapsed"]`) {
		t.Error("no stylesheet rule reads the rail state, so the menu button changes nothing")
	}
	// The collapsed rail must still be a rail: width and icons, not nothing.
	if !strings.Contains(css, `:root[data-nav="collapsed"] { --nav-w: 72px; }`) {
		t.Error("the collapsed state does not narrow the rail")
	}
	if !strings.Contains(css, `:root[data-nav="collapsed"] nav a .nav-label { display: none; }`) {
		t.Error("the collapsed state does not hide labels, so collapsing does nothing visible")
	}
}

// TestChoosingAProjectIsNotAnError keeps a precondition from being reported as
// a failure.
//
// Three services list per project. With none selected they answered with
// Unavailable, which the client renders as a red "could not read this from the
// local instance" — so a working instance greeted the user with errors on
// three of its screens. A prompt and a failure are different things and the
// screen has to be able to tell them apart.
func TestChoosingAProjectIsNotAnError(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if !strings.Contains(js, "if (data.prompt)") {
		t.Fatal("the client does not handle a prompt, so a precondition renders as a failure")
	}
	// The prompt must be handled before the error branch, or it never runs.
	if strings.Index(js, "if (data.prompt)") > strings.Index(js, "if (data.unavailable)") {
		t.Error("the prompt branch comes after the unavailable branch")
	}
	// A prompt must not be rendered through the error state. The branch is
	// bounded at its closing brace so the check cannot read the next one.
	promptIdx := strings.Index(js, "if (data.prompt)")
	rest := js[promptIdx:]
	end := strings.Index(rest, "\n  }")
	if end < 0 {
		t.Fatal("could not find the end of the prompt branch")
	}
	branch := rest[:end]
	if strings.Contains(branch, "errorState") {
		t.Error("the prompt is rendered as an error state")
	}
	if !strings.Contains(branch, "emptyState") {
		t.Error("the prompt is not rendered as an ordinary empty state")
	}
}

// TestConsoleOpensOnAProject checks the console adopts the instance's project
// rather than opening on "All projects", which is what made the per-project
// screens error on first load.
func TestConsoleOpensOnAProject(t *testing.T) {
	js := consoleAsset(t, "console.js")
	if !strings.Contains(js, "DEFAULT_PROJECT") {
		t.Fatal("the client has no notion of a default project")
	}
	// Asserted on behaviour rather than on a variable name: the default has to
	// reach the URL, however the code spells it.
	if !strings.Contains(js, `searchParams.set("project"`) {
		t.Error("the selected project is never written to the URL, so a reload or a shared " +
			"link lands back on no project")
	}
	if !strings.Contains(js, "if (!selected && DEFAULT_PROJECT)") {
		t.Error("the default project does not seed the selection when the URL names none")
	}
}

// TestToolbarSearchLooksAcrossServices keeps the most prominent control in
// the console from going back to filtering whatever table is on screen.
//
// It used to copy its text into the page's filter input, so the "Search
// resources" box could only narrow the page already open — and did nothing at
// all on a screen without a table.
func TestToolbarSearchLooksAcrossServices(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if strings.Contains(js, `filter.value = search.value`) {
		t.Error("the toolbar search still copies into the page filter instead of searching")
	}
	if !strings.Contains(js, "/api/search?q=") {
		t.Error("the toolbar search does not call the search endpoint")
	}
	if !strings.Contains(js, "async function renderSearch(") {
		t.Error("there is no results screen to render what the search found")
	}
}

// A global key handler must survive an event whose target is not an Element.
//
// The target of a key event can be the document itself, and calling matches()
// on one that is not an Element throws inside the handler.
func TestGlobalKeyHandlerGuardsNonElementTargets(t *testing.T) {
	js := consoleAsset(t, "console.js")
	if !strings.Contains(js, "t instanceof Element && (t.matches(") {
		t.Error("the shortcut handler calls matches() without checking the target is an " +
			"Element, which throws when the target is the document")
	}
}

// TestTablesSort keeps the header a control rather than a label.
func TestTablesSort(t *testing.T) {
	js := consoleAsset(t, "console.js")
	if !strings.Contains(js, `class: "sort-button"`) {
		t.Fatal("table headers are not sort controls")
	}
	// The arrow lives in the header, so sorting has to redraw the header as
	// well as the body — otherwise the table sorts without saying by what.
	if !strings.Contains(js, "drawHead();") {
		t.Error("sorting does not redraw the header, so the active column is never shown")
	}
	if !strings.Contains(js, `"aria-sort"`) {
		t.Error("sorted columns are not announced")
	}
	if !strings.Contains(js, `if (c === "Actions") return el("th", { scope: "col", text: c });`) {
		t.Error("the Actions column is offered as sortable, which sorts nothing")
	}
}

// A screen whose primary key is not called "name" must be able to say so.
func TestScreensNameTheirKeyColumn(t *testing.T) {
	js := consoleAsset(t, "console.js")
	if !strings.Contains(js, `data.nameColumn || "Name"`) {
		t.Error("the first column is hardcoded, so a screen with its own identifier " +
			"ends up with two columns headed the same")
	}
	if !strings.Contains(js, "const noun = data.noun ||") {
		t.Error("the filter and empty state cannot be given a noun, so a screen titled " +
			"\"Resource Manager\" reads \"Filter resource manager\"")
	}
}

// TestMenusHangFromTheirOwnControl is the regression test for a menu that
// opened nowhere near the button that opened it.
//
// The shared .panel rule pins itself to `right: 12px`, and the toolbar is
// sticky — which makes it the containing block — so a panel that does not
// override it lands against the far right of the window. The project picker
// sits at the left-hand end of the toolbar, so its menu appeared most of a
// screen away from the control it belongs to.
//
// A menu anchors to its own control, which means the control needs to be the
// containing block and the panel needs to stop inheriting the right-hand pin.
func TestMenusHangFromTheirOwnControl(t *testing.T) {
	css := consoleAsset(t, "console.css")

	// Without a positioned ancestor the offsets resolve against the toolbar.
	if !strings.Contains(css, ".project-picker { position: relative; }") {
		t.Error("the picker is not a containing block, so its menu positions " +
			"against the toolbar instead of against the picker")
	}
	// Overriding left alone is not enough: right: 12px still wins.
	if !strings.Contains(css, "left: 0; right: auto;") {
		t.Error("the picker menu does not clear the inherited right-hand pin, so it " +
			"opens against the far right of the window")
	}
}

// TestNavigationNamesTheProductsBeingEmulated keeps the navigation saying what
// a developer is actually pointing an SDK at.
//
// The whole claim of this console is that an application cannot tell the
// difference, so the navigation carries the real product names and the real
// groupings. "Serverless" described a category; "Services" described a noun.
// Neither told anyone that the thing behind the screen answers the Cloud Run
// API.
func TestNavigationNamesTheProductsBeingEmulated(t *testing.T) {
	js := consoleAsset(t, "console.js")

	for _, product := range []string{
		"Cloud Run", "Cloud Storage", "Pub/Sub", "Cloud Tasks",
		"Secret Manager", "Resource Manager", "Vertex AI",
	} {
		if !strings.Contains(js, `title: "`+product) {
			t.Errorf("the navigation does not name %q", product)
		}
	}

	// The console's own section headings, not invented categories.
	for _, section := range []string{
		"Compute", "Kubernetes Engine", "Storage", "Integration services",
		"AI and machine learning", "Security", "Operations", "IAM and admin",
	} {
		if !strings.Contains(js, `section: "`+section+`"`) {
			t.Errorf("the navigation does not group under %q", section)
		}
	}

	if strings.Contains(js, `section: "Serverless"`) {
		t.Error(`"Serverless" is a category, not the product being emulated`)
	}
}

// TestProductIconsAreVendoredAndSelfContained covers the icons the console
// ships.
//
// These are Google's own published product icons, used to identify the product
// each screen emulates. Two things have to hold: they must be present, and
// they must reference nothing remote — this console must render with no
// network at all, and an icon that fetched something would break that quietly
// for anyone running offline.
func TestProductIconsAreVendoredAndSelfContained(t *testing.T) {
	entries, err := assets.ReadDir("assets/icons")
	if err != nil {
		t.Fatalf("read icons: %v", err)
	}

	svgs := map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".svg") {
			continue
		}
		svgs[strings.TrimSuffix(e.Name(), ".svg")] = true

		b, err := assets.ReadFile("assets/icons/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		body := string(b)
		for _, remote := range []string{"http://", "https://", "<image"} {
			// xmlns declarations are namespace identifiers, not fetches.
			cleaned := strings.ReplaceAll(body, `xmlns="http://www.w3.org/2000/svg"`, "")
			cleaned = strings.ReplaceAll(cleaned, `xmlns:xlink="http://www.w3.org/1999/xlink"`, "")
			if strings.Contains(cleaned, remote) {
				t.Errorf("%s references %q; the console must render with no network",
					e.Name(), remote)
			}
		}
	}

	// Every screen the client says has a product icon must actually have one,
	// or the navigation shows a broken image.
	js := consoleAsset(t, "console.js")
	block := js[strings.Index(js, "const PRODUCT_ICONS = new Set(["):]
	block = block[:strings.Index(block, "]")]
	for _, part := range strings.Split(block, ",") {
		name := strings.Trim(strings.TrimSpace(part), `"`)
		if name == "" || strings.HasPrefix(name, "const") {
			continue
		}
		if !svgs[name] {
			t.Errorf("console.js claims a product icon for %q but assets/icons/%s.svg is missing",
				name, name)
		}
	}
}

// Deleting the icon directory has to remain a working way out, because the
// terms for that artwork are not established — see assets/icons/PROVENANCE.md.
// That is only true while the console still carries its own marks.
func TestConsoleKeepsItsOwnFallbackMarks(t *testing.T) {
	js := consoleAsset(t, "console.js")
	if !strings.Contains(js, "const ICONS = {") {
		t.Fatal("the fallback line drawings are gone, so removing the vendored icons " +
			"would leave the navigation with no marks at all")
	}
	for _, screen := range []string{"dashboard", "run", "storage"} {
		if !strings.Contains(js, screen+":") {
			t.Errorf("no fallback mark for %q", screen)
		}
	}
}
