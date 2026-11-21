package console

import (
	"strings"
	"testing"
)

// TestRowAndDialogFocusForUnavailableActions (#1857): a row's action the
// provider marks Unavailable is drawn disabled in the row's menu with its
// reason, as on a page (a GKE node pool's Upgrade); and a dialog whose
// every field is immutable (a cluster's Connect) still takes focus, on its
// first button, so Escape and Tab stay in it.
func TestRowAndDialogFocusForUnavailableActions(t *testing.T) {
	src := consoleAsset(t, "console.js")
	for _, want := range []string{
		"unavailable: a.unavailable,",
		`const b = at && at.querySelector("button:not([disabled])");`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
}

// TestCheckTreeAndUnavailableChartAssets (#2070): a listing's CheckTree
// is drawn in place of the shared filter bar (Object Browser's API groups
// and kinds), above the sticky bar, each group a checkbox mixed while some
// of its kinds are unchecked and a disclosure button for them; and a chart
// series marked Unavailable keeps its card, saying why.
func TestCheckTreeAndUnavailableChartAssets(t *testing.T) {
	src := consoleAsset(t, "console.js")
	for _, want := range []string{
		"function checkTreeFilter({ tree, onChange })",
		"const bar = data.checkTree\n    ? checkTreeFilter(",
		"data.checkTree ? bar.node : null,",
		"box.indeterminate = ",
		`"aria-expanded": "false", "aria-controls": id,`,
		"(sv.points || []).length || sv.unavailable",
		`el("p", { class: "unavailable", text: opts.unavailable })`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	css := consoleAsset(t, "console.css")
	for _, want := range []string{".check-tree-groups {", "minmax(min(100%, 16rem), 1fr)", "prefers-reduced-motion: no-preference"} {
		if !strings.Contains(css, want) {
			t.Errorf("console.css is missing %q", want)
		}
	}
}
