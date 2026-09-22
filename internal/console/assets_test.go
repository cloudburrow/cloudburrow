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
