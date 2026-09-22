package console

import (
	"os"
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
