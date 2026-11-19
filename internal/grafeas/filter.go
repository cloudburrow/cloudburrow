package grafeas

import (
	"fmt"
	"strings"
	"unicode"
)

// A filter is Container Analysis's occurrence and note filter: comparisons
// of a field with a quoted (or bare) value, has_prefix(field, "prefix"),
// joined by AND, OR and NOT and grouped by parentheses, as gcloud's
// filter_util builds them and as the API reference's examples write them
// (`kind="VULNERABILITY" AND resourceUrl="..."`,
// `noteProjectId="goog-vulnz" AND noteId="CVE-..."`,
// `has_prefix(resourceUrl, "https://...")`).
type filter interface {
	match(fields func(string) ([]string, bool)) bool
}

type andF struct{ l, r filter }
type orF struct{ l, r filter }
type notF struct{ f filter }
type cmpF struct{ field, op, value string }
type prefixF struct{ field, prefix string }
type allF struct{}

func (f andF) match(v func(string) ([]string, bool)) bool { return f.l.match(v) && f.r.match(v) }
func (f orF) match(v func(string) ([]string, bool)) bool  { return f.l.match(v) || f.r.match(v) }
func (f notF) match(v func(string) ([]string, bool)) bool { return !f.f.match(v) }
func (allF) match(func(string) ([]string, bool)) bool     { return true }

func (f cmpF) match(v func(string) ([]string, bool)) bool {
	vals, _ := v(f.field)
	for _, x := range vals {
		if compare(x, f.op, f.value) {
			return true
		}
	}
	// != holds for a field with no value.
	return len(vals) == 0 && f.op == "!="
}

func compare(x, op, v string) bool {
	switch op {
	case "=":
		return x == v
	case "!=":
		return x != v
	case ":":
		return strings.Contains(x, v)
	case "<":
		return x < v
	case "<=":
		return x <= v
	case ">":
		return x > v
	case ">=":
		return x >= v
	}
	return false
}

func (f prefixF) match(v func(string) ([]string, bool)) bool {
	vals, _ := v(f.field)
	for _, x := range vals {
		if strings.HasPrefix(x, f.prefix) {
			return true
		}
	}
	return false
}

// parseFilter parses s, checking every field it names with known.
func parseFilter(s string, known func(string) bool) (filter, error) {
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return allF{}, nil
	}
	p := &parser{toks: toks, known: known}
	f, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.i != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q in filter", p.toks[p.i].s)
	}
	return f, nil
}

type token struct {
	s      string
	quoted bool
}

func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '(' || c == ')' || c == ',':
			out = append(out, token{s: string(c)})
			i++
		case c == '"' || c == '\'':
			j := i + 1
			var b strings.Builder
			for ; j < len(s) && s[j] != c; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string in filter")
			}
			out = append(out, token{s: b.String(), quoted: true})
			i = j + 1
		case strings.ContainsRune("=!<>:", rune(c)):
			j := i + 1
			if j < len(s) && s[j] == '=' {
				j++
			}
			op := s[i:j]
			if op == "!" {
				return nil, fmt.Errorf("unexpected ! in filter")
			}
			out = append(out, token{s: op})
			i = j
		default:
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && !strings.ContainsRune("()=!<>:,\"'", rune(s[j])) {
				j++
			}
			out = append(out, token{s: s[i:j]})
			i = j
		}
	}
	return out, nil
}

type parser struct {
	toks  []token
	i     int
	known func(string) bool
}

func (p *parser) peek() (token, bool) {
	if p.i < len(p.toks) {
		return p.toks[p.i], true
	}
	return token{}, false
}

func (p *parser) keyword(k string) bool {
	t, ok := p.peek()
	if ok && !t.quoted && strings.EqualFold(t.s, k) {
		p.i++
		return true
	}
	return false
}

func (p *parser) or() (filter, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.keyword("OR") {
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = orF{l, r}
	}
	return l, nil
}

func (p *parser) and() (filter, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		if p.keyword("AND") {
			r, err := p.unary()
			if err != nil {
				return nil, err
			}
			l = andF{l, r}
			continue
		}
		// Juxtaposition is AND, as in AIP-160.
		if t, ok := p.peek(); ok && (t.s == "(" || (!t.quoted && t.s != ")" && !strings.EqualFold(t.s, "OR"))) {
			r, err := p.unary()
			if err != nil {
				return nil, err
			}
			l = andF{l, r}
			continue
		}
		return l, nil
	}
}

func (p *parser) unary() (filter, error) {
	if p.keyword("NOT") {
		f, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notF{f}, nil
	}
	if t, ok := p.peek(); ok && t.s == "-" {
		p.i++
		f, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notF{f}, nil
	}
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("filter ends early")
	}
	if t.s == "(" && !t.quoted {
		p.i++
		f, err := p.or()
		if err != nil {
			return nil, err
		}
		if c, ok := p.peek(); !ok || c.s != ")" {
			return nil, fmt.Errorf("missing ) in filter")
		}
		p.i++
		return f, nil
	}
	p.i++
	if !t.quoted && strings.EqualFold(t.s, "has_prefix") {
		return p.hasPrefix()
	}
	if t.quoted {
		return nil, fmt.Errorf("a filter term must start with a field, not %q", t.s)
	}
	field := t.s
	if !p.known(field) {
		return nil, fmt.Errorf("invalid filter field %q", field)
	}
	op, ok := p.peek()
	if !ok || !strings.Contains("= != < <= > >= :", op.s) || op.quoted {
		return nil, fmt.Errorf("expected a comparison after %q", field)
	}
	p.i++
	v, ok := p.peek()
	if !ok || v.s == "(" || v.s == ")" {
		return nil, fmt.Errorf("expected a value after %s%s", field, op.s)
	}
	p.i++
	return cmpF{field: field, op: op.s, value: v.s}, nil
}

func (p *parser) hasPrefix() (filter, error) {
	expect := func(s string) error {
		t, ok := p.peek()
		if !ok || t.s != s || t.quoted {
			return fmt.Errorf("has_prefix: expected %q", s)
		}
		p.i++
		return nil
	}
	if err := expect("("); err != nil {
		return nil, err
	}
	f, ok := p.peek()
	if !ok || f.quoted || !p.known(f.s) {
		return nil, fmt.Errorf("has_prefix: invalid field")
	}
	p.i++
	if err := expect(","); err != nil {
		return nil, err
	}
	v, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("has_prefix: expected a prefix")
	}
	p.i++
	if err := expect(")"); err != nil {
		return nil, err
	}
	return prefixF{field: f.s, prefix: v.s}, nil
}
