// Command genstoragev2pb rewrites internal/service/storage/storagev2pb, a copy
// of the Go storage client's generated google.storage.v2 code (#1321), so its
// messages are registered as cloudburrow.storage.v2.* under
// cloudburrow/storage/v2/storage.proto. The cloudburrow binary also links the
// client, which registers google.storage.v2 itself; two registrations of one
// name panic at init. The wire format carries no package name, and the gRPC
// service and method names stay google.storage.v2.Storage, so clients cannot
// tell.
//
// Usage: go run ./tools/genstoragev2pb (after copying storage.pb.go and
// storage_grpc.pb.go from cloud.google.com/go/storage/internal/apiv2/storagepb
// and renaming their package storagev2pb).
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	path    = "internal/service/storage/storagev2pb/storage.pb.go"
	varName = "file_google_storage_v2_storage_proto_rawDesc"
	oldPkg  = "google.storage.v2"
	newPkg  = "cloudburrow.storage.v2"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "genstoragev2pb:", err)
		os.Exit(1)
	}
}

func run() error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return err
	}
	var lit ast.Expr
	ast.Inspect(f, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok && len(vs.Names) == 1 && vs.Names[0].Name == varName {
			lit = vs.Values[0]
		}
		return true
	})
	if lit == nil {
		return fmt.Errorf("%s not found", varName)
	}
	raw, err := concat(lit)
	if err != nil {
		return err
	}
	var fd descriptorpb.FileDescriptorProto
	if err := proto.Unmarshal([]byte(raw), &fd); err != nil {
		return err
	}
	if fd.GetPackage() == newPkg {
		return nil
	}
	fd.Name = proto.String("cloudburrow/storage/v2/storage.proto")
	fd.Package = proto.String(newPkg)
	var fix func(m *descriptorpb.DescriptorProto)
	ref := func(s *string) {
		if s != nil && strings.HasPrefix(*s, "."+oldPkg+".") {
			*s = "." + newPkg + strings.TrimPrefix(*s, "."+oldPkg)
		}
	}
	fix = func(m *descriptorpb.DescriptorProto) {
		for _, fl := range m.Field {
			ref(fl.TypeName)
			ref(fl.Extendee)
		}
		for _, n := range m.NestedType {
			fix(n)
		}
	}
	for _, m := range fd.MessageType {
		fix(m)
	}
	for _, s := range fd.Service {
		for _, m := range s.Method {
			ref(m.InputType)
			ref(m.OutputType)
		}
	}
	out, err := proto.MarshalOptions{Deterministic: true}.Marshal(&fd)
	if err != nil {
		return err
	}
	start, end := fset.Position(lit.Pos()).Offset, fset.Position(lit.End()).Offset
	var b strings.Builder
	b.Write(src[:start])
	b.WriteString(strconv.Quote(string(out)))
	b.Write(src[end:])
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// concat evaluates a + chain of string literals.
func concat(e ast.Expr) (string, error) {
	switch v := e.(type) {
	case *ast.BasicLit:
		return strconv.Unquote(v.Value)
	case *ast.BinaryExpr:
		l, err := concat(v.X)
		if err != nil {
			return "", err
		}
		r, err := concat(v.Y)
		return l + r, err
	case *ast.ParenExpr:
		return concat(v.X)
	}
	return "", fmt.Errorf("unexpected %T in raw descriptor", e)
}
