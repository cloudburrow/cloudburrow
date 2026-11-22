package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// The handle-keepalive rewrite (cloudburrow/cloudburrow#2134) changes the
// go-googlesql bindings the way their generator, protoc-gen-wasmify-go
// (upstream, in goccy/googlesql-wasm), should emit them. A generated method
// copies the C++ pointers of its receiver and handle arguments into its
// request, then calls invokeMethod, which waits for the module's lock. A
// handle with no Go use after that copy can be finalized in the gap, its
// C++ object freed in the engine's WebAssembly memory, before the call
// reads it: on main CI a freed LanguageOptions inside BuiltinFunctionOptions
// made AddGoogleSQLFunctions2 spin forever. A patch cannot fix this
// systematically (it would touch some 25,000 generated methods), so the
// build rewrites the generated file, and fails if its shapes are not the
// ones below:
//
//   - after every `x, err := invokeMethod(...)`, the method keeps alive its
//     receiver and every parameter that can hold a handle (any but a
//     scalar: a handle, an interface, an options struct, a slice or a map),
//     so a handle nested in an options struct is kept by its struct;
//   - every finalizer moves from the outermost wrapper to the struct that
//     holds the C++ pointer. A wrapper embeds its base classes' wrappers
//     (SimpleCatalog embeds EnumerableCatalog embeds Catalog, which has
//     ptr), and a method of a base class has the base's wrapper as its
//     receiver: keeping that receiver alive does not keep the outer wrapper
//     alive, whose finalizer freed the object. The pointer's holder is
//     reachable from every level, so with the finalizer on it a receiver
//     at any level keeps the object. The finalizer frees as the outer
//     wrapper did, through a wrapper of the same class made for the call
//     (newXNoFinalizer(ptr).free()), and captures nothing else.
const keepAliveRewrite = "handle-keepalive"

// rewrites are the source rewrites sources.json can name, by name. Each
// has a version, part of the build's input hash: bump it when the rewrite
// changes what it writes.
var rewrites = map[string]struct {
	version string
	apply   func([]byte) ([]byte, error)
}{
	keepAliveRewrite: {"1", rewriteKeepAlive},
}

// scalarIdents are the predeclared types that cannot hold a handle.
var scalarIdents = map[string]bool{
	"bool": true, "string": true, "byte": true, "rune": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
}

type edit struct {
	at, end int // the bytes [at, end) are replaced
	text    string
}

func rewriteKeepAlive(src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "googlesql.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset }
	text := func(n ast.Node) string { return string(src[off(n.Pos()):off(n.End())]) }

	// Named types: which are scalars (enums), and each struct's fields.
	scalar := map[string]bool{}
	structs := map[string]*ast.StructType{}
	funcs := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			for _, s := range d.Specs {
				ts, ok := s.(*ast.TypeSpec)
				if !ok {
					continue
				}
				switch t := ts.Type.(type) {
				case *ast.Ident:
					if scalarIdents[t.Name] {
						scalar[ts.Name.Name] = true
					}
				case *ast.StructType:
					structs[ts.Name.Name] = t
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil {
				funcs[d.Name.Name] = true
			}
		}
	}
	isScalar := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && (scalarIdents[id.Name] || scalar[id.Name])
	}
	canHoldHandle := func(e ast.Expr) bool {
		switch t := e.(type) {
		case *ast.Ident:
			return !isScalar(t)
		case *ast.ArrayType:
			if isScalar(t.Elt) {
				return false
			}
			if inner, ok := t.Elt.(*ast.ArrayType); ok && isScalar(inner.Elt) {
				return false // [][]byte
			}
		case *ast.Ellipsis:
			return !isScalar(t.Elt)
		case *ast.FuncType:
			return false
		}
		return true
	}

	// holder is the path from struct name to the embedded struct that has
	// the ptr field ("" for the holder itself), and the holder's name.
	var holder func(name string, depth int) (string, string, error)
	holder = func(name string, depth int) (string, string, error) {
		st, ok := structs[name]
		if !ok || depth > 64 {
			return "", "", fmt.Errorf("type %s: not a wrapper struct", name)
		}
		var embedded []string
		for _, fl := range st.Fields.List {
			if len(fl.Names) == 0 {
				var id *ast.Ident
				if star, ok := fl.Type.(*ast.StarExpr); ok {
					id, _ = star.X.(*ast.Ident)
				}
				if id == nil {
					return "", "", fmt.Errorf("type %s: embeds %s, not a pointer to a wrapper", name, text(fl.Type))
				}
				embedded = append(embedded, id.Name)
				continue
			}
			for _, n := range fl.Names {
				if n.Name == "ptr" {
					if len(embedded) > 0 {
						return "", "", fmt.Errorf("type %s: has ptr and embeds %v", name, embedded)
					}
					return "", name, nil
				}
			}
		}
		if len(embedded) != 1 {
			return "", "", fmt.Errorf("type %s: no ptr field and %d embedded wrappers", name, len(embedded))
		}
		p, h, err := holder(embedded[0], depth+1)
		if err != nil {
			return "", "", err
		}
		return "." + embedded[0] + p, h, nil
	}

	var edits []edit
	calls, kept, moved := 0, 0, 0
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil || fd.Name.Name == "invokeMethod" {
			continue
		}
		// The values the call must not outlive: the receiver, then the
		// parameters, in order.
		var keep []string
		if fd.Recv != nil {
			for _, fl := range fd.Recv.List {
				for _, n := range fl.Names {
					if n.Name != "_" {
						keep = append(keep, n.Name)
					}
				}
			}
		}
		for _, fl := range fd.Type.Params.List {
			if !canHoldHandle(fl.Type) {
				continue
			}
			for _, n := range fl.Names {
				if n.Name != "_" {
					keep = append(keep, n.Name)
				}
			}
		}
		for _, st := range fd.Body.List {
			as, ok := st.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 || !isCallTo(as.Rhs[0], "invokeMethod") {
				continue
			}
			calls++
			if len(keep) == 0 {
				continue
			}
			var b strings.Builder
			for _, k := range keep {
				fmt.Fprintf(&b, "\n\truntime.KeepAlive(%s)", k)
			}
			edits = append(edits, edit{at: off(as.End()), end: off(as.End()), text: b.String()})
			kept++
		}

		// Finalizers, in newX(ptr) and NewXFromImpl, on h of type *X.
		var ferr error
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if ferr != nil {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || !isSelectorCall(call, "runtime", "SetFinalizer") {
				return true
			}
			if id, ok := call.Args[0].(*ast.Ident); !ok || id.Name != "h" || len(call.Args) != 2 {
				ferr = fmt.Errorf("%s: runtime.SetFinalizer(%s, ...): not on h", fd.Name.Name, text(call.Args[0]))
				return false
			}
			x, err := resultWrapper(fd)
			if err != nil {
				ferr = err
				return false
			}
			path, base, err := holder(x, 0)
			if err != nil {
				ferr = fmt.Errorf("%s: %w", fd.Name.Name, err)
				return false
			}
			if path == "" {
				return false // h holds ptr itself
			}
			if !funcs["new"+x+"NoFinalizer"] {
				ferr = fmt.Errorf("%s: no new%sNoFinalizer", fd.Name.Name, x)
				return false
			}
			freeCall := "new" + x + "NoFinalizer(b.ptr).free()"
			var fin string
			switch a := call.Args[1].(type) {
			case *ast.Ident:
				if a.Name != "nil" {
					ferr = fmt.Errorf("%s: finalizer %s", fd.Name.Name, a.Name)
					return false
				}
				fin = "nil"
			case *ast.SelectorExpr: // (*X).free
				if text(a) != "(*"+x+").free" {
					ferr = fmt.Errorf("%s: finalizer %s", fd.Name.Name, text(a))
					return false
				}
				fin = "func(b *" + base + ") { " + freeCall + " }"
			case *ast.FuncLit: // func(h *X) { h.free(); <statements not using h> }
				if len(a.Type.Params.List) != 1 || len(a.Type.Params.List[0].Names) != 1 || a.Type.Params.List[0].Names[0].Name != "h" ||
					text(a.Type.Params.List[0].Type) != "*"+x || len(a.Body.List) == 0 || text(a.Body.List[0]) != "h.free()" {
					ferr = fmt.Errorf("%s: finalizer %s", fd.Name.Name, text(a))
					return false
				}
				rest := a.Body.List[1:]
				var tail []string
				for _, s := range rest {
					if usesIdent(s, "h") || usesIdent(s, "b") {
						ferr = fmt.Errorf("%s: finalizer %s uses h", fd.Name.Name, text(a))
						return false
					}
					tail = append(tail, text(s))
				}
				fin = "func(b *" + base + ") { " + strings.Join(append([]string{freeCall}, tail...), "; ") + " }"
			default:
				ferr = fmt.Errorf("%s: finalizer %s", fd.Name.Name, text(call.Args[1]))
				return false
			}
			edits = append(edits, edit{at: off(call.Args[0].Pos()), end: off(call.Args[1].End()), text: "h" + path + ", " + fin})
			moved++
			return false
		})
		if ferr != nil {
			return nil, ferr
		}
	}

	// Every call to invokeMethod must be one of those seen: a statement of
	// a function's body.
	all := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && isCallTo(c, "invokeMethod") {
			all++
		}
		return true
	})
	if calls == 0 || all != calls {
		return nil, fmt.Errorf("%d calls to invokeMethod, %d of them statements `x, err := invokeMethod(...)` of a function body", all, calls)
	}
	if moved == 0 {
		return nil, fmt.Errorf("no finalizer of an embedding wrapper found")
	}
	fmt.Printf("bqengine: %s: %d of %d calls keep their handles alive; %d finalizers moved to the pointer's holder\n", keepAliveRewrite, kept, calls, moved)

	sort.Slice(edits, func(i, j int) bool { return edits[i].at < edits[j].at })
	var out bytes.Buffer
	last := 0
	for _, e := range edits {
		if e.at < last {
			return nil, fmt.Errorf("overlapping edits at byte %d", e.at)
		}
		out.Write(src[last:e.at])
		out.WriteString(e.text)
		last = e.end
	}
	out.Write(src[last:])
	if _, err := parser.ParseFile(token.NewFileSet(), "googlesql.go", out.Bytes(), parser.SkipObjectResolution); err != nil {
		return nil, fmt.Errorf("rewritten: %w", err)
	}
	return out.Bytes(), nil
}

// resultWrapper is X of a function returning *X or (*X, error).
func resultWrapper(fd *ast.FuncDecl) (string, error) {
	if fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
		if star, ok := fd.Type.Results.List[0].Type.(*ast.StarExpr); ok {
			if id, ok := star.X.(*ast.Ident); ok {
				return id.Name, nil
			}
		}
	}
	return "", fmt.Errorf("%s: sets a finalizer but does not return *X", fd.Name.Name)
}

func isCallTo(e ast.Expr, name string) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := c.Fun.(*ast.Ident)
	return ok && id.Name == name
}

func isSelectorCall(c *ast.CallExpr, pkg, name string) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func usesIdent(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}
