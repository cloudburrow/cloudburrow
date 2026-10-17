package repo

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// guardedElsewhere names every http.Server whose Handler does not say
// hostguard itself, and why that is safe. A new server is either wrapped
// with hostguard.Wrap (the Host check of #676 and the Origin check of
// #1586) or listed here with its reason.
var guardedElsewhere = map[string]string{
	"internal/console/console.go":            "s.Handler() returns hostguard.Wrap(sameOriginOnly(...))",
	"internal/metadata/server.go":            "s.Handler() returns hostguard.Wrap, except for a pod's own workload host",
	"internal/service/secrets/server.go":     "s.handler() returns h2c.NewHandler(hostguard.Wrap(...))",
	"internal/service/vertexai/lifecycle.go": "mixedHandler serves hostguard.Wrap'd REST and gRPC",
	"internal/netfwd/guard.go":               "the guard itself: guardHandler checks Host and Origin",
	"internal/bigtablefront/front.go":        "runs in the Bigtable pod; the host reaches it through a guarded tunnel",
	"internal/pubsubfront/upgrade.go":        "runs in the Pub/Sub pod; the host reaches it through a guarded tunnel",
	"internal/pubsubfront/push.go":           "the push relay listens on the Pub/Sub pod's loopback only",
	"internal/callfront/callfront.go":        "runs in the emulator pods; the host reaches them through a guarded tunnel",
	"internal/bigqueryfront/run.go":          "runs in the BigQuery pod; the host reaches it through a guarded tunnel",
}

// TestEveryHTTPListenerIsGuarded (#1586): three in-process REST APIs
// (Service Usage, Cloud SQL Admin, Backup and DR) were served without the
// Host check every other listener has, so a DNS-rebound page could drive
// them. This fails when an http.Server is added without it.
func TestEveryHTTPListenerIsGuarded(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	dir := strings.TrimSpace(string(root))
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(dir, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(dir, path)
			rel = filepath.ToSlash(rel)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isHTTPServer(lit.Type) {
					return true
				}
				handler := ""
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Handler" {
							var b strings.Builder
							_ = printer.Fprint(&b, fset, kv.Value)
							handler = b.String()
						}
					}
				}
				if strings.Contains(handler, "hostguard.") {
					return true
				}
				if _, ok := guardedElsewhere[rel]; ok {
					seen[rel] = true
					return true
				}
				t.Errorf("%s: an http.Server with Handler %q is not behind hostguard.Wrap; wrap it, or add it to guardedElsewhere with the reason it is safe (ADR-0004, #1586)",
					fset.Position(lit.Pos()), handler)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for rel := range guardedElsewhere {
		if !seen[rel] {
			t.Errorf("guardedElsewhere names %s, which has no unwrapped http.Server any more; remove the entry", rel)
		}
	}
}

func isHTTPServer(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && sel.Sel.Name == "Server"
}
