package kms

import (
	"strings"
	"unicode"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The list filter language of https://docs.cloud.google.com/kms/docs/sorting-and-filtering
// (#408): comparisons `field op value` with = != : < > <= =>; negation with
// NOT or -; AND (a space is equivalent) and OR, where "OR takes precedence
// over AND", and parentheses. A filter is parsed whole, then every field it
// names must be one this listing supports, with an operator it supports:
// otherwise it is UNIMPLEMENTED naming the field, never ignored. A syntax
// error is INVALID_ARGUMENT (UNVERIFIED).

// fieldSupport lists, per field, the operators a listing evaluates. A key
// ending in "." is a prefix, such as "labels.".
type fieldSupport map[string][]string

// filterExpr is a parsed filter.
type filterExpr interface {
	match(values func(field string) (string, bool)) bool
}

type cmpExpr struct{ field, op, value string }
type notExpr struct{ e filterExpr }
type andExpr []filterExpr
type orExpr []filterExpr

func (c cmpExpr) match(values func(string) (string, bool)) bool {
	v, ok := values(c.field)
	if !ok {
		return false
	}
	switch c.op {
	case "=":
		return v == c.value
	case "!=":
		return v != c.value
	case ":":
		return strings.Contains(strings.ToLower(v), strings.ToLower(c.value))
	}
	return false
}
func (n notExpr) match(v func(string) (string, bool)) bool { return !n.e.match(v) }
func (a andExpr) match(v func(string) (string, bool)) bool {
	for _, e := range a {
		if !e.match(v) {
			return false
		}
	}
	return true
}
func (o orExpr) match(v func(string) (string, bool)) bool {
	for _, e := range o {
		if e.match(v) {
			return true
		}
	}
	return false
}

// parseFilter parses filter and checks every comparison against support.
// An empty filter is nil: everything matches.
func parseFilter(filter string, support fieldSupport) (filterExpr, error) {
	if strings.TrimSpace(filter) == "" {
		return nil, nil
	}
	toks, err := tokenize(filter)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	e, err := p.and()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, apierror.InvalidArgument("filter %q: unexpected %q", filter, p.toks[p.pos])
	}
	if err := checkSupport(e, support); err != nil {
		return nil, err
	}
	return e, nil
}

func checkSupport(e filterExpr, support fieldSupport) error {
	switch x := e.(type) {
	case cmpExpr:
		ops, ok := support[x.field]
		if !ok {
			for prefix, o := range support {
				if strings.HasSuffix(prefix, ".") && strings.HasPrefix(x.field, prefix) && len(x.field) > len(prefix) {
					ops, ok = o, true
				}
			}
		}
		if !ok {
			return apierror.Unimplemented("filtering on %q is not implemented", x.field)
		}
		for _, op := range ops {
			if op == x.op {
				return nil
			}
		}
		return apierror.Unimplemented("filtering on %q with %q is not implemented: use %s", x.field, x.op, strings.Join(ops, " or "))
	case notExpr:
		return checkSupport(x.e, support)
	case andExpr:
		for _, s := range x {
			if err := checkSupport(s, support); err != nil {
				return err
			}
		}
	case orExpr:
		for _, s := range x {
			if err := checkSupport(s, support); err != nil {
				return err
			}
		}
	}
	return nil
}

func tokenize(s string) ([]string, error) {
	var toks []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(' || c == ')':
			toks = append(toks, string(c))
			i++
		case c == '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				return nil, apierror.InvalidArgument("filter %q: an unterminated quoted string", s)
			}
			toks = append(toks, s[i:i+j+2])
			i += j + 2
		case strings.HasPrefix(s[i:], "!=") || strings.HasPrefix(s[i:], "<=") || strings.HasPrefix(s[i:], "=>") || strings.HasPrefix(s[i:], ">="):
			toks = append(toks, s[i:i+2])
			i += 2
		case c == '=' || c == ':' || c == '<' || c == '>':
			toks = append(toks, string(c))
			i++
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n()\"=:<>!", rune(s[j])) {
				j++
			}
			if j == i {
				return nil, apierror.InvalidArgument("filter %q: unexpected %q", s, string(c))
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks, nil
}

type parser struct {
	toks []string
	pos  int
}

func (p *parser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

// and: or { [AND] or } — AND, or a space, joins; it binds more loosely than
// OR, per the documentation.
func (p *parser) and() (filterExpr, error) {
	var parts andExpr
	for {
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		parts = append(parts, e)
		if p.peek() == "AND" {
			p.pos++
			continue
		}
		if t := p.peek(); t == "" || t == ")" {
			break
		}
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return parts, nil
}

func (p *parser) or() (filterExpr, error) {
	var parts orExpr
	for {
		e, err := p.unary()
		if err != nil {
			return nil, err
		}
		parts = append(parts, e)
		if p.peek() != "OR" {
			break
		}
		p.pos++
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return parts, nil
}

func (p *parser) unary() (filterExpr, error) {
	switch t := p.peek(); {
	case t == "NOT":
		p.pos++
		e, err := p.unary()
		return notExpr{e}, err
	case strings.HasPrefix(t, "-") && len(t) > 1:
		p.toks[p.pos] = t[1:]
		e, err := p.unary()
		return notExpr{e}, err
	case t == "(":
		p.pos++
		e, err := p.and()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, apierror.InvalidArgument("filter: a missing closing parenthesis")
		}
		p.pos++
		return e, nil
	}
	return p.comparison()
}

func (p *parser) comparison() (filterExpr, error) {
	field := p.peek()
	if field == "" || !isField(field) {
		return nil, apierror.InvalidArgument("filter: expected a field name, found %q", field)
	}
	p.pos++
	op := p.peek()
	switch op {
	case "=", "!=", ":", "<", ">", "<=", "=>", ">=":
	default:
		return nil, apierror.InvalidArgument("filter: expected an operator after %q, found %q", field, op)
	}
	p.pos++
	value := p.peek()
	if value == "" || value == "(" || value == ")" || value == "AND" || value == "OR" {
		return nil, apierror.InvalidArgument("filter: expected a value after %s%s", field, op)
	}
	p.pos++
	if strings.HasPrefix(value, `"`) {
		value = strings.Trim(value, `"`)
	}
	if op == ">=" {
		op = "=>"
	}
	return cmpExpr{field: field, op: op, value: value}, nil
}

func isField(s string) bool {
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
				return false
			}
		}
	}
	return !strings.ContainsAny(s[:1], "0123456789")
}
