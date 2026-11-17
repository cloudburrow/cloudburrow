// Package iamcond compiles and evaluates the condition of an IAM policy
// binding (#1742, #1745): the subset of CEL Google's IAM Conditions accept,
// with every attribute and function Google's "Attribute reference for IAM
// Conditions" (docs.cloud.google.com/iam/docs/conditions-attribute-reference,
// read 2026-11-17) documents for an allow policy's role binding.
//
// Supported, as that page lists them:
//
//   - the logical operators &&, || and !, at most 12 of them in one
//     expression (Google's IAM quotas page, "Logic operators in a role
//     binding's condition expression");
//   - resource.name: ==, !=, startsWith(), endsWith(), extract();
//   - resource.type and resource.service: == and !=;
//   - resource tags: resource.hasTagKey(key), resource.hasTagKeyId(id),
//     resource.matchTag(key, value) and resource.matchTagId(keyID,
//     valueID), over the resource's effective tags (its own bindings and
//     those it inherits);
//   - request.time: <, <=, >, >=, == and !=, against timestamp("RFC 3339"),
//     date("YYYY-MM-DD"), and either of them plus or minus duration("Ns");
//     getDate, getDayOfMonth, getDayOfWeek, getDayOfYear, getFullYear,
//     getHours, getMilliseconds, getMinutes, getMonth and getSeconds, each
//     with an optional time zone (UTC by default), with the ranges the
//     page gives (getDate is 1-based, getDayOfMonth 0-based, Sunday is 0,
//     January is 0);
//   - request.path and request.host (strings: IAP for App Engine and
//     Cloud Run), request.auth.access_levels (a list of strings: IAP),
//     destination.ip (a string) and destination.port (an integer: IAP TCP
//     forwarding);
//   - api.getAttribute(name, default), whose type is its default's, and a
//     list's hasOnly(list);
//   - compute.isForwardingRuleCreationOperation() and
//     compute.matchLoadBalancingSchemes(list);
//   - string, integer and boolean literals, parentheses, and `in` against
//     a list.
//
// principal.type and principal.subject are Principal Access Boundary
// attributes: the page documents them for that policy's bindings only,
// and Compile refuses them in a role binding. Compile also refuses
// another attribute or function, and an expression that is not a boolean.
//
// Evaluation follows Google's allow policies: a role is granted only when
// its condition evaluates to true. An expression that cannot be evaluated
// (an invalid timestamp() argument, an unknown time zone, an attribute the
// checked request or resource does not have: request.path on a request
// that is not IAP's, tags where the check does not know them) grants
// nothing ("If the input parameter is invalid, then the condition will
// never allow access to any resource"; "the part of the expression using
// it never grants access").
package iamcond

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// MaxLogicalOperators is Google's limit on the logic operators of one
// allow policy binding's condition.
const MaxLogicalOperators = 12

// Attributes are what a permission check knows about its request. An empty
// resource attribute is one the checked resource does not have: an
// expression that reads it is not evaluated, and grants nothing.
type Attributes struct {
	ResourceName    string
	ResourceType    string
	ResourceService string
	RequestTime     time.Time

	// Tags are the resource's effective tags, read only when TagsKnown:
	// a check that does not know them grants nothing on a tag condition.
	Tags      []Tag
	TagsKnown bool

	// RequestPath and RequestHost are an IAP request's URL path and host.
	RequestPath string
	RequestHost string
	// AccessLevels are the Access Context Manager levels an IAP request
	// satisfies; nil when the request is not IAP's.
	AccessLevels []string
	// DestinationIP and DestinationPort are an IAP TCP forwarding
	// tunnel's destination; 0 is no port.
	DestinationIP   string
	DestinationPort int64

	// API are the request's API attributes (api.getAttribute): a list of
	// strings is a []string; a key absent here returns the default.
	API map[string]any

	// ForwardingRule is set on a Compute Engine request: whether it
	// creates a forwarding rule, and the load balancing schemes it
	// affects.
	ForwardingRule *ForwardingRule
}

// Tag is one effective tag of a resource: its namespaced key and short
// value (resource.matchTag), and their permanent IDs (resource.matchTagId).
type Tag struct {
	NamespacedKey string // "123456789012/env"
	ShortValue    string // "prod"
	KeyID         string // "tagKeys/123"
	ValueID       string // "tagValues/456"
}

// ForwardingRule is what compute.* conditions read.
type ForwardingRule struct {
	Creation bool
	Schemes  []string
}

// Expr is a compiled condition.
type Expr struct{ root node }

// Compile parses and type-checks a condition expression.
func Compile(src string) (*Expr, error) {
	p := &parser{src: src}
	if err := p.lex(); err != nil {
		return nil, err
	}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		t := p.peek()
		return nil, fmt.Errorf("syntax error at column %d: unexpected %q", t.pos+1, t.text)
	}
	if p.logical > MaxLogicalOperators {
		return nil, fmt.Errorf("the expression has %d logical operators; a condition allows at most %d", p.logical, MaxLogicalOperators)
	}
	ty, err := n.check()
	if err != nil {
		return nil, err
	}
	if ty != tyBool {
		return nil, fmt.Errorf("the expression is a %s; a condition must be a boolean", ty)
	}
	return &Expr{root: n}, nil
}

// Eval is whether the condition grants under attrs: false when it
// evaluates to false or cannot be evaluated.
func (e *Expr) Eval(attrs Attributes) bool {
	if e == nil {
		return false
	}
	v, err := e.root.eval(&attrs)
	if err != nil {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// Eval compiles src and evaluates it: false when it does not compile.
func Eval(src string, attrs Attributes) bool {
	e, err := Compile(src)
	if err != nil {
		return false
	}
	return e.Eval(attrs)
}

// ---- types and values ----

type ty string

const (
	tyBool      ty = "bool"
	tyInt       ty = "int"
	tyString    ty = "string"
	tyTimestamp ty = "google.protobuf.Timestamp"
	tyDuration  ty = "google.protobuf.Duration"
	tyList      ty = "list"
)

// ---- lexer ----

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tString
	tInt
	tOp
)

type token struct {
	kind tokKind
	text string
	str  string
	num  int64
	pos  int
}

type parser struct {
	src     string
	toks    []token
	i       int
	logical int
}

func (p *parser) lex() error {
	s := p.src
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '_' || unicode.IsLetter(rune(c)):
			j := i
			for j < len(s) && (s[j] == '_' || unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j]))) {
				j++
			}
			p.toks = append(p.toks, token{kind: tIdent, text: s[i:j], pos: i})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			n, err := strconv.ParseInt(s[i:j], 10, 64)
			if err != nil {
				return fmt.Errorf("syntax error at column %d: integer %s out of range", i+1, s[i:j])
			}
			p.toks = append(p.toks, token{kind: tInt, text: s[i:j], num: n, pos: i})
			i = j
		case c == '"' || c == '\'':
			j := i + 1
			var b strings.Builder
			for {
				if j >= len(s) {
					return fmt.Errorf("syntax error at column %d: unterminated string", i+1)
				}
				if s[j] == c {
					j++
					break
				}
				if s[j] == '\\' && j+1 < len(s) {
					switch s[j+1] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case '\\', '"', '\'':
						b.WriteByte(s[j+1])
					default:
						return fmt.Errorf("syntax error at column %d: invalid escape \\%c", j+1, s[j+1])
					}
					j += 2
					continue
				}
				b.WriteByte(s[j])
				j++
			}
			p.toks = append(p.toks, token{kind: tString, text: s[i:j], str: b.String(), pos: i})
			i = j
		default:
			op := ""
			for _, o := range []string{"&&", "||", "==", "!=", "<=", ">=", "<", ">", "!", "(", ")", ".", ",", "[", "]", "+", "-"} {
				if strings.HasPrefix(s[i:], o) {
					op = o
					break
				}
			}
			if op == "" {
				return fmt.Errorf("syntax error at column %d: unexpected character %q", i+1, c)
			}
			p.toks = append(p.toks, token{kind: tOp, text: op, pos: i})
			i += len(op)
		}
	}
	p.toks = append(p.toks, token{kind: tEOF, text: "end of expression", pos: len(s)})
	return nil
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }

func (p *parser) isOp(op string) bool {
	t := p.peek()
	return t.kind == tOp && t.text == op
}

func (p *parser) expect(op string) error {
	if !p.isOp(op) {
		t := p.peek()
		return fmt.Errorf("syntax error at column %d: expected %q, found %q", t.pos+1, op, t.text)
	}
	p.i++
	return nil
}

// ---- parser ----

func (p *parser) parseOr() (node, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.isOp("||") {
		p.i++
		p.logical++
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &logical{op: "||", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (node, error) {
	l, err := p.parseRel()
	if err != nil {
		return nil, err
	}
	for p.isOp("&&") {
		p.i++
		p.logical++
		r, err := p.parseRel()
		if err != nil {
			return nil, err
		}
		l = &logical{op: "&&", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseRel() (node, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.kind == tOp && (t.text == "==" || t.text == "!=" || t.text == "<" || t.text == "<=" || t.text == ">" || t.text == ">="):
		case t.kind == tIdent && t.text == "in":
		default:
			return l, nil
		}
		p.i++
		r, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		l = &binary{op: t.text, l: l, r: r, pos: t.pos}
	}
}

func (p *parser) parseAdd() (node, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.isOp("+") || p.isOp("-") {
		t := p.next()
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &binary{op: t.text, l: l, r: r, pos: t.pos}
	}
	return l, nil
}

func (p *parser) parseUnary() (node, error) {
	if p.isOp("!") {
		p.i++
		p.logical++
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &not{x: x}, nil
	}
	if p.isOp("-") {
		t := p.next()
		if p.peek().kind != tInt {
			return nil, fmt.Errorf("syntax error at column %d: unary minus applies only to an integer here", t.pos+1)
		}
		n := p.next()
		return &lit{v: -n.num, t: tyInt}, p.postfixCheck()
	}
	return p.parsePostfix()
}

// postfixCheck refuses a member access on a negated literal.
func (p *parser) postfixCheck() error {
	if p.isOp(".") {
		return fmt.Errorf("syntax error at column %d: unexpected \".\"", p.peek().pos+1)
	}
	return nil
}

func (p *parser) parsePostfix() (node, error) {
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.isOp(".") {
		p.i++
		t := p.next()
		if t.kind != tIdent {
			return nil, fmt.Errorf("syntax error at column %d: expected a name after \".\", found %q", t.pos+1, t.text)
		}
		if p.isOp("(") {
			args, err := p.parseArgs()
			if err != nil {
				return nil, err
			}
			x = &call{fn: t.text, recv: x, args: args, pos: t.pos}
			continue
		}
		x = &selectN{x: x, field: t.text, pos: t.pos}
	}
	return x, nil
}

func (p *parser) parseArgs() ([]node, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var args []node
	if p.isOp(")") {
		p.i++
		return args, nil
	}
	for {
		a, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		if p.isOp(",") {
			p.i++
			continue
		}
		return args, p.expect(")")
	}
}

func (p *parser) parsePrimary() (node, error) {
	t := p.next()
	switch t.kind {
	case tString:
		return &lit{v: t.str, t: tyString}, nil
	case tInt:
		return &lit{v: t.num, t: tyInt}, nil
	case tIdent:
		switch t.text {
		case "true":
			return &lit{v: true, t: tyBool}, nil
		case "false":
			return &lit{v: false, t: tyBool}, nil
		}
		if p.isOp("(") {
			args, err := p.parseArgs()
			if err != nil {
				return nil, err
			}
			return &call{fn: t.text, args: args, pos: t.pos}, nil
		}
		return &ident{name: t.text, pos: t.pos}, nil
	case tOp:
		switch t.text {
		case "(":
			x, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			return x, p.expect(")")
		case "[":
			var elems []node
			if p.isOp("]") {
				p.i++
				return &list{elems: elems}, nil
			}
			for {
				e, err := p.parseOr()
				if err != nil {
					return nil, err
				}
				elems = append(elems, e)
				if p.isOp(",") {
					p.i++
					continue
				}
				return &list{elems: elems}, p.expect("]")
			}
		}
	}
	return nil, fmt.Errorf("syntax error at column %d: unexpected %q", t.pos+1, t.text)
}
