package repo

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// clientConstructors are the official Go client constructors whose clients
// export built-in metrics to Google (Cloud Monitoring) whatever endpoint they
// are given, by import path. internal/localclient wraps each with the export
// off; see its package comment for where each library turns it on.
var clientConstructors = map[string][]string{
	"cloud.google.com/go/spanner":  {"NewClient", "NewClientWithConfig", "NewMultiEndpointClient", "NewMultiEndpointClientWithConfig"},
	"cloud.google.com/go/bigtable": {"NewClient", "NewClientWithConfig"},
	"cloud.google.com/go/storage":  {"NewGRPCClient"},
}

// TestOfficialClientsGoThroughLocalclient: every Spanner and Bigtable data
// client and every Cloud Storage gRPC client in the repository is made by
// internal/localclient, which turns their built-in metrics off. A test that
// made a Spanner client with option.WithEndpoint alone exported to
// monitoring.googleapis.com every minute; a new direct call would again.
func TestOfficialClientsGoThroughLocalclient(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	checked := 0
	for _, f := range bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0}) {
		rel := filepath.ToSlash(string(f))
		if strings.HasPrefix(rel, "third_party/") || strings.HasPrefix(rel, "internal/localclient/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range directClientCalls(t, rel, src) {
			t.Errorf("%s: %s: make it with internal/localclient, which turns the client's built-in metrics export to Google off", rel, call)
		}
		checked++
	}
	if checked < 100 {
		t.Fatalf("checked only %d Go files: is this the repository?", checked)
	}
}

// directClientCalls is each call in src of a constructor in
// clientConstructors, as "line N: pkg.Func".
func directClientCalls(t *testing.T, name string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	local := map[string]string{} // the name a file uses for an import -> its path
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if _, ok := clientConstructors[path]; !ok {
			continue
		}
		n := filepath.Base(path)
		if imp.Name != nil {
			n = imp.Name.Name
		}
		local[n] = path
	}
	if len(local) == 0 {
		return nil
	}
	var calls []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		for _, fn := range clientConstructors[local[id.Name]] {
			if sel.Sel.Name == fn {
				calls = append(calls, "line "+strconv.Itoa(fset.Position(sel.Pos()).Line)+": "+id.Name+"."+fn)
			}
		}
		return true
	})
	return calls
}

// The check finds a direct call, under an import alias too, and leaves a
// call through localclient alone.
func TestDirectClientCallsFindsAliases(t *testing.T) {
	src := []byte(`package p

import (
	sp "cloud.google.com/go/spanner"
	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/localclient"
)

func f() {
	_, _ = sp.NewClient(nil, "")
	_, _ = bigtable.NewClientWithConfig(nil, "", "", bigtable.ClientConfig{})
	_, _ = storage.NewClient(nil)
	_, _ = localclient.StorageGRPC(nil)
	g := storage.NewGRPCClient
	_ = g
}
`)
	got := directClientCalls(t, "p.go", src)
	want := []string{"line 12: sp.NewClient", "line 13: bigtable.NewClientWithConfig", "line 16: storage.NewGRPCClient"}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Errorf("directClientCalls = %q, want %q", got, want)
	}
}
