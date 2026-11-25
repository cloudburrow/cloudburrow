package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestBrowserTestsRunWhereTheirScreensAreServed holds ci.yml's browser
// step to the screens each test of test/browser needs: a test that asks
// for a screen (needService) skips where the console does not draw it, and
// CI fails the step on that skip. Main run 45422957668 skipped 54 tests in
// the storage shard, whose instance serves no Compute Engine and no
// Dataproc, because Landing 168's new tests fell to it as "the rest".
//
// Every test of test/browser but the never-run ones
// (COMPAT_BROWSER_NEVER_TESTS) is selected by exactly one browser shard,
// ci.yml's (COMPAT_BROWSER_SHARDS) or a nightly job's
// (COMPAT_BROWSER_NIGHTLY_SHARDS, #2234), by compat_browser_select in
// scripts/compat-shards.sh, and that shard's
// --services (compat_shard) serve every screen it asks for by name, here
// or in a function of test/browser it calls, by consoleCoverages.
func TestBrowserTestsRunWhereTheirScreensAreServed(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "compat-shards.sh"))
	if err != nil {
		t.Fatal(err)
	}
	prog := `set -eu; . "$1"; echo "NEVER=$COMPAT_BROWSER_NEVER_TESTS"; echo "SHARDS=$COMPAT_BROWSER_SHARDS $COMPAT_BROWSER_NIGHTLY_SHARDS"
for s in $COMPAT_BROWSER_SHARDS $COMPAT_BROWSER_NIGHTLY_SHARDS; do compat_shard "$s"; compat_browser_select "$s"
echo "$s.SERVICES=$SERVICES"; echo "$s.RUN=$BROWSER_RUN"; echo "$s.SKIP=$BROWSER_SKIP"; done`
	out, err := exec.Command(bash, "-c", prog, "bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("compat-shards.sh: %v\n%s", err, out)
	}
	v := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, val, _ := strings.Cut(line, "=")
		v[k] = val
	}
	never := regexp.MustCompile(v["NEVER"])
	shards := strings.Fields(v["SHARDS"])
	runs := func(shard, name string) bool {
		if r := v[shard+".RUN"]; r != "" && !regexp.MustCompile(r).MatchString(name) {
			return false
		}
		return !regexp.MustCompile(v[shard+".SKIP"]).MatchString(name)
	}

	// The screen IDs each service draws, and those every instance draws.
	owner := map[string]config.Service{}
	for svc, c := range consoleCoverages {
		for _, id := range c.Providers {
			owner[id] = svc
		}
	}
	always := map[string]bool{}
	for _, id := range clusterScreens {
		always[id] = true
	}
	// The screens internal/console appends that are no provider: Online
	// prediction where Cloud Run runs its container (#869), and the local
	// AI's Embeddings and Local models, which only the localai shard's
	// instance starts (-local-ai-embeddings, #1985).
	owner["ai-predict"] = config.ServiceRun
	localAI := map[string]bool{"ai-embed": true, "ai-local": true}

	tests, needs := browserTestScreens(t)
	if len(tests) == 0 {
		t.Fatal("found no test in test/browser")
	}
	for _, name := range tests {
		if never.MatchString(name) {
			continue
		}
		var in []string
		for _, s := range shards {
			if runs(s, name) {
				in = append(in, s)
			}
		}
		// TestKubernetesEnginePages* holds with and without the Kubernetes
		// Engine API, so it runs in served and in storage (#1913).
		if strings.HasPrefix(name, "TestKubernetesEnginePages") && slices.Equal(in, []string{"storage", "served"}) {
			in = []string{"served"}
		}
		if len(in) != 1 {
			t.Errorf("%s runs in the browser step of %v; want exactly one shard", name, in)
			continue
		}
		served := map[config.Service]bool{}
		for _, s := range strings.Split(v[in[0]+".SERVICES"], ",") {
			served[config.Service(s)] = true
		}
		for _, id := range needs[name] {
			if always[id] {
				continue
			}
			if localAI[id] {
				if in[0] != "localai" {
					t.Errorf("%s runs in the %s shard, whose instance has no local AI for its %s screen: it would skip there", name, in[0], id)
				}
				continue
			}
			svc, ok := owner[id]
			if !ok {
				t.Errorf("%s needs the %s screen, which no service of consoleCoverages draws", name, id)
				continue
			}
			if !served[svc] {
				t.Errorf("%s runs in the %s shard, whose instance does not serve %s for its %s screen: it would skip there", name, in[0], svc, id)
			}
		}
	}
}

// browserTestScreens parses test/browser and returns its Test functions,
// sorted, and the screen IDs each asks for with needService(t, "<id>"),
// itself or through the package's functions it calls.
func browserTestScreens(t *testing.T) ([]string, map[string][]string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "test", "browser", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	direct := map[string][]string{}
	calls := map[string][]string{}
	var tests []string
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if strings.HasPrefix(name, "Test") && fn.Type.Params.NumFields() == 1 {
				tests = append(tests, name)
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := c.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				if id.Name == "needService" && len(c.Args) == 2 {
					if lit, ok := c.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							direct[name] = append(direct[name], s)
						}
					}
					return true
				}
				calls[name] = append(calls[name], id.Name)
				return true
			})
		}
	}
	needs := map[string][]string{}
	for _, name := range tests {
		seen := map[string]bool{}
		var walk func(string)
		walk = func(fn string) {
			if seen[fn] {
				return
			}
			seen[fn] = true
			needs[name] = append(needs[name], direct[fn]...)
			for _, c := range calls[fn] {
				walk(c)
			}
		}
		walk(name)
	}
	slices.Sort(tests)
	return tests, needs
}
