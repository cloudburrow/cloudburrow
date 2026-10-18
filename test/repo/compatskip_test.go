package repo

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every compat test asks for each endpoint it uses before it uses it, and
// skips where that endpoint is not served: CI runs shards with partial
// service sets (scripts/compat-shards.sh), and a developer's instance serves
// what they started. TestBigQueryNumericRounding and
// TestBigQueryNestedNumericRounding failed rather than skipped without
// CLOUDBURROW_TEST_BIGQUERY_STORAGE (#1625): their Storage Write API
// subtests asked the parent test's harness for the endpoint, and its Skip,
// on the parent's T from the subtest's goroutine, ends the subtest as a
// failure ("subtest may have called FailNow on a parent test").
//
// This finds, in test/compat:
//   - a CLOUDBURROW_TEST_* endpoint read with os.Getenv or os.LookupEnv
//     whose value is never compared with "" (nor its ok checked), in the
//     function that reads it, so an unserved endpoint is dialed as "" (and
//     a client given no endpoint reaches for Google's);
//   - the same for a helper that returns such a read unchecked: each of its
//     callers checks the value instead;
//   - in a t.Run closure, a call that can skip (Harness.Endpoint, and every
//     function reaching it through its *Harness parameter) given a harness
//     the closure did not make for itself: h.For(t) binds it to the subtest.
func TestCompatTestsSkipWhereTheirEndpointsAreNotServed(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "compat", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files in test/compat")
	}
	srcs := map[string][]byte{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		srcs[filepath.Base(f)] = raw
	}
	for _, p := range endpointSkipProblems(t, srcs, compatEndpointVars(t)) {
		t.Error(p)
	}
}

// The check fails on planted bad tests, and passes their fixed versions.
func TestEndpointSkipCheckFindsPlantedTests(t *testing.T) {
	const header = `package compat

import (
	"os"
	"strings"
	"testing"
)

const EnvKMS = "CLOUDBURROW_TEST_KMS"

type Harness struct{ t *testing.T }

func New(t *testing.T) *Harness { return &Harness{t: t} }
func (h *Harness) Endpoint(env string) string { h.t.Skip(); return "" }
func (h *Harness) For(t *testing.T) *Harness { return &Harness{t: t} }
func kmsClient(t *testing.T, h *Harness) string { return h.Endpoint(EnvKMS) }
func optionalKMS() string { return strings.TrimSpace(os.Getenv(EnvKMS)) }
func kmsIfSet(h *Harness) string {
	if os.Getenv(EnvKMS) == "" {
		return ""
	}
	return h.Endpoint(EnvKMS)
}
func dial(string) {}
`
	vars := map[string]bool{"CLOUDBURROW_TEST_KMS": true, "CLOUDBURROW_TEST_PUBSUB": true}
	for _, c := range []struct {
		name, body string
		want       string // a substring of the one problem, or "" for none
	}{
		{"unchecked Getenv", `func TestA(t *testing.T) { dial(os.Getenv(EnvKMS)) }`, "CLOUDBURROW_TEST_KMS"},
		{"unchecked literal", `func TestA(t *testing.T) { addr := os.Getenv("CLOUDBURROW_TEST_PUBSUB"); dial(addr) }`, "CLOUDBURROW_TEST_PUBSUB"},
		{"checked", `func TestA(t *testing.T) { addr := os.Getenv(EnvKMS); if addr == "" { t.Skip() }; dial(addr) }`, ""},
		{"checked in the condition", `func TestA(t *testing.T) { if strings.TrimSpace(os.Getenv(EnvKMS)) == "" { t.Skip() } }`, ""},
		{"LookupEnv ok", `func TestA(t *testing.T) { v, ok := os.LookupEnv(EnvKMS); if !ok { t.Skip() }; dial(v) }`, ""},
		{"not an endpoint", `func TestA(t *testing.T) { dial(os.Getenv("CLOUDBURROW_TEST_CLI_ARGS")) }`, ""},
		{"unchecked helper", `func TestA(t *testing.T) { dial(optionalKMS()) }`, "optionalKMS"},
		{"unchecked IfSet helper", `func TestA(t *testing.T) { h := New(t); dial(kmsIfSet(h)) }`, "kmsIfSet"},
		{"checked IfSet helper", `func TestA(t *testing.T) { h := New(t); if a := kmsIfSet(h); a != "" { dial(a) } }`, ""},
		{"checked helper", `func TestA(t *testing.T) { if a := optionalKMS(); a != "" { dial(a) } }`, ""},
		{"parent harness in a subtest", `func TestA(t *testing.T) { h := New(t); t.Run("x", func(t *testing.T) { dial(h.Endpoint(EnvKMS)) }) }`, "h.Endpoint"},
		{"parent harness to a helper in a subtest", `func TestA(t *testing.T) { h := New(t); t.Run("x", func(t *testing.T) { dial(kmsClient(t, h)) }) }`, "kmsClient"},
		{"subtest's own harness", `func TestA(t *testing.T) { h := New(t); t.Run("x", func(t *testing.T) { h := h.For(t); dial(kmsClient(t, h)) }) }`, ""},
		{"endpoint asked in the parent", `func TestA(t *testing.T) { h := New(t); a := h.Endpoint(EnvKMS); t.Run("x", func(t *testing.T) { dial(a) }) }`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := endpointSkipProblems(t, map[string][]byte{"planted_test.go": []byte(header + c.body + "\n")}, vars)
			switch {
			case c.want == "" && len(got) != 0:
				t.Errorf("problems %q, want none", got)
			case c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want)):
				t.Errorf("problems %q, want one naming %q", got, c.want)
			}
		})
	}
}

// compatEndpointVars is every CLOUDBURROW_TEST_* variable compat-env.sh
// exports that names a service's endpoint: all of them but the instance's
// fixtures and values that are not addresses.
func compatEndpointVars(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "compat-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\nALL=\(\n(.*?)\n\)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("compat-env.sh has no ALL=( ... ) list")
	}
	notEndpoints := map[string]bool{}
	for _, v := range strings.Fields("CREDENTIALS KUBECONFIG CLUSTER ADMIN_TOKEN CLI CLI_ARGS CORS_ORIGIN MEMORYSTORE_PROJECT MYSQL_PASSWORD BIGQUERY_PROJECT GCLOUD TOFU") {
		notEndpoints[v] = true
	}
	vars := map[string]bool{}
	for _, v := range strings.Fields(string(m[1])) {
		if !notEndpoints[v] {
			vars["CLOUDBURROW_TEST_"+v] = true
		}
	}
	if !vars["CLOUDBURROW_TEST_BIGQUERY_STORAGE"] || !vars["CLOUDBURROW_TEST_KMS"] {
		t.Fatalf("compat-env.sh's ALL list lost the endpoints: %v", vars)
	}
	return vars
}

// endpointSkipProblems parses srcs (file name to source, one package) and
// returns one line per problem.
func endpointSkipProblems(t *testing.T, srcs map[string][]byte, endpointVars map[string]bool) []string {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	names := make([]string, 0, len(srcs))
	for n := range srcs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, err := parser.ParseFile(fset, n, srcs[n], parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}

	// The package's string constants, for os.Getenv(EnvKMS).
	consts := map[string]string{}
	funcs := map[string]*ast.FuncDecl{}   // top-level functions by name
	methods := map[string]*ast.FuncDecl{} // *Harness methods by name
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, s := range d.Specs {
					vs := s.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								consts[n.Name], _ = strconv.Unquote(lit.Value)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				if d.Recv == nil {
					funcs[d.Name.Name] = d
				} else if len(d.Recv.List) == 1 && isHarnessType(d.Recv.List[0].Type) {
					methods[d.Name.Name] = d
				}
			}
		}
	}
	endpointRead := func(e ast.Expr) (string, bool) {
		c, ok := e.(*ast.CallExpr)
		if !ok || len(c.Args) != 1 || !isSel(c.Fun, "os", "Getenv") && !isSel(c.Fun, "os", "LookupEnv") {
			return "", false
		}
		var v string
		switch a := c.Args[0].(type) {
		case *ast.BasicLit:
			v, _ = strconv.Unquote(a.Value)
		case *ast.Ident:
			v = consts[a.Name]
		}
		return v, endpointVars[v]
	}

	// Skippers: Harness.Endpoint, and each function or *Harness method that
	// reaches a skipper through its *Harness parameter (its index, -1 for
	// the receiver).
	skipper := map[string]int{} // function -> index of its *Harness parameter
	skipMethod := map[string]bool{"Endpoint": true}
	harnessParams := func(fn *ast.FuncDecl) map[string]bool {
		hs := map[string]bool{}
		if fn.Recv != nil {
			for _, n := range fn.Recv.List[0].Names {
				hs[n.Name] = true
			}
		}
		for _, f := range fn.Type.Params.List {
			if isHarnessType(f.Type) {
				for _, n := range f.Names {
					hs[n.Name] = true
				}
			}
		}
		return hs
	}
	// reachesSkip reports whether body calls a skipper with one of hs.
	reachesSkip := func(body ast.Node, hs map[string]bool) (string, bool) {
		var found string
		ast.Inspect(body, func(n ast.Node) bool {
			if found != "" {
				return false
			}
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := c.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := f.X.(*ast.Ident); ok && hs[x.Name] && skipMethod[f.Sel.Name] {
					found = x.Name + "." + f.Sel.Name
				}
				// h.t.Skip*, h.t.Fatal* on the harness's own T.
				if inner, ok := f.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "t" && strings.HasPrefix(f.Sel.Name, "Skip") {
					if x, ok := inner.X.(*ast.Ident); ok && hs[x.Name] {
						found = x.Name + ".t." + f.Sel.Name
					}
				}
			case *ast.Ident:
				if i, ok := skipper[f.Name]; ok && i >= 0 && i < len(c.Args) {
					if a, ok := c.Args[i].(*ast.Ident); ok && hs[a.Name] {
						found = f.Name
					}
				}
			}
			return true
		})
		return found, found != ""
	}
	harnessIndex := func(fn *ast.FuncDecl) int {
		i := 0
		for _, f := range fn.Type.Params.List {
			n := len(f.Names)
			if n == 0 {
				n = 1
			}
			if isHarnessType(f.Type) {
				return i
			}
			i += n
		}
		return -1
	}
	for changed := true; changed; {
		changed = false
		for name, fn := range funcs {
			if _, ok := skipper[name]; ok {
				continue
			}
			if i := harnessIndex(fn); i >= 0 {
				if _, ok := reachesSkip(fn.Body, harnessParams(fn)); ok {
					skipper[name] = i
					changed = true
				}
			}
		}
		for name, fn := range methods {
			if skipMethod[name] {
				continue
			}
			if _, ok := reachesSkip(fn.Body, harnessParams(fn)); ok {
				skipMethod[name] = true
				changed = true
			}
		}
	}

	// Optional helpers: functions returning an endpoint read unchecked, so
	// their callers must check the value. A fixed point, for helpers of
	// helpers.
	optional := map[string]string{} // helper name -> the variable it reads
	optionalCall := func(e ast.Expr) (string, string, bool) {
		e = unwrapTrim(e)
		if v, ok := endpointRead(e); ok {
			return v, "", true
		}
		if c, ok := e.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok {
				if v, ok := optional[id.Name]; ok {
					return v, id.Name, true
				}
			}
		}
		return "", "", false
	}
	for changed := true; changed; {
		changed = false
		for name, fn := range funcs {
			if _, ok := optional[name]; ok {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if _, ok := n.(*ast.FuncLit); ok {
					return false
				}
				if r, ok := n.(*ast.ReturnStmt); ok && len(r.Results) == 1 {
					if v, _, ok := optionalCall(r.Results[0]); ok {
						optional[name] = v
						changed = true
					}
				}
				return true
			})
			// A helper that asks for an endpoint, and returns "" without
			// skipping when it is not served (runShardEndpointIfSet).
			if _, ok := optional[name]; !ok && returnsEmpty(fn.Body) {
				if _, asks := reachesSkip(fn.Body, harnessParams(fn)); asks {
					optional[name] = "an endpoint"
					changed = true
				}
			}
		}
	}

	var problems []string
	report := func(pos token.Pos, format string, args ...any) {
		problems = append(problems, fset.Position(pos).String()+": "+fmt.Sprintf(format, args...))
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if _, ok := optional[fn.Name.Name]; ok {
				continue // its callers check
			}
			checkEndpointReads(fn.Body, optionalCall, report)
			checkSubtests(fset, fn, skipper, skipMethod, report)
		}
	}
	return problems
}

// checkEndpointReads reports each endpoint read in body whose value is
// neither compared with "" where it is read nor assigned to a variable
// that body compares with "" (or, for LookupEnv, whose ok it tests).
func checkEndpointReads(body *ast.BlockStmt, read func(ast.Expr) (string, string, bool), report func(token.Pos, string, ...any)) {
	checked := map[string]bool{} // variables compared with "" or tested
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if n.Op == token.EQL || n.Op == token.NEQ {
				for _, pair := range [][2]ast.Expr{{n.X, n.Y}, {n.Y, n.X}} {
					if isEmptyString(pair[1]) {
						if id, ok := unwrapTrim(pair[0]).(*ast.Ident); ok {
							checked[id.Name] = true
						}
					}
				}
			}
		case *ast.UnaryExpr:
			if id, ok := n.X.(*ast.Ident); ok && n.Op == token.NOT {
				checked[id.Name] = true
			}
		case *ast.IfStmt:
			if id, ok := n.Cond.(*ast.Ident); ok {
				checked[id.Name] = true
			}
		case *ast.CallExpr:
			// net.SplitHostPort("") fails, and the caller tests its error.
			if isSel(n.Fun, "net", "SplitHostPort") && len(n.Args) == 1 {
				if id, ok := unwrapTrim(n.Args[0]).(*ast.Ident); ok {
					checked[id.Name] = true
				}
			}
		}
		return true
	})
	// Reads compared in place: os.Getenv(X) == "", split by
	// net.SplitHostPort, or only printed by a Skip or a Log.
	inPlace := map[ast.Expr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && (isSel(c.Fun, "net", "SplitHostPort") || strings.HasPrefix(s.Sel.Name, "Skip") || strings.HasPrefix(s.Sel.Name, "Log")) {
				for _, a := range c.Args {
					inPlace[unwrapTrim(a)] = true
				}
			}
		}
		if b, ok := n.(*ast.BinaryExpr); ok && (b.Op == token.EQL || b.Op == token.NEQ) {
			for _, pair := range [][2]ast.Expr{{b.X, b.Y}, {b.Y, b.X}} {
				if isEmptyString(pair[1]) {
					inPlace[unwrapTrim(pair[0])] = true
				}
			}
		}
		return true
	})
	// Reads assigned to variables.
	assigned := map[ast.Expr][]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Rhs) == 1 {
				var names []string
				for _, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						names = append(names, id.Name)
					}
				}
				assigned[unwrapTrim(n.Rhs[0])] = names
			} else if len(n.Lhs) == len(n.Rhs) {
				for i, r := range n.Rhs {
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						assigned[unwrapTrim(r)] = []string{id.Name}
					}
				}
			}
		case *ast.ValueSpec:
			for i, r := range n.Values {
				if i < len(n.Names) {
					assigned[unwrapTrim(r)] = []string{n.Names[i].Name}
				}
			}
		}
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		v, helper, ok := read(e)
		if !ok {
			return true
		}
		if inPlace[unwrapTrim(e)] {
			return false
		}
		for _, name := range assigned[unwrapTrim(e)] {
			if checked[name] {
				return false
			}
		}
		if helper != "" {
			report(e.Pos(), "%s() may return \"\" (%s is not exported where its service is not served); compare its value with \"\" and skip", helper, v)
		} else {
			report(e.Pos(), "%s is read and never compared with \"\"; ask h.Endpoint(%s) (or skip when it is empty) before using it", v, v)
		}
		return false
	})
}

// checkSubtests reports, in each t.Run closure in body, a call that can
// skip given a harness the closure did not declare: its Skip would run on
// the parent's T from the subtest's goroutine, which fails the subtest.
func checkSubtests(fset *token.FileSet, fn *ast.FuncDecl, skipper map[string]int, skipMethod map[string]bool, report func(token.Pos, string, ...any)) {
	harnesses := harnessesIn(fn)
	line := func(lit *ast.FuncLit) int { return fset.Position(lit.Pos()).Line }
	tName := func(lit *ast.FuncLit) string {
		if names := lit.Type.Params.List[0].Names; len(names) == 1 {
			return names[0].Name
		}
		return "t"
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || len(c.Args) != 2 {
			return true
		}
		if sel, ok := c.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Run" {
			return true
		}
		lit, ok := c.Args[1].(*ast.FuncLit)
		if !ok || len(lit.Type.Params.List) != 1 {
			return true
		}
		own := declaredIn(lit)
		ast.Inspect(lit.Body, func(n ast.Node) bool {
			// A nested t.Run is visited by the outer Inspect on its own.
			if inner, ok := n.(*ast.CallExpr); ok && inner != c && len(inner.Args) == 2 {
				if sel, ok := inner.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
					if _, ok := inner.Args[1].(*ast.FuncLit); ok {
						return false
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := f.X.(*ast.Ident); ok && skipMethod[f.Sel.Name] && !own[x.Name] && harnesses[x.Name] {
					report(call.Pos(), "%s.%s in the subtest at line %d uses the parent test's harness, whose Skip fails the subtest; begin it with %s := %s.For(%s)", x.Name, f.Sel.Name, line(lit), x.Name, x.Name, tName(lit))
				}
			case *ast.Ident:
				if i, ok := skipper[f.Name]; ok && i < len(call.Args) {
					if a, ok := call.Args[i].(*ast.Ident); ok && !own[a.Name] {
						report(call.Pos(), "%s(..., %s, ...) in the subtest at line %d uses the parent test's harness, whose Skip fails the subtest; begin it with %s := %s.For(%s)", f.Name, a.Name, line(lit), a.Name, a.Name, tName(lit))
					}
				}
			}
			return true
		})
		return true
	})
}

// harnessesIn is the names fn gives a *Harness: its receiver and
// parameters of that type, and the variables it assigns New(...) or
// x.For(...) to.
func harnessesIn(fn *ast.FuncDecl) map[string]bool {
	hs := map[string]bool{}
	var fields []*ast.Field
	if fn.Recv != nil {
		fields = append(fields, fn.Recv.List...)
	}
	fields = append(fields, fn.Type.Params.List...)
	for _, f := range fields {
		if isHarnessType(f.Type) {
			for _, n := range f.Names {
				hs[n.Name] = true
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		a, ok := n.(*ast.AssignStmt)
		if !ok || len(a.Lhs) != len(a.Rhs) {
			return true
		}
		for i, r := range a.Rhs {
			c, ok := r.(*ast.CallExpr)
			if !ok {
				continue
			}
			isNew := false
			switch f := c.Fun.(type) {
			case *ast.Ident:
				isNew = f.Name == "New"
			case *ast.SelectorExpr:
				isNew = f.Sel.Name == "For"
			}
			if id, ok := a.Lhs[i].(*ast.Ident); ok && isNew {
				hs[id.Name] = true
			}
		}
		return true
	})
	return hs
}

// declaredIn is the names a function literal declares: its parameters and
// every variable its body declares.
func declaredIn(lit *ast.FuncLit) map[string]bool {
	own := map[string]bool{}
	for _, f := range lit.Type.Params.List {
		for _, n := range f.Names {
			own[n.Name] = true
		}
	}
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						own[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, id := range n.Names {
				own[id.Name] = true
			}
		case *ast.FuncLit:
			for _, f := range n.Type.Params.List {
				for _, id := range f.Names {
					own[id.Name] = true
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{n.Key, n.Value} {
				if id, ok := e.(*ast.Ident); ok && n.Tok == token.DEFINE {
					own[id.Name] = true
				}
			}
		}
		return true
	})
	return own
}

// returnsEmpty reports whether body, outside its function literals,
// returns "".
func returnsEmpty(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if r, ok := n.(*ast.ReturnStmt); ok && len(r.Results) == 1 && isEmptyString(r.Results[0]) {
			found = true
		}
		return !found
	})
	return found
}

func isHarnessType(e ast.Expr) bool {
	s, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == "Harness"
}

func isSel(e ast.Expr, pkg, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != name {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func isEmptyString(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

// unwrapTrim strips strings.TrimSpace and parentheses.
func unwrapTrim(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.CallExpr:
			if isSel(x.Fun, "strings", "TrimSpace") && len(x.Args) == 1 {
				e = x.Args[0]
				continue
			}
			return e
		default:
			return e
		}
	}
}
