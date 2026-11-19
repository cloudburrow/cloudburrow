package config

import (
	"flag"
	"regexp"
	"strconv"
	"testing"
)

// TestNoTwoServicesShareADefaultPort fails when two endpoints get the same
// fixed default port. Two branches each took "the next free port" (the API
// Keys API and the Kubernetes Engine API both took 9029), and each was right
// on its own; this is what catches the pair once both land.
func TestNoTwoServicesShareADefaultPort(t *testing.T) {
	t.Parallel()
	seen := map[int]string{}
	for _, np := range Default().Endpoints.Named() {
		if np.Port == 0 {
			continue // OS-assigned
		}
		if other, dup := seen[np.Port]; dup {
			t.Errorf("endpoints.%s and endpoints.%s share the default port %d", other, np.Name, np.Port)
			continue
		}
		seen[np.Port] = np.Name
	}
	def := Default()
	if err := def.Validate(); err != nil {
		t.Errorf("the default configuration does not validate: %v", err)
	}
}

// defaultInUsage is the "default N" a port flag's help text states.
var defaultInUsage = regexp.MustCompile(`default (\d+)\)`)

// TestPortFlagHelpStatesTheRealDefault checks every --port-* flag's
// "(…; default N)" against the port Default() really gives the endpoint the
// flag sets, so the help text (and the help goldens made from it) cannot keep
// naming a port that moved, nor two flags claim the same one.
func TestPortFlagHelpStatesTheRealDefault(t *testing.T) {
	t.Parallel()
	fs, _ := newFlagSet(nil)
	defaults := map[string]int{}
	for _, np := range Default().Endpoints.Named() {
		defaults[np.Name] = np.Port
	}
	claimed := map[int]string{}
	checked := 0
	fs.VisitAll(func(f *flag.Flag) {
		m := defaultInUsage.FindStringSubmatch(f.Usage)
		if len(f.Name) < 6 || f.Name[:5] != "port-" || m == nil {
			return
		}
		stated, _ := strconv.Atoi(m[1])
		if other, dup := claimed[stated]; dup {
			t.Errorf("--%s and --%s both say default %d", other, f.Name, stated)
		}
		claimed[stated] = f.Name
		// Find the endpoint this flag sets: the one that moves to a sentinel.
		const sentinel = 61234
		cfg, err := Load(Options{Args: []string{"--" + f.Name, strconv.Itoa(sentinel)}, Getenv: envMap(nil), WorkDir: t.TempDir()})
		if err != nil {
			t.Errorf("--%s %d: %v", f.Name, sentinel, err)
			return
		}
		field := ""
		for _, np := range cfg.Endpoints.Named() {
			if np.Port == sentinel {
				field = np.Name
			}
		}
		if field == "" {
			t.Errorf("--%s sets no endpoint", f.Name)
			return
		}
		checked++
		if got := defaults[field]; got != stated {
			t.Errorf("--%s says default %d, but endpoints.%s defaults to %d", f.Name, stated, field, got)
		}
	})
	if checked < 20 {
		t.Fatalf("checked %d port flags; the help-text pattern no longer matches", checked)
	}
}
