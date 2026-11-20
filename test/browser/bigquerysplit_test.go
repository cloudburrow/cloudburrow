//go:build browser

package browser

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// splitState is what the Studio's workspace shows: each pane's tabs, the
// one selected in each, and whether the right pane is drawn.
type splitState struct {
	Split         bool
	Left, Right   []string
	LeftSel       string
	RightSel      string
	Focused       string
	LeftBox       []float64
	RightBox      []float64
	RightHidden   bool
	StripsVisible int
}

const splitStateJS = `(() => {
	const pane = (side) => [...document.querySelectorAll('.bq-tabs[data-pane="' + side + '"] .tab')];
	const sel = (side) => (pane(side).find((b) => b.classList.contains("is-selected")) || {}).textContent || "";
	const box = (side) => { const r = document.querySelector('.bq-split-pane[data-pane="' + side + '"]').getBoundingClientRect(); return [r.left, r.top, r.right, r.bottom]; };
	return {
		split: document.querySelector(".bq-split").classList.contains("is-split"),
		left: pane("left").map((b) => b.textContent), right: pane("right").map((b) => b.textContent),
		leftSel: sel("left"), rightSel: sel("right"),
		focused: document.activeElement ? (document.activeElement.textContent || "") : "",
		leftBox: box("left"), rightBox: box("right"),
		rightHidden: document.querySelector('.bq-split-pane[data-pane="right"]').hidden,
		stripsVisible: [...document.querySelectorAll(".bq-tabs")].filter((s) => s.getClientRects().length).length,
	};
})()`

func (p *tab) splitState() splitState {
	p.t.Helper()
	var s splitState
	p.eval(splitStateJS, &s)
	return s
}

// tabMenuItem opens the tab menu of the tab titled title from the keyboard
// (focus its Open menu button, Enter) and moves to item with the arrow
// keys, then presses Enter on it.
func (p *tab) tabMenuItem(title, item string) {
	p.t.Helper()
	button := fmt.Sprintf(`.bq-tab-wrap .bq-node-menu[aria-label=%q]`, "Open menu for "+title)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, button))
	p.run(chromedp.Focus(chromedp.CSS(button)), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.getAttribute("role") === "menuitem"`)
	for i := 0; i < 8; i++ {
		var at string
		p.eval(`document.activeElement.textContent`, &at)
		if at == item {
			p.run(chromedp.KeyEvent(kb.Enter))
			return
		}
		p.run(chromedp.KeyEvent(kb.ArrowDown))
	}
	p.t.Fatalf("the menu of %q has no enabled %q", title, item)
}

// TestBigQueryStudioSplitsTabsIntoTwoPanes (#2005), in the emulators shard:
// Google's "split the tabs into two panes and view them side by side"
// (https://docs.cloud.google.com/bigquery/docs/bigquery-web-ui). With one
// tab open, Split tab to left and right are unavailable; with two, Split
// tab to right draws a second pane with that tab, each pane keeps its own
// selected tab, the panes and their selections survive a reload, Move tab
// to left pane puts the last tab back and closes the right pane, and Close
// other tabs leaves one. All of it from the keyboard: the tab menu opens
// with Enter, its items take Up and Down, Escape closes it, and focus
// follows the moved tab. The panes are side by side at 1440px and stack at
// 640px and 320px.
func TestBigQueryStudioSplitsTabsIntoTwoPanes(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	studio := "/bigquery-studio?project=" + url.QueryEscape(project)
	p.run(chromedp.EmulateViewport(1440, 900))
	p.navigate(studio)
	p.waitFor(`document.querySelector(".bq-tree") !== null && document.querySelector(".bq-tabs .tab") !== null`)

	// One tab: the split items are there but unavailable.
	p.run(chromedp.Focus(chromedp.CSS(`.bq-tab-wrap .bq-node-menu`)), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.getAttribute("role") === "menuitem"`)
	var disabled []string
	p.eval(`[...document.activeElement.closest(".bq-menu").querySelectorAll("button[disabled]")].map((b) => b.textContent)`, &disabled)
	if got := strings.Join(disabled, "|"); !strings.Contains(got, "Split tab to left") || !strings.Contains(got, "Split tab to right") {
		t.Errorf("with one tab open, the unavailable items are %q", disabled)
	}
	p.run(chromedp.KeyEvent(kb.Escape))
	var back string
	p.eval(`document.activeElement.getAttribute("aria-label") || ""`, &back)
	if !strings.HasPrefix(back, "Open menu for ") {
		t.Errorf("Escape left focus on %q, not the menu's button", back)
	}

	// Two tabs; split the query to the right.
	p.clickText(".bq-new-query", "SQL query")
	p.waitFor(`document.querySelectorAll(".bq-tabs .tab").length === 2`)
	p.tabMenuItem("Untitled query", "Split tab to right")
	p.waitFor(`document.querySelector(".bq-split.is-split") !== null`)
	s := p.splitState()
	if strings.Join(s.Left, "|") != "Home" || strings.Join(s.Right, "|") != "Untitled query" || s.RightSel != "Untitled query" || s.LeftSel != "Home" {
		t.Errorf("after Split tab to right: %+v", s)
	}
	if s.Focused != "Untitled query" {
		t.Errorf("focus is on %q after the split, not the moved tab", s.Focused)
	}
	if s.StripsVisible != 2 {
		t.Errorf("%d tab strips are drawn, want 2", s.StripsVisible)
	}
	// Side by side at 1440px: the right pane starts where the left ends,
	// on the same line.
	if !(s.RightBox[0] >= s.LeftBox[2]-1 && s.RightBox[1] < s.LeftBox[3]) {
		t.Errorf("at 1440px the panes are not side by side: left %v, right %v", s.LeftBox, s.RightBox)
	}

	// A split tab menu moves rather than splits.
	p.run(chromedp.Focus(chromedp.CSS(`.bq-tabs[data-pane="right"] .bq-node-menu`)), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.getAttribute("role") === "menuitem"`)
	var items []string
	p.eval(`[...document.activeElement.closest(".bq-menu").querySelectorAll("button")].map((b) => b.textContent)`, &items)
	if got := strings.Join(items, "|"); got != "Move tab to left pane|Close tab|Close other tabs" {
		t.Errorf("a right-pane tab's menu is %q", items)
	}
	p.run(chromedp.KeyEvent(kb.Escape))

	// Each pane keeps its own selected tab: a new query in the left pane
	// leaves the right's selected.
	p.run(chromedp.Click(chromedp.CSS(`.bq-tabs[data-pane="left"] .bq-new-query`)))
	p.waitFor(`document.querySelectorAll('.bq-tabs[data-pane="left"] .tab').length === 2`)
	s = p.splitState()
	if s.LeftSel != "Untitled query 2" || s.RightSel != "Untitled query" {
		t.Errorf("after a new query in the left pane: %+v", s)
	}
	// Arrow keys move between a strip's tabs.
	p.run(chromedp.Focus(chromedp.CSS(`.bq-tabs[data-pane="left"] .tab`)), chromedp.KeyEvent(kb.ArrowRight))
	var at string
	p.eval(`document.activeElement.textContent`, &at)
	if at != "Untitled query 2" {
		t.Errorf("ArrowRight from Home moved focus to %q", at)
	}

	// The panes and each one's selection are kept in this browser.
	p.navigate(studio)
	p.waitFor(`document.querySelector(".bq-split.is-split") !== null`)
	if r := p.splitState(); r.LeftSel != "Untitled query 2" || r.RightSel != "Untitled query" || strings.Join(r.Left, "|") != "Home|Untitled query 2" {
		t.Errorf("after a reload: %+v", r)
	}

	// Narrow screens: the panes stack.
	for _, w := range []int64{640, 320} {
		p.run(chromedp.EmulateViewport(w, 900))
		p.waitFor(`(() => { const l = document.querySelector('.bq-split-pane[data-pane="left"]').getBoundingClientRect();
			const r = document.querySelector('.bq-split-pane[data-pane="right"]').getBoundingClientRect(); return r.top >= l.bottom - 1; })()`)
		if s := p.splitState(); s.RightBox[1] < s.LeftBox[3]-1 {
			t.Errorf("at %dpx the panes do not stack: left %v, right %v", w, s.LeftBox, s.RightBox)
		}
		for _, fl := range p.reflowFailures() {
			t.Errorf("split at %dpx: %s", w, fl)
		}
	}
	p.run(chromedp.EmulateViewport(1440, 900))

	// Move tab to left pane on the right pane's only tab: one pane again.
	p.tabMenuItem("Untitled query", "Move tab to left pane")
	p.waitFor(`document.querySelector(".bq-split.is-split") === null`)
	s = p.splitState()
	if !s.RightHidden || len(s.Right) != 0 || len(s.Left) != 3 || s.LeftSel != "Untitled query" || s.Focused != "Untitled query" {
		t.Errorf("after Move tab to left pane: %+v", s)
	}

	// Split tab to left: the tab to the left, every other to the right.
	p.tabMenuItem("Home", "Split tab to left")
	p.waitFor(`document.querySelector(".bq-split.is-split") !== null`)
	if s = p.splitState(); strings.Join(s.Left, "|") != "Home" || len(s.Right) != 2 {
		t.Errorf("after Split tab to left: %+v", s)
	}

	// Close other tabs leaves the one, in one pane.
	p.tabMenuItem("Home", "Close other tabs")
	p.waitFor(`document.querySelectorAll(".bq-tabs .tab").length === 1`)
	if s = p.splitState(); s.Split || strings.Join(s.Left, "|") != "Home" {
		t.Errorf("after Close other tabs: %+v", s)
	}
}

// TestBigQueryStudioSplitTabsMeetWCAG (#2005), in the emulators shard:
// Studio split into two panes, and its open tab menu, pass the console's
// WCAG 2.1 AA audit (names, contrast in both themes, reduced motion and
// reflow at 320px and 640px), and at 1440px the split workspace has every
// control named, reachable by Tab with a focus ring, and nothing past the
// window.
func TestBigQueryStudioSplitTabsMeetWCAG(t *testing.T) {
	needService(t, "bigquery")
	project := servedProject(t)
	studio := "/bigquery-studio?project=" + url.QueryEscape(project)
	ready := `document.querySelector(".bq-tree") !== null && document.querySelector(".bq-tabs .tab") !== null`
	// split opens a second tab and splits it to the right, unless the
	// workspace (kept in the browser tab) is split already.
	split := `if (!document.querySelector(".bq-split.is-split")) {
		document.querySelector(".bq-new-query").click();
		const wraps = document.querySelectorAll('.bq-tabs[data-pane="left"] .bq-tab-wrap');
		const w = wraps[wraps.length - 1];
		w.querySelector(".bq-node-menu").click();
		[...w.querySelectorAll(".bq-menu-item")].find((b) => b.textContent === "Split tab to right").click();
	}`
	menu := split + `; document.querySelector('.bq-tabs[data-pane="right"] .bq-node-menu').click()`
	auditScreens(t, []a11yPage{
		{"Studio split into two panes", studio, ready, split},
		{"Studio split tab menu", studio, ready, menu},
	})

	p := open(t)
	p.run(chromedp.EmulateViewport(1440, 900), reducedMotion())
	p.navigate(studio)
	p.waitFor(ready)
	p.eval(`(() => { `+split+`; return true; })()`, nil)
	p.waitFor(`document.querySelector(".bq-split.is-split") !== null`)
	// Reload so the audit starts from the top of the page, as a visitor's
	// Tab does, with the split kept in the browser tab.
	p.navigate(studio)
	p.waitFor(ready + ` && document.querySelector(".bq-split.is-split") !== null`)
	for _, u := range p.unnamedControls() {
		t.Errorf("split at 1440px: an unnamed %s", u)
	}
	for _, fl := range p.contrastFailures() {
		t.Errorf("split at 1440px: %s", fl)
	}
	for _, fl := range p.reflowFailures() {
		t.Errorf("split at 1440px: %s", fl)
	}
	for _, fl := range p.keyboardFailures() {
		t.Errorf("split at 1440px: %s", fl)
	}
	var panes int
	p.eval(`[...document.querySelectorAll(".bq-split.is-split .bq-split-pane")].filter((n) => n.getClientRects().length).length`, &panes)
	if panes != 2 {
		t.Errorf("at 1440px %d panes are drawn, want 2", panes)
	}
}
