package queryfront

import (
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Cursors of a named database's queries (#1885). The emulator's cursor is
// a protobuf of its own (an App Engine compiled cursor) that names the
// project of the entity it stops after: in a named database, the emulator
// project the front keeps it in (measured: "…j;cloudburrow-database-…r…",
// field 13 of the cursor's key). A caller never sees that project: the
// front rewrites every cursor of an answer, each app field of it (field
// 13 of an App Engine reference, at any depth) equal to the emulator
// project becoming the project, and every cursor of a call back. Only that
// field: a key name equal to the project is a key name. Cursors are opaque to callers, and what they send back is
// what the front gave them.

// cursorAppField is the field of an App Engine reference naming its
// project ("app").
const cursorAppField = 13

// rewriteCursor is cursor b with every app field equal to from, at any
// depth of nested messages, replaced by to; b itself when
// none is, or when it is not a protobuf.
func rewriteCursor(b []byte, from, to string) []byte {
	out, changed, ok := rewriteMessage(b, from, to, 0)
	if !ok || !changed {
		return b
	}
	return out
}

func rewriteMessage(b []byte, from, to string, depth int) ([]byte, bool, bool) {
	if depth > 8 {
		return b, false, true
	}
	var out []byte
	changed := false
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false, false
		}
		b = b[n:]
		if typ != protowire.BytesType {
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return nil, false, false
			}
			out = protowire.AppendTag(out, num, typ)
			out = append(out, b[:m]...)
			b = b[m:]
			continue
		}
		v, m := protowire.ConsumeBytes(b)
		if m < 0 {
			return nil, false, false
		}
		b = b[m:]
		switch {
		case num == cursorAppField && string(v) == from:
			v, changed = []byte(to), true
		case len(v) > 0:
			if inner, c, ok := rewriteMessage(v, from, to, depth+1); ok && c {
				v, changed = inner, true
			}
		}
		out = protowire.AppendTag(out, num, protowire.BytesType)
		out = protowire.AppendBytes(out, v)
	}
	return out, changed, true
}

// rewriteCursors rewrites every cursor field in m (start_cursor,
// end_cursor, skipped_cursor, cursor, at any depth) from one project to
// another.
func rewriteCursors(m protoreflect.Message, from, to string) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Kind() == protoreflect.BytesKind && !fd.IsList() && strings.HasSuffix(string(fd.Name()), "cursor"):
			if nb := rewriteCursor(v.Bytes(), from, to); string(nb) != string(v.Bytes()) {
				m.Set(fd, protoreflect.ValueOfBytes(nb))
			}
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					rewriteCursors(mv.Message(), from, to)
					return true
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					rewriteCursors(l.Get(i).Message(), from, to)
				}
			}
		case fd.Message() != nil:
			rewriteCursors(v.Message(), from, to)
		}
		return true
	})
}
