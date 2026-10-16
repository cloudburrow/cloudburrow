package pubsubfront

// Protocol Buffer schemas (#1406). Google's emulator refuses every
// PROTOCOL_BUFFER schema ("Protocol buffer support not implemented in
// emulator"), so the front reads the definition itself. No .proto compiler
// is a dependency (protocompile and jhump/protoreflect are not in go.mod),
// so this is a parser of the language a Pub/Sub schema is written in, which
// builds a FileDescriptorProto and leaves every rule the descriptor itself
// carries (duplicate numbers and names, proto3's zero first enum value, map
// keys, json_name clashes) to protodesc, the protobuf module's own checker.
//
// What a schema may hold is what Google documents for one
// (https://cloud.google.com/pubsub/docs/schemas#protocol-buffer-schemas): a
// proto2 or proto3 definition with one top-level type, a message, which may
// nest messages and enums; no imports, since a schema is one self-contained
// definition. Services, extensions, groups and editions are refused too:
// none can describe a message's payload in a schema.
//
// A message is then checked against the message the definition defines, in
// the encoding the topic names: BINARY is the wire format, JSON proto3's JSON
// mapping, as Google validates them.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// maxFieldNumber is the largest field number protobuf allows.
const maxFieldNumber = 536870911

// parseProtoSchema reads a PROTOCOL_BUFFER schema definition and returns
// the one top-level message it defines.
func parseProtoSchema(def string) (md protoreflect.MessageDescriptor, err error) {
	toks, err := tokenizeProto(def)
	if err != nil {
		return nil, err
	}
	p := &protoParser{toks: toks, types: map[string]typeKind{}}
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(protoError)
			if !ok {
				panic(r)
			}
			md, err = nil, pe.err
		}
	}()
	fd := p.file()
	p.resolve()
	f, err := protodesc.NewFile(fd, new(protoregistry.Files))
	if err != nil {
		return nil, err
	}
	return f.Messages().Get(0), nil
}

// validateProtoMessage checks that data is a message md describes, in enc.
// A binary message must carry every required field, as proto.Unmarshal
// checks; a JSON one must name no field md lacks.
func validateProtoMessage(md protoreflect.MessageDescriptor, data []byte, enc pubsubpb.Encoding) error {
	m := dynamicpb.NewMessage(md)
	switch enc {
	case pubsubpb.Encoding_BINARY:
		return proto.UnmarshalOptions{}.Unmarshal(data, m)
	case pubsubpb.Encoding_JSON:
		return protojson.UnmarshalOptions{}.Unmarshal(data, m)
	}
	return errors.New("encoding unspecified")
}

// The tokenizer.

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tNum
	tStr
	tPunct
)

// token is one token; a string's text is its decoded value.
type token struct {
	kind      tokKind
	text      string
	line, col int
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) int {
	switch {
	case isDigit(c):
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return int(c-'A') + 10
}

// tokenizeProto splits a definition into tokens. An identifier keeps its
// dots, so a qualified name (".pkg.Msg" or "Outer.Inner") is one token.
func tokenizeProto(s string) ([]token, error) {
	var toks []token
	line, lineStart := 1, 0
	errAt := func(at int, format string, args ...any) error {
		return fmt.Errorf("%d:%d: %s", line, at-lineStart+1, fmt.Sprintf(format, args...))
	}
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\n':
			i++
			line++
			lineStart = i
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
		case strings.HasPrefix(s[i:], "//"):
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, errAt(i, "unterminated comment")
			}
			stop := i + 2 + end + 2
			for ; i < stop; i++ {
				if s[i] == '\n' {
					line++
					lineStart = i + 1
				}
			}
		case isIdentStart(c) || (c == '.' && i+1 < len(s) && isIdentStart(s[i+1])):
			j := i + 1
			for j < len(s) && (isIdentPart(s[j]) || s[j] == '.') {
				j++
			}
			toks = append(toks, token{tIdent, s[i:j], line, i - lineStart + 1})
			i = j
		case isDigit(c) || (c == '.' && i+1 < len(s) && isDigit(s[i+1])):
			j := i + 1
			hex := strings.HasPrefix(strings.ToLower(s[i:]), "0x")
			for j < len(s) {
				d := s[j]
				if isIdentPart(d) || d == '.' {
					j++
					continue
				}
				if (d == '+' || d == '-') && !hex && (s[j-1] == 'e' || s[j-1] == 'E') {
					j++
					continue
				}
				break
			}
			toks = append(toks, token{tNum, s[i:j], line, i - lineStart + 1})
			i = j
		case c == '"' || c == '\'':
			v, n, err := unquoteProto(s[i:])
			if err != nil {
				return nil, errAt(i, "%v", err)
			}
			toks = append(toks, token{tStr, v, line, i - lineStart + 1})
			i += n
		default:
			toks = append(toks, token{tPunct, string(c), line, i - lineStart + 1})
			i++
		}
	}
	return append(toks, token{tEOF, "", line, i - lineStart + 1}), nil
}

// unquoteProto decodes the string literal s starts with, protobuf's C-style
// escapes and all, and returns it and how many bytes it took.
func unquoteProto(s string) (string, int, error) {
	q := s[0]
	var b []byte
	i := 1
	for {
		if i >= len(s) || s[i] == '\n' {
			return "", 0, errors.New("unterminated string")
		}
		c := s[i]
		if c == q {
			return string(b), i + 1, nil
		}
		i++
		if c != '\\' {
			b = append(b, c)
			continue
		}
		if i >= len(s) {
			return "", 0, errors.New("unterminated string")
		}
		e := s[i]
		i++
		switch e {
		case 'a':
			b = append(b, 7)
		case 'b':
			b = append(b, 8)
		case 'f':
			b = append(b, 12)
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'v':
			b = append(b, 11)
		case '\\', '\'', '"', '?':
			b = append(b, e)
		case 'x', 'X':
			v, n := 0, 0
			for n < 2 && i < len(s) && isHex(s[i]) {
				v = v*16 + hexVal(s[i])
				i++
				n++
			}
			if n == 0 {
				return "", 0, errors.New(`\x with no hex digits`)
			}
			b = append(b, byte(v))
		case 'u', 'U':
			width := 4
			if e == 'U' {
				width = 8
			}
			v := 0
			for n := 0; n < width; n++ {
				if i >= len(s) || !isHex(s[i]) {
					return "", 0, fmt.Errorf(`\%c needs %d hex digits`, e, width)
				}
				v = v*16 + hexVal(s[i])
				i++
			}
			if !utf8.ValidRune(rune(v)) {
				return "", 0, fmt.Errorf(`\%c%0*x is not a valid code point`, e, width, v)
			}
			b = utf8.AppendRune(b, rune(v))
		default:
			if e < '0' || e > '7' {
				return "", 0, fmt.Errorf(`invalid escape \%c`, e)
			}
			v, n := int(e-'0'), 1
			for n < 3 && i < len(s) && s[i] >= '0' && s[i] <= '7' {
				v = v*8 + int(s[i]-'0')
				i++
				n++
			}
			if v > 255 {
				return "", 0, fmt.Errorf(`octal escape \%o is above \377`, v)
			}
			b = append(b, byte(v))
		}
	}
}

// cEscape is b as a bytes field's default value is written in a
// descriptor: C-style escapes.
func cEscape(b string) string {
	var sb strings.Builder
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == '\\' || c == '"' || c == '\'':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			sb.WriteByte(c)
		default:
			fmt.Fprintf(&sb, "\\%03o", c)
		}
	}
	return sb.String()
}

// The parser.

// protoError carries a parse error up through the recursive descent.
type protoError struct{ err error }

type typeKind int

const (
	kindPackage typeKind = iota
	kindMessage
	kindEnum
)

// typeRef is a field whose type names a message or enum, resolved once the
// whole definition is read, since a type may be used before it is defined.
type typeRef struct {
	field *descriptorpb.FieldDescriptorProto
	scope string
	name  string
	at    token
}

type protoParser struct {
	toks   []token
	pos    int
	proto3 bool
	pkg    string
	// types is every message and enum by full name (no leading dot), and
	// every package prefix, as protoc's scopes hold them.
	types map[string]typeKind
	refs  []typeRef
}

var scalarTypes = map[string]descriptorpb.FieldDescriptorProto_Type{
	"double":   descriptorpb.FieldDescriptorProto_TYPE_DOUBLE,
	"float":    descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
	"int32":    descriptorpb.FieldDescriptorProto_TYPE_INT32,
	"int64":    descriptorpb.FieldDescriptorProto_TYPE_INT64,
	"uint32":   descriptorpb.FieldDescriptorProto_TYPE_UINT32,
	"uint64":   descriptorpb.FieldDescriptorProto_TYPE_UINT64,
	"sint32":   descriptorpb.FieldDescriptorProto_TYPE_SINT32,
	"sint64":   descriptorpb.FieldDescriptorProto_TYPE_SINT64,
	"fixed32":  descriptorpb.FieldDescriptorProto_TYPE_FIXED32,
	"fixed64":  descriptorpb.FieldDescriptorProto_TYPE_FIXED64,
	"sfixed32": descriptorpb.FieldDescriptorProto_TYPE_SFIXED32,
	"sfixed64": descriptorpb.FieldDescriptorProto_TYPE_SFIXED64,
	"bool":     descriptorpb.FieldDescriptorProto_TYPE_BOOL,
	"string":   descriptorpb.FieldDescriptorProto_TYPE_STRING,
	"bytes":    descriptorpb.FieldDescriptorProto_TYPE_BYTES,
}

func describe(t token) string {
	switch t.kind {
	case tEOF:
		return "the end of the definition"
	case tStr:
		return strconv.Quote(t.text)
	}
	return fmt.Sprintf("%q", t.text)
}

func (p *protoParser) fail(t token, format string, args ...any) {
	panic(protoError{fmt.Errorf("%d:%d: %s", t.line, t.col, fmt.Sprintf(format, args...))})
}

func (p *protoParser) peek() token { return p.toks[p.pos] }

func (p *protoParser) peekAt(n int) token {
	if p.pos+n >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.pos+n]
}

func (p *protoParser) next() token {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func isPunct(t token, punct string) bool { return t.kind == tPunct && t.text == punct }

func (p *protoParser) accept(punct string) bool {
	if isPunct(p.peek(), punct) {
		p.pos++
		return true
	}
	return false
}

func (p *protoParser) expect(punct string) {
	if t := p.next(); !isPunct(t, punct) {
		p.fail(t, "expected %q, found %s", punct, describe(t))
	}
}

func (p *protoParser) ident() token {
	t := p.next()
	if t.kind != tIdent {
		p.fail(t, "expected a name, found %s", describe(t))
	}
	return t
}

// simpleIdent is a name with no dots: a message's, field's or value's own.
func (p *protoParser) simpleIdent() token {
	t := p.ident()
	if strings.Contains(t.text, ".") {
		p.fail(t, "expected a name without dots, found %s", describe(t))
	}
	return t
}

// str reads one string literal, or several adjacent ones, which join.
func (p *protoParser) str() string {
	t := p.next()
	if t.kind != tStr {
		p.fail(t, "expected a string, found %s", describe(t))
	}
	s := t.text
	for p.peek().kind == tStr {
		s += p.next().text
	}
	return s
}

// intLit reads an integer, decimal, hex or octal, with an optional minus.
func (p *protoParser) intLit() (int64, token) {
	neg := p.accept("-")
	t := p.next()
	if t.kind != tNum {
		p.fail(t, "expected a number, found %s", describe(t))
	}
	v, err := strconv.ParseInt(t.text, 0, 64)
	if err != nil {
		p.fail(t, "%q is not an integer", t.text)
	}
	if neg {
		v = -v
	}
	return v, t
}

func join(scope, name string) string {
	if scope == "" {
		return name
	}
	return scope + "." + name
}

func parentScope(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[:i]
	}
	return ""
}

func (p *protoParser) register(full string, kind typeKind, at token) {
	if _, ok := p.types[full]; ok {
		p.fail(at, "%q is already defined", full)
	}
	p.types[full] = kind
}

// skipStatement skips an option's value, aggregate or not, to its ';'.
func (p *protoParser) skipStatement() {
	depth := 0
	for {
		t := p.next()
		switch {
		case t.kind == tEOF:
			p.fail(t, "expected \";\", found %s", describe(t))
		case isPunct(t, "{"):
			depth++
		case isPunct(t, "}"):
			depth--
		case isPunct(t, ";") && depth == 0:
			return
		}
	}
}

func (p *protoParser) file() *descriptorpb.FileDescriptorProto {
	fd := &descriptorpb.FileDescriptorProto{Name: proto.String("schema.proto"), Syntax: proto.String("proto2")}
	first, sawType := true, false
	for {
		t := p.peek()
		if t.kind == tEOF {
			break
		}
		if p.accept(";") {
			continue
		}
		if t.kind != tIdent {
			p.fail(t, "expected a top-level statement, found %s", describe(t))
		}
		p.next()
		switch t.text {
		case "syntax":
			if !first {
				p.fail(t, "the syntax statement must come first")
			}
			p.expect("=")
			at := p.peek()
			switch s := p.str(); s {
			case "proto2":
			case "proto3":
				p.proto3 = true
				fd.Syntax = proto.String("proto3")
			default:
				p.fail(at, "unknown syntax %q: a schema is \"proto2\" or \"proto3\"", s)
			}
			p.expect(";")
		case "edition":
			p.fail(t, "editions are not supported: use syntax = \"proto2\" or \"proto3\"")
		case "package":
			if fd.Package != nil {
				p.fail(t, "the package is declared twice")
			}
			if sawType {
				p.fail(t, "the package must be declared before any type")
			}
			n := p.ident()
			if strings.HasPrefix(n.text, ".") || strings.HasSuffix(n.text, ".") || strings.Contains(n.text, "..") {
				p.fail(n, "%q is not a package name", n.text)
			}
			fd.Package = proto.String(n.text)
			p.pkg = n.text
			for s := n.text; s != ""; s = parentScope(s) {
				p.types[s] = kindPackage
			}
			p.expect(";")
		case "import":
			p.fail(t, "imports are not supported: a Pub/Sub schema is one self-contained definition")
		case "option":
			p.skipStatement()
		case "message":
			sawType = true
			fd.MessageType = append(fd.MessageType, p.message(p.pkg))
		case "enum":
			sawType = true
			fd.EnumType = append(fd.EnumType, p.enum(p.pkg))
		case "service", "extend":
			p.fail(t, "%s is not supported in a schema definition", t.text)
		default:
			p.fail(t, "expected a top-level statement, found %s", describe(t))
		}
		first = false
	}
	// "A schema definition can have only one top-level message type"
	// (Google's schema docs); a top-level enum is a second type.
	if len(fd.MessageType) != 1 || len(fd.EnumType) != 0 {
		panic(protoError{fmt.Errorf("a Protocol Buffer schema must define exactly one top-level type, a message; "+
			"this one defines %d top-level messages and %d top-level enums", len(fd.MessageType), len(fd.EnumType))})
	}
	return fd
}

func (p *protoParser) message(scope string) *descriptorpb.DescriptorProto {
	nt := p.simpleIdent()
	full := join(scope, nt.text)
	p.register(full, kindMessage, nt)
	m := &descriptorpb.DescriptorProto{Name: proto.String(nt.text)}
	p.expect("{")
	var optionals []*descriptorpb.FieldDescriptorProto
	for !p.accept("}") {
		t := p.peek()
		if t.kind == tEOF {
			p.fail(t, "message %s is not closed", nt.text)
		}
		if p.accept(";") {
			continue
		}
		if t.kind != tIdent {
			p.fail(t, "expected a field or a definition, found %s", describe(t))
		}
		if t.text == "map" && isPunct(p.peekAt(1), "<") {
			m.Field = append(m.Field, p.mapField(m, full))
			continue
		}
		switch t.text {
		case "message":
			p.next()
			m.NestedType = append(m.NestedType, p.message(full))
		case "enum":
			p.next()
			m.EnumType = append(m.EnumType, p.enum(full))
		case "option":
			p.next()
			p.skipStatement()
		case "reserved":
			p.next()
			p.reserved(m)
		case "extensions", "extend":
			p.fail(t, "%s is not supported in a schema definition", t.text)
		case "oneof":
			p.next()
			p.oneof(m, full)
		default:
			f := p.field(full, -1)
			m.Field = append(m.Field, f)
			if f.GetProto3Optional() {
				optionals = append(optionals, f)
			}
		}
	}
	// A proto3 optional field sits alone in a synthetic oneof, after
	// every real one, as protoc writes it.
	for _, f := range optionals {
		m.OneofDecl = append(m.OneofDecl, &descriptorpb.OneofDescriptorProto{Name: proto.String("_" + f.GetName())})
		f.OneofIndex = proto.Int32(int32(len(m.OneofDecl) - 1))
	}
	return m
}

// field reads a field; oneof is the index of the oneof it is in, or -1.
func (p *protoParser) field(scope string, oneof int32) *descriptorpb.FieldDescriptorProto {
	t := p.peek()
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	hasLabel, explicit := false, false
	switch t.text {
	case "optional", "required", "repeated":
		if oneof >= 0 {
			p.fail(t, "fields in a oneof take no label")
		}
		p.next()
		hasLabel = true
		switch t.text {
		case "optional":
			explicit = true
		case "required":
			if p.proto3 {
				p.fail(t, "required fields are not allowed in proto3")
			}
			label = descriptorpb.FieldDescriptorProto_LABEL_REQUIRED
		case "repeated":
			label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		}
	}
	if !p.proto3 && oneof < 0 && !hasLabel {
		p.fail(t, "expected \"required\", \"optional\", or \"repeated\", found %s", describe(t))
	}
	typ := p.ident()
	switch {
	case typ.text == "group":
		p.fail(typ, "groups are not supported")
	case typ.text == "map" && isPunct(p.peek(), "<"):
		p.fail(typ, "map fields take no label")
	}
	name := p.simpleIdent()
	p.expect("=")
	num, at := p.intLit()
	if num < 1 || num > maxFieldNumber {
		p.fail(at, "field number %d is out of range: field numbers are 1 to %d", num, maxFieldNumber)
	}
	f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name.text), Number: proto.Int32(int32(num)), Label: label.Enum()}
	p.setType(f, scope, typ)
	if p.proto3 && explicit {
		f.Proto3Optional = proto.Bool(true)
	}
	if oneof >= 0 {
		f.OneofIndex = proto.Int32(oneof)
	}
	if p.accept("[") {
		p.fieldOptions(f)
	}
	p.expect(";")
	return f
}

func (p *protoParser) setType(f *descriptorpb.FieldDescriptorProto, scope string, typ token) {
	if s, ok := scalarTypes[typ.text]; ok {
		f.Type = s.Enum()
		return
	}
	p.refs = append(p.refs, typeRef{field: f, scope: scope, name: typ.text, at: typ})
}

// constant is an option's value.
type constant struct {
	text      string
	str       bool
	aggregate bool
	at        token
}

func (p *protoParser) constant() constant {
	t := p.peek()
	switch {
	case isPunct(t, "-"):
		p.next()
		n := p.next()
		if n.kind != tNum && !(n.kind == tIdent && (n.text == "inf" || n.text == "nan")) {
			p.fail(n, "expected a number, found %s", describe(n))
		}
		return constant{text: "-" + n.text, at: t}
	case t.kind == tStr:
		return constant{text: p.str(), str: true, at: t}
	case t.kind == tNum || t.kind == tIdent:
		p.next()
		return constant{text: t.text, at: t}
	case isPunct(t, "{"):
		p.next()
		for depth := 1; depth > 0; {
			n := p.next()
			switch {
			case n.kind == tEOF:
				p.fail(n, "expected \"}\", found %s", describe(n))
			case isPunct(n, "{"):
				depth++
			case isPunct(n, "}"):
				depth--
			}
		}
		return constant{aggregate: true, at: t}
	}
	p.fail(t, "expected a value, found %s", describe(t))
	return constant{}
}

// optionName reads an option's name: plain, or a custom "(name).sub".
func (p *protoParser) optionName() string {
	var name string
	if p.accept("(") {
		name = "(" + p.ident().text + ")"
		p.expect(")")
	} else {
		name = p.ident().text
	}
	for t := p.peek(); t.kind == tIdent && strings.HasPrefix(t.text, "."); t = p.peek() {
		name += p.next().text
	}
	return name
}

// fieldOptions reads a field's options after its "[": default, packed and
// json_name mean something to a schema; the rest are read and ignored.
func (p *protoParser) fieldOptions(f *descriptorpb.FieldDescriptorProto) {
	for {
		at := p.peek()
		name := p.optionName()
		p.expect("=")
		v := p.constant()
		switch name {
		case "default":
			if p.proto3 {
				p.fail(at, "explicit default values are not allowed in proto3")
			}
			if f.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
				p.fail(at, "repeated fields cannot have default values")
			}
			f.DefaultValue = proto.String(p.defaultText(f, v))
		case "packed":
			if v.text != "true" && v.text != "false" || v.str {
				p.fail(v.at, "packed is true or false")
			}
			if f.Options == nil {
				f.Options = &descriptorpb.FieldOptions{}
			}
			f.Options.Packed = proto.Bool(v.text == "true")
		case "json_name":
			if !v.str {
				p.fail(v.at, "json_name is a string")
			}
			f.JsonName = proto.String(v.text)
		}
		if p.accept("]") {
			return
		}
		p.expect(",")
	}
}

// defaultText is a default value as a descriptor holds it.
func (p *protoParser) defaultText(f *descriptorpb.FieldDescriptorProto, v constant) string {
	if v.aggregate {
		p.fail(v.at, "a default value cannot be an aggregate")
	}
	if f.Type == nil {
		// An enum, resolved later: its default is a value's name.
		if v.str || strings.HasPrefix(v.text, "-") {
			p.fail(v.at, "an enum's default is one of its values")
		}
		return v.text
	}
	switch f.GetType() {
	case descriptorpb.FieldDescriptorProto_TYPE_STRING:
		if !v.str {
			p.fail(v.at, "a string's default is a string")
		}
		return v.text
	case descriptorpb.FieldDescriptorProto_TYPE_BYTES:
		if !v.str {
			p.fail(v.at, "a bytes field's default is a string")
		}
		return cEscape(v.text)
	case descriptorpb.FieldDescriptorProto_TYPE_BOOL:
		if v.str || (v.text != "true" && v.text != "false") {
			p.fail(v.at, "a bool's default is true or false")
		}
		return v.text
	case descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, descriptorpb.FieldDescriptorProto_TYPE_FLOAT:
		if v.str {
			p.fail(v.at, "a number's default is a number")
		}
		return v.text
	case descriptorpb.FieldDescriptorProto_TYPE_UINT32, descriptorpb.FieldDescriptorProto_TYPE_UINT64,
		descriptorpb.FieldDescriptorProto_TYPE_FIXED32, descriptorpb.FieldDescriptorProto_TYPE_FIXED64:
		n, err := strconv.ParseUint(v.text, 0, 64)
		if v.str || err != nil {
			p.fail(v.at, "%q is not an unsigned integer", v.text)
		}
		return strconv.FormatUint(n, 10)
	}
	n, err := strconv.ParseInt(v.text, 0, 64)
	if v.str || err != nil {
		p.fail(v.at, "%q is not an integer", v.text)
	}
	return strconv.FormatInt(n, 10)
}

func (p *protoParser) oneof(m *descriptorpb.DescriptorProto, scope string) {
	nt := p.simpleIdent()
	idx := int32(len(m.OneofDecl))
	m.OneofDecl = append(m.OneofDecl, &descriptorpb.OneofDescriptorProto{Name: proto.String(nt.text)})
	p.expect("{")
	n := 0
	for !p.accept("}") {
		t := p.peek()
		if t.kind == tEOF {
			p.fail(t, "oneof %s is not closed", nt.text)
		}
		if p.accept(";") {
			continue
		}
		if t.kind == tIdent && t.text == "option" {
			p.next()
			p.skipStatement()
			continue
		}
		if t.kind == tIdent && t.text == "map" && isPunct(p.peekAt(1), "<") {
			p.fail(t, "map fields are not allowed in a oneof")
		}
		m.Field = append(m.Field, p.field(scope, idx))
		n++
	}
	if n == 0 {
		p.fail(nt, "oneof %s has no fields", nt.text)
	}
}

// mapEntryName is the nested message protoc makes for a map field.
func mapEntryName(field string) string {
	var sb strings.Builder
	upper := true
	for i := 0; i < len(field); i++ {
		c := field[i]
		if c == '_' {
			upper = true
			continue
		}
		if upper && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		upper = false
		sb.WriteByte(c)
	}
	return sb.String() + "Entry"
}

func (p *protoParser) mapField(m *descriptorpb.DescriptorProto, scope string) *descriptorpb.FieldDescriptorProto {
	p.next() // map
	p.expect("<")
	kt := p.ident()
	kty, ok := scalarTypes[kt.text]
	switch {
	case !ok, kty == descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, kty == descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
		kty == descriptorpb.FieldDescriptorProto_TYPE_BYTES:
		p.fail(kt, "a map's key is an integral type, bool or string, not %s", describe(kt))
	}
	p.expect(",")
	vt := p.ident()
	p.expect(">")
	name := p.simpleIdent()
	p.expect("=")
	num, at := p.intLit()
	if num < 1 || num > maxFieldNumber {
		p.fail(at, "field number %d is out of range: field numbers are 1 to %d", num, maxFieldNumber)
	}
	entryName := mapEntryName(name.text)
	entryFull := scope + "." + entryName
	p.register(entryFull, kindMessage, name)
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	key := &descriptorpb.FieldDescriptorProto{Name: proto.String("key"), Number: proto.Int32(1), Label: optional.Enum(), Type: kty.Enum()}
	val := &descriptorpb.FieldDescriptorProto{Name: proto.String("value"), Number: proto.Int32(2), Label: optional.Enum()}
	p.setType(val, entryFull, vt)
	m.NestedType = append(m.NestedType, &descriptorpb.DescriptorProto{
		Name:    proto.String(entryName),
		Field:   []*descriptorpb.FieldDescriptorProto{key, val},
		Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
	})
	f := &descriptorpb.FieldDescriptorProto{
		Name: proto.String(name.text), Number: proto.Int32(int32(num)),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String("." + entryFull),
	}
	if p.accept("[") {
		p.fieldOptions(f)
	}
	p.expect(";")
	return f
}

// reserved reads a message's reserved numbers or names; a descriptor's
// ranges end one past the last number.
func (p *protoParser) reserved(m *descriptorpb.DescriptorProto) {
	if p.peek().kind == tStr {
		for {
			m.ReservedName = append(m.ReservedName, p.str())
			if !p.accept(",") {
				break
			}
		}
		p.expect(";")
		return
	}
	for {
		lo, at := p.intLit()
		hi := lo
		if t := p.peek(); t.kind == tIdent && t.text == "to" {
			p.next()
			if t := p.peek(); t.kind == tIdent && t.text == "max" {
				p.next()
				hi = maxFieldNumber
			} else {
				hi, _ = p.intLit()
			}
		}
		if lo < 1 || hi < lo || hi > maxFieldNumber {
			p.fail(at, "reserved range %d to %d is not a range of field numbers", lo, hi)
		}
		m.ReservedRange = append(m.ReservedRange, &descriptorpb.DescriptorProto_ReservedRange{
			Start: proto.Int32(int32(lo)), End: proto.Int32(int32(hi + 1))})
		if !p.accept(",") {
			break
		}
	}
	p.expect(";")
}

func (p *protoParser) enum(scope string) *descriptorpb.EnumDescriptorProto {
	nt := p.simpleIdent()
	p.register(join(scope, nt.text), kindEnum, nt)
	e := &descriptorpb.EnumDescriptorProto{Name: proto.String(nt.text)}
	p.expect("{")
	for !p.accept("}") {
		t := p.peek()
		if t.kind == tEOF {
			p.fail(t, "enum %s is not closed", nt.text)
		}
		if p.accept(";") {
			continue
		}
		if t.kind != tIdent {
			p.fail(t, "expected an enum value, found %s", describe(t))
		}
		switch t.text {
		case "option":
			p.next()
			name := p.optionName()
			p.expect("=")
			v := p.constant()
			p.expect(";")
			if name == "allow_alias" {
				e.Options = &descriptorpb.EnumOptions{AllowAlias: proto.Bool(v.text == "true")}
			}
		case "reserved":
			p.next()
			p.enumReserved(e)
		default:
			vn := p.simpleIdent()
			p.expect("=")
			n, at := p.intLit()
			if n < -1<<31 || n > 1<<31-1 {
				p.fail(at, "enum value %d is out of range", n)
			}
			e.Value = append(e.Value, &descriptorpb.EnumValueDescriptorProto{Name: proto.String(vn.text), Number: proto.Int32(int32(n))})
			if p.accept("[") {
				for {
					p.optionName()
					p.expect("=")
					p.constant()
					if p.accept("]") {
						break
					}
					p.expect(",")
				}
			}
			p.expect(";")
		}
	}
	if len(e.Value) == 0 {
		p.fail(nt, "enum %s has no values", nt.text)
	}
	return e
}

// enumReserved reads an enum's reserved numbers or names; an enum's ranges
// include their end.
func (p *protoParser) enumReserved(e *descriptorpb.EnumDescriptorProto) {
	if p.peek().kind == tStr {
		for {
			e.ReservedName = append(e.ReservedName, p.str())
			if !p.accept(",") {
				break
			}
		}
		p.expect(";")
		return
	}
	for {
		lo, at := p.intLit()
		hi := lo
		if t := p.peek(); t.kind == tIdent && t.text == "to" {
			p.next()
			if t := p.peek(); t.kind == tIdent && t.text == "max" {
				p.next()
				hi = 1<<31 - 1
			} else {
				hi, _ = p.intLit()
			}
		}
		if hi < lo || lo < -1<<31 || hi > 1<<31-1 {
			p.fail(at, "reserved range %d to %d is not a range of enum values", lo, hi)
		}
		e.ReservedRange = append(e.ReservedRange, &descriptorpb.EnumDescriptorProto_EnumReservedRange{
			Start: proto.Int32(int32(lo)), End: proto.Int32(int32(hi))})
		if !p.accept(",") {
			break
		}
	}
	p.expect(";")
}

// resolve gives every field that names a type the type's full name, found
// as protoc finds it: the name's first part is looked up in the field's
// scope and then each enclosing one, and the rest within what it names.
func (p *protoParser) resolve() {
	for _, r := range p.refs {
		full, kind := p.lookup(r)
		r.field.TypeName = proto.String("." + full)
		if kind == kindMessage {
			r.field.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
		} else {
			r.field.Type = descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum()
		}
	}
}

func (p *protoParser) lookup(r typeRef) (string, typeKind) {
	if abs, ok := strings.CutPrefix(r.name, "."); ok {
		if k, ok := p.types[abs]; ok && k != kindPackage {
			return abs, k
		}
		p.fail(r.at, "%q is not defined", r.name)
	}
	first, _, _ := strings.Cut(r.name, ".")
	for s := r.scope; ; s = parentScope(s) {
		if k, ok := p.types[join(s, first)]; ok {
			full := join(s, r.name)
			if fk, ok := p.types[full]; ok && fk != kindPackage {
				return full, fk
			}
			if k != kindPackage {
				p.fail(r.at, "%q resolves to %q, which is not defined", r.name, full)
			}
		}
		if s == "" {
			break
		}
	}
	p.fail(r.at, "%q is not defined", r.name)
	return "", 0
}
