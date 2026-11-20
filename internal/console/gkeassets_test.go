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
