package archtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const module = "github.com/cloudburrow/cloudburrow"

// allowedServiceImports lists the known cases of one service or adapter
// package depending, directly or transitively, on another's, keyed
// "from -> to" by module-relative import path. It is empty: the one violation
// found when the test was added, internal/service/scheduler importing
// internal/service/tasks for tasks.Backoff, was removed by moving the Cloud
// Tasks retry schedule into internal/sched (#599 PR 1). Keep it empty; a
// cross-service need goes through a narrow interface declared by the consumer
// and wired in internal/lifecycle (docs/architecture.md §3, rule 1).
var allowedServiceImports = map[string]string{}

// allowedKubectl lists the packages, other than internal/cluster and
// internal/k8s, that exec kubectl today, keyed by module-relative import path.
// Each PR of #599 that moves a package onto the internal/k8s runner removes
// its entry here (docs/architecture.md §3, rule 2). PR 2 moved
// internal/service/secrets (and KMS, which borrowed its runner) and
// internal/adapter/run; PR 3 moved internal/netfwd (its reads and its
// port-forward), internal/images and internal/components.
var allowedKubectl = map[string]string{
	// consolelogs.go, consolemetrics.go, consoleproviders.go, logs.go,
	// pgsnapshot.go and clusterhost.go shell out to kubectl directly.
	"cmd/cloudburrow": "console, logs, pgsnapshot and cluster-host wiring; moves to internal/k8s in #599 PR 4",
}

// allowedCmdKubeHelpers lists cmd files that use a kubectl helper exported by
// a service or adapter package, keyed "file: package.Symbol". It is empty:
// the three found when the test was added (KMS and Secret Manager wiring
// using secrets.KubectlRunner, Cloud Run wiring using run.ExecRunner) were
// removed when both moved onto internal/k8s (#599 PR 2). Keep it empty; cmd
// wiring builds a k8s.Runner and hands it to the service.
var allowedCmdKubeHelpers = map[string]string{}

// pkg is the part of `go list -json` output the rules read.
type pkg struct {
	ImportPath     string
	Dir            string
	GoFiles        []string
	CgoFiles       []string
	IgnoredGoFiles []string
	Deps           []string
}

// sources returns the package's non-test Go files, including those excluded
// by build constraints, so a kubectl call behind a tag is still seen.
func (p pkg) sources() []string {
	var files []string
	for _, group := range [][]string{p.GoFiles, p.CgoFiles, p.IgnoredGoFiles} {
		for _, f := range group {
			if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
				files = append(files, filepath.Join(p.Dir, f))
			}
		}
	}
	sort.Strings(files)
	return files
}

var (
	loadOnce sync.Once
	loaded   []pkg
	loadErr  error
	rootDir  string
)

// packages lists every package in the module, with its transitive
// dependencies, once per test binary.
func packages(t *testing.T) []pkg {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	loadOnce.Do(func() {
		rootDir, loadErr = filepath.Abs(filepath.Join("..", ".."))
		if loadErr != nil {
			return
		}
		if _, loadErr = os.Stat(filepath.Join(rootDir, "go.mod")); loadErr != nil {
			return
		}
		cmd := exec.Command("go", "list", "-e", "-json", "./...")
		cmd.Dir = rootDir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			loadErr = fmt.Errorf("go list -json ./...: %w: %s", err, stderr.String())
			return
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for dec.More() {
			var p pkg
			if loadErr = dec.Decode(&p); loadErr != nil {
				return
			}
			loaded = append(loaded, p)
		}
	})
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(loaded) == 0 {
		t.Fatal("go list found no packages")
	}
	return loaded
}

// rel trims the module path from an import path.
func rel(importPath string) string { return strings.TrimPrefix(importPath, module+"/") }

// relFile makes a file path module-relative, with forward slashes.
func relFile(file string) string {
	if r, err := filepath.Rel(rootDir, file); err == nil {
		return filepath.ToSlash(r)
	}
	return file
}

// under reports whether importPath is dir or a package below it.
func under(importPath, dir string) bool {
	p := module + "/" + dir
	return importPath == p || strings.HasPrefix(importPath, p+"/")
}

// serviceOf names the service or adapter an import path belongs to
// ("internal/service/tasks", "internal/adapter/run"), or "" for any other
// package. Subpackages belong to their service.
func serviceOf(importPath string) string {
	for _, root := range []string{"internal/service/", "internal/adapter/"} {
		prefix := module + "/" + root
		if rest, ok := strings.CutPrefix(importPath, prefix); ok && rest != "" {
			name, _, _ := strings.Cut(rest, "/")
			return root + name
		}
	}
	return ""
}

// compare fails for every finding the allow-list does not name, and for every
// allow-list entry that no longer matches a finding, so the list can only
// shrink as the code is fixed.
func compare(t *testing.T, rule string, found map[string][]string, allowed map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := allowed[k]; !ok {
			t.Errorf("%s: %s\n\tat %s", rule, k, strings.Join(found[k], ", "))
		}
	}
	stale := make([]string, 0)
	for k := range allowed {
		if _, ok := found[k]; !ok {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		t.Errorf("allow-list entry %q no longer matches anything; remove it", k)
	}
}

// parse reads one Go file, comments dropped.
func parse(t *testing.T, fset *token.FileSet, file string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return f
}

// Rule 1: adapters (and the services they are wired as) may not import each
// other. Deps is transitive, so importing another service through a third
// package is caught too.
func TestServicesDoNotImportOtherServices(t *testing.T) {
	found := map[string][]string{}
	for _, p := range packages(t) {
		own := serviceOf(p.ImportPath)
		if own == "" {
			continue
		}
		for _, dep := range p.Deps {
			if other := serviceOf(dep); other != "" && other != own {
				k := rel(p.ImportPath) + " -> " + rel(dep)
				found[k] = append(found[k], "go list -deps "+rel(p.ImportPath))
			}
		}
	}
	compare(t, "a service or adapter package imports another service's (docs/architecture.md §3 rule 1)", found, allowedServiceImports)
}

// Rule 2: only internal/cluster and internal/k8s exec kubectl.
//
// A package execs kubectl, for this test, when a non-test file passes the
// program name as a call argument: the string literal "kubectl" (or a path
// whose base is kubectl), or a constant declared in the same package with that
// value. That covers exec.Command, exec.CommandContext and every Runner-style
// wrapper the module uses, which take the program name as an argument
// (runner.Run(ctx, "kubectl", ...), Run(ctx, stdin, "kubectl", ...)).
//
// It does not see a name that reaches the call through a variable, a slice,
// a struct field or a constant from another package; one built at run time;
// or kubectl inside a shell string (sh -c "kubectl ..."). Those are for
// review. It ignores shadowing, which can only over-report. A package that
// only looks kubectl up (internal/doctor's LookPath) does not exec it and is
// not reported.
func TestOnlyTheClusterPackagesExecKubectl(t *testing.T) {
	found := map[string][]string{}
	for _, p := range packages(t) {
		if !under(p.ImportPath, "internal") && !under(p.ImportPath, "cmd") {
			continue
		}
		if under(p.ImportPath, "internal/cluster") || under(p.ImportPath, "internal/k8s") {
			continue
		}
		for _, site := range kubectlSites(t, p) {
			k := rel(p.ImportPath)
			found[k] = append(found[k], site)
		}
	}
	compare(t, "a package outside internal/cluster and internal/k8s execs kubectl (docs/architecture.md §3 rule 2)", found, allowedKubectl)
}

// kubectlSites returns file:line for each call in p that names kubectl.
func kubectlSites(t *testing.T, p pkg) []string {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range p.sources() {
		files = append(files, parse(t, fset, name))
	}
	// Constants holding the program name, anywhere in the package.
	consts := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			gd, ok := n.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				return true
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, v := range vs.Values {
					if i < len(vs.Names) && isKubectlLiteral(v) {
						consts[vs.Names[i].Name] = true
					}
				}
			}
			return true
		})
	}
	var sites []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, arg := range call.Args {
				id, isIdent := arg.(*ast.Ident)
				if isKubectlLiteral(arg) || (isIdent && consts[id.Name]) {
					pos := fset.Position(arg.Pos())
					sites = append(sites, fmt.Sprintf("%s:%d", relFile(pos.Filename), pos.Line))
					break
				}
			}
			return true
		})
	}
	return sites
}

// isKubectlLiteral reports whether e is a string literal naming the kubectl
// program, bare or by path.
func isKubectlLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && path.Base(filepath.ToSlash(s)) == "kubectl"
}

// Rule 2, from the cmd side: cmd/ borrows no kubectl helper from a service or
// adapter package. A kubectl helper is an exported identifier whose name
// contains "Kubectl", or is named ExecRunner as the Run adapter's was. It is a
// name check: the only runner is internal/k8s's, and a service exporting a new
// one under another name is for review.
func TestCmdBorrowsNoServiceKubeHelpers(t *testing.T) {
	found := map[string][]string{}
	for _, p := range packages(t) {
		if !under(p.ImportPath, "cmd") {
			continue
		}
		fset := token.NewFileSet()
		for _, name := range p.sources() {
			f := parse(t, fset, name)
			imported := map[string]string{} // local name -> service import path
			for _, imp := range f.Imports {
				ip, err := strconv.Unquote(imp.Path.Value)
				if err != nil || serviceOf(ip) == "" {
					continue
				}
				local := path.Base(ip)
				if imp.Name != nil {
					local = imp.Name.Name
				}
				imported[local] = ip
			}
			if len(imported) == 0 {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				ip, ok := imported[x.Name]
				if !ok || !isKubeHelper(sel.Sel.Name) {
					return true
				}
				pos := fset.Position(sel.Pos())
				k := relFile(pos.Filename) + ": " + rel(ip) + "." + sel.Sel.Name
				found[k] = append(found[k], fmt.Sprintf("%s:%d", relFile(pos.Filename), pos.Line))
				return true
			})
		}
	}
	compare(t, "cmd/ uses a service's kubectl helper (docs/architecture.md §3 rule 2; #599)", found, allowedCmdKubeHelpers)
}

func isKubeHelper(name string) bool {
	return strings.Contains(name, "Kubectl") || name == "ExecRunner"
}

// The rules must see something, or a broken go list or parse would pass them
// silently.
func TestTheRulesSeeTheModule(t *testing.T) {
	var services, sites, k8sSites int
	for _, p := range packages(t) {
		if serviceOf(p.ImportPath) != "" {
			services++
		}
		if under(p.ImportPath, "internal/cluster") {
			sites += len(kubectlSites(t, p))
		}
		if under(p.ImportPath, "internal/k8s") {
			k8sSites += len(kubectlSites(t, p))
		}
	}
	if services < 2 {
		t.Errorf("found %d service and adapter packages; the listing is broken", services)
	}
	if sites == 0 {
		t.Error("found no kubectl call in internal/cluster; the detection is broken")
	}
	// internal/k8s is the permitted exec site for everything else; if the
	// detection cannot see its runner, the exemption proves nothing.
	if k8sSites == 0 {
		t.Error("found no kubectl call in internal/k8s; the detection is broken")
	}
}
