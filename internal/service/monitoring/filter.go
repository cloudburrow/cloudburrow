package monitoring

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The Monitoring filter language
// (docs.cloud.google.com/monitoring/api/v3/filters, read 2026-11-19):
//
//	filter   = or
//	or       = and { "OR" and }
//	and      = unary { [ "AND" ] unary }
//	unary    = ( "NOT" | "-" ) unary | "(" or ")" | selector op value
//	selector = "project" | "metric.type" | "resource.type"
//	         | "metric.labels." key | "resource.labels." key   ("label." too)
//	op       = "=" | "!=" | ":" | ">" | ">=" | "<" | "<="
//	value    = string | number | true | false
//	         | starts_with(string) | ends_with(string) | has_substring(string)
//	         | one_of(string, ...) | monitoring.regex.full_match(string)
//
// ":" is "has": a substring of the value. A comparison with a function
// tests the function; != negates it. metadata.* selectors (system and user
// labels) are not served and are INVALID_ARGUMENT, as is anything else the
// grammar does not have: a filter that ignored a term would return series
// the caller excluded.

// filterKind is what a filter selects, which decides its selectors.
type filterKind int

const (
	filterSeries filterKind = iota
	filterDescriptors
	filterResources
)

// subject is what a filter is evaluated against.
type subject struct {
	project, metricType, resourceType string
	metricLabels, resourceLabels      map[string]string
}

type node interface{ eval(subject) bool }

type andNode []node
type orNode []node
type notNode struct{ n node }

func (a andNode) eval(s subject) bool {
	for _, n := range a {
		if !n.eval(s) {
			return false
		}
	}
	return true
}

func (o orNode) eval(s subject) bool {
	for _, n := range o {
		if n.eval(s) {
			return true
		}
	}
	return false
}

func (n notNode) eval(s subject) bool { return !n.n.eval(s) }

type cmpNode struct {
	get func(subject) (string, bool)
	op  string
	// test is the value or function test.
	test func(string) bool
	// num is the numeric value of an ordered comparison.
	num float64
	str string
	// isType is a metric.type comparison.
	isType bool
}

func (c cmpNode) eval(s subject) bool {
	got, ok := c.get(s)
	if !ok {
		return false
	}
	switch c.op {
	case "=":
		return c.test(got)
	case "!=":
		return !c.test(got)
	case ":":
		return strings.Contains(got, c.str)
	}
	g, err := strconv.ParseFloat(got, 64)
	cmp := 0
	if err == nil {
		switch {
		case g < c.num:
			cmp = -1
		case g > c.num:
			cmp = 1
		}
	} else {
		cmp = strings.Compare(got, c.str)
	}
	switch c.op {
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	default:
		return cmp <= 0
	}
}

// Filter is a parsed filter; an empty one matches everything.
type Filter struct {
	root node
	// metricType is the metric type the filter's top-level conjunction
	// pins with metric.type = "T", which ListTimeSeries requires.
	metricType string
}

func (f *Filter) match(s subject) bool { return f.root == nil || f.root.eval(s) }

func badFilter(filter, why string, a ...any) error {
	return apierror.InvalidArgument("Field filter had an invalid value of %q: "+why, append([]any{filter}, a...)...)
}

type parser struct {
	src  string
	toks []string
	i    int
	kind filterKind
}

func lex(src string) ([]string, error) {
	var out []string
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return nil, badFilter(src, "unterminated string")
			}
			out = append(out, src[i:j+1])
			i = j + 1
		case strings.HasPrefix(src[i:], "!=") || strings.HasPrefix(src[i:], ">=") || strings.HasPrefix(src[i:], "<="):
			out = append(out, src[i:i+2])
			i += 2
		case strings.ContainsRune("()=:<>,", rune(c)):
			out = append(out, string(c))
			i++
		default:
			j := i
			for j < len(src) && !strings.ContainsRune(" \t\r\n()=:<>,!\"'", rune(src[j])) {
				j++
			}
			if j == i {
				return nil, badFilter(src, "unexpected %q", string(c))
			}
			out = append(out, src[i:j])
			i = j
		}
	}
	return out, nil
}

// parseFilter parses a filter for kind.
func parseFilter(src string, kind filterKind) (*Filter, error) {
	if strings.TrimSpace(src) == "" {
		return &Filter{}, nil
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks, kind: kind}
	root, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.i < len(p.toks) {
		return nil, badFilter(src, "unexpected %q", p.toks[p.i])
	}
	f := &Filter{root: root}
	f.metricType = pinnedType(root)
	return f, nil
}

// pinnedType is the metric type root's conjunction requires by equality.
func pinnedType(n node) string {
	switch v := n.(type) {
	case andNode:
		for _, c := range v {
			if t := pinnedType(c); t != "" {
				return t
			}
		}
	case *cmpNode:
		if v.op == "=" && v.str != "" && v.isType {
			return v.str
		}
	}
	return ""
}

func (p *parser) peek() string {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return ""
}

func (p *parser) next() string { t := p.peek(); p.i++; return t }

func (p *parser) or() (node, error) {
	first, err := p.and()
	if err != nil {
		return nil, err
	}
	out := orNode{first}
	for p.peek() == "OR" {
		p.i++
		n, err := p.and()
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

func (p *parser) and() (node, error) {
	var out andNode
	for {
		t := p.peek()
		if t == "" || t == ")" || t == "OR" {
			break
		}
		if t == "AND" {
			p.i++
			continue
		}
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, badFilter(p.src, "expected a comparison")
	}
	if len(out) == 1 {
		return out[0], nil
	}
	return out, nil
}

func (p *parser) unary() (node, error) {
	switch t := p.peek(); {
	case t == "NOT" || t == "-":
		p.i++
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	case strings.HasPrefix(t, "-") && len(t) > 1:
		p.toks[p.i] = t[1:]
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	case t == "(":
		p.i++
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.next() != ")" {
			return nil, badFilter(p.src, "missing )")
		}
		return n, nil
	}
	return p.comparison()
}

func unquote(t string) (string, bool) {
	if len(t) >= 2 && (t[0] == '"' || t[0] == '\'') {
		var b strings.Builder
		for i := 1; i < len(t)-1; i++ {
			if t[i] == '\\' && i+1 < len(t)-1 {
				i++
			}
			b.WriteByte(t[i])
		}
		return b.String(), true
	}
	return t, false
}

// selector resolves a selector to its getter.
func (p *parser) selector(sel string) (func(subject) (string, bool), bool, error) {
	labelOf := func(prefixes []string, m func(subject) map[string]string) (func(subject) (string, bool), bool) {
		for _, pre := range prefixes {
			if k, ok := strings.CutPrefix(sel, pre); ok && k != "" {
				k, _ = unquote(k)
				return func(s subject) (string, bool) { v, ok := m(s)[k]; return v, ok }, true
			}
		}
		return nil, false
	}
	switch sel {
	case "project":
		return func(s subject) (string, bool) { return s.project, true }, false, nil
	case "metric.type":
		if p.kind != filterResources {
			return func(s subject) (string, bool) { return s.metricType, true }, true, nil
		}
	case "resource.type":
		if p.kind != filterDescriptors {
			return func(s subject) (string, bool) { return s.resourceType, true }, false, nil
		}
	}
	if p.kind == filterSeries {
		if g, ok := labelOf([]string{"metric.labels.", "metric.label."}, func(s subject) map[string]string { return s.metricLabels }); ok {
			return g, false, nil
		}
		if g, ok := labelOf([]string{"resource.labels.", "resource.label."}, func(s subject) map[string]string { return s.resourceLabels }); ok {
			return g, false, nil
		}
		if strings.HasPrefix(sel, "metadata.") {
			return nil, false, badFilter(p.src, "metadata selectors (%s) are not supported here: CloudBurrow keeps no system or user metadata labels", sel)
		}
	}
	return nil, false, badFilter(p.src, "unknown selector %q", sel)
}

func (p *parser) comparison() (node, error) {
	sel := p.next()
	// A quoted label key is its own token after the dot.
	for strings.HasSuffix(sel, ".") && strings.HasPrefix(p.peek(), `"`) {
		sel += p.next()
	}
	get, isType, err := p.selector(sel)
	if err != nil {
		return nil, err
	}
	op := p.next()
	switch op {
	case "=", "!=", ":", ">", ">=", "<", "<=":
	default:
		return nil, badFilter(p.src, "expected an operator after %s, got %q", sel, op)
	}
	val := p.next()
	if val == "" {
		return nil, badFilter(p.src, "expected a value after %s %s", sel, op)
	}
	c := &cmpNode{get: get, op: op, isType: isType}
	if s, quoted := unquote(val); quoted {
		c.str = s
		c.test = func(got string) bool { return got == s }
	} else if p.peek() == "(" {
		if op != "=" && op != "!=" {
			return nil, badFilter(p.src, "%s takes = or != with a function", sel)
		}
		p.i++
		var args []string
		for {
			a := p.next()
			s, quoted := unquote(a)
			if !quoted {
				return nil, badFilter(p.src, "%s's arguments are strings", val)
			}
			args = append(args, s)
			if sep := p.next(); sep == ")" {
				break
			} else if sep != "," {
				return nil, badFilter(p.src, "expected , or ) in %s(...)", val)
			}
		}
		test, err := function(p.src, val, args)
		if err != nil {
			return nil, err
		}
		c.test = test
	} else {
		c.str = val
		if n, err := strconv.ParseFloat(val, 64); err == nil {
			c.num = n
		} else if val != "true" && val != "false" {
			return nil, badFilter(p.src, "%q is not a string, a number or a boolean; quote strings", val)
		}
		c.test = func(got string) bool { return got == val }
	}
	if (op == ">" || op == ">=" || op == "<" || op == "<=") && c.test == nil {
		return nil, badFilter(p.src, "ordered comparisons take a value")
	}
	return c, nil
}

func function(src, name string, args []string) (func(string) bool, error) {
	one := func() error {
		if len(args) != 1 {
			return badFilter(src, "%s takes one argument", name)
		}
		return nil
	}
	switch name {
	case "starts_with":
		if err := one(); err != nil {
			return nil, err
		}
		return func(s string) bool { return strings.HasPrefix(s, args[0]) }, nil
	case "ends_with":
		if err := one(); err != nil {
			return nil, err
		}
		return func(s string) bool { return strings.HasSuffix(s, args[0]) }, nil
	case "has_substring":
		if err := one(); err != nil {
			return nil, err
		}
		return func(s string) bool { return strings.Contains(s, args[0]) }, nil
	case "one_of":
		return func(s string) bool {
			for _, a := range args {
				if s == a {
					return true
				}
			}
			return false
		}, nil
	case "monitoring.regex.full_match":
		if err := one(); err != nil {
			return nil, err
		}
		re, err := regexp.Compile(`^(?:` + args[0] + `)$`)
		if err != nil {
			return nil, badFilter(src, "the regular expression is not RE2: %v", err)
		}
		return re.MatchString, nil
	}
	return nil, badFilter(src, "unknown function %s", name)
}
