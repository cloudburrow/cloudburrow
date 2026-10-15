package tasks

// ListQueues filters (#1255). The reference says any Queue field can be
// filtered on with <=, <, >=, >, !=, = and :, in the syntax of Cloud
// Logging's filters, and gives "state: PAUSED" as its sample. Documented,
// not measured against Google:
//
//   - a restriction is FIELD OP VALUE; FIELD is a dotted path of Queue
//     field names, snake_case or lowerCamelCase (rate_limits.max_burst_size,
//     retryConfig.maxAttempts), ending on a scalar, an enum, a Duration or
//     a Timestamp;
//   - ":" is a case-insensitive substring match on a string and equality on
//     anything else, and FIELD:* means the field is set (not its zero value);
//   - an enum is compared by its value's name, with =, != and : only;
//   - numbers compare numerically, durations as seconds ("90s" or 90),
//     timestamps as RFC 3339, quoted ("2026-01-01T00:00:00Z"), since an
//     unquoted ":" is the has operator;
//   - restrictions combine with AND (or a space), OR and NOT (or a leading
//     "-"); OR binds tighter than AND, and parentheses group.
//
// A path that is no Queue field, or a value its field cannot hold, is
// INVALID_ARGUMENT. Regular expressions ("=~") are refused UNIMPLEMENTED:
// an ignored filter would return every queue and look like a filter that
// matched them all (#578).

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

type qfilterNode struct {
	op   string // AND, OR, NOT, or "" for a restriction
	kids []*qfilterNode

	path  []protoreflect.FieldDescriptor
	cmp   string
	value string
}

// queueFilter compiles a ListQueues filter into a predicate.
func queueFilter(filter string) (func(*taskspb.Queue) bool, error) {
	if strings.TrimSpace(filter) == "" {
		return func(*taskspb.Queue) bool { return true }, nil
	}
	toks, err := lexQueueFilter(filter)
	if err != nil {
		return nil, err
	}
	p := &qfilterParser{toks: toks, filter: filter}
	root, err := p.and()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, apierror.InvalidArgument("ListQueues filter %q: unexpected %q", filter, p.toks[p.pos].text)
	}
	return func(q *taskspb.Queue) bool { return root.eval(q.ProtoReflect()) }, nil
}

type qtoken struct {
	text   string
	quoted bool
}

func lexQueueFilter(s string) ([]qtoken, error) {
	var toks []qtoken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '(' || c == ')':
			toks = append(toks, qtoken{text: string(c)})
			i++
		case c == '"':
			j := i + 1
			var b strings.Builder
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, apierror.InvalidArgument("ListQueues filter %q has an unterminated string", s)
			}
			toks = append(toks, qtoken{text: b.String(), quoted: true})
			i = j + 1
		case strings.ContainsRune("<>=!:~", rune(c)):
			j := i + 1
			for j < len(s) && strings.ContainsRune("<>=!~", rune(s[j])) {
				j++
			}
			toks = append(toks, qtoken{text: s[i:j]})
			i = j
		default:
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && !strings.ContainsRune("()\"<>=!:~", rune(s[j])) {
				j++
			}
			toks = append(toks, qtoken{text: s[i:j]})
			i = j
		}
	}
	return toks, nil
}

type qfilterParser struct {
	toks   []qtoken
	pos    int
	filter string
}

func (p *qfilterParser) word(w string) bool {
	return p.pos < len(p.toks) && !p.toks[p.pos].quoted && p.toks[p.pos].text == w
}

func (p *qfilterParser) and() (*qfilterNode, error) {
	n := &qfilterNode{op: "AND"}
	for p.pos < len(p.toks) && !p.word(")") {
		if p.word("AND") {
			p.pos++
			continue
		}
		k, err := p.or()
		if err != nil {
			return nil, err
		}
		n.kids = append(n.kids, k)
	}
	if len(n.kids) == 0 {
		return nil, apierror.InvalidArgument("ListQueues filter %q has an empty expression", p.filter)
	}
	return n, nil
}

func (p *qfilterParser) or() (*qfilterNode, error) {
	n := &qfilterNode{op: "OR"}
	for {
		k, err := p.unary()
		if err != nil {
			return nil, err
		}
		n.kids = append(n.kids, k)
		if !p.word("OR") {
			return n, nil
		}
		p.pos++
	}
}

func (p *qfilterParser) unary() (*qfilterNode, error) {
	if p.pos >= len(p.toks) {
		return nil, apierror.InvalidArgument("ListQueues filter %q ends where a restriction is expected", p.filter)
	}
	t := p.toks[p.pos]
	switch {
	case !t.quoted && t.text == "NOT":
		p.pos++
		k, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &qfilterNode{op: "NOT", kids: []*qfilterNode{k}}, nil
	case !t.quoted && strings.HasPrefix(t.text, "-") && len(t.text) > 1:
		p.toks[p.pos].text = t.text[1:]
		k, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &qfilterNode{op: "NOT", kids: []*qfilterNode{k}}, nil
	case !t.quoted && t.text == "(":
		p.pos++
		k, err := p.and()
		if err != nil {
			return nil, err
		}
		if !p.word(")") {
			return nil, apierror.InvalidArgument("ListQueues filter %q has an unclosed parenthesis", p.filter)
		}
		p.pos++
		return k, nil
	}
	return p.restriction()
}

func (p *qfilterParser) restriction() (*qfilterNode, error) {
	field := p.toks[p.pos].text
	p.pos++
	if p.pos >= len(p.toks) || p.toks[p.pos].quoted {
		return nil, apierror.InvalidArgument("ListQueues filter %q: %q is not a FIELD OPERATOR VALUE restriction", p.filter, field)
	}
	op := p.toks[p.pos].text
	p.pos++
	switch op {
	case "=", "!=", "<", ">", "<=", ">=", ":":
	case "=~", "!~", "~":
		return nil, apierror.Unimplemented("ListQueues filter %q: regular expressions (%s) are not implemented", p.filter, op)
	default:
		return nil, apierror.InvalidArgument("ListQueues filter %q: %q is not an operator", p.filter, op)
	}
	if p.pos >= len(p.toks) || (!p.toks[p.pos].quoted && (p.toks[p.pos].text == "(" || p.toks[p.pos].text == ")")) {
		return nil, apierror.InvalidArgument("ListQueues filter %q: %s%s needs a value", p.filter, field, op)
	}
	value := p.toks[p.pos].text
	p.pos++
	path, err := queueFieldPath(field)
	if err != nil {
		return nil, apierror.InvalidArgument("ListQueues filter %q: %v", p.filter, err)
	}
	n := &qfilterNode{path: path, cmp: op, value: value}
	if err := n.check(); err != nil {
		return nil, apierror.InvalidArgument("ListQueues filter %q: %v", p.filter, err)
	}
	return n, nil
}

type filterError string

func (e filterError) Error() string { return string(e) }

// queueFieldPath resolves a dotted field path on Queue.
func queueFieldPath(field string) ([]protoreflect.FieldDescriptor, error) {
	md := (&taskspb.Queue{}).ProtoReflect().Descriptor()
	var path []protoreflect.FieldDescriptor
	parts := strings.Split(field, ".")
	for i, part := range parts {
		fds := md.Fields()
		fd := fds.ByName(protoreflect.Name(part))
		if fd == nil {
			fd = fds.ByJSONName(part)
		}
		if fd == nil || fd.IsList() || fd.IsMap() {
			return nil, filterError(strconv.Quote(field) + " is not a Queue field that can be filtered on")
		}
		path = append(path, fd)
		if fd.Kind() != protoreflect.MessageKind {
			if i != len(parts)-1 {
				return nil, filterError(strconv.Quote(field) + " is not a Queue field that can be filtered on")
			}
			return path, nil
		}
		md = fd.Message()
		if leafMessage(md) {
			if i != len(parts)-1 {
				return nil, filterError(strconv.Quote(field) + " is not a Queue field that can be filtered on")
			}
			return path, nil
		}
	}
	return nil, filterError(strconv.Quote(field) + " is a message; name one of its fields")
}

func leafMessage(md protoreflect.MessageDescriptor) bool {
	n := md.FullName()
	return n == "google.protobuf.Duration" || n == "google.protobuf.Timestamp"
}

// check refuses a value the restriction's field cannot hold.
func (n *qfilterNode) check() error {
	fd := n.path[len(n.path)-1]
	if n.value == "*" {
		if n.cmp != ":" {
			return filterError("* is a value only with \":\"")
		}
		return nil
	}
	switch {
	case fd.Kind() == protoreflect.EnumKind:
		if n.cmp != "=" && n.cmp != "!=" && n.cmp != ":" {
			return filterError(string(fd.Name()) + " is an enum, compared with =, != or : only")
		}
		if v := fd.Enum().Values().ByName(protoreflect.Name(n.value)); v == nil || v.Number() == 0 {
			return filterError(strconv.Quote(n.value) + " is not a value of " + string(fd.Name()))
		}
	case fd.Kind() == protoreflect.MessageKind && fd.Message().FullName() == "google.protobuf.Duration":
		if _, err := parseFilterSeconds(n.value); err != nil {
			return filterError(string(fd.Name()) + " takes a duration in seconds such as 90s, got " + strconv.Quote(n.value))
		}
	case fd.Kind() == protoreflect.MessageKind:
		if _, err := time.Parse(time.RFC3339Nano, n.value); err != nil {
			return filterError(string(fd.Name()) + " takes an RFC 3339 time, got " + strconv.Quote(n.value))
		}
	case fd.Kind() == protoreflect.StringKind:
	case fd.Kind() == protoreflect.BoolKind:
		if _, err := strconv.ParseBool(n.value); err != nil {
			return filterError(string(fd.Name()) + " takes true or false, got " + strconv.Quote(n.value))
		}
	default:
		if _, err := strconv.ParseFloat(n.value, 64); err != nil {
			return filterError(string(fd.Name()) + " takes a number, got " + strconv.Quote(n.value))
		}
	}
	return nil
}

func parseFilterSeconds(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
}

func (n *qfilterNode) eval(m protoreflect.Message) bool {
	switch n.op {
	case "AND":
		for _, k := range n.kids {
			if !k.eval(m) {
				return false
			}
		}
		return true
	case "OR":
		for _, k := range n.kids {
			if k.eval(m) {
				return true
			}
		}
		return false
	case "NOT":
		return !n.kids[0].eval(m)
	}
	for _, fd := range n.path[:len(n.path)-1] {
		m = m.Get(fd).Message()
	}
	fd := n.path[len(n.path)-1]
	if n.value == "*" {
		return m.Has(fd)
	}
	if n.cmp == "!=" {
		return !n.matches(m, fd, "=")
	}
	return n.matches(m, fd, n.cmp)
}

func (n *qfilterNode) matches(m protoreflect.Message, fd protoreflect.FieldDescriptor, cmp string) bool {
	v := m.Get(fd)
	switch {
	case fd.Kind() == protoreflect.EnumKind:
		return string(fd.Enum().Values().ByNumber(v.Enum()).Name()) == n.value
	case fd.Kind() == protoreflect.StringKind:
		if cmp == ":" {
			return strings.Contains(strings.ToLower(v.String()), strings.ToLower(n.value))
		}
		return compareFilter(strings.Compare(v.String(), n.value), cmp)
	case fd.Kind() == protoreflect.BoolKind:
		want, _ := strconv.ParseBool(n.value)
		return (cmp == "=" || cmp == ":") && v.Bool() == want
	case fd.Kind() == protoreflect.MessageKind:
		msg := v.Message()
		secs := float64(msg.Get(msg.Descriptor().Fields().ByName("seconds")).Int()) +
			float64(msg.Get(msg.Descriptor().Fields().ByName("nanos")).Int())/1e9
		var want float64
		if fd.Message().FullName() == "google.protobuf.Duration" {
			want, _ = parseFilterSeconds(n.value)
		} else {
			t, _ := time.Parse(time.RFC3339Nano, n.value)
			want = float64(t.UnixNano()) / 1e9
		}
		return compareFloat(secs, want, cmp)
	}
	want, _ := strconv.ParseFloat(n.value, 64)
	var got float64
	switch fd.Kind() {
	case protoreflect.DoubleKind, protoreflect.FloatKind:
		got = v.Float()
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind, protoreflect.Fixed32Kind, protoreflect.Fixed64Kind:
		got = float64(v.Uint())
	default:
		got = float64(v.Int())
	}
	return compareFloat(got, want, cmp)
}

func compareFloat(got, want float64, cmp string) bool {
	c := 0
	if got < want {
		c = -1
	} else if got > want {
		c = 1
	}
	return compareFilter(c, cmp)
}

func compareFilter(c int, cmp string) bool {
	switch cmp {
	case "=", ":":
		return c == 0
	case "<":
		return c < 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	case ">=":
		return c >= 0
	}
	return false
}

// QueueFilter compiles a ListQueues filter for callers outside the API,
// such as the console (#1255): the same code ListQueues evaluates.
func QueueFilter(filter string) (func(Queue) bool, error) {
	keep, err := queueFilter(filter)
	if err != nil {
		return nil, err
	}
	return func(q Queue) bool { return keep(toProtoQueue(q)) }, nil
}
