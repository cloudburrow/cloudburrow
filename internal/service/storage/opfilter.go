package storage

// buckets.operations.list filter (#1280), in AIP-160's syntax, which the
// method's reference names. Evaluated on the operation as the JSON API
// returns it:
//
//   - a restriction is FIELD OP VALUE, with OP one of =, !=, <, <=, >, >=
//     and ":" (has: a case-insensitive substring, and FIELD:* for a field
//     that is set);
//   - FIELD is a dotted path from the top of the Operation: name, done,
//     error.code, error.message, or anything under metadata or response
//     (metadata.@type, metadata.commonMetadata.type,
//     metadata.commonMetadata.createTime, metadata.succeededCount);
//   - done takes true or false; two RFC 3339 times compare as times, two
//     numbers as numbers, and anything else as strings;
//   - restrictions combine with AND (or a space), OR and NOT (or a leading
//     "-"); OR binds tighter than AND, and parentheses group.
//
// A field outside those, a malformed expression or a regular expression is
// 400 invalid (UNVERIFIED: the reference does not give the refusal). A path
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/status-codes
// an operation does not have does not match it.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type opFilterNode struct {
	op    string // AND, OR, NOT, or "" for a restriction
	kids  []*opFilterNode
	path  []string
	cmp   string
	value string
}

type opToken struct {
	text   string
	quoted bool
}

// operationFilter compiles a buckets.operations.list filter.
func operationFilter(filter string) (func(map[string]any) bool, error) {
	if strings.TrimSpace(filter) == "" {
		return func(map[string]any) bool { return true }, nil
	}
	toks, err := lexOpFilter(filter)
	if err != nil {
		return nil, err
	}
	p := &opFilterParser{toks: toks, filter: filter}
	root, err := p.and()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, p.fail("unexpected %q", p.toks[p.pos].text)
	}
	return root.eval, nil
}

func lexOpFilter(s string) ([]opToken, error) {
	var toks []opToken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '(' || c == ')':
			toks = append(toks, opToken{text: string(c)})
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
				return nil, badRequest("Invalid argument: filter %q has an unterminated string", s)
			}
			toks = append(toks, opToken{text: b.String(), quoted: true})
			i = j + 1
		case strings.ContainsRune("<>=!:~", rune(c)):
			j := i + 1
			for j < len(s) && strings.ContainsRune("<>=!~", rune(s[j])) {
				j++
			}
			toks = append(toks, opToken{text: s[i:j]})
			i = j
		default:
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && !strings.ContainsRune("()\"<>=!:~", rune(s[j])) {
				j++
			}
			toks = append(toks, opToken{text: s[i:j]})
			i = j
		}
	}
	return toks, nil
}

type opFilterParser struct {
	toks   []opToken
	pos    int
	filter string
}

func (p *opFilterParser) fail(format string, a ...any) error {
	return badRequest("Invalid argument: filter %q: %s", p.filter, fmt.Sprintf(format, a...))
}

func (p *opFilterParser) word(w string) bool {
	return p.pos < len(p.toks) && !p.toks[p.pos].quoted && p.toks[p.pos].text == w
}

func (p *opFilterParser) and() (*opFilterNode, error) {
	n := &opFilterNode{op: "AND"}
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
		return nil, p.fail("an empty expression")
	}
	return n, nil
}

func (p *opFilterParser) or() (*opFilterNode, error) {
	n := &opFilterNode{op: "OR"}
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

func (p *opFilterParser) unary() (*opFilterNode, error) {
	if p.pos >= len(p.toks) {
		return nil, p.fail("it ends where a restriction is expected")
	}
	t := p.toks[p.pos]
	switch {
	case !t.quoted && t.text == "NOT":
		p.pos++
		k, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &opFilterNode{op: "NOT", kids: []*opFilterNode{k}}, nil
	case !t.quoted && strings.HasPrefix(t.text, "-") && len(t.text) > 1:
		p.toks[p.pos].text = t.text[1:]
		k, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &opFilterNode{op: "NOT", kids: []*opFilterNode{k}}, nil
	case !t.quoted && t.text == "(":
		p.pos++
		k, err := p.and()
		if err != nil {
			return nil, err
		}
		if !p.word(")") {
			return nil, p.fail("an unclosed parenthesis")
		}
		p.pos++
		return k, nil
	}
	return p.restriction()
}

func (p *opFilterParser) restriction() (*opFilterNode, error) {
	field := p.toks[p.pos].text
	p.pos++
	if p.pos >= len(p.toks) || p.toks[p.pos].quoted {
		return nil, p.fail("%q is not a FIELD OPERATOR VALUE restriction", field)
	}
	op := p.toks[p.pos].text
	p.pos++
	switch op {
	case "=", "!=", "<", ">", "<=", ">=", ":":
	default:
		return nil, p.fail("%q is not an operator", op)
	}
	if p.pos >= len(p.toks) || (!p.toks[p.pos].quoted && (p.toks[p.pos].text == "(" || p.toks[p.pos].text == ")")) {
		return nil, p.fail("%s%s needs a value", field, op)
	}
	value := p.toks[p.pos].text
	p.pos++
	path := strings.Split(field, ".")
	switch {
	case len(path) == 1 && (path[0] == "name" || path[0] == "done"):
	case len(path) == 2 && path[0] == "error" && (path[1] == "code" || path[1] == "message"):
	case len(path) > 1 && (path[0] == "metadata" || path[0] == "response"):
	default:
		return nil, p.fail("%q is not an Operation field that can be filtered on", field)
	}
	if value == "*" && op != ":" {
		return nil, p.fail("* is a value only with \":\"")
	}
	if field == "done" && value != "*" {
		if _, err := strconv.ParseBool(value); err != nil {
			return nil, p.fail("done takes true or false, got %q", value)
		}
	}
	return &opFilterNode{path: path, cmp: op, value: value}, nil
}

func (n *opFilterNode) eval(op map[string]any) bool {
	switch n.op {
	case "AND":
		for _, k := range n.kids {
			if !k.eval(op) {
				return false
			}
		}
		return true
	case "OR":
		for _, k := range n.kids {
			if k.eval(op) {
				return true
			}
		}
		return false
	case "NOT":
		return !n.kids[0].eval(op)
	}
	var v any = op
	for _, part := range n.path {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if v, ok = m[part]; !ok {
			if n.path[0] == "done" {
				v = false // done is false when it is not given
				break
			}
			return false
		}
	}
	if n.value == "*" {
		return v != nil
	}
	got := scalarString(v)
	if n.cmp == "!=" {
		return !compareOpValue(got, n.value, "=")
	}
	return compareOpValue(got, n.value, n.cmp)
}

// scalarString is a JSON scalar as filter text; "" for an object or list.
func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return numberString(v)
}

func compareOpValue(got, want, cmp string) bool {
	if cmp == ":" {
		return strings.Contains(strings.ToLower(got), strings.ToLower(want))
	}
	c := strings.Compare(got, want)
	if g, err := time.Parse(time.RFC3339Nano, got); err == nil {
		if w, err := time.Parse(time.RFC3339Nano, want); err == nil {
			c = g.Compare(w)
		}
	} else if g, err := strconv.ParseFloat(got, 64); err == nil {
		if w, err := strconv.ParseFloat(want, 64); err == nil {
			c = 0
			if g < w {
				c = -1
			} else if g > w {
				c = 1
			}
		}
	} else if strings.EqualFold(got, "true") || strings.EqualFold(got, "false") {
		if strings.EqualFold(got, want) {
			c = 0
		}
	}
	switch cmp {
	case "=":
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
