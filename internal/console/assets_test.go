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
