package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestEveryProviderHasAConsoleRoute: every screen consoleProviders builds is
// reachable from the console (the UI parity rule). Memorystore, Backup and
// DR, Cloud SQL instances, Resource hierarchy, Tags and APIs & Services each
// had a provider and console API pages but no ROUTES entry, so the page
// answered "No such screen" and the drawer had no row for them.
func TestEveryProviderHasAConsoleRoute(t *testing.T) {
	b, err := os.ReadFile("../../internal/console/assets/console.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	i := strings.Index(js, "const ROUTES = [")
	if i < 0 {
		t.Fatal("console.js has no ROUTES")
	}
	block := js[i:]
	block = block[:strings.Index(block, "\n];")]
	routed := map[string]bool{}
	for _, entry := range regexp.MustCompile(`(?s)\{ path:.*?\}`).FindAllString(block, -1) {
		if !strings.Contains(entry, "section:") {
			continue // a route the navigation does not list
		}
		if m := regexp.MustCompile(`service: "([a-z0-9-]+)"`).FindStringSubmatch(entry); m != nil {
			routed[m[1]] = true
		}
	}
	d := coverageDeps(t)
	for _, p := range consoleProviders(d, clusterMetrics(d.cfg.KubeconfigPath()), console.NewSeries(console.SeriesLimit, nil)) {
		if !routed[p.ID()] {
			t.Errorf("provider %q (%s) has no ROUTES entry with a section in console.js: its screen is unreachable from the console", p.ID(), p.Title())
		}
	}
}
