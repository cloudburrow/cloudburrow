// Package sysparams implements Google's REST system parameters
// (https://docs.cloud.google.com/apis/docs/system-parameters) once, for every
// JSON front CloudBurrow serves (#1954): the `fields` / `$fields` partial
// response and its `X-Goog-FieldMask` header and `x-goog-fieldmask` gRPC
// metadata, `prettyPrint`, `$.xgafv` (the error format), `callback` (JSONP),
// `$outputDefaults`, and the parameters every API accepts and ignores here
// (quotaUser, userIp, key, ...).
//
// The transcoder (internal/transport/rest) applies them with the response's
// descriptor, so a mask naming a field the response has not is refused as
// Google refuses it; the hand-written fronts are wrapped by Wrap, which
// applies them to the JSON they write.
package sysparams

import (
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A Mask is a parsed field selection: each selected field, by the name the
// request gave, mapped to the selection within it, nil for the whole field.
// "*" selects every field. A nil Mask selects everything.
//
// The grammar is the partial-response one Google documents
// (https://docs.cloud.google.com/storage/docs/json_api/v1/how-tos/performance#partial-response):
// a comma-separated list of fields; "a/b" selects b within a; "a(b,c)"
// selects b and c within a, nesting to any depth; "*" is every field; a
// selection under an array applies to each element. A FieldMask's JSON form
// ("a.b,c", as X-Goog-FieldMask carries it) is the same selection, so "." is
// read as "/".
type Mask map[string]Mask

// Parse parses a fields value. "" is a nil Mask: every field. A malformed
// value is 400 INVALID_ARGUMENT "Invalid field selection <value>", Google's
// documented answer to an invalid fields parameter.
func Parse(spec string) (Mask, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	p := &parser{s: spec}
	m, err := p.list(false)
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, invalid(spec)
	}
	return m, nil
}

func invalid(sel string) error {
	return apierror.InvalidArgument("Invalid field selection %s", sel)
}

type parser struct {
	s string
	i int
}

func (p *parser) skipSpace() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

// list parses items separated by commas, up to the closing parenthesis
// when nested.
func (p *parser) list(nested bool) (Mask, error) {
	m := Mask{}
	for {
		if err := p.item(m); err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		if nested {
			if p.i >= len(p.s) || p.s[p.i] != ')' {
				return nil, invalid(p.s)
			}
			p.i++
		}
		return m, nil
	}
}

// item parses name, name/item or name(list) into m.
func (p *parser) item(m Mask) error {
	p.skipSpace()
	start := p.i
	for p.i < len(p.s) && !strings.ContainsRune(",/.() \t", rune(p.s[p.i])) {
		p.i++
	}
	name := p.s[start:p.i]
	if name == "" || (strings.Contains(name, "*") && name != "*") {
		return invalid(p.s)
	}
	p.skipSpace()
	if p.i < len(p.s) {
		switch p.s[p.i] {
		case '/', '.':
			p.i++
			sub := Mask{}
			if err := p.item(sub); err != nil {
				return err
			}
			m.add(name, sub)
			return nil
		case '(':
			p.i++
			sub, err := p.list(true)
			if err != nil {
				return err
			}
			m.add(name, sub)
			return nil
		}
	}
	m.add(name, nil)
	return nil
}

// add merges sub into m[name]: the whole field wins over a part of it.
func (m Mask) add(name string, sub Mask) {
	old, ok := m[name]
	switch {
	case !ok:
		m[name] = sub
	case old == nil || sub == nil:
		m[name] = nil
	default:
		for k, v := range sub {
			old.add(k, v)
		}
	}
}

// String is the mask in the partial-response grammar, sorted.
func (m Mask) String() string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		if sub := m[k]; sub != nil {
			b.WriteString("(" + sub.String() + ")")
		}
	}
	return b.String()
}

// Prune keeps what m selects of v, a decoded JSON value (any of
// map[string]any, []any and scalars). An array is pruned element by
// element; an object's "@type" (an Any's) is kept whenever the object is
// selected into, so a client can still decode it.
func (m Mask) Prune(v any) any {
	if m == nil {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		if t, ok := x["@type"]; ok {
			out["@type"] = t
		}
		for k, sub := range m {
			if k == "*" {
				for kk, vv := range x {
					out[kk] = sub.Prune(vv)
				}
				continue
			}
			if vv, ok := x[k]; ok {
				out[k] = sub.Prune(vv)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = m.Prune(e)
		}
		return out
	default:
		return v
	}
}

// fieldOf is the field of md a mask names, by its JSON or its proto name.
func fieldOf(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	if fd := md.Fields().ByJSONName(name); fd != nil {
		return fd
	}
	return md.Fields().ByName(protoreflect.Name(name))
}

// open are the messages whose JSON form is not their fields: any selection
// within them is accepted.
var open = map[protoreflect.FullName]bool{
	"google.protobuf.Any": true, "google.protobuf.Struct": true, "google.protobuf.Value": true,
	"google.protobuf.ListValue": true,
}

// Validate refuses a mask naming a field md has not, as Google does:
// INVALID_ARGUMENT "Invalid field selection <path>". Within an Any, a
// Struct or a map, every name is accepted, since the JSON is not the
// descriptor's.
func (m Mask) Validate(md protoreflect.MessageDescriptor) error {
	return m.validate(md, "")
}

func (m Mask) validate(md protoreflect.MessageDescriptor, prefix string) error {
	if m == nil || open[md.FullName()] {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sub := m[k]
		path := prefix + k
		if k == "*" {
			continue
		}
		fd := fieldOf(md, k)
		if fd == nil {
			return invalid(path)
		}
		if sub == nil || fd.IsMap() {
			continue
		}
		if fd.Message() == nil {
			return invalid(path + "/" + sub.String())
		}
		if err := sub.validate(fd.Message(), path+"/"); err != nil {
			return err
		}
	}
	return nil
}

// PruneProto clears from msg every field m does not select, in place: the
// partial response of a gRPC call (x-goog-fieldmask) or of alt=proto. An
// Any is unpacked, pruned and packed again when its type is linked.
func (m Mask) PruneProto(msg protoreflect.Message) {
	if m == nil || !msg.IsValid() {
		return
	}
	if msg.Descriptor().FullName() == "google.protobuf.Any" {
		pruneAny(msg, m)
		return
	}
	if open[msg.Descriptor().FullName()] {
		return
	}
	star, all := m["*"]
	var clear []protoreflect.FieldDescriptor
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		sub, ok := m[fd.JSONName()]
		if !ok {
			sub, ok = m[string(fd.Name())]
		}
		if !ok && all {
			sub, ok = star, true
		}
		if !ok {
			clear = append(clear, fd)
			return true
		}
		if sub == nil {
			return true
		}
		switch {
		case fd.IsMap():
			mp := v.Map()
			var drop []protoreflect.MapKey
			mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				ks, kok := sub[k.String()]
				if !kok {
					ks, kok = sub["*"]
				}
				if !kok {
					drop = append(drop, k)
				} else if fd.MapValue().Message() != nil {
					ks.PruneProto(mv.Message())
				}
				return true
			})
			for _, k := range drop {
				mp.Clear(k)
			}
		case fd.IsList() && fd.Message() != nil:
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				sub.PruneProto(l.Get(i).Message())
			}
		case fd.Message() != nil:
			sub.PruneProto(v.Message())
		}
		return true
	})
	for _, fd := range clear {
		msg.Clear(fd)
	}
}

func pruneAny(msg protoreflect.Message, m Mask) {
	a, ok := msg.Interface().(*anypb.Any)
	if !ok {
		return
	}
	inner, err := a.UnmarshalNew()
	if err != nil {
		return // a type this binary does not link: left whole
	}
	m.PruneProto(inner.ProtoReflect())
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(inner)
	if err == nil {
		a.Value = b
	}
}
