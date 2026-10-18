package run

import (
	"strconv"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The ListOperations filter (#1263): the AIP-160 subset that names an
// operation's own fields. Comparisons `field op value` on
//
//   - done        (= or !=, true or false),
//   - name        (= or !=, or : for a substring),
//   - error.code  (= or !=, a google.rpc.Code number; an operation that has
//     not failed has none and matches only !=),
//
// joined by AND (or a space), OR, NOT or a leading "-", and parentheses, with
// OR binding tighter than AND as AIP-160 says. Any other field is
// INVALID_ARGUMENT naming it (UNVERIFIED: Google documents no code for it),
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/longrunning/operations.proto
// never ignored, since ignoring it would list operations the filter excludes.

type opFilter interface {
	match(op *longrunningpb.Operation) bool
}

type opCmp struct{ field, op, value string }
type opNot struct{ e opFilter }
type opAnd []opFilter
type opOr []opFilter

func (c opCmp) match(op *longrunningpb.Operation) bool {
	var v string
	has := true
	switch c.field {
	case "done":
		v = strconv.FormatBool(op.GetDone())
	case "name":
		v = op.GetName()
	case "error.code":
		if op.GetError() == nil {
			has = false
		} else {
			v = strconv.Itoa(int(op.GetError().GetCode()))
		}
	}
	switch c.op {
	case "=":
		return has && v == c.value
	case "!=":
		return !has || v != c.value
	case ":":
		return has && strings.Contains(v, c.value)
	}
	return false
}

func (n opNot) match(op *longrunningpb.Operation) bool { return !n.e.match(op) }
func (a opAnd) match(op *longrunningpb.Operation) bool {
	for _, e := range a {
		if !e.match(op) {
			return false
		}
	}
	return true
}
func (o opOr) match(op *longrunningpb.Operation) bool {
	for _, e := range o {
		if e.match(op) {
			return true
		}
	}
	return false
}

// parseOpFilter parses a ListOperations filter; an empty one is nil.
func parseOpFilter(filter string) (opFilter, error) {
	if strings.TrimSpace(filter) == "" {
		return nil, nil
	}
	toks, err := opTokens(filter)
	if err != nil {
		return nil, err
	}
	p := &opParser{toks: toks, src: filter}
	e, err := p.and()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, apierror.InvalidArgument("filter %q: unexpected %q", filter, p.toks[p.pos])
	}
	return e, nil
}

func opTokens(s string) ([]string, error) {
	var toks []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(' || c == ')' || c == '=' || c == ':':
			toks = append(toks, string(c))
			i++
		case strings.HasPrefix(s[i:], "!="):
			toks = append(toks, "!=")
			i += 2
		case c == '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				return nil, apierror.InvalidArgument("filter %q: an unterminated quoted string", s)
			}
			toks = append(toks, s[i:i+j+2])
			i += j + 2
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n()\"=:!<>", rune(s[j])) {
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

type opParser struct {
	toks []string
	pos  int
	src  string
}

func (p *opParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *opParser) and() (opFilter, error) {
	var parts opAnd
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

func (p *opParser) or() (opFilter, error) {
	var parts opOr
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

func (p *opParser) unary() (opFilter, error) {
	switch t := p.peek(); {
	case t == "NOT":
		p.pos++
		e, err := p.unary()
		return opNot{e}, err
	case strings.HasPrefix(t, "-") && len(t) > 1:
		p.toks[p.pos] = t[1:]
		e, err := p.unary()
		return opNot{e}, err
	case t == "(":
		p.pos++
		e, err := p.and()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, apierror.InvalidArgument("filter %q: a missing closing parenthesis", p.src)
		}
		p.pos++
		return e, nil
	}
	return p.comparison()
}

func (p *opParser) comparison() (opFilter, error) {
	field := p.peek()
	switch field {
	case "done", "name", "error.code":
	case "", "(", ")", "=", "!=", ":", "AND", "OR":
		return nil, apierror.InvalidArgument("filter %q: expected a field name, found %q", p.src, field)
	default:
		return nil, apierror.InvalidArgument("filter %q: %q is not a field ListOperations filters on: use done, name or error.code", p.src, field)
	}
	p.pos++
	op := p.peek()
	if op != "=" && op != "!=" && op != ":" {
		return nil, apierror.InvalidArgument("filter %q: expected =, != or : after %q, found %q", p.src, field, op)
	}
	if op == ":" && field != "name" {
		return nil, apierror.InvalidArgument("filter %q: %q takes = or !=, not :", p.src, field)
	}
	p.pos++
	value := p.peek()
	if value == "" || value == "(" || value == ")" || value == "AND" || value == "OR" {
		return nil, apierror.InvalidArgument("filter %q: expected a value after %s%s", p.src, field, op)
	}
	p.pos++
	value = strings.Trim(value, `"`)
	switch field {
	case "done":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return nil, apierror.InvalidArgument("filter %q: done compares with true or false, not %q", p.src, value)
		}
		value = strconv.FormatBool(b)
	case "error.code":
		if _, err := strconv.Atoi(value); err != nil {
			return nil, apierror.InvalidArgument("filter %q: error.code compares with a number, not %q", p.src, value)
		}
	}
	return opCmp{field: field, op: op, value: value}, nil
}
