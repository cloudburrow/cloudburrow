package storagejobs

import (
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// applyMask copies the fields paths name (lowerCamel or snake_case, dotted
// into messages) from src to dst; a path src leaves unset clears it.
func applyMask(dst, src proto.Message, paths []string) error {
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if err := copyPath(dst.ProtoReflect(), src.ProtoReflect(), strings.Split(p, ".")); err != nil {
			return err
		}
	}
	return nil
}

func field(m protoreflect.Message, name string) protoreflect.FieldDescriptor {
	fs := m.Descriptor().Fields()
	if f := fs.ByName(protoreflect.Name(name)); f != nil {
		return f
	}
	return fs.ByJSONName(name)
}

func copyPath(dst, src protoreflect.Message, path []string) error {
	f := field(dst, path[0])
	if f == nil {
		return invalid("Unknown field %q in update_mask.", path[0])
	}
	if len(path) == 1 {
		if src.Has(f) {
			dst.Set(f, src.Get(f))
		} else {
			dst.Clear(f)
		}
		return nil
	}
	if f.Message() == nil || f.IsList() || f.IsMap() {
		return invalid("Field %q of update_mask is not a message.", path[0])
	}
	if !src.Has(f) {
		dst.Clear(f)
		return nil
	}
	return copyPath(dst.Mutable(f).Message(), src.Get(f).Message(), path[1:])
}
