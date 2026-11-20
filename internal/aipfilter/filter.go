// Package aipfilter is the shared list `filter` (google.aip.dev/160) and
// `order_by` (google.aip.dev/132) implementation for list methods whose
// resources are protocol buffer messages (#1943).
//
// A filter is compiled against the resource's message descriptor, so an
// unknown field, a value of the wrong type or bad syntax is refused with
// INVALID_ARGUMENT before anything is listed, never silently ignored. The
// grammar is AIP-160's:
//
//   - a restriction is FIELD OP VALUE, OP one of = != < <= > >= and the
//     ":" (has) operator; FIELD is a snake_case (or JSON camelCase) path
//     through nested messages, such as transport.pubsub.topic, and through a
//     map by key, such as labels.env;
//   - restrictions combine with AND (or whitespace), OR and NOT (or a leading
//     "-"); OR binds tighter than AND, and parentheses group;
//   - a value is bare text, a number, true/false, or a quoted string; a
//     parenthesised value list, state:(ACTIVE OR CREATING), matches any one;
//   - "=" on a string matches the whole value, with "*" as a wildcard
//     (name = "*-prod"); ":" on a string is a case-insensitive substring
//     match; FIELD:* is true when the field is set (non-empty for a
//     repeated field or map), MAP:KEY when the map has KEY, and
//     MESSAGE:FIELD when that subfield of the message is set;
//   - a repeated field matches when any element does (and != when none is
//     equal);
//   - enums compare by value name, timestamps as RFC 3339 strings (or a
//     date, midnight UTC), durations as "3.5s" (or any Go duration);
//   - the regular-expression operators "~" and "!~", which gcloud applies on
//     the client and the APIs do not take, and function calls are refused.
package aipfilter

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Option restricts or adjusts how a filter or order_by is compiled.
type Option func(*config)

type config struct {
	fields   map[string]bool   // allowed top-level paths; nil allows every field
	aliases  map[string]string // filter name -> descriptor path
	computed map[string]func(proto.Message) string
}

// Computed adds a string-valued filter field that is not a message field,
// such as Vertex AI's endpoint (an Endpoint's ID); it takes = != : and
// wildcards as a string field does.
func Computed(name string, get func(proto.Message) string) Option {
	return func(c *config) {
		if c.computed == nil {
			c.computed = map[string]func(proto.Message) string{}
		}
		c.computed[name] = get
	}
}

// computedRestriction is FIELD OP VALUE on a Computed field.
type computedRestriction struct {
	get  func(proto.Message) string
	op   string
	vals []value
}

func (r *computedRestriction) eval(m protoreflect.Message) bool {
	s := r.get(m.Interface())
	for _, v := range r.vals {
		var ok bool
		switch {
		case r.op == ":" && v.s == "*":
			ok = s != ""
		case v.pat:
			ok = Wildcard(v.s, s)
		case r.op == ":":
			ok = strings.Contains(strings.ToLower(s), strings.ToLower(v.s))
		case r.op == "=" || r.op == "!=":
			ok = s == v.s
		default:
			ok = cmpOrdered(s, v.s, r.op)
		}
		if ok {
			return r.op != "!="
		}
	}
	return r.op == "!="
}

// Fields restricts a filter or an order_by to the documented fields. A name
// allows that path and every path under it ("labels" allows labels.env).
func Fields(names ...string) Option {
	return func(c *config) {
		if c.fields == nil {
			c.fields = map[string]bool{}
		}
		for _, n := range names {
			c.fields[n] = true
		}
	}
}

// Alias maps a documented filter name to the descriptor path it reads, for
// an API whose filter names differ from its message's fields.
func Alias(name, fieldPath string) Option {
	return func(c *config) {
		if c.aliases == nil {
			c.aliases = map[string]string{}
		}
		c.aliases[name] = fieldPath
	}
}

func newConfig(opts []Option) *config {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *config) allowed(p string) bool {
	if c.fields == nil {
		return true
	}
	if s := snake(p); s != p && c.allowed(s) {
		return true
	}
	for {
		if c.fields[p] {
			return true
		}
		i := strings.LastIndexByte(p, '.')
		if i < 0 {
			return false
		}
		p = p[:i]
	}
}

// snake turns a JSON camelCase path (displayName) into its proto name
// (display_name).
func snake(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 && p[i-1] != '.' {
				b.WriteByte('_')
			}
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (c *config) resolve(p string) string {
	if a, ok := c.aliases[p]; ok {
		return a
	}
	return p
}

// Filter is a compiled filter; a nil *Filter matches everything.
type Filter struct {
	src  string
	root node
}

// String is the filter's source text.
func (f *Filter) String() string {
	if f == nil {
		return ""
	}
	return f.src
}

// Match reports whether m passes the filter.
func (f *Filter) Match(m proto.Message) bool {
	if f == nil {
		return true
	}
	return f.root.eval(m.ProtoReflect())
}

// Compile parses filter for resources of message type md. An empty filter
// compiles to nil, which matches everything.
func Compile(filter string, md protoreflect.MessageDescriptor, opts ...Option) (*Filter, error) {
	if strings.TrimSpace(filter) == "" {
		return nil, nil
	}
	p := &parser{src: filter, md: md, cfg: newConfig(opts)}
	n, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if !p.eof() {
		return nil, p.errorf("unexpected %q at offset %d", p.rest(), p.pos)
	}
	return &Filter{src: filter, root: n}, nil
}

// Apply returns the items that pass filter, compiled for T's message type.
func Apply[T proto.Message](filter string, items []T, opts ...Option) ([]T, error) {
	var zero T
	f, err := Compile(filter, zero.ProtoReflect().Descriptor(), opts...)
	if err != nil || f == nil {
		return items, err
	}
	out := make([]T, 0, len(items))
	for _, it := range items {
		if f.Match(it) {
			out = append(out, it)
		}
	}
	return out, nil
}

// --- evaluation --------------------------------------------------------

type node interface {
	eval(m protoreflect.Message) bool
}

type andNode []node
type orNode []node
type notNode struct{ n node }

func (a andNode) eval(m protoreflect.Message) bool {
	for _, n := range a {
		if !n.eval(m) {
			return false
		}
	}
	return true
}

func (o orNode) eval(m protoreflect.Message) bool {
	for _, n := range o {
		if n.eval(m) {
			return true
		}
	}
	return false
}

func (n notNode) eval(m protoreflect.Message) bool { return !n.n.eval(m) }

// step is one segment of a field path: a field, or a map key.
type step struct {
	fd     protoreflect.FieldDescriptor
	mapKey string // set when this step indexes the map field fd
	isKey  bool
}

// restriction is FIELD OP VALUE over one resolved path.
type restriction struct {
	steps  []step
	op     string
	any    bool   // FIELD:*
	hasKey string // MAP:KEY
	keyOp  bool
	vals   []value // OR-ed
}

// value is a parsed comparison operand for the leaf's kind.
type value struct {
	s   string
	n   float64
	b   bool
	t   time.Time
	d   time.Duration
	pat bool // s contains a wildcard
}

// leaf is one value reached by a path; set=false when a message on the way
// is unset.
type leaf struct {
	v   protoreflect.Value
	fd  protoreflect.FieldDescriptor
	set bool
}

func (r *restriction) eval(m protoreflect.Message) bool {
	last := r.steps[len(r.steps)-1]
	if r.any || r.keyOp {
		return r.evalPresence(m)
	}
	leaves := collect(m, r.steps)
	if r.op == "!=" {
		for _, l := range leaves {
			for _, v := range r.vals {
				if compare(l, last.fd, "=", v) {
					return false
				}
			}
		}
		return true
	}
	for _, l := range leaves {
		for _, v := range r.vals {
			if compare(l, last.fd, r.op, v) {
				return true
			}
		}
	}
	return false
}

// evalPresence is FIELD:* and MAP:KEY.
func (r *restriction) evalPresence(m protoreflect.Message) bool {
	parent := r.steps[:len(r.steps)-1]
	last := r.steps[len(r.steps)-1]
	for _, pm := range messagesAt(m, parent) {
		fd := last.fd
		if last.isKey {
			mp := pm.Get(fd).Map()
			if mp.Has(protoreflect.ValueOfString(last.mapKey).MapKey()) {
				return true
			}
			continue
		}
		switch {
		case fd.IsMap():
			mp := pm.Get(fd).Map()
			if r.keyOp {
				if mp.Has(protoreflect.ValueOfString(r.hasKey).MapKey()) {
					return true
				}
			} else if mp.Len() > 0 {
				return true
			}
		case fd.IsList():
			if pm.Get(fd).List().Len() > 0 {
				return true
			}
		default:
			if pm.Has(fd) {
				return true
			}
		}
	}
	return false
}

// messagesAt returns the messages a path of message steps reaches, fanning
// out over repeated fields; an unset message is read as an empty one.
func messagesAt(m protoreflect.Message, steps []step) []protoreflect.Message {
	cur := []protoreflect.Message{m}
	for _, s := range steps {
		var next []protoreflect.Message
		for _, c := range cur {
			switch {
			case s.isKey:
				v := c.Get(s.fd).Map().Get(protoreflect.ValueOfString(s.mapKey).MapKey())
				if v.IsValid() {
					next = append(next, v.Message())
				}
			case s.fd.IsList():
				l := c.Get(s.fd).List()
				for i := 0; i < l.Len(); i++ {
					next = append(next, l.Get(i).Message())
				}
			default:
				// An unset message reads as its defaults, as a proto getter does.
				next = append(next, c.Get(s.fd).Message())
			}
		}
		cur = next
	}
	return cur
}

// collect returns the leaf values a path reaches. An unset scalar is its
// default; an unset message leaf is reported with set=false.
func collect(m protoreflect.Message, steps []step) []leaf {
	parents := messagesAt(m, steps[:len(steps)-1])
	last := steps[len(steps)-1]
	var out []leaf
	for _, pm := range parents {
		switch {
		case last.isKey:
			v := pm.Get(last.fd).Map().Get(protoreflect.ValueOfString(last.mapKey).MapKey())
			if v.IsValid() {
				out = append(out, leaf{v: v, fd: last.fd.MapValue(), set: true})
			}
		case last.fd.IsList():
			l := pm.Get(last.fd).List()
			for i := 0; i < l.Len(); i++ {
				out = append(out, leaf{v: l.Get(i), fd: last.fd, set: true})
			}
		default:
			set := last.fd.Kind() != protoreflect.MessageKind || pm.Has(last.fd)
			out = append(out, leaf{v: pm.Get(last.fd), fd: last.fd, set: set})
		}
	}
	return out
}

// leafKind classifies the type a comparison sees.
type leafKind int

const (
	kString leafKind = iota
	kNumber
	kBool
	kEnum
	kTime
	kDuration
	kMessage
)

func kindOf(fd protoreflect.FieldDescriptor) leafKind {
	switch fd.Kind() {
	case protoreflect.StringKind, protoreflect.BytesKind:
		return kString
	case protoreflect.BoolKind:
		return kBool
	case protoreflect.EnumKind:
		return kEnum
	case protoreflect.MessageKind, protoreflect.GroupKind:
		switch fd.Message().FullName() {
		case "google.protobuf.Timestamp":
			return kTime
		case "google.protobuf.Duration":
			return kDuration
		case "google.protobuf.StringValue", "google.protobuf.BytesValue":
			return kString
		case "google.protobuf.BoolValue":
			return kBool
		case "google.protobuf.Int32Value", "google.protobuf.Int64Value", "google.protobuf.UInt32Value",
			"google.protobuf.UInt64Value", "google.protobuf.FloatValue", "google.protobuf.DoubleValue":
			return kNumber
		}
		return kMessage
	default:
		return kNumber
	}
}

// scalar reads a leaf as the Go value its kind compares, unwrapping
// well-known types.
func scalar(l leaf, fd protoreflect.FieldDescriptor) (s string, n float64, b bool, t time.Time, d time.Duration) {
	v := l.v
	k := kindOf(fd)
	if fd.Kind() == protoreflect.MessageKind {
		if !l.set {
			return
		}
		msg := v.Message()
		md := msg.Descriptor()
		switch k {
		case kTime:
			sec := msg.Get(md.Fields().ByName("seconds")).Int()
			nanos := msg.Get(md.Fields().ByName("nanos")).Int()
			t = time.Unix(sec, nanos).UTC()
			return
		case kDuration:
			sec := msg.Get(md.Fields().ByName("seconds")).Int()
			nanos := msg.Get(md.Fields().ByName("nanos")).Int()
			d = time.Duration(sec)*time.Second + time.Duration(nanos)
			return
		}
		inner := md.Fields().ByName("value")
		v = msg.Get(inner)
		fd = inner
	}
	switch fd.Kind() {
	case protoreflect.StringKind:
		s = v.String()
	case protoreflect.BytesKind:
		s = string(v.Bytes())
	case protoreflect.BoolKind:
		b = v.Bool()
	case protoreflect.EnumKind:
		num := v.Enum()
		if ev := fd.Enum().Values().ByNumber(num); ev != nil {
			s = string(ev.Name())
		} else {
			s = strconv.Itoa(int(num))
		}
		n = float64(num)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n = float64(v.Int())
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n = float64(v.Uint())
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		n = v.Float()
	}
	return
}

func cmpOrdered[T int | float64 | string](a, b T, op string) bool {
	switch op {
	case "=", ":":
		return a == b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func sign(c int) int {
	switch {
	case c < 0:
		return -1
	case c > 0:
		return 1
	}
	return 0
}

func compare(l leaf, fd protoreflect.FieldDescriptor, op string, v value) bool {
	if l.fd != nil && !l.fd.IsMap() {
		fd = l.fd
	}
	k := kindOf(fd)
	if (k == kTime || k == kDuration) && !l.set {
		return false
	}
	s, n, b, t, d := scalar(l, fd)
	switch k {
	case kString:
		switch op {
		case "=":
			if v.pat {
				return Wildcard(v.s, s)
			}
			return s == v.s
		case ":":
			if v.pat {
				return Wildcard(v.s, s)
			}
			return strings.Contains(strings.ToLower(s), strings.ToLower(v.s))
		}
		return cmpOrdered(s, v.s, op)
	case kEnum:
		return s == v.s || (v.s != "" && strconv.Itoa(int(n)) == v.s)
	case kBool:
		return b == v.b
	case kNumber:
		return cmpOrdered(n, v.n, op)
	case kTime:
		return cmpOrdered(sign(t.Compare(v.t)), 0, op)
	case kDuration:
		return cmpOrdered(int(sign(int(d-v.d))), 0, op)
	}
	return false
}

// Wildcard reports whether s matches pattern, where each "*" matches any
// run of characters, "/" included, as AIP-160 wildcards do.
func Wildcard(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// --- parsing -----------------------------------------------------------

type parser struct {
	src string
	pos int
	md  protoreflect.MessageDescriptor
	cfg *config
}

func (p *parser) errorf(format string, a ...any) error {
	return apierror.InvalidArgument("invalid filter %q: "+format, append([]any{p.src}, a...)...)
}

func (p *parser) eof() bool    { return p.pos >= len(p.src) }
func (p *parser) rest() string { return p.src[p.pos:] }
func (p *parser) peekByte() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) skipWS() {
	for !p.eof() && isSpace(p.src[p.pos]) {
		p.pos++
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// keyword reports whether kw (AND, OR, NOT) is next, as a whole word.
func (p *parser) keyword(kw string) bool {
	p.skipWS()
	if !strings.HasPrefix(p.rest(), kw) {
		return false
	}
	end := p.pos + len(kw)
	return end == len(p.src) || isSpace(p.src[end]) || p.src[end] == '('
}

func (p *parser) parseExpression() (node, error) {
	first, err := p.parseSequence()
	if err != nil {
		return nil, err
	}
	out := andNode{first}
	for p.keyword("AND") {
		p.pos += 3
		n, err := p.parseSequence()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if len(out) == 1 {
		return first, nil
	}
	return out, nil
}

// parseSequence is factors separated by whitespace: an implicit AND.
func (p *parser) parseSequence() (node, error) {
	first, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	out := andNode{first}
	for {
		p.skipWS()
		if p.eof() || p.peekByte() == ')' || p.keyword("AND") || p.keyword("OR") {
			break
		}
		n, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if len(out) == 1 {
		return first, nil
	}
	return out, nil
}

func (p *parser) parseFactor() (node, error) {
	first, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	out := orNode{first}
	for p.keyword("OR") {
		p.pos += 2
		n, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if len(out) == 1 {
		return first, nil
	}
	return out, nil
}

func (p *parser) parseTerm() (node, error) {
	p.skipWS()
	if p.keyword("NOT") {
		p.pos += 3
		n, err := p.parseSimple()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	}
	if p.peekByte() == '-' && p.pos+1 < len(p.src) && !isSpace(p.src[p.pos+1]) {
		p.pos++
		n, err := p.parseSimple()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	}
	return p.parseSimple()
}

func (p *parser) parseSimple() (node, error) {
	p.skipWS()
	if p.eof() {
		return nil, p.errorf("expected a restriction at the end of the filter")
	}
	if p.peekByte() == '(' {
		p.pos++
		n, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.peekByte() != ')' {
			return nil, p.errorf("missing \")\" at offset %d", p.pos)
		}
		p.pos++
		return n, nil
	}
	return p.parseRestriction()
}

const opChars = "=!<>:~"

// readName reads a field path: text up to whitespace, an operator or a
// parenthesis. A map key may be bracketed and quoted, as in Artifact
// Registry's annotations.["external_link"]; it reads as annotations.external_link.
func (p *parser) readName() string {
	var b strings.Builder
	for !p.eof() {
		c := p.src[p.pos]
		if c == '[' {
			end := strings.IndexByte(p.src[p.pos:], ']')
			if end < 0 {
				break
			}
			key := strings.TrimSpace(p.src[p.pos+1 : p.pos+end])
			if len(key) >= 2 && (key[0] == '"' || key[0] == '\'' || key[0] == '`') && key[len(key)-1] == key[0] {
				key = key[1 : len(key)-1]
			}
			if !strings.HasSuffix(b.String(), ".") {
				b.WriteByte('.')
			}
			b.WriteString(key)
			p.pos += end + 1
			continue
		}
		if isSpace(c) || strings.IndexByte(opChars+"()\"'", c) >= 0 {
			break
		}
		b.WriteByte(c)
		p.pos++
	}
	return b.String()
}

func (p *parser) readOp() string {
	p.skipWS()
	for _, op := range []string{"!=", "<=", ">=", "!~", "=", "<", ">", ":", "~"} {
		if strings.HasPrefix(p.rest(), op) {
			p.pos += len(op)
			return op
		}
	}
	return ""
}

// readValue reads one operand: a quoted string, or bare text up to
// whitespace or a parenthesis (colons included, for unquoted timestamps).
func (p *parser) readValue() (string, bool, error) {
	p.skipWS()
	if p.eof() {
		return "", false, p.errorf("a restriction needs a value")
	}
	c := p.peekByte()
	if c == '"' || c == '\'' {
		p.pos++
		var b strings.Builder
		for !p.eof() {
			ch := p.src[p.pos]
			if ch == '\\' && p.pos+1 < len(p.src) {
				b.WriteByte(p.src[p.pos+1])
				p.pos += 2
				continue
			}
			if ch == c {
				p.pos++
				return b.String(), true, nil
			}
			b.WriteByte(ch)
			p.pos++
		}
		return "", false, p.errorf("unterminated string")
	}
	start := p.pos
	for !p.eof() {
		ch := p.src[p.pos]
		if isSpace(ch) || ch == '(' || ch == ')' {
			break
		}
		p.pos++
	}
	if p.pos == start {
		return "", false, p.errorf("a restriction needs a value at offset %d", p.pos)
	}
	return p.src[start:p.pos], false, nil
}

func (p *parser) parseRestriction() (node, error) {
	p.skipWS()
	name := p.readName()
	if name == "" {
		return nil, p.errorf("expected a field name at offset %d", p.pos)
	}
	if p.peekByte() == '(' {
		return nil, apierror.Unimplemented("invalid filter %q: function calls such as %s() are not implemented", p.src, name)
	}
	op := p.readOp()
	if op == "" {
		return nil, p.errorf("%q is not a FIELD OPERATOR VALUE restriction; global search terms are not supported", name)
	}
	if op == "~" || op == "!~" {
		return nil, apierror.Unimplemented("invalid filter %q: the regular-expression operator %q is a gcloud client-side feature the API does not take", p.src, op)
	}
	if !p.cfg.allowed(name) {
		return nil, p.errorf("field %q is not a filterable field", name)
	}
	if get, ok := p.cfg.computed[name]; ok {
		v, q, err := p.readValue()
		if err != nil {
			return nil, err
		}
		pat := strings.Contains(v, "*") && !(op == ":" && v == "*" && !q)
		return &computedRestriction{get: get, op: op, vals: []value{{s: v, pat: pat}}}, nil
	}
	r := &restriction{op: op}
	steps, err := resolvePath(p.md, p.cfg.resolve(name), true)
	if err != nil {
		return nil, p.errorf("%v", err)
	}
	r.steps = steps
	last := steps[len(steps)-1]

	// Value list: FIELD OP (a OR b).
	var raws []string
	var quoted []bool
	p.skipWS()
	if p.peekByte() == '(' {
		p.pos++
		for {
			p.skipWS()
			if p.peekByte() == ')' {
				p.pos++
				break
			}
			if p.keyword("OR") {
				p.pos += 2
				continue
			}
			if p.eof() {
				return nil, p.errorf("unclosed value list")
			}
			v, q, err := p.readValue()
			if err != nil {
				return nil, err
			}
			raws = append(raws, v)
			quoted = append(quoted, q)
		}
		if len(raws) == 0 {
			return nil, p.errorf("empty value list")
		}
	} else {
		v, q, err := p.readValue()
		if err != nil {
			return nil, err
		}
		raws, quoted = []string{v}, []bool{q}
	}

	if op == ":" && len(raws) == 1 && raws[0] == "*" && !quoted[0] {
		r.any = true
		return r, nil
	}
	fd := last.fd
	if !last.isKey && fd.IsMap() {
		if op != ":" {
			return nil, p.errorf("a map field such as %q takes only \":\" (has key), or a key path such as %s.KEY", name, name)
		}
		r.keyOp, r.hasKey = true, raws[0]
		if len(raws) > 1 {
			return nil, p.errorf("a has-key restriction takes one key")
		}
		return r, nil
	}
	if last.isKey {
		fd = fd.MapValue()
	}
	k := kindOf(fd)
	if k == kMessage && op == ":" && len(raws) == 1 && !quoted[0] && !last.isKey {
		// MESSAGE:FIELD is true when that subfield is set, as in Eventarc's
		// documented destination:gke.
		if sub := fieldByName(fd.Message(), raws[0]); sub != nil {
			r.steps = append(r.steps, step{fd: sub})
			r.any = true
			return r, nil
		}
	}
	switch {
	case k == kMessage:
		return nil, p.errorf("field %q is a message; compare one of its fields, or use %s:*", name, name)
	case (k == kEnum || k == kBool) && op != "=" && op != "!=" && op != ":":
		return nil, p.errorf("operator %q does not apply to the %s field %q", op, kindName(k), name)
	}
	for i, raw := range raws {
		v, err := parseValue(raw, quoted[i], k, fd)
		if err != nil {
			return nil, p.errorf("field %q: %v", name, err)
		}
		r.vals = append(r.vals, v)
	}
	return r, nil
}

func kindName(k leafKind) string {
	switch k {
	case kEnum:
		return "enum"
	case kBool:
		return "boolean"
	}
	return "scalar"
}

func parseValue(raw string, quoted bool, k leafKind, fd protoreflect.FieldDescriptor) (value, error) {
	switch k {
	case kString:
		return value{s: raw, pat: strings.Contains(raw, "*")}, nil
	case kEnum:
		if fd.Enum().Values().ByName(protoreflect.Name(raw)) == nil {
			if n, err := strconv.Atoi(raw); err != nil || fd.Enum().Values().ByNumber(protoreflect.EnumNumber(n)) == nil {
				return value{}, errorString("%q is not a value of enum %s", raw, fd.Enum().Name())
			}
		}
		return value{s: raw}, nil
	case kBool:
		switch raw {
		case "true", "TRUE", "True":
			return value{b: true}, nil
		case "false", "FALSE", "False":
			return value{b: false}, nil
		}
		return value{}, errorString("%q is not true or false", raw)
	case kNumber:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(n) {
			return value{}, errorString("%q is not a number", raw)
		}
		return value{n: n}, nil
	case kTime:
		t, err := ParseTime(raw)
		if err != nil {
			return value{}, err
		}
		return value{t: t}, nil
	case kDuration:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return value{}, errorString("%q is not a duration such as \"3.5s\"", raw)
		}
		return value{d: d}, nil
	}
	return value{}, errorString("cannot compare this field")
}

// ParseTime reads an AIP-160 timestamp: RFC 3339, or a date (midnight UTC).
func ParseTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t, nil
	}
	return time.Time{}, errorString("%q is not an RFC 3339 timestamp such as \"2024-01-02T15:04:05Z\"", raw)
}

type errString string

func (e errString) Error() string { return string(e) }

func errorString(format string, a ...any) error {
	return errString(fmt.Sprintf(format, a...))
}

// resolvePath turns a dotted path into steps through md. A segment after a
// map field is that map's key; allowMap lets the path end on a map.
func resolvePath(md protoreflect.MessageDescriptor, p string, allowMap bool) ([]step, error) {
	segs := strings.Split(p, ".")
	var steps []step
	cur := md
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		if seg == "" {
			return nil, errorString("field path %q has an empty segment", p)
		}
		if cur == nil {
			return nil, errorString("field %q has no subfield %q", strings.Join(segs[:i], "."), seg)
		}
		fd := fieldByName(cur, seg)
		if fd == nil {
			return nil, errorString("unknown field %q", strings.Join(segs[:i+1], "."))
		}
		if fd.IsMap() {
			if i+1 < len(segs) {
				key := strings.Join(segs[i+1:], ".")
				if fd.MapValue().Kind() == protoreflect.MessageKind && kindOf(fd.MapValue()) == kMessage {
					// labels-style string maps are the norm; a message-valued map
					// takes one key segment and continues into the message.
					key = segs[i+1]
					steps = append(steps, step{fd: fd, isKey: true, mapKey: key})
					i++
					cur = fd.MapValue().Message()
					continue
				}
				steps = append(steps, step{fd: fd, isKey: true, mapKey: key})
				return steps, nil
			}
			if !allowMap {
				return nil, errorString("field %q is a map; name a key, such as %s.KEY", p, p)
			}
			steps = append(steps, step{fd: fd})
			return steps, nil
		}
		steps = append(steps, step{fd: fd})
		if fd.Kind() == protoreflect.MessageKind && kindOf(fd) == kMessage {
			cur = fd.Message()
		} else {
			cur = nil
		}
	}
	return steps, nil
}

func fieldByName(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	if fd := md.Fields().ByName(protoreflect.Name(name)); fd != nil {
		return fd
	}
	return md.Fields().ByJSONName(name)
}
