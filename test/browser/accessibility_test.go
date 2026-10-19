//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// This file is the console's WCAG 2.1 AA audit (parity §5, Accessibility;
// #1604), run in headless Chrome against rendered pages rather than against
// the assets' text:
//
//   - every interactive node in Chrome's accessibility tree has a name, and
//     every table has column headers (TestEveryControlHasAnAccessibleName);
//   - every text run, placeholder and input boundary meets its contrast
//     minimum in the light and the dark theme (TestContrastInBothThemes);
//   - a page can be walked and used by keyboard alone, with a visible focus
//     indicator on every stop (TestKeyboardAloneWalksAPageAndCreatesATopic,
//     TestToolbarPanelsCloseOnEscapeAndGiveFocusBack);
//   - nothing animates under prefers-reduced-motion (TestReducedMotionStopsMotion);
//   - pages reflow at 320 CSS pixels, which is also 1280px at 400% zoom, and
//     at 200% zoom of a 1280px window (TestReflowAt320pxAnd200PercentZoom).
//
// The other products' screens are accessibility_services_test.go's. The
// BigQuery screens are served only by the emulators shard's instance, so
// their names and contrast are TestBigQueryScreensAreNamedAndLegible.

// a11yFixture is one project holding a bucket with an object, a topic with
// its default subscription and a log entry, so each audited page has rows,
// row menus and tables to measure rather than an empty state.
type a11yFixture struct {
	project, bucket, topic, sub string
}

func newA11yFixture(t *testing.T) a11yFixture {
	t.Helper()
	needService(t, "storage")
	needService(t, "pubsub")
	project := uniqueProject(t)
	f := a11yFixture{
		project: project,
		bucket:  fmt.Sprintf("a11y-%s", strings.TrimPrefix(project, "cb-browser-")),
		topic:   "projects/" + project + "/topics/a11y-topic",
		sub:     "projects/" + project + "/subscriptions/a11y-topic-sub",
	}
	q := "?project=" + project
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+f.bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	uploadObject(t, project, []string{f.bucket}, "notes.txt", "hello\n")
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/pubsub"+q, `{"name":"a11y-topic","defaultSubscription":"true"}`); code != http.StatusOK {
		t.Fatalf("create a topic through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/objects/storage?"+url.Values{"project": {project}, "name": {f.bucket, "notes.txt"}}.Encode(), "")
		consoleDo(t, http.MethodDelete, "/api/resources/storage"+q+"&name="+f.bucket, "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-subscriptions"+q+"&name="+url.QueryEscape(f.sub), "")
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub"+q+"&name="+url.QueryEscape(f.topic), "")
	})
	return f
}

// a11yPage is one audited screen: where it is, what shows it has rendered,
// and what to do once it has (open a dialog or a panel) before it is read.
type a11yPage struct {
	name, path, ready, then string
}

func (f a11yFixture) pages() []a11yPage {
	q := "?project=" + f.project
	return []a11yPage{
		{"dashboard", "/" + q, settled, ""},
		{"products", "/products" + q, settled, ""},
		{"buckets", "/storage/browser" + q, rowReady(f.bucket), ""},
		{"objects", "/storage/browser/" + f.bucket + q, rowReady("notes.txt"), ""},
		{"create bucket", "/storage/browser/create" + q, settled + ` && document.querySelector("#view form input") !== null`, ""},
		{"topics", "/pubsub/topics" + q, rowReady(f.topic), ""},
		{"topic", "/pubsub/topics/a11y-topic" + q, settled, ""},
		{"create topic", "/pubsub/topics" + q, rowReady(f.topic),
			`[...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Create topic").click()`},
		{"subscriptions", "/pubsub/subscriptions" + q, rowReady(f.sub), ""},
		{"subscription", "/pubsub/subscriptions/a11y-topic-sub" + q, settled, ""},
		{"logs", "/logs" + q, settled, ""},
		{"activity", "/activity" + q, settled, ""},
		{"requests", "/requests" + q, settled, ""},
		{"connect", "/connect" + q, settled, ""},
		{"settings panel", "/" + q, settled, `document.querySelector("#settings").click()`},
		{"project picker", "/" + q, settled, `document.querySelector("#project-button").click()`},
		{"row menu", "/pubsub/topics" + q, rowReady(f.topic),
			`document.querySelector("#view tbody tr .overflow-trigger").click()`},
		{"delete dialog", "/storage/browser" + q, rowReady(f.bucket),
			`document.querySelector("#view tbody tr .overflow-trigger").click();
			 [...document.querySelectorAll("#view .overflow-item")].find((b) => b.textContent.trim() === "Delete").click()`},
		{"info panel", "/pubsub/topics" + q, rowReady(f.topic),
			`document.querySelector("#view tbody tr[role=button]").click()`},
		{"storage settings", "/storage/settings" + q, settled, ""},
		{"snapshots", "/pubsub/snapshots" + q, settled, ""},
		{"schemas", "/pubsub/schemas" + q, settled, ""},
		{"notifications", "/" + q, settled, `document.querySelector("#notifications").click()`},
		{"account", "/" + q, settled, `document.querySelector("#account").click()`},
		{"search", "/" + q, settled,
			`const s = document.querySelector("#search"); s.focus(); s.value = "a11y"; s.dispatchEvent(new Event("input", { bubbles: true }))`},
	}
}

// settled is true once a screen has its heading and no loading skeleton.
const settled = `document.querySelector("#view h1") !== null && document.querySelector("#view .skeleton") === null`

// rowReady is true once a list's table shows a row naming s.
func rowReady(s string) string {
	return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, s)
}

// show navigates to pg and waits for it, then performs its action and waits
// for what it opens to finish animating in.
func (p *tab) show(pg a11yPage) {
	p.t.Helper()
	p.navigate(pg.path)
	p.waitFor(pg.ready)
	if pg.then != "" {
		p.eval(`(() => { `+pg.then+`; return true; })()`, nil)
	}
	// What it opened has finished fading or sliding in, so it is measured as
	// it is seen rather than part-way to opaque.
	// An overlay is made visible a frame after it is shown, so two frames
	// pass before its transition, if any, is looked for.
	p.eval(`new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r(true))))`, nil, awaitPromise)
	p.waitFor(`document.getAnimations().every((a) => a.playState !== "running" || a.effect.getTiming().iterations === Infinity)`)
}

// interactiveRoles are the ARIA roles a user operates, so each needs a name
// (WCAG 4.1.2).
var interactiveRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true, "combobox": true,
	"checkbox": true, "radio": true, "switch": true, "slider": true, "spinbutton": true,
	"menuitem": true, "menuitemcheckbox": true, "menuitemradio": true, "tab": true,
	"option": true, "listbox": true, "treeitem": true,
}

// unnamedControls reads Chrome's accessibility tree and describes every
// interactive node it exposes without a name, and every table it exposes
// without a column header.
func (p *tab) unnamedControls() []string {
	p.t.Helper()
	nodes, err := p.axTree()
	if err != nil {
		p.t.Fatalf("read the accessibility tree: %v", err)
	}
	byID := map[accessibility.NodeID]*accessibility.Node{}
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	var out []string
	for _, n := range nodes {
		if n.Ignored || n.Role == nil {
			continue
		}
		role := strings.Trim(string(n.Role.Value), `"`)
		switch {
		case interactiveRoles[role]:
			if axName(n) == "" {
				out = append(out, role+" "+p.describe(n.BackendDOMNodeID))
			}
		case role == "table" || role == "grid":
			if !hasDescendantRole(n, byID, "columnheader", 0) {
				out = append(out, role+" with no column header "+p.describe(n.BackendDOMNodeID))
			}
		}
	}
	return out
}

func axName(n *accessibility.Node) string {
	if n.Name == nil {
		return ""
	}
	return strings.TrimSpace(strings.Trim(string(n.Name.Value), `"`))
}

func hasDescendantRole(n *accessibility.Node, byID map[accessibility.NodeID]*accessibility.Node, role string, depth int) bool {
	if depth > 6 {
		return false
	}
	for _, id := range n.ChildIDs {
		c := byID[id]
		if c == nil {
			continue
		}
		if c.Role != nil && strings.Trim(string(c.Role.Value), `"`) == role {
			return true
		}
		if hasDescendantRole(c, byID, role, depth+1) {
			return true
		}
	}
	return false
}

// describe names a DOM node for a failure message: its tag, id, class and
// a little of its markup.
func (p *tab) describe(id cdp.BackendNodeID) string {
	if id == 0 {
		return "(no DOM node)"
	}
	res, err := chromedp.Call(p.ctx, dom.GetOuterHTML, dom.GetOuterHTMLParams{BackendNodeID: id})
	html := res.OuterHTML
	if err != nil {
		return fmt.Sprintf("(node %d: %v)", id, err)
	}
	if len(html) > 160 {
		html = html[:160] + "…"
	}
	return html
}

// TestEveryControlHasAnAccessibleName: on every audited page and open
// dialog, panel and menu, each button, link, field, tab and menu item that
// Chrome's accessibility tree exposes has a name, and each table has column
// headers (WCAG 1.3.1, 4.1.2).
func TestEveryControlHasAnAccessibleName(t *testing.T) {
	f := newA11yFixture(t)
	p := open(t)
	for _, pg := range f.pages() {
		p.show(pg)
		for _, u := range p.unnamedControls() {
			t.Errorf("%s: an unnamed %s", pg.name, u)
		}
	}
}

// contrastAuditJS measures the page as rendered: every visible text run
// against the colour actually behind it (each ancestor's background
// composited in turn, with opacity), every shown placeholder, and the
// boundary of every text field and select against what surrounds it. Each
// failure is "ratio needed what fg-on-bg".
const contrastAuditJS = `(() => {
  const parse = (s) => {
    let m = s.match(/^rgba?\(([^)]+)\)$/);
    if (m) { const v = m[1].split(/[\s,\/]+/).filter(Boolean).map(Number); return [v[0], v[1], v[2], v.length > 3 ? v[3] : 1]; }
    m = s.match(/^color\(srgb ([^)]+)\)$/);
    if (m) { const v = m[1].split(/[\s\/]+/).filter(Boolean).map(Number); return [v[0] * 255, v[1] * 255, v[2] * 255, v.length > 3 ? v[3] : 1]; }
    return null;
  };
  const over = (top, under) => {
    const a = top[3];
    return [top[0] * a + under[0] * (1 - a), top[1] * a + under[1] * (1 - a), top[2] * a + under[2] * (1 - a), 1];
  };
  const lum = (c) => {
    const ch = c.slice(0, 3).map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); });
    return 0.2126 * ch[0] + 0.7152 * ch[1] + 0.0722 * ch[2];
  };
  const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
  const hex = (c) => "#" + c.slice(0, 3).map((v) => Math.round(v).toString(16).padStart(2, "0")).join("");
  const unparsed = new Set();
  // The colour behind el, or null where an image or gradient is.
  const behind = (el) => {
    const layers = [];
    for (let n = el; n; n = n.parentElement) {
      const s = getComputedStyle(n);
      if (s.backgroundImage !== "none" && !n.classList.contains("skeleton")) return null;
      const c = parse(s.backgroundColor);
      if (!c) { unparsed.add(s.backgroundColor); return null; }
      if (c[3] > 0) layers.push(c);
      if (c[3] >= 1) break;
    }
    let bg = [255, 255, 255, 1];
    for (let i = layers.length - 1; i >= 0; i--) bg = over(layers[i], bg);
    return bg;
  };
  const opacity = (el) => { let o = 1; for (let n = el; n; n = n.parentElement) o *= Number(getComputedStyle(n).opacity); return o; };
  const shown = (el) => {
    if (el.closest("[hidden], [inert], .sr-only, svg, .skip:not(:focus)")) return false;
    const s = getComputedStyle(el);
    return s.visibility === "visible" && s.display !== "none";
  };
  const exempt = (el) => el.closest(":disabled, [aria-disabled=true]");
  const fails = new Map();
  const fail = (need, r, what, fg, bg) => {
    const k = what + " " + hex(fg) + " on " + hex(bg);
    if (!fails.has(k)) fails.set(k, r.toFixed(2) + " < " + need + ": " + k);
  };
  const where = (el) => {
    const id = el.id ? "#" + el.id : "";
    const cls = typeof el.className === "string" && el.className ? "." + el.className.trim().split(/\s+/).join(".") : "";
    const p = el.parentElement;
    const pid = p ? (p.id ? "#" + p.id : (typeof p.className === "string" && p.className ? "." + p.className.trim().split(/\s+/)[0] : p.tagName.toLowerCase())) : "";
    return pid + " > " + el.tagName.toLowerCase() + id + cls;
  };
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  const seen = new Set();
  for (let t = walker.nextNode(); t; t = walker.nextNode()) {
    if (!t.textContent.trim()) continue;
    const el = t.parentElement;
    if (!el || seen.has(el) || !shown(el) || exempt(el)) continue;
    seen.add(el);
    const range = document.createRange(); range.selectNodeContents(t);
    const r = range.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    const s = getComputedStyle(el);
    const c = parse(s.color);
    const bg = behind(el);
    if (!c) { unparsed.add(s.color); continue; }
    if (!bg) continue;
    const fg = over([c[0], c[1], c[2], c[3] * opacity(el)], bg);
    const size = parseFloat(s.fontSize), bold = Number(s.fontWeight) >= 700;
    const need = size >= 24 || (bold && size >= 18.66) ? 3 : 4.5;
    const got = ratio(fg, bg);
    if (got < need) fail(need, got, "text " + JSON.stringify(t.textContent.trim().slice(0, 30)) + " in " + where(el), fg, bg);
  }
  for (const el of document.querySelectorAll("input, textarea, select")) {
    if (!shown(el) || exempt(el)) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    const type = (el.getAttribute("type") || "text").toLowerCase();
    if (["checkbox", "radio", "hidden", "range", "color", "file", "submit", "button"].includes(type)) continue;
    const s = getComputedStyle(el);
    const around = behind(el.parentElement);
    if (!around) continue;
    if (el.placeholder && !el.value) {
      const ph = parse(getComputedStyle(el, "::placeholder").color);
      const own = behind(el);
      if (ph && own) {
        const fg = over(ph, own), got = ratio(fg, own);
        if (got < 4.5) fail(4.5, got, "placeholder of " + where(el), fg, own);
      }
    }
    // 1.4.11: the field's boundary, its border or its own fill, against
    // what surrounds it.
    const own = behind(el);
    let best = own ? ratio(own, around) : 0;
    for (const side of ["Top", "Right", "Bottom", "Left"]) {
      if (parseFloat(s["border" + side + "Width"]) < 1 || s["border" + side + "Style"] === "none") continue;
      const b = parse(s["border" + side + "Color"]);
      if (b) best = Math.max(best, ratio(over(b, around), around));
    }
    if (best < 3) {
      const b = parse(s.borderBottomColor) || [0, 0, 0, 0];
      fail(3, best, "boundary of " + where(el), over(b, around), around);
    }
  }
  return { Fails: [...fails.values()], Unparsed: [...unparsed] };
})()`

// contrastFailures runs contrastAuditJS on the page as it is.
func (p *tab) contrastFailures() []string {
	p.t.Helper()
	var out struct{ Fails, Unparsed []string }
	p.eval(contrastAuditJS, &out)
	for _, u := range out.Unparsed {
		out.Fails = append(out.Fails, "a colour the audit cannot read: "+u)
	}
	sort.Strings(out.Fails)
	return out.Fails
}

// setTheme chooses a theme the way the stored preference does, without the
// settings panel, so the page under audit is not covered by it.
func (p *tab) setTheme(theme string) {
	p.t.Helper()
	p.eval(fmt.Sprintf(`(() => { localStorage.setItem("cb-theme", %q); return true; })()`, theme), nil)
}

// TestContrastInBothThemes: every audited page, in the light and the dark
// theme, has text at 4.5:1 (3:1 when large) and text fields whose boundary
// is 3:1 against their surroundings (WCAG 1.4.3, 1.4.11). Disabled controls
// are exempt, as WCAG makes them.
func TestContrastInBothThemes(t *testing.T) {
	f := newA11yFixture(t)
	for _, theme := range []string{"light", "dark"} {
		t.Run(theme, func(t *testing.T) {
			p := open(t)
			p.run(reducedMotion())
			p.navigate("/")
			p.setTheme(theme)
			for _, pg := range f.pages() {
				p.show(pg)
				for _, fl := range p.contrastFailures() {
					t.Errorf("%s, %s theme: %s", pg.name, theme, fl)
				}
			}
		})
	}
}

// tagControlsJS marks every visible, enabled control a keyboard user must
// be able to reach with a key of its own (two links can share a label: a
// pinned product and its row in its category), and returns the keys.
const tagControlsJS = `(() => {
  const out = [];
  let n = 0;
  for (const a of document.querySelectorAll("a[href], button, input, select, textarea, summary, [tabindex]")) {
    if (a.matches(":disabled, [tabindex='-1']") || a.closest("[hidden], [inert]")) continue;
    if (a.type === "radio" && !a.checked && document.querySelector('input[type=radio][name="' + a.name + '"]:checked')) continue;
    const s = getComputedStyle(a), r = a.getBoundingClientRect();
    if (s.visibility !== "visible" || r.width === 0 || r.height === 0) continue;
    const label = (a.getAttribute("aria-label") || a.textContent || a.value || a.id || a.tagName).trim().replace(/\s+/g, " ").slice(0, 50);
    a.dataset.a11yKey = (n++) + " " + a.tagName.toLowerCase() + (a.id ? "#" + a.id : "") + " " + label;
    out.push(a.dataset.a11yKey);
  }
  return out;
})()`

// focusStopJS describes the focused element and whether its focus is drawn:
// an outline of at least 1px, or a box shadow, while it is on screen.
const focusStopJS = `(() => {
  const a = document.activeElement;
  if (!a || a === document.body || a === document.documentElement) return { Body: true };
  const s = getComputedStyle(a), r = a.getBoundingClientRect();
  const ring = (s.outlineStyle !== "none" && parseFloat(s.outlineWidth) >= 1) || s.boxShadow !== "none";
  const key = a.dataset.a11yKey || ("untagged " + a.outerHTML.slice(0, 80));
  return { Key: key, Ring: ring, Matches: a.matches(":focus-visible"),
           Visible: r.width > 0 && r.height > 0 && r.bottom > 0 && r.right > 0 && s.visibility === "visible" };
})()`

type focusStop struct {
	Body, Ring, Visible, Matches bool
	Key                          string
}

// walkByTab presses Tab from a freshly loaded page until focus comes back to
// where it started or leaves the document, and returns each stop in order.
func (p *tab) walkByTab(limit int) []focusStop {
	p.t.Helper()
	var stops []focusStop
	for i := 0; i < limit; i++ {
		p.run(chromedp.KeyEvent(kb.Tab))
		var s focusStop
		p.eval(focusStopJS, &s)
		if s.Body || (len(stops) > 0 && s.Key == stops[0].Key) {
			return stops
		}
		stops = append(stops, s)
	}
	p.t.Fatalf("%d presses of Tab did not come back round: focus is trapped near %v", limit, stops[len(stops)-1].Key)
	return nil
}

// TestKeyboardAloneWalksAPageAndCreatesATopic: on the Pub/Sub topics list,
// freshly loaded, the first Tab lands on the skip link (WCAG 2.4.1), and Tab
// then visits every visible control, each with a focus indicator drawn and
// on screen, and comes back round rather than being trapped (2.1.1, 2.1.2,
// 2.4.7). Then, with no pointer, Create topic is reached and pressed, its
// form's name field is reached and typed into, Enter creates the topic, the
// outcome is announced in a live region (4.1.3), and focus is in the page.
func TestKeyboardAloneWalksAPageAndCreatesATopic(t *testing.T) {
	f := newA11yFixture(t)
	p := open(t)
	p.run(reducedMotion())
	p.navigate("/pubsub/topics?project=" + f.project)
	p.waitFor(rowReady(f.topic))

	var want []string
	p.eval(tagControlsJS, &want)
	stops := p.walkByTab(400)
	if len(stops) == 0 || !strings.HasSuffix(stops[0].Key, "Skip to main content") {
		first := "nothing"
		if len(stops) > 0 {
			first = stops[0].Key
		}
		t.Errorf("the first Tab on a freshly loaded page lands on %q, not the skip link", first)
	}
	reached := map[string]bool{}
	for _, s := range stops {
		reached[s.Key] = true
		if !s.Visible {
			t.Errorf("Tab stops on %q, which is not on screen", s.Key)
		} else if !s.Ring {
			t.Errorf("Tab stops on %q with no focus indicator drawn (focus-visible %v)", s.Key, s.Matches)
		}
	}
	for _, w := range want {
		if !reached[w] {
			t.Errorf("Tab never reaches %q", w)
		}
	}

	// Create a topic with the keyboard alone.
	p.navigate("/pubsub/topics?project=" + f.project)
	p.waitFor(rowReady(f.topic))
	p.tabTo(`a.tagName === "BUTTON" && a.textContent.trim() === "Create topic"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#view #f-name") !== null`)
	var focused string
	p.eval(`document.activeElement.id`, &focused)
	if focused != "f-name" {
		p.tabTo(`a.id === "f-name"`)
	}
	p.run(chromedp.KeyEvent("kbd-topic"), chromedp.KeyEvent(kb.Enter))
	created := "projects/" + f.project + "/topics/kbd-topic"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub?project="+f.project+"&name="+url.QueryEscape(created), "")
	})
	p.waitFor(`(() => { const h = document.querySelector("#snackbars"), l = document.querySelector("#live");
		return [h, l].some((n) => n && n.textContent.includes("kbd-topic")); })()`)
	var live struct {
		Polite bool
		Where  string
	}
	p.eval(`(() => { const h = document.querySelector("#snackbars");
		return { Polite: !!h && h.getAttribute("aria-live") === "polite",
		         Where: (document.activeElement || document.body).outerHTML.slice(0, 120) }; })()`, &live)
	if !live.Polite {
		t.Errorf("the snackbars that announced the topic are not a polite live region")
	}
	var inPage bool
	p.eval(`document.activeElement !== document.body && document.querySelector("#main").contains(document.activeElement)`, &inPage)
	if !inPage {
		t.Errorf("after the topic was created, focus is on %s rather than in the page", live.Where)
	}
}

// TestToolbarPanelsCloseOnEscapeAndGiveFocusBack: each panel the toolbar
// opens (project picker, notifications, account, settings) opens from the
// keyboard, says it is expanded, and closes on Escape with focus back on
// the button that opened it (WCAG 2.1.1, 2.4.3).
func TestToolbarPanelsCloseOnEscapeAndGiveFocusBack(t *testing.T) {
	p := open(t)
	p.navigate("/")
	p.waitFor(`document.querySelector("#view h1") !== null`)
	for _, c := range []struct{ button, panel string }{
		{"project-button", "project-panel"},
		{"notifications", "notifications-panel"},
		{"account", "account-panel"},
		{"settings", "settings-panel"},
	} {
		p.tabTo(fmt.Sprintf(`a.id === %q`, c.button))
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(fmt.Sprintf(`!document.getElementById(%q).hidden && document.getElementById(%q).getAttribute("aria-expanded") === "true"`, c.panel, c.button))
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(fmt.Sprintf(`document.getElementById(%q).hidden`, c.panel))
		var back, expanded string
		p.eval(`document.activeElement.id`, &back)
		p.eval(fmt.Sprintf(`document.getElementById(%q).getAttribute("aria-expanded")`, c.button), &expanded)
		if back != c.button || expanded != "false" {
			t.Errorf("Escape on %s: focus on #%s, aria-expanded %q; want #%s and false", c.panel, back, expanded, c.button)
		}
	}
}

// TestReducedMotionStopsMotion: with prefers-reduced-motion: reduce, no
// element of an audited page runs an animation or a transition longer than
// a frame (WCAG 2.3.3), including the drawer, the busy bar and the dialogs.
func TestReducedMotionStopsMotion(t *testing.T) {
	f := newA11yFixture(t)
	p := open(t)
	p.run(reducedMotion())
	for _, pg := range f.pages() {
		p.show(pg)
		for _, m := range p.motionFailures() {
			t.Errorf("%s: under reduced motion, %s", pg.name, m)
		}
	}
}

// motionFailures describes every element of the page as it is that runs an
// animation or a transition longer than a frame.
func (p *tab) motionFailures() []string {
	p.t.Helper()
	var moving []string
	p.eval(`(() => {
			const long = (v) => v.split(",").some((d) => parseFloat(d) * (d.trim().endsWith("ms") ? 1 : 1000) > 20);
			const out = new Set();
			for (const el of document.querySelectorAll("*")) {
				const s = getComputedStyle(el);
				const anim = s.animationName !== "none" && long(s.animationDuration) && s.animationIterationCount !== "0";
				const tr = s.transitionProperty !== "none" && long(s.transitionDuration);
				if (anim || tr) out.add(el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") +
					(typeof el.className === "string" && el.className ? "." + el.className.trim().split(/\s+/).join(".") : "") +
					(anim ? " animation " + s.animationName + " " + s.animationDuration : " transition " + s.transitionProperty + " " + s.transitionDuration));
			}
			return [...out].slice(0, 20);
		})()`, &moving)
	return moving
}

// TestReflowAt320pxAnd200PercentZoom: at 320 CSS pixels wide (WCAG 1.4.10:
// 1280px at 400% zoom) and at 640 (1280px at 200%, 1.4.4) every audited
// page scrolls only vertically, its heading and primary actions are on
// screen, and nothing but a table's own region is wider than the viewport.
func TestReflowAt320pxAnd200PercentZoom(t *testing.T) {
	f := newA11yFixture(t)
	for _, width := range []int64{320, 640} {
		t.Run(fmt.Sprintf("%dpx", width), func(t *testing.T) {
			p := open(t)
			p.run(chromedp.EmulateViewport(width, 720), reducedMotion())
			for _, pg := range f.pages() {
				p.show(pg)
				for _, fl := range p.reflowFailures() {
					t.Errorf("%s at %dpx: %s", pg.name, width, fl)
				}
			}
		})
	}
}

// reflowFailures says whether the page as it is scrolls sideways, and names
// what but a table's own region is wider than the viewport.
func (p *tab) reflowFailures() []string {
	p.t.Helper()
	var r struct {
		Scroll, Client float64
		Wide           []string
	}
	p.eval(`(() => {
		const doc = document.scrollingElement, vw = document.documentElement.clientWidth;
		const wide = new Set();
		for (const el of document.querySelectorAll("#view *, .toolbar *, .modal *, .panel *")) {
			if (el.closest(".table-wrap, [hidden], pre, .code, .terminal-drawer, .sr-only")) continue;
			const s = getComputedStyle(el);
			if (s.display === "none" || s.visibility !== "visible" || s.position === "fixed" && el.closest(".nav-scrim")) continue;
			const b = el.getBoundingClientRect();
			if (b.width === 0 || b.height === 0) continue;
			if (b.right > vw + 1 || b.left < -1) {
				let clipped = false;
				for (let a = el.parentElement; a; a = a.parentElement) {
					const o = getComputedStyle(a).overflowX;
					if (o === "auto" || o === "scroll" || o === "hidden" || o === "clip") { clipped = true; break; }
				}
				if (!clipped) wide.add(el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") +
					(typeof el.className === "string" && el.className ? "." + el.className.trim().split(/\s+/).join(".") : "") +
					" " + Math.round(b.left) + ".." + Math.round(b.right));
			}
		}
		// What widens a page that scrolls sideways when none of the above
		// does: the outermost element past the viewport, wherever it is.
		if (doc.scrollWidth > doc.clientWidth && wide.size === 0) {
			for (const el of document.querySelectorAll("body *")) {
				const b = el.getBoundingClientRect();
				if (b.right <= vw + 1 || el.closest("[hidden]")) continue;
				let clipped = false;
				for (let a = el.parentElement; a; a = a.parentElement) {
					const o = getComputedStyle(a).overflowX;
					if (o === "auto" || o === "scroll" || o === "hidden" || o === "clip") { clipped = true; break; }
				}
				if (clipped) continue;
				wide.add(el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") +
					(typeof el.className === "string" && el.className ? "." + el.className.trim().split(/\s+/).join(".") : "") +
					" " + Math.round(b.left) + ".." + Math.round(b.right));
				if (wide.size >= 3) break;
			}
			// Or text running out of a box that itself fits.
			const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
			for (let t = walker.nextNode(); t && wide.size < 3; t = walker.nextNode()) {
				if (!t.textContent.trim() || !t.parentElement || t.parentElement.closest("[hidden]")) continue;
				const range = document.createRange(); range.selectNodeContents(t);
				const b = range.getBoundingClientRect();
				if (b.right > vw + 1) wide.add("text " + JSON.stringify(t.textContent.trim().slice(0, 40)) + " in " +
					t.parentElement.tagName.toLowerCase() + (t.parentElement.className ? "." + String(t.parentElement.className).trim().split(/\s+/).join(".") : "") +
					" " + Math.round(b.left) + ".." + Math.round(b.right));
			}
		}
		return { Scroll: doc.scrollWidth, Client: doc.clientWidth, Wide: [...wide].slice(0, 10) };
	})()`, &r)
	var out []string
	if r.Scroll > r.Client {
		out = append(out, fmt.Sprintf("the page scrolls sideways (%v wide in %v)", r.Scroll, r.Client))
	}
	for _, w := range r.Wide {
		out = append(out, "past the viewport: "+w)
	}
	return out
}

// TestBigQueryScreensAreNamedAndLegible: the BigQuery datasets list, a
// dataset, its query editor, its Create table form and the Job history, in
// both themes, have a name on every control, column headers on every table
// and text and field boundaries that meet their contrast minimums.
func TestBigQueryScreensAreNamedAndLegible(t *testing.T) {
	needService(t, "bigquery")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	ds := fmt.Sprintf("a11y_%d", uniqueSuffix())
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	h1 := settled
	pages := []a11yPage{
		{"datasets", "/bigquery" + q, rowReady(ds), ""},
		{"dataset", "/bigquery/" + ds + q, h1, ""},
		{"query editor", "/bigquery/" + ds + q + "&tab=query", h1 + ` && document.querySelector("#view .query-editor") !== null`, ""},
		{"create table", "/bigquery/" + ds + q, h1,
			`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent.trim() === "Create table").click()`},
		{"job history", "/bigquery-jobs" + q, h1, ""},
	}
	for _, theme := range []string{"light", "dark"} {
		t.Run(theme, func(t *testing.T) {
			p := open(t)
			p.run(reducedMotion())
			p.navigate("/")
			p.setTheme(theme)
			for _, pg := range pages {
				p.show(pg)
				for _, u := range p.unnamedControls() {
					t.Errorf("%s: an unnamed %s", pg.name, u)
				}
				for _, fl := range p.contrastFailures() {
					t.Errorf("%s, %s theme: %s", pg.name, theme, fl)
				}
			}
		})
	}
}

// uniqueSuffix is a number no other test run on this instance used.
func uniqueSuffix() int64 {
	return time.Now().UnixNano() % 1e12
}
