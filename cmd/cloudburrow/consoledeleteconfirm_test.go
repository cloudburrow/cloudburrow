package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestEveryDeleterHasADeleteConfirmRule holds #1931: every provider the
// console can delete through says how Google's console confirms that kind's
// delete, in console.DeleteConfirmRules, so a new product cannot ship with
// a confirmation nobody checked. A row is either documented (a style and
// the Google doc whose console steps describe the dialog) or `UNVERIFIED`,
// which keeps the name typed back.
func TestEveryDeleterHasADeleteConfirmRule(t *testing.T) {
	d := coverageDeps(t)
	seen := map[string]bool{}
	for _, p := range consoleProviders(d, clusterMetrics(d.cfg.KubeconfigPath()), console.NewSeries(console.SeriesLimit, nil)) {
		if _, ok := p.(console.Deleter); !ok {
			continue
		}
		seen[p.ID()] = true
		if _, ok := p.(console.DeleteConfirmer); ok {
			continue
		}
		if _, ok := console.DeleteConfirmRules[p.ID()]; !ok {
			t.Errorf("%s deletes, and console.DeleteConfirmRules has no row saying how Google confirms it", p.ID())
		}
	}
	styles := []string{console.DeleteConfirmName, console.DeleteConfirmWord, console.DeleteConfirmWordUpper, console.DeleteConfirmClick}
	for id, r := range console.DeleteConfirmRules {
		if !seen[id] {
			t.Errorf("console.DeleteConfirmRules names %s, which is no provider that deletes", id)
		}
		if !slices.Contains(styles, r.Style) {
			t.Errorf("%s: style %q is none of %v", id, r.Style, styles)
		}
		if r.Verified != strings.HasPrefix(r.Source, "https://docs.cloud.google.com/") {
			t.Errorf("%s: a verified row names its Google doc, an `UNVERIFIED` one none: verified %v, source %q", id, r.Verified, r.Source)
		}
		if !r.Verified && r.Style != console.DeleteConfirmName {
			t.Errorf("%s: `UNVERIFIED`, so it keeps the name typed back, not %q", id, r.Style)
		}
	}
}
