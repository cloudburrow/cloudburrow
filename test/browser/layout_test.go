//go:build browser

package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// viewports is the table in docs/console-parity.md §5, one width from each
// row: what the drawer does when the page loads at that width, and whether
// the cross-service search is a field or a control in the toolbar.
var viewports = []struct {
	width  int64
	nav    string // "docked" or "closed", as data-nav reads on load
	search string // "field" or "control"
}{
	{1280, "docked", "field"},
	{1024, "closed", "field"},
	{800, "closed", "control"},
	{360, "closed", "control"},
}

// TestViewportsDrawerBadgeAndTableScroll renders the Cloud Scheduler list at
// 1280, 1024, 800 and 360px (parity §5, §7 "Rendered at each viewport") and,
// at each width, asserts the drawer's state, that the LOCAL badge and the
// navigation are painted, and that the page never scrolls sideways while a
// wide table scrolls inside its own region.
//
// "Painted" is read from the pixels Chrome drew, not from the DOM: a region
// of the screenshot where the badge is must be mostly the badge's own
// background with its text on it, and a navigation link's label and icon
// must put ink on the drawer. docs/console-verification.md records a drawer
// whose DOM was correct while nothing was drawn; a DOM check passes on that,
// this does not.
//
// The table is made wide by its data rather than by the test: one job whose
// ID is 200 characters with no break opportunity, so its row is wider than
// any of the four widths.
func TestViewportsDrawerBadgeAndTableScroll(t *testing.T) {
	needService(t, "scheduler")
	project := uniqueProject(t)
	wide := "browser_" + strings.Repeat("wide_", 40)
	job := createSchedulerJob(t, project, wide)

	for _, vp := range viewports {
		t.Run(fmt.Sprintf("%dpx", vp.width), func(t *testing.T) {
			p := open(t)
			p.run(chromedp.EmulateViewport(vp.width, 800), reducedMotion())
			p.navigate("/scheduler/jobs?project=" + project)
			p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`,
				fmt.Sprintf(`#view .table-wrap tr[aria-label=%q]`, "Inspect "+job)))

			// The LOCAL badge, at every width (parity §2).
			p.assertBadgePainted()

			// The page never scrolls sideways; the table's own region does.
			var scroll struct {
				DocScroll, DocClient, PageX       float64
				WrapScroll, WrapClient, WrapLeft  float64
				WrapRight, ViewportWidth, WrapTop float64
			}
			p.eval(`(() => {
				window.scrollTo(100000, window.scrollY);
				const doc = document.scrollingElement, w = document.querySelector("#view .table-wrap");
				w.scrollLeft = 100000;
				const r = w.getBoundingClientRect();
				return { DocScroll: doc.scrollWidth, DocClient: doc.clientWidth, PageX: window.scrollX,
				         WrapScroll: w.scrollWidth, WrapClient: w.clientWidth, WrapLeft: w.scrollLeft,
				         WrapRight: r.right, ViewportWidth: window.innerWidth, WrapTop: r.top };
			})()`, &scroll)
			if scroll.DocScroll > scroll.DocClient || scroll.PageX != 0 {
				t.Errorf("the page scrolls sideways: scrollWidth %v, clientWidth %v, scrolled to x=%v", scroll.DocScroll, scroll.DocClient, scroll.PageX)
			}
			if scroll.WrapScroll <= scroll.WrapClient || scroll.WrapLeft <= 0 {
				t.Errorf("the wide table does not scroll in its own region: scrollWidth %v, clientWidth %v, scrolled to %v", scroll.WrapScroll, scroll.WrapClient, scroll.WrapLeft)
			}
			if scroll.WrapRight > scroll.ViewportWidth+0.5 {
				t.Errorf("the table's region ends at x=%v, past the %vpx viewport", scroll.WrapRight, scroll.ViewportWidth)
			}
			p.eval(`(() => { document.querySelector("#view .table-wrap").scrollLeft = 0; return true; })()`, nil)

			p.assertSearch(vp.search)
			p.assertDrawer(vp.nav)
			// The overlays opened above are closed again; the badge is still there.
			p.assertBadgePainted()
		})
	}
}

// TestDialogTakesFocusClosesOnEscapeAndGivesItBack: the Create queue dialog,
// opened from the keyboard, takes focus to its first field, marks itself as
// a modal dialog named by its heading, makes the page behind it inert and
// keeps Tab and Shift+Tab inside it. Escape closes it without sending
// anything, and focus returns to the button that opened it (parity §5,
// Accessibility).
func TestDialogTakesFocusClosesOnEscapeAndGivesItBack(t *testing.T) {
	needService(t, "tasks")
	p := open(t)
	project := uniqueProject(t)
	p.navigate("/tasks/queues?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create queue")`)

	p.tabTo(`a.matches("#view .state button.primary")`)
	p.eval(`document.activeElement.setAttribute("data-cb-trigger", ""); true`, nil)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)

	var opened struct {
		Role, Modal, Name, Focus string
		FocusInside, MainInert   bool
		BannerInert              bool
	}
	p.eval(`(() => { const d = document.querySelector(".modal"), a = document.activeElement;
		const label = document.getElementById(d.getAttribute("aria-labelledby"));
		return { Role: d.getAttribute("role"), Modal: d.getAttribute("aria-modal"),
		         Name: label ? label.textContent : "", Focus: a ? a.id : "",
		         FocusInside: d.contains(a), MainInert: document.getElementById("main").closest("[inert]") !== null,
		         BannerInert: document.querySelector('[role="banner"]').inert }; })()`, &opened)
	if opened.Role != "dialog" || opened.Modal != "true" || opened.Name != "Create queue" {
		t.Errorf("the dialog is role=%q aria-modal=%q named %q; want a modal dialog named Create queue", opened.Role, opened.Modal, opened.Name)
	}
	if !opened.FocusInside || opened.Focus != "f-name" {
		t.Errorf("focus did not move into the dialog's first field: on #%s, inside the dialog %v", opened.Focus, opened.FocusInside)
	}
	if !opened.MainInert || !opened.BannerInert {
		t.Errorf("the page behind the dialog is not inert: main %v, banner %v", opened.MainInert, opened.BannerInert)
	}

	// Tab and Shift+Tab stay inside, however far they go.
	inside := `document.querySelector(".modal").contains(document.activeElement)`
	for i := 0; i < 8; i++ {
		p.run(chromedp.KeyEvent(kb.Tab))
		var ok bool
		p.eval(inside, &ok)
		if !ok {
			t.Fatalf("Tab %d took focus out of the dialog", i+1)
		}
	}
	for i := 0; i < 8; i++ {
		p.run(chromedp.KeyEvent(kb.Tab, chromedp.KeyModifiers(input.ModifierShift)))
		var ok bool
		p.eval(inside, &ok)
		if !ok {
			t.Fatalf("Shift+Tab %d took focus out of the dialog", i+1)
		}
	}

	p.run(chromedp.KeyEvent(kb.Escape))
	p.waitFor(`document.querySelector(".modal") === null`)
	var after struct {
		Trigger   bool
		MainInert bool
		Focus     string
	}
	p.eval(`({ Trigger: document.activeElement.hasAttribute("data-cb-trigger"),
	           MainInert: document.getElementById("main").closest("[inert]") !== null,
	           Focus: document.activeElement.outerHTML.slice(0, 120) })`, &after)
	if !after.Trigger {
		t.Errorf("Escape closed the dialog but focus went to %s, not the Create queue button", after.Focus)
	}
	if after.MainInert {
		t.Error("the page is still inert after the dialog closed")
	}
	if posts := p.posts("/api/resources/tasks"); len(posts) != 0 {
		t.Errorf("closing the dialog with Escape sent a create: %v", posts)
	}
}

// TestRowMenuClosesOnEscapeAndGivesFocusBack: a queue row's actions menu,
// opened from the keyboard, closes on Escape whether focus is on one of its
// items or still on the button that opened it; either way focus ends on that
// button, the button says the menu is collapsed, and nothing is sent (parity
// §5, Accessibility; #771).
func TestRowMenuClosesOnEscapeAndGivesFocusBack(t *testing.T) {
	needService(t, "tasks")
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/locations/us-central1/queues/browser-menu"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/tasks?project="+project, `{"name":"browser-menu","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a queue through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/tasks?project="+project+"&name="+url.QueryEscape(name), "")
	})

	p.navigate("/tasks/queues?project=" + project)
	trigger := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, trigger))
	onTrigger := fmt.Sprintf(`a === document.querySelector(%q)`, trigger)
	menuOpen := fmt.Sprintf(`!document.querySelector(%q).parentElement.querySelector(".overflow-menu").hidden`, trigger)

	type state struct {
		Open      bool
		Expanded  string
		OnTrigger bool
		Focus     string
	}
	read := func() state {
		var s state
		p.eval(fmt.Sprintf(`(() => { const b = document.querySelector(%q), a = document.activeElement;
			return { Open: !b.parentElement.querySelector(".overflow-menu").hidden,
			         Expanded: b.getAttribute("aria-expanded"), OnTrigger: a === b,
			         Focus: a ? a.outerHTML.slice(0, 120) : "" }; })()`, trigger), &s)
		return s
	}
	openFromKeyboard := func() {
		p.tabTo(onTrigger)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(menuOpen)
	}

	// From an item: Tab moves from the trigger into the open menu.
	openFromKeyboard()
	p.run(chromedp.KeyEvent(kb.Tab))
	var inMenu bool
	p.eval(`document.activeElement.getAttribute("role") === "menuitem"`, &inMenu)
	if !inMenu {
		t.Fatalf("Tab from the open menu's trigger did not reach one of its items: %s", read().Focus)
	}
	p.run(chromedp.KeyEvent(kb.Escape))
	if s := read(); s.Open || s.Expanded != "false" || !s.OnTrigger {
		t.Fatalf("Escape on a menu item: menu open %v, aria-expanded %q, focus on %s; want it closed, collapsed and focus on its trigger", s.Open, s.Expanded, s.Focus)
	}

	// From the trigger itself, which keeps focus while the menu is open.
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(menuOpen)
	p.run(chromedp.KeyEvent(kb.Escape))
	if s := read(); s.Open || s.Expanded != "false" || !s.OnTrigger {
		t.Errorf("Escape on the trigger: menu open %v, aria-expanded %q, focus on %s; want it closed, collapsed and focus on its trigger", s.Open, s.Expanded, s.Focus)
	}

	if del := p.sent(http.MethodDelete, "/api/resources/tasks"); len(del) != 0 {
		t.Errorf("closing the menu with Escape sent a delete: %v", del)
	}
	if acts := p.posts("/api/actions/tasks"); len(acts) != 0 {
		t.Errorf("closing the menu with Escape ran an action: %v", acts)
	}
}

// --- layout assertions ----------------------------------------------------

// assertBadgePainted: the LOCAL badge is on screen, not covered, and the
// pixels where it is are its own background with its text drawn on it.
func (p *tab) assertBadgePainted() {
	p.t.Helper()
	if why := p.hitTest(".local-badge"); why != "" {
		p.t.Errorf("the LOCAL badge is not on screen: %s", why)
		return
	}
	var bg string
	p.eval(`getComputedStyle(document.querySelector(".local-badge")).backgroundColor`, &bg)
	want, ok := parseRGB(bg)
	if !ok {
		p.t.Errorf("the LOCAL badge has no background colour to be seen by: %q", bg)
		return
	}
	img := p.capture(".local-badge")
	if img == nil {
		return
	}
	if share := colourShare(img, want); share < 0.3 {
		p.t.Errorf("only %.0f%% of the LOCAL badge's box is painted in its background %s", share*100, bg)
	}
	if ink := inkShare(img); ink < 0.02 {
		p.t.Errorf("the LOCAL badge's text is not painted: %.1f%% of its box differs from its background", ink*100)
	}
}

// assertSearch: the cross-service search is the field itself where it fits,
// and a toolbar control that opens it where it does not (parity §5).
func (p *tab) assertSearch(want string) {
	p.t.Helper()
	var shown struct{ Field, Toggle string }
	p.eval(`({ Field: getComputedStyle(document.querySelector(".search")).display,
	           Toggle: getComputedStyle(document.getElementById("search-toggle")).display })`, &shown)
	switch want {
	case "field":
		if shown.Toggle != "none" {
			p.t.Errorf("the search control is shown (display %s) where the field fits", shown.Toggle)
		}
		if why := p.hitTest("#search"); why != "" {
			p.t.Errorf("the search field is not on screen: %s", why)
		}
	case "control":
		if shown.Field != "none" {
			p.t.Errorf("the search field takes its place in the bar (display %s) below 960px", shown.Field)
		}
		if why := p.hitTest("#search-toggle"); why != "" {
			p.t.Fatalf("the collapsed search control is not on screen: %s", why)
		}
		if img := p.capture("#search-toggle"); img != nil && inkShare(img) < 0.02 {
			p.t.Error("the collapsed search control's icon is not painted")
		}
		p.run(chromedp.Click("#search-toggle", chromedp.ByQuery))
		p.waitFor(`document.documentElement.getAttribute("data-search") === "open" && document.activeElement === document.getElementById("search")`)
		if why := p.hitTest("#search"); why != "" {
			p.t.Errorf("the search field opened from the control is not on screen: %s", why)
		}
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`!document.documentElement.hasAttribute("data-search") && document.activeElement === document.getElementById("search-toggle")`)
	}
}

// assertDrawer: docked, the navigation is painted beside the content with no
// scrim and nothing inert; closed, it is not shown, and the menu button opens
// it as an overlay above a scrim that takes focus in and gives it back on
// Escape.
func (p *tab) assertDrawer(want string) {
	p.t.Helper()
	var state string
	p.eval(`document.documentElement.dataset.nav || ""`, &state)
	if state != want {
		p.t.Fatalf("the drawer loads %q; parity §5 says %q at this width", state, want)
	}
	switch want {
	case "docked":
		var s struct {
			ScrimHidden, MainInert bool
			NavRight, MainLeft     float64
		}
		p.eval(`({ ScrimHidden: document.getElementById("nav-scrim").hidden,
		           MainInert: document.getElementById("main").inert,
		           NavRight: document.getElementById("nav").getBoundingClientRect().right,
		           MainLeft: document.getElementById("main").getBoundingClientRect().left })`, &s)
		if !s.ScrimHidden || s.MainInert {
			p.t.Errorf("a docked drawer sits behind a scrim (%v) or makes the page inert (%v)", !s.ScrimHidden, s.MainInert)
		}
		if s.MainLeft < s.NavRight-0.5 {
			p.t.Errorf("the docked drawer overlaps the content: it ends at x=%v and the content starts at x=%v", s.NavRight, s.MainLeft)
		}
		p.assertNavPainted()
	case "closed":
		var vis string
		p.eval(`getComputedStyle(document.getElementById("nav")).visibility`, &vis)
		if vis != "hidden" {
			p.t.Errorf("the closed drawer is still shown (visibility %s)", vis)
		}
		p.run(chromedp.Click("#nav-toggle", chromedp.ByQuery))
		p.waitFor(`document.documentElement.dataset.nav === "open" && document.getElementById("nav").contains(document.activeElement)`)
		var s struct {
			ScrimHidden, MainInert bool
			Behind                 string
		}
		// A point on the content beside the drawer is the scrim's, not the page's.
		p.eval(`(() => { const n = document.getElementById("nav").getBoundingClientRect();
			const x = Math.min(window.innerWidth - 4, n.right + 8), y = Math.round(window.innerHeight / 2);
			const hit = document.elementFromPoint(x, y);
			return { ScrimHidden: document.getElementById("nav-scrim").hidden,
			         MainInert: document.getElementById("main").inert,
			         Behind: hit ? (hit.id || hit.className || hit.tagName) : "" }; })()`, &s)
		if s.ScrimHidden || !s.MainInert {
			p.t.Errorf("the overlaid drawer has no scrim (%v) or leaves the page interactive (%v)", s.ScrimHidden, !s.MainInert)
		}
		if s.Behind != "nav-scrim" {
			p.t.Errorf("beside the overlaid drawer the page shows %q, not the scrim", s.Behind)
		}
		p.assertNavPainted()
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.documentElement.dataset.nav === "closed" && document.activeElement === document.getElementById("nav-toggle")`)
	}
}

// assertNavPainted: the first navigation link is on screen with its icon and
// its label both drawn.
func (p *tab) assertNavPainted() {
	p.t.Helper()
	p.eval(`(() => { document.querySelectorAll("[data-cb-nav]").forEach((n) => n.removeAttribute("data-cb-nav"));
		const a = [...document.querySelectorAll("#nav-list a")].find((a) => a.getClientRects().length);
		if (a) { a.querySelector(".nav-icon").setAttribute("data-cb-nav", "icon");
		         a.querySelector(".nav-label").setAttribute("data-cb-nav", "label"); }
		return true; })()`, nil)
	for _, part := range []string{"icon", "label"} {
		sel := fmt.Sprintf(`[data-cb-nav=%q]`, part)
		if why := p.hitTest(sel); why != "" {
			p.t.Errorf("the navigation's first link's %s is not on screen: %s", part, why)
			continue
		}
		if img := p.capture(sel); img != nil {
			if ink := inkShare(img); ink < 0.02 {
				p.t.Errorf("the navigation's first link's %s is not painted: %.1f%% of its box differs from the drawer", part, ink*100)
			}
		}
	}
}

// hitTest says why the element sel matches is not on screen, or "" when it
// is: it has a size, is not hidden by style, lies inside the viewport, and the
// point at its centre belongs to it rather than to something drawn over it.
func (p *tab) hitTest(sel string) string {
	p.t.Helper()
	var why string
	p.eval(fmt.Sprintf(`(() => {
		const n = document.querySelector(%q);
		if (!n) return "there is no such element";
		const r = n.getBoundingClientRect(), s = getComputedStyle(n);
		if (r.width < 1 || r.height < 1) return "it has no size (" + r.width + "x" + r.height + ")";
		if (s.display === "none" || s.visibility !== "visible" || Number(s.opacity) === 0) return "it is hidden by style";
		if (r.left < -0.5 || r.top < -0.5 || r.right > innerWidth + 0.5 || r.bottom > innerHeight + 0.5)
			return "it lies outside the viewport, at " + [r.left, r.top, r.right, r.bottom].map(Math.round).join(",");
		const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
		if (!hit || !(hit === n || n.contains(hit))) return "it is covered by " + (hit ? hit.outerHTML.slice(0, 100) : "nothing");
		return "";
	})()`, sel), &why)
	return why
}

// capture is the part of the viewport the element sel matches occupies, as
// Chrome painted it, inset by a pixel so a border or a rounded corner does
// not count as the element's content.
func (p *tab) capture(sel string) image.Image {
	p.t.Helper()
	var r struct{ X, Y, W, H float64 }
	p.eval(fmt.Sprintf(`(() => { const r = document.querySelector(%q).getBoundingClientRect();
		return { X: r.left, Y: r.top, W: r.width, H: r.height }; })()`, sel), &r)
	x, y := math.Ceil(r.X)+1, math.Ceil(r.Y)+1
	w, h := math.Floor(r.X+r.W)-1-x, math.Floor(r.Y+r.H)-1-y
	if w < 2 || h < 2 {
		p.t.Errorf("%s is too small to read its pixels (%vx%v)", sel, r.W, r.H)
		return nil
	}
	var buf []byte
	p.run(chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		buf, err = page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			WithClip(&page.Viewport{X: x, Y: y, Width: w, Height: h, Scale: 1}).
			Do(ctx)
		return err
	}))
	img, err := png.Decode(bytes.NewReader(buf))
	if err != nil {
		p.t.Errorf("decode the screenshot of %s: %v", sel, err)
		return nil
	}
	return img
}

// inkShare is the share of the image's pixels that differ clearly from its
// most common colour: the text or icon drawn on a background.
func inkShare(img image.Image) float64 {
	counts := map[[3]uint8]int{}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			counts[rgbAt(img, x, y)]++
		}
	}
	var bg [3]uint8
	most := -1
	for c, n := range counts {
		if n > most {
			bg, most = c, n
		}
	}
	return shareWhere(img, func(c [3]uint8) bool { return distance(c, bg) > 60 })
}

// colourShare is the share of the image's pixels within a small distance of want.
func colourShare(img image.Image, want [3]uint8) float64 {
	return shareWhere(img, func(c [3]uint8) bool { return distance(c, want) <= 24 })
}

func shareWhere(img image.Image, keep func([3]uint8) bool) float64 {
	b := img.Bounds()
	total, kept := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			total++
			if keep(rgbAt(img, x, y)) {
				kept++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(kept) / float64(total)
}

func rgbAt(img image.Image, x, y int) [3]uint8 {
	r, g, b, _ := img.At(x, y).RGBA()
	return [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)}
}

func distance(a, b [3]uint8) int {
	d := 0
	for i := range a {
		v := int(a[i]) - int(b[i])
		if v < 0 {
			v = -v
		}
		d += v
	}
	return d
}

var rgbRE = regexp.MustCompile(`^rgba?\(\s*(\d+)[,\s]+(\d+)[,\s]+(\d+)\s*(?:[,/]\s*([\d.]+)\s*)?\)$`)

// parseRGB reads a computed colour, which Chrome reports as rgb() or rgba();
// a transparent one is not a colour anything can be seen by.
func parseRGB(s string) ([3]uint8, bool) {
	m := rgbRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [3]uint8{}, false
	}
	if m[4] != "" {
		if a, err := strconv.ParseFloat(m[4], 64); err != nil || a < 0.99 {
			return [3]uint8{}, false
		}
	}
	var out [3]uint8
	for i := 0; i < 3; i++ {
		v, err := strconv.Atoi(m[i+1])
		if err != nil || v > 255 {
			return [3]uint8{}, false
		}
		out[i] = uint8(v)
	}
	return out, true
}

// reducedMotion turns the drawer's and the dialogs' transitions off, so what
// is asserted is where they end rather than a frame on the way.
func reducedMotion() chromedp.Action {
	return emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-reduced-motion", Value: "reduce"}})
}

// needService skips unless this instance's console has the screen: CI runs
// each test in a shard whose instance serves it and fails the step on a skip.
func needService(t *testing.T, id string) {
	t.Helper()
	code, body := consoleDo(t, http.MethodGet, "/api/services", "")
	if code != http.StatusOK {
		t.Fatalf("list the console's services = %d: %s", code, body)
	}
	var s struct{ Services []struct{ ID string } }
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("decode the console's services: %v: %s", err, body)
	}
	for _, sv := range s.Services {
		if sv.ID == id {
			return
		}
	}
	t.Skipf("this instance's console has no %s screen; start it with that service enabled", id)
}

// createSchedulerJob makes a job through the console API that will not fire
// while a test runs: once a year, at a loopback address nothing listens on.
func createSchedulerJob(t *testing.T, project, id string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"name": id, "location": "us-central1", "schedule": "0 0 1 1 *", "timeZone": "Etc/UTC",
		"uri": "http://127.0.0.1:9/cloudburrow-browser",
	})
	if code, resp := consoleDo(t, http.MethodPost, "/api/resources/scheduler?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("create a Cloud Scheduler job through the console API = %d: %s", code, resp)
	}
	name := "projects/" + project + "/locations/us-central1/jobs/" + id
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/scheduler?project="+project+"&name="+url.QueryEscape(name), "")
	})
	return name
}
