package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestConsoleDocumentAISpeechStorageBrowse (#2097): every gs:// field of
// Document AI's and Speech-to-Text's forms (a console.Field literal whose
// Pattern takes gs://) has Browse, the shared Cloud Storage picker, of the
// kind Google's dialog chooses: an object for Speech's Audio file, a folder
// or an object for a Source path, a folder otherwise; New workspace's
// bucket chooses a bucket.
func TestConsoleDocumentAISpeechStorageBrowse(t *testing.T) {
	want := map[string]string{
		"input": "storage-path", "source": "storage-path", "uri": "storage-object",
		"output": "storage", "gcsPrefix": "storage", "documents": "storage",
	}
	files, err := filepath.Glob("consoledocumentai*.go")
	if err != nil {
		t.Fatal(err)
	}
	more, _ := filepath.Glob("consolespeech*.go")
	seen := map[string]bool{}
	for _, f := range append(files, more...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			vals := map[string]string{}
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				k, ok1 := kv.Key.(*ast.Ident)
				v, ok2 := kv.Value.(*ast.BasicLit)
				if ok1 && ok2 && v.Kind == token.STRING {
					s, _ := strconv.Unquote(v.Value)
					vals[k.Name] = s
				}
			}
			if !strings.Contains(vals["Pattern"], "gs://") {
				return true
			}
			name := vals["Name"]
			seen[name] = true
			if vals["Browse"] != want[name] {
				t.Errorf("%s: field %q has Browse %q, want %q", f, name, vals["Browse"], want[name])
			}
			return true
		})
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("no gs:// field %q was found", name)
		}
	}
	for _, f := range workspaceFields([]string{"ws"}) {
		if f.Name == "workspaceBucket" && f.Browse != "storage-bucket" {
			t.Errorf("New workspace's bucket has Browse %q", f.Browse)
		}
	}
}
