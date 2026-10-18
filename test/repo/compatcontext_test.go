package repo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The harness's Context carries a 60-second deadline, and a context derived
// from it can never outlast it: context.WithTimeout(h.Context(), 30*time.Minute)
// still ends after 60 seconds. Thirty-three compat tests waited that way for
// builds and deploys and failed when a runner was slow (functions.yml run
// 38919482569), so a test that needs longer takes its own context from
// context.Background().
func TestCompatLongContextsDoNotDeriveFromTheHarness(t *testing.T) {
	re := regexp.MustCompile(`WithTimeout\(h\.Context\(\),\s*[0-9]+\s*\*\s*time\.(Minute|Hour)`)
	files, err := filepath.Glob(filepath.Join("..", "compat", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files in test/compat")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d derives a context of minutes from h.Context(), which ends after 60s; use context.Background()", filepath.Base(f), i+1)
			}
		}
	}
}

// A cleanup runs after its test function returns, so after the function's
// `defer cancel()` has ended the context it made: a t.Cleanup that deletes
// with that ctx fails at once, silently, and leaves the resource behind.
// TestRunNamesPerProject's cleanups did, so project b's worker pool,
// instance, job and service outlived every run, and the next run's
// CreateWorkerPool in b was AlreadyExists. A cleanup takes a context of its
// own (context.Background(), or context.WithoutCancel(ctx)).
func TestCompatCleanupsDoNotUseACancelledContext(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "compat", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files in test/compat")
	}
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, pos := range cleanupsUsingCancelledContext(fn.Body) {
				t.Errorf("%s: a t.Cleanup uses ctx, which %s's deferred cancel has ended by then; give the cleanup a context of its own",
					fset.Position(pos), fn.Name.Name)
			}
		}
	}
}

// cleanupsUsingCancelledContext finds each t.Cleanup that, in a function
// body that makes ctx with context.With* and defers its cancel, uses that
// ctx before declaring one of its own, at its first use.
func cleanupsUsingCancelledContext(body *ast.BlockStmt) []token.Pos {
	cancelled, deferred := false, false
	for _, st := range body.List {
		switch s := st.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) == 2 && isIdent(s.Lhs[0], "ctx") && len(s.Rhs) == 1 {
				if call, ok := s.Rhs[0].(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isIdent(sel.X, "context") && strings.HasPrefix(sel.Sel.Name, "With") {
						cancelled = true
					}
				}
			}
		case *ast.DeferStmt:
			if c, ok := s.Call.Fun.(*ast.Ident); ok && c.Name == "cancel" && len(s.Call.Args) == 0 {
				deferred = true
			}
		}
	}
	if !cancelled || !deferred {
		return nil
	}
	var out []token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Cleanup" || !isIdent(sel.X, "t") {
			return true
		}
		lit, ok := call.Args[0].(*ast.FuncLit)
		if !ok {
			return true
		}
		// The first declaration of ctx in the cleanup shadows the test's from
		// there on.
		own := token.NoPos
		ast.Inspect(lit.Body, func(n ast.Node) bool {
			if a, ok := n.(*ast.AssignStmt); ok && a.Tok == token.DEFINE && own == token.NoPos {
				for _, l := range a.Lhs {
					if isIdent(l, "ctx") {
						own = a.Pos()
					}
				}
			}
			return true
		})
		// One finding per cleanup: its first use of the test's ctx.
		first := token.NoPos
		ast.Inspect(lit.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "ctx" && (own == token.NoPos || id.Pos() < own) && first == token.NoPos {
				first = id.Pos()
			}
			return first == token.NoPos
		})
		if first != token.NoPos {
			out = append(out, first)
		}
		return false
	})
	return out
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func TestCleanupsUsingCancelledContext(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"the deferred cancel's ctx", `ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.Cleanup(func() { _, _ = c.Delete(ctx, name) })`, 1},
		{"nested, with a wait", `ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range names {
		t.Cleanup(func() {
			if op, err := c.Delete(ctx, n); err == nil {
				_, _ = op.Wait(ctx)
			}
		})
	}`, 1},
		{"a context of its own", `ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = c.Delete(ctx, name)
	})`, 0},
		{"no deferred cancel", `ctx := h.Context()
	t.Cleanup(func() { _, _ = c.Delete(ctx, name) })`, 0},
		{"a cleanup that uses no context", `ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.Cleanup(func() { _ = c.Close() })`, 0},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\nfunc f() {\n\t"+c.body+"\n}\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := cleanupsUsingCancelledContext(file.Decls[0].(*ast.FuncDecl).Body); len(got) != c.want {
			t.Errorf("%s: %d cleanups found, want %d", c.name, len(got), c.want)
		}
	}
}
