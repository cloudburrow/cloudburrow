package console

import (
	"os"
	"regexp"
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

// TestNavigationMenuIsADrawer holds the menu to the shape the console it
// mirrors uses: closed, overlaid, or docked.
//
// It replaces a test about a collapsed icon rail, which is no longer what this
// is. The defect that test was written for — a bare `nav a span` rule hiding
// the icons along with the labels — is covered by
// TestTheServicesRailIsScopedToItsOwnElement and by the label class still
// being the thing the stylesheet targets.
func TestNavigationMenuIsADrawer(t *testing.T) {
	css := consoleAsset(t, "console.css")
	js := consoleAsset(t, "console.js")

	for _, state := range []string{"open", "docked", "closed"} {
		if !strings.Contains(css, `:root[data-nav="`+state+`"]`) {
			t.Errorf("no stylesheet rule for the %q state", state)
		}
	}
	// Overlaid it must float above the page; docked it must make room.
	if !strings.Contains(css, `:root[data-nav="docked"] .layout { padding-left: var(--nav-w); }`) {
		t.Error("a docked menu does not make room for itself, so it covers the page")
	}
	if !strings.Contains(css, ".nav-scrim") {
		t.Error("there is no scrim, so an overlaid menu is not modal")
	}
	// A modal that cannot be left by keyboard is a trap.
	// Written as a guard clause now that the same handler also traps Tab.
	if !strings.Contains(js, `if (navState() !== "open") return;`) ||
		!strings.Contains(js, `if (e.key === "Escape") { closeNav({ focusToggle: true }); return; }`) {
		t.Error("Escape does not close the overlaid menu")
	}
	if !strings.Contains(js, "if (focusToggle) document.getElementById(\"nav-toggle\").focus();") {
		t.Error("focus is not returned to the control that opened the menu")
	}
	// An overlaid menu that survives a link hides the page the link opened.
	if !strings.Contains(js, `if (navState() === "open" && e.target.closest("a")) closeNav();`) {
		t.Error("following a link does not close an overlaid menu")
	}
}

// TestMenuButtonDrivesTheMenuAtEveryWidth is the regression test for a control
// that did nothing.
//
// The menu button once set an attribute only one media query read, so above
// 960px it was visible, focusable and announced as a menu control while having
// no effect whatsoever. The menu's state is now document-level and the
// stylesheet reads it at every width.
func TestMenuButtonDrivesTheMenuAtEveryWidth(t *testing.T) {
	js := consoleAsset(t, "console.js")
	css := consoleAsset(t, "console.css")

	if !strings.Contains(js, "document.documentElement.dataset.nav = state") {
		t.Error("the menu button does not set a document-level state, so it can only " +
			"work inside whatever media query happens to read its attribute")
	}
	if !strings.Contains(css, `:root[data-nav="open"] #nav`) {
		t.Error("no stylesheet rule reads the menu state, so the button changes nothing")
	}
	// The button has to report what it did.
	if !strings.Contains(js, `toggle.setAttribute("aria-expanded"`) {
		t.Error("the menu button does not announce whether the menu is open")
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

	// Google's own product categories, taken from the category-icons set they
	// publish beside the product icons — not names invented here. Each one
	// also has to have an icon, or the group heading renders bare.
	for _, section := range []string{
		"Serverless computing", "Containers", "Storage", "Databases",
		"Integration services", "AI and machine learning",
		"Security and identity", "Operations", "Management tools",
	} {
		if !strings.Contains(js, `section: "`+section+`"`) {
			t.Errorf("the navigation does not group under %q", section)
		}
	}

	if strings.Contains(js, `section: "Serverless"`) && !strings.Contains(js, `section: "Serverless computing"`) {
		t.Error(`"Serverless" is not one of Google's category names`)
	}

	// Every category must map to a published category icon, or the heading
	// renders without one while its neighbours have them.
	css := consoleAsset(t, "console.js")
	for _, section := range []string{
		"Serverless computing", "Containers", "Storage", "Databases",
		"Integration services", "AI and machine learning",
		"Security and identity", "Operations", "Management tools",
	} {
		if !strings.Contains(css, `"`+section+`":`) {
			t.Errorf("category %q has no entry in CATEGORY_ICONS", section)
		}
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

// TestTheServicesRailIsScopedToItsOwnElement stops the sidebar's styling from
// claiming every navigation on the page.
//
// The rail was styled with a bare `nav` selector, which gave it a fixed width
// and a right-hand border. The breadcrumb is a <nav> too — it is a navigation
// — so it inherited a 256px box and a vertical rule down the middle of the
// detail screen.
func TestTheServicesRailIsScopedToItsOwnElement(t *testing.T) {
	css := consoleAsset(t, "console.css")

	for _, line := range strings.Split(css, "\n") {
		trimmed := strings.TrimSpace(line)
		// A selector line, not a declaration or a comment.
		if !strings.HasSuffix(trimmed, "{") && !strings.HasSuffix(trimmed, ",") {
			continue
		}
		if strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		for _, selector := range strings.Split(trimmed, ",") {
			selector = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(selector), "{"))
			if selector == "nav" || strings.HasPrefix(selector, "nav ") ||
				strings.HasPrefix(selector, "nav[") || strings.HasPrefix(selector, "nav:") {
				t.Errorf("bare `nav` selector %q styles every navigation on the page, "+
					"including the breadcrumb; scope it to #nav", selector)
			}
		}
	}
}

// TestEveryCapabilityReachesTheClient stops a capability from being added to
// the struct and forgotten in the handler.
//
// `detail` was: the field existed, the interface check set it, and
// /api/services built its own map without it — so rows that could be opened
// were not clickable and the feature was invisible. The handler must report
// every capability the struct carries.
func TestEveryCapabilityReachesTheClient(t *testing.T) {
	src, err := os.ReadFile("console.go")
	if err != nil {
		t.Fatalf("read console.go: %v", err)
	}
	body := string(src)

	start := strings.Index(body, "type capabilities struct {")
	if start < 0 {
		t.Fatal("capabilities struct not found")
	}
	end := strings.Index(body[start:], "\n}")
	fields := body[start : start+end]

	// The JSON name of each capability must appear in the services handler.
	handler := body[strings.Index(body, "func (s *Server) handleServices"):]
	handler = handler[:strings.Index(handler, "\n}")]

	for _, line := range strings.Split(fields, "\n") {
		i := strings.Index(line, `json:"`)
		if i < 0 {
			continue
		}
		name := line[i+len(`json:"`):]
		name = name[:strings.IndexAny(name, `",`)]
		if name == "" {
			continue
		}
		if !strings.Contains(handler, `"`+name+`"`) {
			t.Errorf("capability %q is never sent by handleServices, so the client "+
				"cannot act on it", name)
		}
	}
}

// TestNavigationMenuHasTheConsoleStructure keeps the navigation shaped like
// the one it mirrors rather than a flat list of links.
//
// The console this emulates opens on a pinned section, then groups the rest
// under product categories that expand. A flat list is a list to read; this is
// a place to look, and the difference is the whole reason the real one is
// built that way.
func TestNavigationMenuHasTheConsoleStructure(t *testing.T) {
	js := consoleAsset(t, "console.js")

	for _, piece := range []struct{ needle, why string }{
		{`text: "Pinned"`, "there is no pinned section"},
		{`class: "nav-group"`, "categories are not controls, so they cannot expand"},
		{`class: "nav-pin"`, "products cannot be pinned"},
		{`"aria-expanded"`, "an expanding group does not announce its state"},
		{`"aria-pressed"`, "the pin toggle does not announce its state"},
		{"PINNED_KEY", "pins are not remembered between loads"},
		{"OPEN_GROUPS_KEY", "which groups are open is not remembered"},
	} {
		if !strings.Contains(js, piece.needle) {
			t.Errorf("%s (looked for %s)", piece.why, piece.needle)
		}
	}
}

// The category icons are Google's published set, so every category the
// navigation names must have one on disk.
func TestCategoryIconsExistForEveryCategory(t *testing.T) {
	js := consoleAsset(t, "console.js")

	block := js[strings.Index(js, "const CATEGORY_ICONS = {"):]
	block = block[:strings.Index(block, "};")]

	entries, err := assets.ReadDir("assets/icons/categories")
	if err != nil {
		t.Fatalf("read category icons: %v", err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[strings.TrimSuffix(e.Name(), ".svg")] = true
	}

	found := 0
	for _, line := range strings.Split(block, "\n") {
		i := strings.LastIndex(line, `"`)
		j := strings.LastIndex(line[:max(i, 0)], `"`)
		if i < 0 || j < 0 || i == j {
			continue
		}
		slug := line[j+1 : i]
		if slug == "" || strings.Contains(slug, " ") {
			continue
		}
		found++
		if !have[slug] {
			t.Errorf("category icon %q is referenced but assets/icons/categories/%s.svg is missing",
				slug, slug)
		}
	}
	if found == 0 {
		t.Error("no category icons are referenced at all")
	}
}

// TestTheMenuControlIsNotDuplicated keeps one menu button on screen.
//
// The drawer had its own header with a hamburger and a "Navigation menu"
// title, while the toolbar above it already carried a hamburger and the
// product name. Opening the menu put two identical controls on screen, one
// above the other, and named the same thing twice.
//
// The toolbar's button is the only one. The drawer is announced by the nav
// element's own aria-label, which is what that attribute is for.
func TestTheMenuControlIsNotDuplicated(t *testing.T) {
	html := consoleAsset(t, "index.html")

	if n := strings.Count(html, `<path d="M3 6h18M3 12h18M3 18h18"/>`); n != 1 {
		t.Errorf("the hamburger glyph appears %d times; there is one menu button", n)
	}
	if strings.Contains(html, `id="nav-close"`) {
		t.Error("the drawer carries its own close control, duplicating the toolbar's")
	}
	if strings.Contains(html, "nav-title") {
		t.Error("the drawer names itself in text as well as in aria-label")
	}
	// The label has to be there, since it is now the only thing naming it.
	if !strings.Contains(html, `aria-label="Navigation menu"`) {
		t.Error("the nav has no accessible name")
	}

	js := consoleAsset(t, "console.js")
	if strings.Contains(js, `getElementById("nav-close")`) {
		t.Error("console.js still wires a close control that no longer exists")
	}
}

// TestCatalogueSitsBehindMoreProducts holds the menu's shape.
//
// Pinned products are what a developer reaches for; the rest of the catalogue
// is one level further in, behind "More products", rather than nine category
// rows stacked under the pinned ones. Listing them all at the top level made
// a pinned product and a category look like peers.
func TestCatalogueSitsBehindMoreProducts(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if !strings.Contains(js, `text: "More products"`) {
		t.Fatal("the catalogue is not behind a More products control")
	}
	if !strings.Contains(js, "MORE_OPEN_KEY") {
		t.Error("whether the catalogue is open is not remembered")
	}
	// Expanding every category while the catalogue is shut would show nothing.
	if !strings.Contains(js, "writeStored(MORE_OPEN_KEY, true);") {
		t.Error(`"View all products" does not open the catalogue itself`)
	}
}

// TestPinnedProductsCanBeReordered covers the behaviour the real menu has and
// a set cannot express.
//
// Pins were held in a Set, which has no order, so the pinned block always came
// out in declaration order however the developer arranged it.
func TestPinnedProductsCanBeReordered(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if strings.Contains(js, "new Set(readStored(PINNED_KEY") {
		t.Error("pins are stored in a Set, which cannot hold the order they are shown in")
	}
	for _, needle := range []string{
		"function movePinned(", `"dragstart"`, `"drop"`, `draggable: draggable ? "true" : null`,
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("pinned rows are not reorderable by dragging (looked for %s)", needle)
		}
	}
}

// TestNavigationLinksCarryOnlyTheScope covers #140.
//
// Links were built as `entry.path + location.search`, so a detail screen's
// ?resource= rode along to every product in the menu: clicking the product you
// were already inside re-opened the resource instead of returning to its list,
// and clicking any other product asked it for a resource by that name.
func TestNavigationLinksCarryOnlyTheScope(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if strings.Contains(js, "href: entry.path + location.search") {
		t.Error("navigation links still carry the whole query string")
	}
	if !strings.Contains(js, "function scopeSearch()") {
		t.Fatal("there is no single definition of what travels between screens")
	}
	// The allow-list must be an allow-list, not a deny-list: a deny-list has to
	// be updated every time a screen invents a parameter.
	block := js[strings.Index(js, "function scopeSearch()"):]
	block = block[:strings.Index(block, "\n}")]
	if !strings.Contains(block, `["project"]`) {
		t.Error("scopeSearch does not copy a named set of parameters")
	}
	for _, leaked := range []string{"resource", "\"q\""} {
		if strings.Contains(block, leaked) {
			t.Errorf("scopeSearch mentions %s; it should name what travels, not what does not", leaked)
		}
	}
}

// TestTheDrawerRevealsTheActiveProduct covers #137.
//
// Both the catalogue and every category default to closed, so a deep link left
// the drawer with no row for the screen on display and nothing marked — the
// menu could not answer "where am I" for most of the catalogue.
func TestTheDrawerRevealsTheActiveProduct(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if !strings.Contains(js, "active ? active.section : null") {
		t.Fatal("the drawer does not work out which category holds the current screen")
	}
	if !strings.Contains(js, "moreStored || Boolean(revealed)") {
		t.Error("revealing the active product does not open the catalogue")
	}
	if !strings.Contains(js, `openGroups().has(section) || section === revealed`) {
		t.Error("revealing the active product does not open its category")
	}
	// Revealing must not fight the controls. A toggle acts on the stored
	// preference; computing it from the effective state left the control stuck,
	// because on a revealed screen the effective state is always open.
	if !strings.Contains(js, "writeStored(MORE_OPEN_KEY, !moreStored)") {
		t.Error("the catalogue toggle acts on the effective state rather than the " +
			"stored preference, so on a revealed screen it can only ever close")
	}
	if !strings.Contains(js, "REVEAL_SUSPENDED = true") {
		t.Error("using a menu control does not suspend the reveal, so the reveal " +
			"immediately undoes what the user just did")
	}
	if !strings.Contains(js, "REVEAL_SUSPENDED = false") {
		t.Error("the reveal is never restored, so it works once and never again")
	}
}

// TestTheNavPinCanTakeFocus covers #127.
//
// The pin was hidden with display:none until hover, which removes an element
// from the tab order entirely — so a control that exists as much for keyboard
// users as for anyone else could only ever be reached with a mouse.
func TestTheNavPinCanTakeFocus(t *testing.T) {
	css := consoleAsset(t, "console.css")

	block := css[strings.Index(css, ".nav-pin {"):]
	block = block[:strings.Index(block, "\n}")]
	if strings.Contains(block, "display: none") {
		t.Error(".nav-pin is hidden with display:none, which removes it from the tab order")
	}
	if !strings.Contains(block, "opacity: 0") {
		t.Error(".nav-pin is not hidden by opacity, so it is either always visible or not focusable")
	}
	if !strings.Contains(css, ".nav-pin:focus-visible { opacity: 1; }") &&
		!strings.Contains(css, ".nav-item:hover .nav-pin, .nav-pin:focus-visible { opacity: 1; }") {
		t.Error("a focused pin is not brought back into view")
	}
}

// TestSearchAnswersBothQuestions covers #139 and #158.
//
// The toolbar search matched resource names only, so it could not answer
// "where is Cloud Tasks" — and a resource it did find linked to the screen
// that lists it rather than to the resource, asking the user to search twice.
func TestSearchAnswersBothQuestions(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if !strings.Contains(js, "const screens = ROUTES.filter((r) =>") {
		t.Error("the search does not match products and pages")
	}
	if strings.Contains(js, "const pathFor = (service) =>") {
		t.Error("search results still link to a product's list page")
	}
	if !strings.Contains(js, "caps.detail ? detailHref(r, hit.name)") {
		t.Error("a result whose row can be opened does not link to the row")
	}
}

// TestTheOverlaidDrawerTrapsFocus covers #146.
func TestTheOverlaidDrawerTrapsFocus(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if !strings.Contains(js, `if (e.key !== "Tab") return;`) {
		t.Fatal("Tab is not handled while the menu is modal, so focus walks out of it")
	}
	if !strings.Contains(js, `main.setAttribute("inert", "")`) {
		t.Error("the content behind a modal menu is not inert")
	}
	// inert cannot be undone on a descendant, so marking the toolbar would
	// disable the menu button that closes the menu.
	if strings.Contains(js, `toolbar.setAttribute("inert"`) {
		t.Error("marking the toolbar inert would disable the menu button itself")
	}
}

// TestTheFilterMatchesEveryDisplayedColumn covers #141.
//
// It matched the name only, so typing a value that is plainly on screen in
// another column returned nothing — which reads as "there are none" rather
// than "I only look at one column".
func TestTheFilterMatchesEveryDisplayedColumn(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if strings.Contains(js, "data.items.filter((i) => !q || i.name.toLowerCase().includes(q))") {
		t.Error("the filter still matches the name column only")
	}
	if !strings.Contains(js, "const matches = (item, q) =>") {
		t.Fatal("there is no predicate that spans the displayed columns")
	}
	block := js[strings.Index(js, "const matches = (item, q) =>"):]
	block = block[:strings.Index(block, "\n  };")]
	for _, part := range []string{"item.name", "item.status", "dataColumns().some"} {
		if !strings.Contains(block, part) {
			t.Errorf("the filter does not consider %s", part)
		}
	}
}

// TestTablesPaginate covers #142.
func TestTablesPaginate(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if !strings.Contains(js, "const PAGE_SIZES = ") {
		t.Fatal("there is no page size")
	}
	for _, needle := range []string{
		"Rows per page", "page-range", `"Previous page"`, `"Next page"`,
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("the table footer lacks %s", needle)
		}
	}
	// The range has to describe what is on screen, so it counts filtered rows.
	if !strings.Contains(js, "`${from}–${to} of ${total}`") {
		t.Error("the footer does not report a range out of the filtered total")
	}
}

// TestTablesSupportSelectionAndBulkDelete covers #143.
func TestTablesSupportSelectionAndBulkDelete(t *testing.T) {
	js := consoleAsset(t, "console.js")
	css := consoleAsset(t, "console.css")

	for _, needle := range []string{
		"const selectable =", "Select all ", "deleteSelected", "selection-count",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("row selection is incomplete (missing %s)", needle)
		}
	}
	// A bulk action that is always enabled invites a click with nothing chosen.
	if !strings.Contains(js, `bulk.disabled = n === 0`) {
		t.Error("the bulk delete is not disabled while nothing is selected")
	}
	// The page's actions and the table's filter are different bars.
	if !strings.Contains(css, ".action-bar") || !strings.Contains(css, ".filter-bar") {
		t.Error("the action bar and the filter bar are not separated")
	}
}

// TestDestructiveActionsAskForTheName covers #134, and #162's marking of them.
//
// window.confirm cannot say which resource, cannot be styled, and is one
// reflexive Enter away from deleting the wrong thing.
func TestDestructiveActionsAskForTheName(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if strings.Contains(js, "return window.confirm(") {
		t.Error("deletes still go through window.confirm")
	}
	if !strings.Contains(js, "function confirmDestructive({ title, detail, confirmWord, onConfirm })") {
		t.Fatal("there is no typed-name confirmation")
	}
	if !strings.Contains(js, `Type ${confirmWord} exactly to confirm.`) {
		t.Error("the confirmation does not require the name back")
	}
	if !strings.Contains(js, "is-destructive") {
		t.Error("destructive row actions are not marked apart from the others")
	}
}

// TestOutcomesUseASnackbarNotAnAlert covers #131.
//
// window.alert blocks the page, cannot be styled, and said nothing at all when
// an action succeeded — so the only feedback the console gave was for failure,
// and it stopped the world to give it.
func TestOutcomesUseASnackbarNotAnAlert(t *testing.T) {
	js := consoleAsset(t, "console.js")

	for _, line := range strings.Split(js, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "window.alert(") {
			t.Errorf("window.alert still reports an outcome: %s", trimmed)
		}
	}
	if !strings.Contains(js, `function notify(message, kind = "info")`) {
		t.Fatal("there is no snackbar")
	}
	// An error is the one outcome worth reading twice, so it does not vanish.
	if !strings.Contains(js, `if (kind !== "error") setTimeout(() => bar.remove(), NOTIFY_MS);`) {
		t.Error("errors are dismissed on a timer, so one can disappear before it is read")
	}
}

// TestTablesRefreshInPlace covers #151.
func TestTablesRefreshInPlace(t *testing.T) {
	js := consoleAsset(t, "console.js")

	if !strings.Contains(js, "if (!opts.refetch) return reload();") {
		t.Fatal("the table cannot reload its own rows, so a refresh rebuilds the screen")
	}
	// Rebuilding the screen throws away sort, filter, page and scroll and
	// flashes a skeleton over data that was already correct.
	if !strings.Contains(js, "refetch: () => api(") {
		t.Error("no screen supplies a refetch, so the in-place path is never taken")
	}
	// A selection may name rows that no longer exist after a reload.
	if !strings.Contains(js, "selected = new Set([...selected].filter((n) => names.has(n)));") {
		t.Error("a refresh keeps selections for rows that are gone")
	}
}

// TestTheTableSurfaceCanActuallyClip covers #161.
//
// A bordered table with collapsed borders cannot clip its cells, so the
// radius was painted over by the children and the header had nothing to stick
// against.
func TestTheTableSurfaceCanActuallyClip(t *testing.T) {
	css := consoleAsset(t, "console.css")

	if !strings.Contains(css, "table { border-radius: 0; border: 0; }") {
		t.Error("the table element still carries a radius it cannot apply")
	}
	if !strings.Contains(css, "thead th {\n  position: sticky;") {
		t.Error("column headers do not stick while the body scrolls")
	}
}

// TestEveryDialogSharesOneModalShell holds the focus behaviour in one place.
//
// The create dialog returned focus to the main region rather than to the
// button that opened it, and nothing stopped Tab walking off the dialog into
// the table behind. Fixing that in one dialog and not the other is how the
// console ends up with a dialog that traps focus and a dialog that does not.
func TestEveryDialogSharesOneModalShell(t *testing.T) {
	src := consoleAsset(t, "console.js")

	if !strings.Contains(src, "function openModal({ labelledBy") {
		t.Fatal("openModal is missing; every dialog is supposed to be built from it")
	}
	for _, want := range []string{
		`const opener = document.activeElement;`,
		`if (opener && opener.isConnected && opener.focus) opener.focus();`,
		`if (e.key !== "Tab") return;`,
		`for (const n of inerted) n.inert = true;`,
		`for (const n of inerted) n.inert = false;`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("openModal is missing %q", want)
		}
	}

	// inert set on an ancestor cannot be cancelled on a descendant, so the
	// live region must never be marked: an announcement made from inside a
	// dialog would be dropped from the accessibility tree.
	if !strings.Contains(src, `n.id !== "live"`) {
		t.Error("the live region is not exempt from inert, so announcements made " +
			"while a dialog is open are silently dropped")
	}

	// Both dialogs, and nothing else, build their overlay.
	shells := strings.Count(src, `class: "modal", role: "dialog"`)
	if shells != 1 {
		t.Errorf("found %d places building a .modal overlay; there should be exactly "+
			"one, inside openModal", shells)
	}
	for _, fn := range []string{"function openCreateForm", "function confirmDestructive"} {
		body := functionBody(t, src, fn)
		if !strings.Contains(body, "openModal({") {
			t.Errorf("%s does not use openModal, so its focus behaviour is its own", fn)
		}
	}
}

// TestValidationErrorsBelongToTheirField.
//
// The form had no novalidate, so the browser's own bubble fired first and
// vanished on the next keystroke; the in-page path wrote into a single banner
// at the top of the dialog and never marked the input. With four fields in the
// Spanner form, "Instance ID is not valid" at the top makes the user hunt for
// which box is wrong.
func TestValidationErrorsBelongToTheirField(t *testing.T) {
	src := consoleAsset(t, "console.js")
	css := consoleAsset(t, "console.css")

	for _, want := range []string{
		`novalidate: true`,
		`class: "form-field-error"`,
		`entry.control.setAttribute("aria-invalid", "true")`,
		`entry.control.addEventListener("blur", () => check(entry));`,
		`[entry.helpId, entry.errorId].filter(Boolean).join(" ")`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	// A pattern failure says what the rule is, rather than that a rule exists.
	if !strings.Contains(src, "if (control.validity.patternMismatch)") {
		t.Error("a pattern failure does not show the field's help text as its message")
	}
	for _, want := range []string{".form-field-error", ".form-row.is-invalid input"} {
		if !strings.Contains(css, want) {
			t.Errorf("console.css has no %s rule, so a failing field is not marked", want)
		}
	}
}

// TestRequiredFieldsAreMarkedInTheForm.
//
// Cloud Run's, Spanner's and Bigtable's forms each mix required and optional
// fields, and the only way to learn which was which was to submit.
func TestRequiredFieldsAreMarkedInTheForm(t *testing.T) {
	src := consoleAsset(t, "console.js")

	if !strings.Contains(src, `class: "required-mark", "aria-hidden": "true", text: "*"`) {
		t.Error("required labels carry no marker")
	}
	if !strings.Contains(src, `class: "form-required-note"`) {
		t.Error("nothing says what the marker means")
	}
	// Read from the control, not from the decoration.
	if !strings.Contains(src, "required: f.required") {
		t.Error("the input lost its own required attribute, which is what " +
			"assistive technology reads")
	}
	if !strings.Contains(consoleAsset(t, "console.css"), ".required-mark") {
		t.Error("console.css has no .required-mark rule")
	}
}

// TestCreateFormsRenderTheControlTheTypeCallsFor.
//
// Every field was an <input>, so Spanner's CREATE TABLE statement was typed
// into a single line and a boolean could not be expressed at all.
func TestCreateFormsRenderTheControlTheTypeCallsFor(t *testing.T) {
	src := consoleAsset(t, "console.js")

	for _, want := range []string{
		`f.type === "textarea"`,
		`el("textarea", { id, name: f.name`,
		`isCheck ? String(e.control.checked) : e.control.value`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	// A pattern on a control that cannot enforce one is a constraint the form
	// claims and nothing applies.
	if !strings.Contains(src, "pattern: isCheck || isArea ? null : (f.pattern || null)") {
		t.Error("a checkbox or textarea can still be given a pattern attribute")
	}
}

// TestInFlightWorkIsVisible.
//
// A create blocks on a revision becoming ready and a Spanner create waits on
// two long-running operations; the server allows each request 60 seconds. For
// that whole minute the console showed nothing at all, so a user could not
// tell a slow deploy from a hung one and clicked again — and a second click
// issued a second request.
func TestInFlightWorkIsVisible(t *testing.T) {
	src := consoleAsset(t, "console.js")
	css := consoleAsset(t, "console.css")
	html := consoleAsset(t, "index.html")

	if !strings.Contains(html, `id="busy-bar"`) {
		t.Error("there is no indeterminate bar under the toolbar")
	}
	if !strings.Contains(src, `bar.hidden = !ops.some((o) => o.state === "running")`) {
		t.Error("nothing drives the busy bar from the outstanding operation count")
	}
	// A busy button keeps its label.
	if !strings.Contains(src, "function setBusy(button, busy)") ||
		!strings.Contains(src, `button.prepend(spinner())`) {
		t.Error("setBusy is missing, so a submitting button looks idle")
	}
	// Running is not a warning.
	if strings.Contains(src, `op.state === "failed" ? "error" : "warn"`) {
		t.Error("a running operation still renders as the amber the tables use " +
			"for Paused, which reads as something having gone wrong")
	}
	if !strings.Contains(src, `op.state === "running"`) ||
		!strings.Contains(src, `class: "status is-working"`) {
		t.Error("a running operation carries no moving indicator")
	}
	// The affected row shows it too, and cannot be acted on twice.
	for _, want := range []string{
		"const operating = new Set();",
		"const rowBusy = (name) => ({",
		`busy ? "is-operating" : ""`,
		`class: "icon-button overflow-trigger", disabled: true,`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q, so the affected row shows nothing", want)
		}
	}
	for _, want := range []string{".spinner", ".busy-bar", "tr.is-operating"} {
		if !strings.Contains(css, want) {
			t.Errorf("console.css has no %s rule", want)
		}
	}
	// Every indicator stops moving under reduced motion. The stylesheet has
	// more than one such block, so all of them are searched rather than the
	// last one — a rule is covered wherever it sits.
	reduced := reducedMotionRules(css)
	for _, want := range []string{
		".spinner { animation: none",
		".busy-bar span { animation: none",
		"tr.is-operating td:first-child { animation: none",
	} {
		if !strings.Contains(reduced, want) {
			t.Errorf("%q is not covered by any reduced-motion block", want)
		}
	}
}

// TestADialogWillNotThrowAwayTypedInput.
//
// A click anywhere on the backdrop removed the dialog and everything in it.
func TestADialogWillNotThrowAwayTypedInput(t *testing.T) {
	src := consoleAsset(t, "console.js")

	if strings.Contains(src, `if (e.target === dialog) close(); });`) {
		t.Error("the backdrop still closes unconditionally")
	}
	if !strings.Contains(src, `if (e.target === dialog) close("backdrop");`) {
		t.Error("the backdrop does not tell canClose which dismissal it is")
	}
	for _, want := range []string{
		`if (reason === "backdrop") return false;`,
		"if (!fields.dirty()) return true;",
		`class: "discard-prompt"`,
		"if (submitting) return false;",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
}

// TestALongFormHasAnAddress.
//
// A dialog has no URL, so a refresh, a back button or a copied link lost it.
func TestALongFormHasAnAddress(t *testing.T) {
	src := consoleAsset(t, "console.js")

	if !strings.Contains(src, `path: ${"`+"`"+`}${listing.path}/create`+"`"+`}`) &&
		!strings.Contains(src, "path: `${listing.path}/create`") {
		t.Error("no create route is generated for product listings")
	}
	for _, want := range []string{
		`if (match.screen === "create") return renderCreatePage(view, match);`,
		"async function renderCreatePage(view, route)",
		"function startCreate(route, create, onDone)",
		"if (create.page) return navigate(`${route.path}/create`);",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	// Both entry points go through it, so the two cannot disagree about where
	// a given product's form opens.
	// Every create button on a listing goes through it, so the two entry
	// points cannot disagree about where a given product's form opens. The
	// project picker is the one deliberate exception: it opens the dialog
	// from the toolbar because it acts on what was created — navigating away
	// to a page would abandon the selection it exists to make.
	for _, fn := range []string{"async function renderList(view, route)",
		"function renderTableInto(view, header, data, noun, reload, route, opts = {})"} {
		body := functionBody(t, src, fn)
		if strings.Contains(body, "openCreateForm(") {
			t.Errorf("%s calls openCreateForm directly, so it opens a dialog for a "+
				"form the backend routed to a page", fn)
		}
		if !strings.Contains(body, "startCreate(route, caps.create") {
			t.Errorf("%s has no create button routed through startCreate", fn)
		}
	}
	if n := strings.Count(src, "startCreate(route, caps.create"); n < 3 {
		t.Errorf("only %d create buttons route through startCreate; the action bar, "+
			"the screen's empty state and the emptied table all have one", n)
	}
	// The threshold is the server's.
	if !strings.Contains(consoleSource(t, "console.go"), "createPageThreshold") {
		t.Error("the field-count threshold is not defined on the server")
	}
}

// TestAnEmptiedTableIsNotAnEmptyFilterResult.
//
// Deleting the last row left the table offering to clear a filter nobody had
// typed, under the heading "No matching services" — which says the rows are
// hidden when they are gone.
func TestAnEmptiedTableIsNotAnEmptyFilterResult(t *testing.T) {
	src := consoleAsset(t, "console.js")

	if !strings.Contains(src, "const query = filter.value.trim();\n      const state = query") {
		t.Error("the empty table draws one state regardless of whether a filter is set")
	}
	if !strings.Contains(src, "text: `No ${noun} yet` }),") {
		t.Error("a table with no rows and no filter does not say so in its own words")
	}
}

// reducedMotionRules concatenates every prefers-reduced-motion block.
func reducedMotionRules(css string) string {
	const marker = "@media (prefers-reduced-motion: reduce)"
	var out strings.Builder
	for i := strings.Index(css, marker); i >= 0; {
		rest := css[i:]
		end := strings.Index(rest, "\n}")
		if end < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:end])
		next := strings.Index(rest[end:], marker)
		if next < 0 {
			break
		}
		i += end + next
	}
	return out.String()
}

// consoleSource reads a Go source file from the package directory.
func consoleSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// functionBody returns the source of one top-level function, from its opening
// line to the first line that closes it at column zero.
func functionBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("%s not found in console.js", decl)
	}
	rest := src[i:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestTheNotificationsPanelIsAViewOfTheServerLedger.
//
// There were two independent ledgers. The panel was fed by an in-memory array
// nothing seeded, so reloading the page erased the record of what had just
// happened while the same operations were still sitting in Activity — the two
// surfaces could flatly contradict each other.
func TestTheNotificationsPanelIsAViewOfTheServerLedger(t *testing.T) {
	src := consoleAsset(t, "console.js")
	html := consoleAsset(t, "index.html")

	if !strings.Contains(src, "async function refreshOperations()") ||
		!strings.Contains(src, "/api/operations?project=") {
		t.Fatal("the panel never reads the server's operations")
	}
	// Populated before it is ever opened, or a reload shows an empty bell
	// beside a full Activity screen.
	main := functionBody(t, src, "async function main()")
	if !strings.Contains(main, "await refreshOperations()") {
		t.Error("the panel is not seeded at startup, so a reload empties it")
	}
	if !strings.Contains(src, `onOpen: () => { refreshOperations().then(markOperationsSeen); }`) {
		t.Error("opening the panel neither refreshes it nor marks it seen")
	}
	// Keyed by the operation id the server returned, or the same operation
	// appears twice.
	for _, want := range []string{
		"function mergedOperations()",
		"const known = new Set(fromServer.map((o) => o.id));",
		"op.succeeded(res.name, res.operation);",
		`op.succeeded("", res.operation);`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	// Each row carries when, and a failure carries why and where to look.
	for _, want := range []string{
		"function relativeTime(date)",
		`text: relativeTime(op.at)`,
		"href: `/logs?operation=${encodeURIComponent(op.id)}`",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	// The badge counts what has not been looked at, not what is running.
	if !strings.Contains(src, "ops.filter((o) => !SEEN_OPERATIONS.has(o.key)).length") {
		t.Error("the badge still counts running operations rather than unseen ones")
	}
	if !strings.Contains(html, `id="notification-stale"`) {
		t.Error("a panel that cannot refresh has nowhere to say so")
	}
	// Activity keeps up with what it is showing.
	if !strings.Contains(src, "function stopActivityPolling()") ||
		!strings.Contains(src, "ACTIVITY_TIMER = setTimeout(() => renderActivity(view)") {
		t.Error("Activity still fetches once, so a running operation stays RUNNING forever")
	}
	if !strings.Contains(functionBody(t, src, "function dispatch(view)"), "stopActivityPolling()") {
		t.Error("the Activity poll is not cleared on a route change")
	}
}

// TestEveryCallIsBounded.
//
// A request that never reaches the server — a wedged tunnel, a dead
// port-forward — left the screen loading with no elapsed time, no cancel and
// no eventual error.
func TestEveryCallIsBounded(t *testing.T) {
	src := consoleAsset(t, "console.js")
	goSrc := consoleSource(t, "console.go")

	for _, want := range []string{
		"const controller = new AbortController();",
		"setTimeout(() => controller.abort(), deadline)",
		"did not answer within",
		"deadline: WRITE_DEADLINE_MS",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("api() is missing %q", want)
		}
	}
	// The escalation: a skeleton that eventually says what it is waiting for.
	if !strings.Contains(src, "STILL_LOADING_MS") ||
		!strings.Contains(src, "`Still loading ${opts.what}…`") {
		t.Error("a skeleton still shimmers forever with nothing to say")
	}
	// A cancel is not reported as a fault the console had.
	if !strings.Contains(src, "const cancelledState = (title, retry)") ||
		strings.Count(src, "isCancelled(err)") < 3 {
		t.Error("cancelling a slow load is reported as a failure of the instance")
	}
	if n := strings.Count(src, "onCancel: () => cancel.abort()"); n < 3 {
		t.Errorf("only %d screens offer a way out of a slow load; the dashboard, "+
			"the list and the detail screen each need one", n)
	}
	// And the server side: every read bounded, not only the listing.
	if strings.Count(goSrc, "context.WithTimeout(r.Context(), readBudget)") < 4 {
		t.Error("a read handler still passes the request's own context straight " +
			"through, so a wedged provider hangs the screen")
	}
	if !strings.Contains(goSrc, "did not answer in time") {
		t.Error("a deadline would reach the screen as Go's own wording")
	}
}

// TestAScriptFailureIsVisible.
//
// A throw inside a render function left whatever was last painted — usually
// a skeleton — on screen for good. A hung backend and a broken script looked
// identical, and both looked like loading.
func TestAScriptFailureIsVisible(t *testing.T) {
	src := consoleAsset(t, "console.js")

	for _, want := range []string{
		`window.addEventListener("error"`,
		`window.addEventListener("unhandledrejection"`,
		"function screenFailed(err, what",
		"function shellFailed(err)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	// route() dispatches to async renders whose promises nobody awaited.
	route := functionBody(t, src, "function route()")
	if !strings.Contains(route, "pending.then(syncStickyOffsets, (err) => screenFailed(err, title()))") {
		t.Error("a render that rejects still fails silently")
	}
	if !strings.Contains(route, "} catch (err) {") {
		t.Error("a render that throws synchronously still fails silently")
	}
	// The two startup failures are reported rather than discarded.
	main := functionBody(t, src, "async function main()")
	if strings.Contains(main, "} catch {") {
		t.Error("main() still swallows a startup failure in a comment-only catch")
	}
}

// TestTheLogStreamStatesItsOwnHealth.
//
// An empty table under a muted "reconnecting…" reads as "there are no logs",
// which sends a developer to debug their own application when the console has
// in fact lost the stream. Logs are where people go when something is already
// wrong, so it is the worst place to be ambiguous.
func TestTheLogStreamStatesItsOwnHealth(t *testing.T) {
	src := consoleAsset(t, "console.js")
	goSrc := consoleSource(t, "logs.go")

	logs := functionBody(t, src, "async function renderLogs(view)")
	for _, want := range []string{
		`"No entries match these filters"`,
		`"The log stream is not connected"`,
		`setStatus("streaming", "ok")`,
		"`reconnecting (attempt ${attempt})`",
		`setStatus("disconnected", "error")`,
		`setStatus("stalled — no data for 45s", "error")`,
		"stream.readyState === EventSource.CLOSED",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("renderLogs is missing %q", want)
		}
	}
	// The status is a .status like every other state in the console, not the
	// one muted grey string on the page.
	if strings.Contains(logs, `el("span", { class: "unavailable", text: "connecting…" })`) {
		t.Error("stream health still has no colour")
	}
	// Stalled detection needs a heartbeat the client can actually observe.
	if strings.Contains(goSrc, `": keepalive`) {
		t.Error("the keepalive is still an SSE comment, which EventSource never " +
			"surfaces — so a stalled stream is indistinguishable from an idle one")
	}
	if !strings.Contains(goSrc, `"event: keepalive\ndata: {}\n\n"`) {
		t.Error("the stream sends no observable heartbeat")
	}
	if !strings.Contains(logs, `stream.addEventListener("keepalive", heard)`) {
		t.Error("the client does not listen for the heartbeat")
	}
}

// TestTheLogsScreenHonoursAnOperationScope.
//
// A failed operation in Activity links to /logs?operation=<id>, and the server
// has always supported the filter — the client simply never read it, so the
// one path built to explain a failure landed on the unfiltered stream of the
// whole instance.
func TestTheLogsScreenHonoursAnOperationScope(t *testing.T) {
	src := consoleAsset(t, "console.js")

	logs := functionBody(t, src, "async function renderLogs(view)")
	for _, want := range []string{
		`params.get("operation")`,
		`if (operation) query.set("operation", operation);`,
		`class: "chip"`,
		`url.searchParams.delete("operation");`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("renderLogs is missing %q", want)
		}
	}
	if !strings.Contains(consoleAsset(t, "console.css"), ".chip {") {
		t.Error("console.css has no chip rule")
	}
}

// TestAFailedPollKeepsTheLastGoodReading.
//
// One transient blip erased a working reading and put an error in its place,
// so the panel flickered between numbers and a failure message on a cluster
// that was fine.
func TestAFailedPollKeepsTheLastGoodReading(t *testing.T) {
	src := consoleAsset(t, "console.js")

	for _, want := range []string{
		"let LAST_METRICS = null;",
		"if (m.unavailable && !LAST_METRICS)",
		"const shown = m.unavailable ? LAST_METRICS.data : m;",
		"`Last reading ${relativeTime(at)} — refresh failed: ${m.unavailable}`",
		`target.classList.toggle("is-stale", Boolean(m.unavailable));`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("renderMetrics is missing %q", want)
		}
	}
	// Freshness that actually ages.
	if strings.Contains(src, "`Updated ${collected}`") {
		t.Error("the panel still shows a static clock time, which looks equally " +
			"fresh at five seconds and five minutes old")
	}
	// One listener, not one per navigation.
	if !strings.Contains(src, "function installVisibilityPause()") {
		t.Error("polling continues against a hidden tab")
	}
	if strings.Count(src, `addEventListener("visibilitychange"`) != 1 {
		t.Error("the visibility listener is registered more than once, so a " +
			"navigation leaves one behind")
	}
	if !strings.Contains(functionBody(t, src, "function dispatch(view)"), "METRICS_TICK = null;") {
		t.Error("a route change leaves the dashboard's poll resumable from a " +
			"screen that is no longer on display")
	}
	if !strings.Contains(consoleAsset(t, "console.css"), ".is-stale .meter") {
		t.Error("a stale reading is not dimmed")
	}
}

// themeBlocks returns the three places a token has to be defined: the light
// root, the forced-dark root, and the system-dark media query. "Same as
// device" removes data-theme entirely, so the media query is the default
// path, not an afterthought.
func themeBlocks(t *testing.T, css string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, start := range map[string]string{
		"light":       ":root {",
		"forced dark": `:root[data-theme="dark"] {`,
		"system dark": `:root:not([data-theme="light"]) {`,
	} {
		i := strings.Index(css, start)
		if i < 0 {
			t.Fatalf("theme block %s (%q) not found", name, start)
		}
		rest := css[i:]
		end := strings.Index(rest, "\n  }")
		if alt := strings.Index(rest, "\n}"); alt >= 0 && (end < 0 || alt < end) {
			end = alt
		}
		out[name] = rest[:end]
	}
	return out
}

// TestBadgesCarryAnOnColour.
//
// .local-badge and .badge set a literal #fff against container colours the
// same stylesheet redefines per theme. With the default "Same as device"
// theme on a dark-mode machine the LOCAL badge was white on #fdd663 — about
// 1.4:1, on the one indicator the parity spec requires to be readable at
// every viewport.
func TestBadgesCarryAnOnColour(t *testing.T) {
	css := consoleAsset(t, "console.css")

	for name, block := range themeBlocks(t, css) {
		for _, token := range []string{"--on-warn:", "--on-error:"} {
			if !strings.Contains(block, token) {
				t.Errorf("%s is missing %s, so a badge in that theme has no legible foreground",
					name, token)
			}
		}
	}
	for _, want := range []string{
		"background: var(--warn); color: var(--on-warn);",
		"background: var(--error); color: var(--on-error);",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("console.css is missing %q", want)
		}
	}
	// The per-theme patch is what the token replaces.
	if strings.Contains(css, `:root[data-theme="dark"] .local-badge`) {
		t.Error("the one-off dark correction is still there beside the token that replaces it")
	}
}

// TestControlsAcknowledgeBeingPressed.
//
// Nothing in the app acknowledged mouse-down, so a click on a row, a nav row
// or a toolbar button gave no confirmation until the screen changed. Hover
// was also expressed three different ways — a swapped surface colour, a tonal
// color-mix, and a border colour used as a background.
func TestControlsAcknowledgeBeingPressed(t *testing.T) {
	css := consoleAsset(t, "console.css")

	for name, block := range themeBlocks(t, css) {
		for _, token := range []string{"--state-hover:", "--state-press:"} {
			if !strings.Contains(block, token) {
				t.Errorf("%s is missing %s", name, token)
			}
		}
	}
	if n := strings.Count(css, ":active"); n < 10 {
		t.Errorf("only %d pressed states in the whole stylesheet", n)
	}
	for _, want := range []string{
		"tbody tr:active { background: var(--state-press); }",
		"#nav a:active { background: var(--state-press); }",
		".icon-button:active { background: var(--state-press); }",
		"button.primary:active",
		"button.secondary:active",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("console.css has no %q", want)
		}
	}
	// A border colour used as a background was the odd one out.
	if strings.Contains(css, ".nav-pin:hover { background: var(--border)") {
		t.Error(".nav-pin still fills with a border colour on hover")
	}
	// Container and label, not one blanket opacity over both.
	if strings.Contains(css, "button[disabled] { opacity: .5") {
		t.Error("disabled is still a blanket opacity, which fades a filled button " +
			"into looking like an enabled one rather than a disabled control")
	}
	if !strings.Contains(css, "--disabled-container:") || !strings.Contains(css, "--disabled-label:") {
		t.Error("there are no disabled container/label tokens")
	}
}

// TestOneScrimAndAThemedDialogShadow.
//
// The drawer dimmed the page with rgba(32,33,36,.4) and the dialog with
// rgba(0,0,0,.45) — the same role, two values — and the dialog was the only
// surface whose shadow was never adjusted for dark mode, where 30% black
// against #202124 is not a shadow.
func TestOneScrimAndAThemedDialogShadow(t *testing.T) {
	css := consoleAsset(t, "console.css")

	for name, block := range themeBlocks(t, css) {
		for _, token := range []string{"--scrim:", "--shadow-dialog:"} {
			if !strings.Contains(block, token) {
				t.Errorf("%s is missing %s", name, token)
			}
		}
	}
	if n := strings.Count(css, "background: var(--scrim)"); n != 2 {
		t.Errorf("%d surfaces use the scrim token; the drawer and the dialog are both scrims", n)
	}
	if strings.Contains(css, "box-shadow: 0 8px 32px rgba(0,0,0,.3)") {
		t.Error("the dialog still carries a hardcoded shadow outside the theme system")
	}
}

// TestTypeIsTokenised.
//
// Colour, radius and shadow were tokenised and type was not, so a density
// change meant a manual sweep — and the sweep had already drifted into a
// fractional 12.5px and a lone 13px that existed nowhere else in the file.
func TestTypeIsTokenised(t *testing.T) {
	css := consoleAsset(t, "console.css")

	// Everything above this line is the token definitions themselves.
	body := css[strings.Index(css, "* { box-sizing: border-box; }"):]
	if i := strings.Index(body, "font-size: 1"); i >= 0 {
		t.Errorf("a bare numeric font-size survives the sweep: %q",
			body[i:min(i+40, len(body))])
	}
	if strings.Contains(body, "12.5px") {
		t.Error("the fractional size is still there; it rounds differently per platform")
	}
	for _, token := range []string{
		"--text-title-size:", "--text-heading-size:", "--text-body-size:",
		"--text-caption-size:", "--text-overline-size:", "--text-badge-size:",
	} {
		if !strings.Contains(css, token) {
			t.Errorf("the type scale is missing %s", token)
		}
	}
	// The two radii the file's own comment insists on.
	for _, literal := range []string{"border-radius: 6px", "border-radius: 4px"} {
		if strings.Contains(body, literal) {
			t.Errorf("%q sits beside the radius tokens it contradicts", literal)
		}
	}
}

// TestOverlaysAnimateFromOneScale.
//
// Panels flipped `hidden` with no transition, the four transitions that did
// exist used four unrelated timings, and the reduced-motion block was
// duplicated verbatim.
func TestOverlaysAnimateFromOneScale(t *testing.T) {
	css := consoleAsset(t, "console.css")
	src := consoleAsset(t, "console.js")

	for _, token := range []string{
		"--motion-fast:", "--motion-standard:", "--ease-standard:", "--ease-decelerate:",
	} {
		if !strings.Contains(css, token) {
			t.Errorf("the motion scale is missing %s", token)
		}
	}
	// No bare durations left outside the token definitions.
	body := css[strings.Index(css, "* { box-sizing: border-box; }"):]
	for _, literal := range []string{"transform .2s", "transform .15s", "width .4s"} {
		if strings.Contains(body, literal) {
			t.Errorf("%q is still written at its use site", literal)
		}
	}
	// One reduced-motion block, not six.
	if n := strings.Count(css, "@media (prefers-reduced-motion: reduce)"); n != 1 {
		t.Errorf("%d reduced-motion blocks; two of them were verbatim duplicates "+
			"and the set will diverge the first time one is edited", n)
	}
	reduced := reducedMotionRules(css)
	for _, want := range []string{".panel", ".modal", ".spinner", ".busy-bar span", ".meter-fill"} {
		if !strings.Contains(reduced, want) {
			t.Errorf("%s is not suppressed under reduced motion", want)
		}
	}
	// hidden cannot be transitioned, so the class has to do it.
	for _, want := range []string{
		"function showOverlay(node)",
		"function hideOverlay(node)",
		`requestAnimationFrame(() => node.classList.add("is-open"))`,
		`const overlayOpen = (node) => node.classList.contains("is-open");`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	// And the panels have to go through it rather than flipping the attribute.
	panel := functionBody(t, src, "function initPanel(buttonId, panelId, opts = {})")
	if strings.Contains(panel, "panel.hidden =") {
		t.Error("initPanel still flips hidden directly, so its panel cannot animate")
	}
}

// TestEveryScreenOpensWithThePageHeader.
//
// Logs Explorer, Activity and the playground emitted a bare h1, and the
// playground put the application top bar — sticky, 64px, elevated — inside a
// content card.
func TestEveryScreenOpensWithThePageHeader(t *testing.T) {
	src := consoleAsset(t, "console.js")

	if !strings.Contains(src, "const pageHeader = (title, subtitle)") {
		t.Fatal("there is no shared page header")
	}
	for _, screen := range []string{`pageHeader("Logs Explorer"`, `pageHeader("Activity"`,
		"pageHeader(PLAYGROUND_TITLE"} {
		if !strings.Contains(src, screen) {
			t.Errorf("a screen does not open with the shared header: %s", screen)
		}
	}
	// The application bar is declared in index.html and nowhere else.
	if strings.Contains(src, `class: "toolbar"`) {
		t.Error("a screen still builds a .toolbar, which is the application top bar")
	}
	if !strings.Contains(src, `class: "card-actions"`) {
		t.Error("the playground's control row has no content class of its own")
	}
	// The heading agrees with the route, the nav entry and the browser tab.
	if strings.Contains(src, `text: "AI Playground"`) {
		t.Error("the playground heading still disagrees with its route title")
	}
	if !strings.Contains(src, `const PLAYGROUND_TITLE = "Vertex AI Studio";`) {
		t.Error("the playground has no single definition of its own name")
	}
}

// TestNarrowWidthsDegradeRatherThanDrop.
//
// Below 960px the cross-service search was deleted outright, and
// .table-wrap's horizontal scroll existed only inside that same query — so
// between 960 and 1279px a wide listing dragged the whole page sideways under
// a sticky toolbar.
func TestNarrowWidthsDegradeRatherThanDrop(t *testing.T) {
	css := consoleAsset(t, "console.css")
	src := consoleAsset(t, "console.js")
	html := consoleAsset(t, "index.html")

	// The scroll rule applies everywhere, not only in the narrow query.
	narrow := css[strings.Index(css, "@media (max-width: 959px)"):]
	narrow = narrow[:strings.Index(narrow, "\n}")]
	if strings.Contains(narrow, ".table-wrap") {
		t.Error("a table's horizontal scroll is still scoped to narrow widths, " +
			"so a wide listing drags the page sideways on a laptop")
	}
	if !strings.Contains(css, "overflow: auto; overflow-x: auto;") {
		t.Error(".table-wrap does not scroll horizontally at every width")
	}
	// Search collapses to a control rather than vanishing.
	if !strings.Contains(html, `id="search-toggle"`) {
		t.Error("there is no narrow-width search control")
	}
	for _, want := range []string{
		"function initSearchToggle()",
		`root.setAttribute("data-search", "open")`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	if !strings.Contains(css, `:root[data-search="open"] .search`) {
		t.Error("the collapsed search has no expanded state")
	}
	// The nav state nothing could ever enter is gone, along with the promise.
	if strings.Contains(css, `data-nav="collapsed"`) {
		t.Error("the stylesheet still carries rules for a nav state applyNavState " +
			"can never set, which is a trap for the next person auditing the drawer")
	}
	if strings.Contains(consoleSource(t, "../../docs/console-parity.md"),
		"| 960–1279px | Navigation collapsed to icons") {
		t.Error("the parity table still promises an icon rail that does not exist")
	}
}

// TestTheShellHasNoDuplicateIDs.
//
// A duplicate id makes getElementById pick one of two controls and leaves the
// other dead, and the markup is hand-written, so nothing else would catch it.
// Shipped one by accident while adding the narrow-width search control.
func TestTheShellHasNoDuplicateIDs(t *testing.T) {
	html := consoleAsset(t, "index.html")

	seen := map[string]int{}
	for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		seen[m[1]]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("id %q appears %d times; getElementById picks one and the "+
				"other control is dead", id, n)
		}
	}
}

// TestADetailScreenSaysWhatTheResourceIs.
//
// The detail header carried the name and then went straight to the child
// listing, so the fields the list row had already shown — a Cloud SQL
// database's Owner, Encoding and Size — were dropped on click-through. Drill-
// down answered "what is inside this" and lost "what is this", which is the
// question the screen's own title implies.
func TestADetailScreenSaysWhatTheResourceIs(t *testing.T) {
	src := consoleAsset(t, "console.js")
	goSrc := consoleSource(t, "console.go")

	// The provider supplies the properties, so a deep link shows the same
	// page as a click-through.
	for _, want := range []string{"type Property struct", "Summary []Property"} {
		if !strings.Contains(goSrc, want) {
			t.Errorf("console.go is missing %q", want)
		}
	}
	if !strings.Contains(src, `class: "card properties"`) {
		t.Error("the detail screen renders no properties card")
	}
	// A provider holding none renders no card rather than an empty one.
	if !strings.Contains(src, "(data.summary || []).length") {
		t.Error("the card is rendered unconditionally, so a provider with no " +
			"extra properties gets an empty one")
	}
}

// TestDetailScreensHaveTabs.
//
// Driller returned one Listing, so every resource had exactly one aspect and
// there was nowhere to put a second live view without inventing a route.
func TestDetailScreensHaveTabs(t *testing.T) {
	src := consoleAsset(t, "console.js")
	css := consoleAsset(t, "console.css")
	goSrc := consoleSource(t, "console.go")

	if !strings.Contains(goSrc, "Detail(ctx context.Context, project, name string) (Detail, error)") {
		t.Fatal("Driller still returns a single Listing, so a resource has one aspect")
	}
	if !strings.Contains(goSrc, "type Section struct") {
		t.Error("there is no Section type")
	}
	for _, want := range []string{
		`role: "tablist"`,
		`role: "tab", id: ` + "`tab-${section.id}`",
		`role: "tabpanel"`,
		`"aria-selected": i === current ? "true" : "false"`,
		`tabindex: i === current ? "0" : "-1"`,
		`e.key === "ArrowRight"`,
		`url.searchParams.set("tab", sections[i].id)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the tab strip is missing %q", want)
		}
	}
	// A strip of one is a control that does nothing.
	if !strings.Contains(src, "if (sections.length > 1)") {
		t.Error("a provider with one section still gets a tab strip")
	}
	// A section that cannot be read says so in its own panel.
	if !strings.Contains(src, "if (list.unavailable)") {
		t.Error("a failed section would render as an empty table, which claims " +
			"the resource holds nothing")
	}
	if !strings.Contains(css, ".tab-strip") || !strings.Contains(css, ".tab.is-selected") {
		t.Error("console.css has no tab strip rules")
	}
}

// TestThePageHeaderAndColumnHeadersStayPut.
//
// Scrolling a listing took the title, the breadcrumb and Create/Refresh off
// screen with it: the two things a reader needs while scrolling a table were
// the two that disappeared.
func TestThePageHeaderAndColumnHeadersStayPut(t *testing.T) {
	css := consoleAsset(t, "console.css")
	src := consoleAsset(t, "console.js")

	if !strings.Contains(css, "position: sticky; top: var(--toolbar-h); z-index: 15;") {
		t.Error("the page header does not pin under the toolbar")
	}
	if !strings.Contains(css, "top: calc(var(--toolbar-h) + var(--page-header-h, 0px));") {
		t.Error("the action bar does not pin under the page header")
	}
	// The offsets are measured, because the header's height depends on
	// whether the screen has a breadcrumb and a tab strip.
	if !strings.Contains(src, "function syncStickyOffsets()") ||
		!strings.Contains(src, `root.style.setProperty("--page-header-h"`) {
		t.Error("the sticky offsets are assumed rather than measured")
	}
	if !strings.Contains(src, `window.addEventListener("resize", syncStickyOffsets)`) {
		t.Error("the offsets are never remeasured, so a resize leaves a gap or an overlap")
	}
	// A pinned header needs an opaque background, or rows show through.
	for _, want := range []string{
		"position: sticky; top: 0; z-index: 1;",
		"background: var(--surface);",
		"box-shadow: inset 0 -1px 0 0 var(--border);",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the sticky column header is missing %q", want)
		}
	}
}

// TestActivityUsesTheSharedTableRenderer.
//
// Activity hand-built its own table with a literal header array and got none
// of the grid behaviour — no sort, no filter, no paging — on the screen a
// developer opens after something failed, which has the most rows of any.
func TestActivityUsesTheSharedTableRenderer(t *testing.T) {
	src := consoleAsset(t, "console.js")

	body := functionBody(t, src, "async function renderActivity(view)")
	if strings.Contains(body, `el("thead"`) || strings.Contains(body, `el("tbody"`) {
		t.Error("Activity still builds its own table, so every table improvement " +
			"will keep skipping it")
	}
	if !strings.Contains(body, "renderTableInto(view, header, listing") {
		t.Error("Activity does not go through the shared renderer")
	}
	// The failed row still reaches its own logs.
	if !strings.Contains(body, "/logs?operation=${encodeURIComponent(op.id)}") {
		t.Error("a failed operation no longer links to its logs")
	}
}
