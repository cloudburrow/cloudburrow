package logfilter

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Record is one log entry as the language sees it, in LogEntry's JSON shape
// (camelCase names; nested objects as map[string]any; repeated fields as
// []any). Field returns one top-level field, so a filter on logName or
// severity need not build the whole entry; Root returns all of it, for
// global restrictions and SEARCH.
type Record interface {
	Field(name string) (any, bool)
	Root() map[string]any
}

// Map is a Record held as one JSON-shaped map.
type Map map[string]any

// Field is the top-level field name.
func (m Map) Field(name string) (any, bool) { v, ok := m[name]; return v, ok }

// Root is m.
func (m Map) Root() map[string]any { return m }

// Options are what a caller adds to the language.
type Options struct {
	// Source evaluates source("RESOURCE"); nil refuses the function.
	Source func(arg string) (func(Record) bool, error)
}

// Filter is a parsed query. A nil *Filter (an empty query) matches every
// entry.
type Filter struct{ root node }

// Match reports whether r satisfies the filter.
func (f *Filter) Match(r Record) bool { return f == nil || f.root.eval(r) }

// Parse parses a query; an empty or comment-only query is a nil Filter.
func Parse(src string, opts Options) (*Filter, error) {
	if len(src) > MaxLen {
		return nil, errorf("Invalid filter: the filter is longer than %d characters", MaxLen)
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	if toks[0].kind == tEOF {
		return nil, nil
	}
	p := &parser{toks: toks, opts: opts}
	n, perr := p.expr()
	if perr != nil {
		return nil, perr
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, syntaxErr(t)
	}
	return &Filter{root: n}, nil
}

type parser struct {
	toks []token
	i    int
	opts Options
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) peekN(n int) token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func isKeyword(t token, kw string) bool { return t.kind == tWord && t.text == kw }

// startsTerm is whether t can begin a term.
func startsTerm(t token) bool {
	switch t.kind {
	case tWord:
		return t.text != "AND" && t.text != "OR"
	case tString, tLParen, tMinus:
		return true
	}
	return false
}

func (p *parser) expr() (node, *Error) {
	first, err := p.or()
	if err != nil {
		return nil, err
	}
	all := andNode{first}
	for {
		t := p.peek()
		if isKeyword(t, "AND") {
			p.next()
			if !startsTerm(p.peek()) {
				return nil, syntaxErr(p.peek())
			}
		} else if !startsTerm(t) {
			break
		}
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		all = append(all, n)
	}
	if len(all) == 1 {
		return first, nil
	}
	return all, nil
}

func (p *parser) or() (node, *Error) {
	first, err := p.unary()
	if err != nil {
		return nil, err
	}
	alts := orNode{first}
	for isKeyword(p.peek(), "OR") {
		p.next()
		if !startsTerm(p.peek()) {
			return nil, syntaxErr(p.peek())
		}
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		alts = append(alts, n)
	}
	if len(alts) == 1 {
		return first, nil
	}
	return alts, nil
}

func (p *parser) unary() (node, *Error) {
	if t := p.peek(); isKeyword(t, "NOT") || t.kind == tMinus {
		p.next()
		if !startsTerm(p.peek()) {
			return nil, syntaxErr(p.peek())
		}
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	}
	return p.primary()
}

// functions are the language's, lower-cased; value functions stand on a
// comparison's left, the others are terms of their own.
var functions = map[string]bool{"search": false, "log_id": false, "sample": false, "ip_in_net": false,
	"source": false, "cast": true, "regexp_extract": true}

func (p *parser) primary() (node, *Error) {
	t := p.peek()
	switch t.kind {
	case tLParen:
		p.next()
		n, err := p.expr()
		if err != nil {
			return nil, err
		}
		if r := p.next(); r.kind != tRParen {
			return nil, syntaxErr(r)
		}
		return n, nil
	case tString:
		p.next()
		if p.peek().kind == tOp {
			return nil, syntaxErr(p.peek())
		}
		return globalNode{newLit(t)}, nil
	case tWord:
		if t.text == "AND" || t.text == "OR" || t.text == "NOT" {
			return nil, syntaxErr(t)
		}
		name := strings.ToLower(t.text)
		if isValue, ok := functions[name]; ok && p.peekN(1).kind == tLParen {
			if isValue {
				lhs, err := p.valueFunc(true)
				if err != nil {
					return nil, err
				}
				return p.comparison(lhs)
			}
			return p.boolFunc()
		}
		if p.peekN(1).kind == tLParen && p.peekN(1).pos == t.end {
			return nil, errorf("Invalid filter: unknown function %q at line %d, column %d", t.text, t.line, t.col)
		}
		start := p.i
		segs, err := p.path()
		if err != nil {
			return nil, err
		}
		if p.peek().kind == tOp {
			fi, ferr := resolveField(segs)
			if ferr != nil {
				return nil, ferr
			}
			return p.comparison(pathLHS{fi})
		}
		src := t
		src.text = sourceText(p.toks[start:p.i])
		src.val = src.text
		return globalNode{newLit(src)}, nil
	}
	return nil, syntaxErr(t)
}

// sourceText rejoins adjacent tokens as written.
func sourceText(ts []token) string {
	var b strings.Builder
	for _, t := range ts {
		if t.kind == tString {
			b.WriteString(t.val)
		} else {
			b.WriteString(t.text)
		}
	}
	return b.String()
}

// path reads a field path: dotted words, where a segment with special
// characters is double-quoted (labels."compute.googleapis.com/resource_id").
func (p *parser) path() ([]string, *Error) {
	t := p.next()
	var segs []string
	add := func(t token, text string) *Error {
		for _, s := range strings.Split(text, ".") {
			if s == "" {
				return syntaxErr(t)
			}
			segs = append(segs, s)
		}
		return nil
	}
	text := t.text
	for {
		open := strings.HasSuffix(text, ".")
		if err := add(t, strings.TrimSuffix(text, ".")); err != nil {
			return nil, err
		}
		if !open {
			return segs, nil
		}
		q := p.peek()
		if q.kind != tString || q.pos != t.end {
			return nil, syntaxErr(q)
		}
		p.next()
		segs = append(segs, q.val)
		w := p.peek()
		if w.kind != tWord || w.pos != q.end || !strings.HasPrefix(w.text, ".") {
			return segs, nil
		}
		p.next()
		t, text = w, w.text[1:]
		if text == "" {
			return nil, syntaxErr(w)
		}
	}
}

func (p *parser) comparison(lhs lhsExpr) (node, *Error) {
	opTok := p.next()
	op := opTok.text
	if op == ":" && p.peek().kind == tStar {
		p.next()
		return presenceNode{lhs}, nil
	}
	r, err := p.rhs(op)
	if err != nil {
		return nil, err
	}
	return cmpNode{lhs: lhs, op: op, rhs: r}, nil
}

func (p *parser) rhs(op string) (rhsNode, *Error) {
	if p.peek().kind == tLParen {
		p.next()
		r, err := p.vAnd(op)
		if err != nil {
			return nil, err
		}
		if t := p.next(); t.kind != tRParen {
			return nil, syntaxErr(t)
		}
		return r, nil
	}
	return p.literal(op)
}

func startsValue(t token) bool {
	switch t.kind {
	case tWord:
		return t.text != "AND" && t.text != "OR"
	case tString, tLParen, tMinus:
		return true
	}
	return false
}

func (p *parser) vAnd(op string) (rhsNode, *Error) {
	first, err := p.vOr(op)
	if err != nil {
		return nil, err
	}
	all := rhsAnd{first}
	for {
		t := p.peek()
		if isKeyword(t, "AND") {
			p.next()
		} else if !startsValue(t) {
			break
		}
		n, err := p.vOr(op)
		if err != nil {
			return nil, err
		}
		all = append(all, n)
	}
	if len(all) == 1 {
		return first, nil
	}
	return all, nil
}

func (p *parser) vOr(op string) (rhsNode, *Error) {
	first, err := p.vUnary(op)
	if err != nil {
		return nil, err
	}
	alts := rhsOr{first}
	for isKeyword(p.peek(), "OR") {
		p.next()
		n, err := p.vUnary(op)
		if err != nil {
			return nil, err
		}
		alts = append(alts, n)
	}
	if len(alts) == 1 {
		return first, nil
	}
	return alts, nil
}

func (p *parser) vUnary(op string) (rhsNode, *Error) {
	t := p.peek()
	negNumber := t.kind == tMinus && p.peekN(1).kind == tWord && p.peekN(1).pos == t.end && isNumber(p.peekN(1).text)
	if isKeyword(t, "NOT") || (t.kind == tMinus && !negNumber) {
		p.next()
		n, err := p.vUnary(op)
		if err != nil {
			return nil, err
		}
		return rhsNot{n}, nil
	}
	if t.kind == tLParen {
		p.next()
		n, err := p.vAnd(op)
		if err != nil {
			return nil, err
		}
		if r := p.next(); r.kind != tRParen {
			return nil, syntaxErr(r)
		}
		return n, nil
	}
	return p.literal(op)
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func (p *parser) literal(op string) (rhsNode, *Error) {
	t := p.next()
	switch t.kind {
	case tMinus:
		w := p.next()
		if w.kind != tWord || w.pos != t.end || !isNumber(w.text) {
			return nil, syntaxErr(w)
		}
		t = token{kind: tWord, text: "-" + w.text, val: "-" + w.text, pos: t.pos, end: w.end, line: t.line, col: t.col}
	case tWord:
		if t.text == "AND" || t.text == "OR" || t.text == "NOT" {
			return nil, syntaxErr(t)
		}
	case tString:
	default:
		return nil, syntaxErr(t)
	}
	l := newLit(t)
	if op == "=~" || op == "!~" {
		if t.kind != tString {
			return nil, errorf("Invalid filter: the regular expression %s must be double-quoted (line %d, column %d)", t.text, t.line, t.col)
		}
		re, err := regexp.Compile(t.val)
		if err != nil {
			return nil, errorf("Invalid filter: invalid regular expression %s: %v", t.text, err)
		}
		l.re = re
	}
	return l, nil
}

// valueFunc is cast(...) or regexp_extract(...), which yield a value: a
// comparison's left side (compared) or another value function's argument.
func (p *parser) valueFunc(compared bool) (lhsExpr, *Error) {
	name := p.next()
	p.next() // (
	fn := strings.ToLower(name.text)
	inner, err := p.valueArg()
	if err != nil {
		return nil, err
	}
	if c := p.next(); c.kind != tComma {
		return nil, syntaxErr(c)
	}
	var out lhsExpr
	switch fn {
	case "cast":
		typ := p.next()
		if typ.kind != tWord {
			return nil, syntaxErr(typ)
		}
		ce := castLHS{inner: inner, to: strings.ToUpper(typ.text), loc: time.UTC}
		switch ce.to {
		case "STRING", "INT64", "FLOAT64", "BOOL", "TIMESTAMP", "DURATION":
		default:
			return nil, errorf("Invalid filter: CAST to %s is not supported; the types are STRING, INT64, FLOAT64, BOOL, TIMESTAMP and DURATION", typ.text)
		}
		if p.peek().kind == tComma {
			p.next()
			opt := p.next()
			if opt.kind != tWord || strings.ToUpper(opt.text) != "TIME_ZONE" || p.next().kind != tLParen {
				return nil, syntaxErr(opt)
			}
			z := p.next()
			if z.kind != tString {
				return nil, syntaxErr(z)
			}
			loc, lerr := time.LoadLocation(z.val)
			if lerr != nil {
				return nil, errorf("Invalid filter: unknown time zone %q", z.val)
			}
			ce.loc = loc
			if r := p.next(); r.kind != tRParen {
				return nil, syntaxErr(r)
			}
		}
		out = ce
	default: // regexp_extract
		re := p.next()
		if re.kind != tString {
			return nil, syntaxErr(re)
		}
		rx, rerr := regexp.Compile(re.val)
		if rerr != nil {
			return nil, errorf("Invalid filter: invalid regular expression %s: %v", re.text, rerr)
		}
		if rx.NumSubexp() != 1 {
			return nil, errorf("Invalid filter: REGEXP_EXTRACT's regular expression %s must have exactly one capture group", re.text)
		}
		out = extractLHS{inner: inner, re: rx}
	}
	if r := p.next(); r.kind != tRParen {
		return nil, syntaxErr(r)
	}
	if compared && p.peek().kind != tOp {
		return nil, errorf("Invalid filter: %s(...) yields a value; compare it, as in %s(...) = VALUE", strings.ToUpper(fn), strings.ToUpper(fn))
	}
	return out, nil
}

// valueArg is a function's value argument: a field path or a nested value
// function.
func (p *parser) valueArg() (lhsExpr, *Error) {
	t := p.peek()
	if t.kind != tWord {
		return nil, syntaxErr(t)
	}
	if isValue := functions[strings.ToLower(t.text)]; isValue && p.peekN(1).kind == tLParen {
		return p.valueFunc(false)
	}
	segs, err := p.path()
	if err != nil {
		return nil, err
	}
	fi, ferr := resolveField(segs)
	if ferr != nil {
		return nil, ferr
	}
	return pathLHS{fi}, nil
}

// boolFunc is SEARCH, log_id, sample, ip_in_net or source.
func (p *parser) boolFunc() (node, *Error) {
	name := p.next()
	p.next() // (
	fn := strings.ToLower(name.text)
	var args []token
	var field *fieldInfo
	for {
		t := p.peek()
		if t.kind == tRParen {
			p.next()
			break
		}
		if len(args) > 0 || field != nil {
			if t.kind != tComma {
				return nil, syntaxErr(t)
			}
			p.next()
			t = p.peek()
		}
		switch {
		case t.kind == tWord && field == nil && len(args) == 0 && (fn == "search" || fn == "sample" || fn == "ip_in_net"):
			segs, err := p.path()
			if err != nil {
				return nil, err
			}
			fi, ferr := resolveField(segs)
			if ferr != nil {
				return nil, ferr
			}
			field = &fi
		case t.kind == tString || t.kind == tWord:
			p.next()
			args = append(args, t)
		default:
			return nil, syntaxErr(t)
		}
	}
	usage := func(form string) *Error {
		return errorf("Invalid filter: %s takes %s (line %d, column %d)", name.text, form, name.line, name.col)
	}
	str := func(i int) (string, bool) {
		if i < len(args) && args[i].kind == tString {
			return args[i].val, true
		}
		return "", false
	}
	switch fn {
	case "search":
		q, ok := str(0)
		if !ok || len(args) != 1 {
			return nil, usage(`a query string: SEARCH("query") or SEARCH(FIELD, "query")`)
		}
		return newSearch(field, q), nil
	case "log_id":
		id, ok := str(0)
		if !ok || len(args) != 1 || field != nil {
			return nil, usage(`one double-quoted log ID: log_id("ID")`)
		}
		return logIDNode{id}, nil
	case "source":
		arg := ""
		if len(args) == 1 && field == nil {
			arg = args[0].val
		} else {
			return nil, usage(`one resource: source("projects/P")`)
		}
		if p.opts.Source == nil {
			return nil, errorf("Invalid filter: source() is not supported here")
		}
		pr, err := p.opts.Source(arg)
		if err != nil {
			if e, ok := err.(*Error); ok {
				return nil, e
			}
			return nil, &Error{Msg: err.Error()}
		}
		return funcNode(pr), nil
	case "sample":
		if field == nil || len(args) != 1 {
			return nil, usage("a field and a fraction: sample(insertId, 0.25)")
		}
		f, err := strconv.ParseFloat(args[0].val, 64)
		if err != nil || f <= 0 || f > 1 {
			return nil, errorf("Invalid filter: sample's fraction %s must be greater than 0 and at most 1", args[0].text)
		}
		return sampleNode{*field, f}, nil
	default: // ip_in_net
		subnet, ok := str(0)
		if field == nil || !ok || len(args) != 1 {
			return nil, usage(`a field and a double-quoted subnet: ip_in_net(httpRequest.remoteIp, "10.0.0.0/8")`)
		}
		pfx, err := parseNet(subnet)
		if err != nil {
			return nil, errorf("Invalid filter: ip_in_net's subnet %q is not an IP address or subnet", subnet)
		}
		return ipNode{*field, pfx}, nil
	}
}

// CheckView refuses what a log view filter may not hold, as the log views
// docs list it (docs.cloud.google.com/logging/docs/logs-views, "Filter
// syntax"): source() and log_id(), and comparisons of resource types
// (resource.type), resource labels (resource.labels.KEY), labels
// (labels.KEY) and App Hub labels (apphub.application.location and the
// like), with any of the language's comparison operators; joined by AND and
// OR; NOT on one source(), log_id() or comparison, never on a compound
// statement. Every other field (severity, logName, timestamp, payloads),
// every other function (SEARCH, sample, ip_in_net, cast, regexp_extract)
// and bare text (a global restriction) are refused.
func (f *Filter) CheckView() error {
	if f == nil {
		return nil
	}
	return checkView(f.root)
}

const viewTakes = "a log view filter takes source(), log_id() and comparisons of resource.type, resource.labels, labels and apphub fields only"

func checkView(n node) error {
	switch v := n.(type) {
	case andNode:
		for _, c := range v {
			if err := checkView(c); err != nil {
				return err
			}
		}
		return nil
	case orNode:
		for _, c := range v {
			if err := checkView(c); err != nil {
				return err
			}
		}
		return nil
	case notNode:
		switch v.n.(type) {
		case andNode, orNode, notNode:
			return errorf("Invalid filter: in a log view filter, NOT applies to one comparison, source() or log_id(), not to a compound statement")
		}
		return checkView(v.n)
	case logIDNode, funcNode:
		return nil
	case cmpNode:
		return checkViewField(v.lhs)
	case presenceNode:
		return checkViewField(v.lhs)
	case globalNode:
		return errorf("Invalid filter: %s; bare text (a global restriction) is not one", viewTakes)
	case searchNode:
		return errorf("Invalid filter: %s; SEARCH is not one", viewTakes)
	}
	return errorf("Invalid filter: %s", viewTakes)
}

// checkViewField refuses a comparison on a field a log view filter does not
// take.
func checkViewField(l lhsExpr) error {
	p, ok := l.(pathLHS)
	if !ok {
		return errorf("Invalid filter: %s; cast() and regexp_extract() are not", viewTakes)
	}
	if viewField(p.fi.path) {
		return nil
	}
	return errorf("Invalid filter: %s; %s is not one of them", viewTakes, strings.Join(p.fi.path, "."))
}

func viewField(path []string) bool {
	switch path[0] {
	case "resource":
		return len(path) == 2 && path[1] == "type" || len(path) >= 3 && path[1] == "labels"
	case "labels":
		return len(path) >= 2
	case "apphub":
		return len(path) >= 3
	}
	return false
}
